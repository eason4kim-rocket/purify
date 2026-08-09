// Package extract owns transport-neutral structured extraction orchestration.
// It reuses the canonical scrape service and keeps HTTP, Search, MCP, and
// future compiler paths from duplicating fetch, schema, evidence, and receipt
// behavior.
package extract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/llm"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/receipts"
	"github.com/use-agent/purify/scrape"
	"github.com/use-agent/purify/scraper"
)

// Runner is the canonical scrape boundary used by extraction.
type Runner interface {
	Run(context.Context, *models.ScrapeRequest, scrape.Observer) (*scrape.Result, error)
}

// StructuredExtractor is the bounded LLM extraction contract.
type StructuredExtractor interface {
	Extract(context.Context, string, json.RawMessage, llm.ExtractParams) (*llm.ExtractResult, error)
	ExtractWithRepair(context.Context, string, json.RawMessage, json.RawMessage, []llm.Violation, llm.ExtractParams) (*llm.ExtractResult, error)
}

// ReceiptSigner signs one field claim after evidence alignment.
type ReceiptSigner interface {
	Sign(receipts.Payload) (string, error)
}

// Artifact keeps the public cleaned page and its selected raw source together.
// Source may be nil only when an external caller deliberately supplies a
// response-only artifact; evidence and deterministic compilation require it.
type Artifact struct {
	Public *models.ScrapeResponse
	Source *scraper.ScrapeResult
}

// Config controls deterministic time injection in tests.
type Config struct {
	Now func() time.Time
}

// OperationError preserves phase timing while retaining the domain cause for
// errors.Is/errors.As and HTTP status mapping.
type OperationError struct {
	Cause  error
	Timing models.ExtractTimingInfo
}

func (e *OperationError) Error() string {
	if e == nil || e.Cause == nil {
		return "extract: operation failed"
	}
	return e.Cause.Error()
}

