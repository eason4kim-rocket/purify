package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/use-agent/purify/cleaner"
	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/llm"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/receipts"
	"github.com/use-agent/purify/scraper"
)

type structuredExtractor interface {
	Extract(context.Context, string, json.RawMessage, llm.ExtractParams) (*llm.ExtractResult, error)
	ExtractWithRepair(context.Context, string, json.RawMessage, json.RawMessage, []llm.Violation, llm.ExtractParams) (*llm.ExtractResult, error)
}

type fieldReceiptSigner interface {
	Sign(receipts.Payload) (string, error)
}

// Extract returns a handler for POST /api/v1/extract.
//
// Flow:
//  1. Parse & validate ExtractRequest, apply defaults.
//  2. DoScrape → raw HTML + JS title.
//  3. Clean (with optional CSS selector) → content.
//  4. LLM Extract → structured JSON.
//  5. Assemble response with timing and LLM usage.
func Extract(sc *scraper.Scraper, cl *cleaner.Cleaner, llmClient structuredExtractor, receiptSigner fieldReceiptSigner) gin.HandlerFunc {
	return func(c *gin.Context) {
		totalStart := time.Now()

		// ── 1. Parse request ────────────────────────────────────────
		var req models.ExtractRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, models.ExtractResponse{
				Success: false,
				Error: &models.ErrorDetail{
					Code:    models.ErrCodeInvalidInput,
					Message: err.Error(),
				},
			})
			return
		}
		req.Defaults()
		normalizedSchema, err := llm.NormalizeSchema(req.Schema)
		if err != nil {
			respondExtractError(c, models.NewScrapeError(models.ErrCodeInvalidInput, "invalid JSON schema", err), models.ExtractTimingInfo{
				TotalMs: time.Since(totalStart).Milliseconds(),
			})
			return
		}
		if err := llm.ValidateSchema(normalizedSchema); err != nil {
			respondExtractError(c, models.NewScrapeError(models.ErrCodeInvalidInput, "invalid JSON schema", err), models.ExtractTimingInfo{
				TotalMs: time.Since(totalStart).Milliseconds(),
			})
			return
		}
		req.Schema = normalizedSchema

		// ── 2. Scrape ───────────────────────────────────────────────
		scrapeReq := req.ToScrapeRequest()
		scrapeReq.Defaults()

		navStart := time.Now()
		result, err := sc.DoScrape(c.Request.Context(), scrapeReq)
		navigationMs := time.Since(navStart).Milliseconds()

		if err != nil {
			respondExtractError(c, err, models.ExtractTimingInfo{
				TotalMs:      time.Since(totalStart).Milliseconds(),
				NavigationMs: navigationMs,
			})
			return
		}

		// ── 3. Clean ────────────────────────────────────────────────
		cleanStart := time.Now()
		var cleanOpts []cleaner.CleanOptions
		if req.CSSSelector != "" {
			cleanOpts = append(cleanOpts, cleaner.CleanOptions{
				CSSSelector: req.CSSSelector,
			})
		}
		scrapeResp, err := cl.Clean(result.RawHTML, req.URL, req.OutputFormat, req.ExtractMode, cleanOpts...)
		cleaningMs := time.Since(cleanStart).Milliseconds()

		if err != nil {
			respondExtractError(c, err, models.ExtractTimingInfo{
				TotalMs:      time.Since(totalStart).Milliseconds(),
				NavigationMs: navigationMs,
				CleaningMs:   cleaningMs,
			})
			return
		}

		// Title fallback.
		if scrapeResp.Metadata.Title == "" {
			scrapeResp.Metadata.Title = result.Title
		}
		scrapeResp.Metadata.FetchMethod = result.FetchMethod

		// ── 4. LLM Extract ──────────────────────────────────────────
		extractStart := time.Now()
		llmResult, violations, err := extractWithValidation(c.Request.Context(), llmClient, scrapeResp.Content, req.Schema, llm.ExtractParams{
			APIKey:  req.LLMAPIKey,
			Model:   req.LLMModel,
			BaseURL: req.LLMBaseURL,
		})
		extractionMs := time.Since(extractStart).Milliseconds()

		if err != nil {
			respondExtractError(c, err, models.ExtractTimingInfo{
				TotalMs:      time.Since(totalStart).Milliseconds(),
				NavigationMs: navigationMs,
				CleaningMs:   cleaningMs,
				ExtractionMs: extractionMs,
			})
			return
		}

		// ── 5. Assemble response ────────────────────────────────────
		var basis *models.EvidenceBasis
		var fieldReceipts *models.FieldReceipts
		var unlocatedRate *float64
		var snapshotID string
		if req.Evidence {
			if result.SnapshotID == "" {
				respondExtractError(c, models.NewScrapeError(models.ErrCodeEvidenceUnavailable, "evidence mode requires snapshot storage", nil), models.ExtractTimingInfo{
					TotalMs:      time.Since(totalStart).Milliseconds(),
					NavigationMs: navigationMs,
					CleaningMs:   cleaningMs,
					ExtractionMs: extractionMs,
				})
				return
			}
			aligned, rate := evidence.AlignAll(llmResult.Data, scrapeResp.Content, result.RawHTML, string(result.SnapshotID), result.FetchedAt)
			typedBasis := models.EvidenceBasis(aligned)
			basis = &typedBasis
			unlocatedRate = &rate
			snapshotID = string(result.SnapshotID)
			sourceURL := result.FinalURL
			if sourceURL == "" {
				sourceURL = req.URL
			}
			signed, signErr := signFieldReceipts(llmResult.Data, typedBasis, sourceURL, time.Now().UTC(), receiptSigner)
			if signErr != nil {
				respondExtractError(c, models.NewScrapeError(models.ErrCodeInternal, "failed to sign evidence receipts", signErr), models.ExtractTimingInfo{
					TotalMs:      time.Since(totalStart).Milliseconds(),
					NavigationMs: navigationMs,
					CleaningMs:   cleaningMs,
					ExtractionMs: extractionMs,
				})
				return
			}
			typedReceipts := models.FieldReceipts(signed)
			fieldReceipts = &typedReceipts
		}
		c.JSON(http.StatusOK, models.ExtractResponse{
			Success:       true,
			Data:          llmResult.Data,
			Partial:       len(violations) > 0,
			Violations:    violations,
			SnapshotID:    snapshotID,
			UnlocatedRate: unlocatedRate,
			Basis:         basis,
			Receipts:      fieldReceipts,
			Metadata:      scrapeResp.Metadata,
			Tokens:        scrapeResp.Tokens,
			Timing: models.ExtractTimingInfo{
				TotalMs:      time.Since(totalStart).Milliseconds(),
				NavigationMs: navigationMs,
				CleaningMs:   cleaningMs,
				ExtractionMs: extractionMs,
			},
			LLMUsage: llmResult.Usage,
		})
	}
}

