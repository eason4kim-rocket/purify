package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/use-agent/purify/cleaner"
	"github.com/use-agent/purify/llm"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/scraper"
)

type structuredExtractor interface {
	Extract(context.Context, string, json.RawMessage, llm.ExtractParams) (*llm.ExtractResult, error)
	ExtractWithRepair(context.Context, string, json.RawMessage, json.RawMessage, []llm.Violation, llm.ExtractParams) (*llm.ExtractResult, error)
}

// Extract returns a handler for POST /api/v1/extract.
//
// Flow:
//  1. Parse & validate ExtractRequest, apply defaults.
//  2. DoScrape → raw HTML + JS title.
//  3. Clean (with optional CSS selector) → content.
//  4. LLM Extract → structured JSON.
//  5. Assemble response with timing and LLM usage.
func Extract(sc *scraper.Scraper, cl *cleaner.Cleaner, llmClient structuredExtractor) gin.HandlerFunc {
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
		c.JSON(http.StatusOK, models.ExtractResponse{
			Success:    true,
			Data:       llmResult.Data,
			Partial:    len(violations) > 0,
			Violations: violations,
			Metadata:   scrapeResp.Metadata,
			Tokens:     scrapeResp.Tokens,
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
	default:
		return http.StatusInternalServerError
	}
}