func (e *OperationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// ExtractTiming lets transport adapters recover phase timing without depending
// on this concrete error type.
func (e *OperationError) ExtractTiming() models.ExtractTimingInfo {
	if e == nil {
		return models.ExtractTimingInfo{}
	}
	return e.Timing
}

// TimingFromError returns phase timing attached by Service.
func TimingFromError(err error) (models.ExtractTimingInfo, bool) {
	var operationError *OperationError
	if !errors.As(err, &operationError) {
		return models.ExtractTimingInfo{}, false
	}
	return operationError.Timing, true
}

// Service coordinates canonical fetch, strict schema validation, one bounded
// repair, optional evidence alignment, and receipt signing.
type Service struct {
	runner    Runner
	extractor StructuredExtractor
	signer    ReceiptSigner
	now       func() time.Time
}

// NewService constructs an extraction service. signer may be nil when evidence
// mode is disabled; evidence requests then fail closed with EVIDENCE_UNAVAILABLE.
func NewService(runner Runner, extractor StructuredExtractor, signer ReceiptSigner, cfg Config) (*Service, error) {
	if runner == nil {
		return nil, errors.New("extract: canonical scrape runner is required")
	}
	if extractor == nil {
		return nil, errors.New("extract: structured extractor is required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{runner: runner, extractor: extractor, signer: signer, now: now}, nil
}

// Extract runs the complete LLM extraction path. The caller-owned request is
// never mutated and cache is forcibly bypassed so evidence is tied to a fresh,
// raw source rather than manufactured from a response-only cache entry.
func (s *Service) Extract(ctx context.Context, request *models.ExtractRequest) (*models.ExtractResponse, error) {
	startedAt := s.now()
	prepared, err := prepareRequest(request)
	if err != nil {
		return nil, s.operationError(err, startedAt, models.ExtractTimingInfo{})
	}

	fetchStartedAt := s.now()
	artifact, err := s.fetchArtifact(ctx, prepared.ToScrapeRequest())
	if err != nil {
		timing := models.ExtractTimingInfo{
			NavigationMs: elapsedMilliseconds(fetchStartedAt, s.now()),
		}
		return nil, s.operationError(err, startedAt, timing)
	}

	response, err := s.extractPreparedArtifact(ctx, artifact, prepared, startedAt)
	if err != nil {
		return nil, err
	}
	return response, nil
}

// FetchArtifact exposes one fresh canonical scrape for Search and other
// orchestrators that need to reuse the same fetch across multiple consumers.
func (s *Service) FetchArtifact(ctx context.Context, request *models.ScrapeRequest) (*Artifact, error) {
	if request == nil {
		return nil, models.NewScrapeError(models.ErrCodeInvalidInput, "scrape request is required", nil)
	}
	cloned := *request
	models.ApplyScrapeOptions(&cloned, models.ScrapeOptionsFromRequest(request))
	cloned.MaxAge = 0
	return s.fetchArtifact(ctx, &cloned)
}

// ExtractArtifact performs structured extraction against an already selected
// canonical artifact without fetching again.
func (s *Service) ExtractArtifact(ctx context.Context, artifact *Artifact, request *models.ExtractRequest) (*models.ExtractResponse, error) {
	startedAt := s.now()
	prepared, err := prepareRequest(request)
	if err != nil {
		return nil, s.operationError(err, startedAt, models.ExtractTimingInfo{})
	}
	return s.extractPreparedArtifact(ctx, artifact, prepared, startedAt)
}

func (s *Service) fetchArtifact(ctx context.Context, request *models.ScrapeRequest) (*Artifact, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	request.MaxAge = 0
	result, err := s.runner.Run(ctx, request, nil)
	if err != nil {
		return nil, err
	}
	if result == nil || result.Response == nil {
		return nil, models.NewScrapeError(models.ErrCodeInternal, "canonical scrape returned an empty result", nil)
	}
	if !result.Response.Success {
		code := models.ErrCodeNavigation
		message := "canonical scrape returned an unsuccessful response"
		if result.Response.Error != nil {
			if result.Response.Error.Code != "" {
				code = result.Response.Error.Code
			}
			if result.Response.Error.Message != "" {
				message = result.Response.Error.Message
			}
		}
		return nil, models.NewScrapeError(code, message, nil)
	}
	return &Artifact{Public: result.Response, Source: result.Source}, nil
}

func (s *Service) extractPreparedArtifact(
	ctx context.Context,
	artifact *Artifact,
	request *models.ExtractRequest,
	startedAt time.Time,
) (*models.ExtractResponse, error) {
	baseTiming, err := validateArtifact(artifact)
	if err != nil {
		return nil, s.operationError(err, startedAt, baseTiming)
	}
	if err := ctx.Err(); err != nil {
		return nil, s.operationError(err, startedAt, baseTiming)
	}

	extractionStartedAt := s.now()
	llmResult, violations, err := extractWithValidation(ctx, s.extractor, artifact.Public.Content, request.Schema, llm.ExtractParams{
		APIKey:  request.LLMAPIKey,
		Model:   request.LLMModel,
		BaseURL: request.LLMBaseURL,
	})
	extractionMs := elapsedMilliseconds(extractionStartedAt, s.now())
	baseTiming.ExtractionMs = extractionMs
	if err != nil {
		return nil, s.operationError(err, startedAt, baseTiming)
	}
	if llmResult == nil || len(llmResult.Data) == 0 || !json.Valid(llmResult.Data) {
		return nil, s.operationError(
			models.NewScrapeError(models.ErrCodeLLMFailure, "structured extractor returned invalid JSON", nil),
			startedAt,
			baseTiming,
		)
	}

	response := &models.ExtractResponse{
		Success:    true,
		Data:       append(json.RawMessage(nil), llmResult.Data...),
		Partial:    len(violations) > 0,
		Violations: append([]models.SchemaViolation(nil), violations...),
		Metadata:   artifact.Public.Metadata,
		Tokens:     artifact.Public.Tokens,
		LLMUsage:   cloneLLMUsage(llmResult.Usage),
	}

	if request.Evidence {
		if artifact.Source == nil || artifact.Source.SnapshotID == "" {
			return nil, s.operationError(
				models.NewScrapeError(models.ErrCodeEvidenceUnavailable, "evidence mode requires a fresh stored snapshot", nil),
				startedAt,
				baseTiming,
			)
		}
		if s.signer == nil {
			return nil, s.operationError(
				models.NewScrapeError(models.ErrCodeEvidenceUnavailable, "evidence receipt signer is unavailable", nil),
				startedAt,
				baseTiming,
			)
		}

		aligned, unlocatedRate := evidence.AlignAll(
			llmResult.Data,
			artifact.Public.Content,
			artifact.Source.RawHTML,
			string(artifact.Source.SnapshotID),
			artifact.Source.FetchedAt,
		)
		basis := models.EvidenceBasis(aligned)
		sourceURL := artifact.Source.FinalURL
		if sourceURL == "" {
			sourceURL = request.URL
		}
		receiptTokens, signErr := signFieldReceipts(
			llmResult.Data,
			basis,
			sourceURL,
			s.now().UTC(),
			s.signer,
		)
		if signErr != nil {
			return nil, s.operationError(
				models.NewScrapeError(models.ErrCodeInternal, "failed to sign evidence receipts", signErr),
				startedAt,
				baseTiming,
			)
		}
		typedReceipts := models.FieldReceipts(receiptTokens)
		response.SnapshotID = string(artifact.Source.SnapshotID)
		response.UnlocatedRate = &unlocatedRate
		response.Basis = &basis
		response.Receipts = &typedReceipts
	}

	baseTiming.TotalMs = elapsedMilliseconds(startedAt, s.now())
	response.Timing = baseTiming
	return response, nil
}

func prepareRequest(request *models.ExtractRequest) (*models.ExtractRequest, error) {
	if request == nil {
		return nil, models.NewScrapeError(models.ErrCodeInvalidInput, "extract request is required", nil)
	}
	prepared := *request
	prepared.Schema = append(json.RawMessage(nil), request.Schema...)
	if request.WaitForNetworkIdle != nil {
		value := *request.WaitForNetworkIdle
		prepared.WaitForNetworkIdle = &value
	}
	prepared.URL = strings.TrimSpace(prepared.URL)
	prepared.Defaults()
	parsed, err := url.ParseRequestURI(prepared.URL)
	if err != nil || parsed.Host == "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, models.NewScrapeError(models.ErrCodeInvalidInput, "url must be an absolute http or https URL", err)
	}
	if strings.TrimSpace(prepared.LLMAPIKey) == "" {
		return nil, models.NewScrapeError(models.ErrCodeInvalidInput, "llm_api_key is required for LLM extraction", nil)
	}
	normalizedSchema, err := llm.NormalizeSchema(prepared.Schema)
	if err != nil {
		return nil, models.NewScrapeError(models.ErrCodeInvalidInput, "invalid JSON schema", err)
	}
	if err := llm.ValidateSchema(normalizedSchema); err != nil {
		return nil, models.NewScrapeError(models.ErrCodeInvalidInput, "invalid JSON schema", err)
	}
	prepared.Schema = normalizedSchema
	return &prepared, nil
}

func validateArtifact(artifact *Artifact) (models.ExtractTimingInfo, error) {
	if artifact == nil || artifact.Public == nil {
		return models.ExtractTimingInfo{}, models.NewScrapeError(models.ErrCodeInternal, "extract artifact is empty", nil)
	}
	timing := models.ExtractTimingInfo{
		NavigationMs: artifact.Public.Timing.NavigationMs,
		CleaningMs:   artifact.Public.Timing.CleaningMs,
	}
	if !artifact.Public.Success {
		return timing, models.NewScrapeError(models.ErrCodeNavigation, "extract artifact is unsuccessful", nil)
	}
	return timing, nil
}

func extractWithValidation(
	ctx context.Context,
	client StructuredExtractor,
	content string,
	schema json.RawMessage,
	params llm.ExtractParams,
) (*llm.ExtractResult, []llm.Violation, error) {
	result, err := client.Extract(ctx, content, schema, params)
	if err != nil {
		return nil, nil, err
	}
	if result == nil || len(result.Data) == 0 {
		return nil, nil, models.NewScrapeError(models.ErrCodeLLMFailure, "structured extractor returned an empty result", nil)
	}
	if !json.Valid(result.Data) {
		return nil, nil, models.NewScrapeError(models.ErrCodeLLMFailure, "structured extractor returned invalid JSON", nil)
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
	if repaired == nil || len(repaired.Data) == 0 {
		return nil, nil, models.NewScrapeError(models.ErrCodeLLMFailure, "structured extractor returned an empty repair", nil)
	}
	if !json.Valid(repaired.Data) {
		return nil, nil, models.NewScrapeError(models.ErrCodeLLMFailure, "structured extractor returned invalid repair JSON", nil)
	}
	repaired.Usage = addLLMUsage(result.Usage, repaired.Usage)

	remaining, err := llm.ValidateAgainstSchema(schema, repaired.Data)
	if err != nil {
		return nil, nil, models.NewScrapeError(models.ErrCodeInvalidInput, "invalid JSON schema", err)
	}
	return repaired, remaining, nil
}

func signFieldReceipts(
	data json.RawMessage,
	basis models.EvidenceBasis,
	sourceURL string,
	issuedAt time.Time,
	signer ReceiptSigner,
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

func cloneLLMUsage(usage *models.LLMUsage) *models.LLMUsage {
	if usage == nil {
		return nil
	}
	cloned := *usage
	return &cloned
}

func (s *Service) operationError(cause error, startedAt time.Time, timing models.ExtractTimingInfo) error {
	if cause == nil {
		cause = models.NewScrapeError(models.ErrCodeInternal, "unknown extraction failure", nil)
	} else if errors.Is(cause, context.DeadlineExceeded) || errors.Is(cause, context.Canceled) {
		var scrapeError *models.ScrapeError
		if !errors.As(cause, &scrapeError) {
			cause = models.NewScrapeError(models.ErrCodeTimeout, "extract request deadline exceeded", cause)
		}
	}
	timing.TotalMs = elapsedMilliseconds(startedAt, s.now())
	return &OperationError{Cause: cause, Timing: timing}
}

func elapsedMilliseconds(startedAt, finishedAt time.Time) int64 {
	if finishedAt.Before(startedAt) {
		return 0
	}
	return finishedAt.Sub(startedAt).Milliseconds()
}