func signFieldReceipts(
	data json.RawMessage,
	basis models.EvidenceBasis,
	sourceURL string,
	issuedAt time.Time,
	signer fieldReceiptSigner,
) (map[string]string, error) {
	if signer == nil {
		return nil, errors.New("receipt signer is unavailable")
	}
	values, err := evidence.LeafValues(data)
	if err != nil {
		return nil, fmt.Errorf("decode leaf values: %w", err)
	}
	if len(values) != len(basis) {
		return nil, fmt.Errorf("evidence/value path count mismatch: %d != %d", len(basis), len(values))
	}
	paths := make([]string, 0, len(values))
	for path := range values {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	tokens := make(map[string]string, len(paths))
	for _, path := range paths {
		anchor, ok := basis[path]
		if !ok {
			return nil, fmt.Errorf("evidence anchor missing for %q", path)
		}
		token, err := signer.Sign(receipts.Payload{
			URL:      sourceURL,
			Path:     path,
			Value:    values[path],
			Anchor:   anchor,
			IssuedAt: issuedAt,
		})
		if err != nil {
			return nil, fmt.Errorf("sign %q: %w", path, err)
		}
		tokens[path] = token
	}
	return tokens, nil
}

// extractWithValidation performs exactly one initial extraction and, only
// when necessary, one repair attempt. It returns the best valid JSON value and
// any violations that remain after the bounded repair.
func extractWithValidation(
	ctx context.Context,
	client structuredExtractor,
	content string,
	schema json.RawMessage,
	params llm.ExtractParams,
) (*llm.ExtractResult, []llm.Violation, error) {
	result, err := client.Extract(ctx, content, schema, params)
	if err != nil {
		return nil, nil, err
	}

	violations, err := llm.ValidateAgainstSchema(schema, result.Data)
	if err != nil {
		return nil, nil, models.NewScrapeError(models.ErrCodeInvalidInput, "invalid JSON schema", err)
	}
	if len(violations) == 0 {
		return result, nil, nil
	}

	repaired, err := client.ExtractWithRepair(ctx, content, schema, result.Data, violations, params)
	if err != nil {
		return nil, nil, err
	}
	repaired.Usage = addLLMUsage(result.Usage, repaired.Usage)

	remaining, err := llm.ValidateAgainstSchema(schema, repaired.Data)
	if err != nil {
		return nil, nil, models.NewScrapeError(models.ErrCodeInvalidInput, "invalid JSON schema", err)
	}
	return repaired, remaining, nil
}

func addLLMUsage(first, second *models.LLMUsage) *models.LLMUsage {
	if first == nil && second == nil {
		return nil
	}
	total := &models.LLMUsage{}
	for _, usage := range []*models.LLMUsage{first, second} {
		if usage == nil {
			continue
		}
		total.PromptTokens += usage.PromptTokens
		total.CompletionTokens += usage.CompletionTokens
		total.TotalTokens += usage.TotalTokens
	}
	return total
}

// respondExtractError maps a ScrapeError to the correct HTTP status and writes
// a structured JSON error response for the extract endpoint.
func respondExtractError(c *gin.Context, err error, timing models.ExtractTimingInfo) {
	scrapeErr, ok := err.(*models.ScrapeError)
	if !ok {
		scrapeErr = models.NewScrapeError(models.ErrCodeInternal, err.Error(), err)
	}

	c.JSON(mapExtractErrorToStatus(scrapeErr), models.ExtractResponse{
		Success: false,
		Error:   scrapeErr.ToDetail(),
		Timing:  timing,
	})
}

// mapExtractErrorToStatus translates error codes to HTTP status codes,
// including LLM-specific codes.
func mapExtractErrorToStatus(e *models.ScrapeError) int {
	switch e.Code {
	case models.ErrCodeTimeout:
		return http.StatusGatewayTimeout
	case models.ErrCodeNavigation:
		return http.StatusBadGateway
	case models.ErrCodeInvalidInput:
		return http.StatusBadRequest
	case models.ErrCodeRateLimited, models.ErrCodeLLMRateLimited:
		return http.StatusTooManyRequests
	case models.ErrCodeUnauthorized, models.ErrCodeLLMAuthFailure:
		return http.StatusUnauthorized
	case models.ErrCodeLLMFailure:
		return http.StatusBadGateway
	case models.ErrCodeEvidenceUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
