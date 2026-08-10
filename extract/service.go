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
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/use-agent/purify/compiler"
	"github.com/use-agent/purify/consensus"
	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/llm"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/publicnet"
	"github.com/use-agent/purify/receipts"
	"github.com/use-agent/purify/scrape"
	"github.com/use-agent/purify/scraper"
	"github.com/use-agent/purify/verify/eav"
)

const (
	maximumExtractURLBytes        = 16 << 10
	maximumExtractSchemaBytes     = 512 << 10
	maximumExtractCredentialBytes = 16 << 10
	maximumExtractBaseURLBytes    = 16 << 10
	maximumExtractModelBytes      = 256
	maximumExtractArtifactBytes   = consensus.MaxSourceDataBytes
	defaultPublicArtifactTimeout  = 30
	extractDataPreflightURL       = "https://preflight.example.com/"
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

// CompiledRepository is the deterministic extractor cache boundary. A lookup
// is read-only except for the repository's attributable template-drift rule;
// callers must report only successful or required-empty executions.
type CompiledRepository interface {
	Lookup(context.Context, compiler.PageKey) (compiler.Extractor, bool, error)
	Touch(context.Context, compiler.PageKey, string) (compiler.Extractor, error)
	RecordEmpty(context.Context, compiler.PageKey, string) (compiler.Extractor, error)
	RecordUse(context.Context, compiler.PageKey, string, compiler.UseOutcome) (compiler.Extractor, error)
}

// CompileObserver admits only bounded provenance references into the managed
// compiler. Request credentials, provider settings, cleaned content, and LLM
// truth output are intentionally absent from this boundary.
type CompileObserver interface {
	Observe(context.Context, compiler.PageKey, string, time.Time, json.RawMessage) error
}

// SourceJudge decides whether one fetched source document is about the
// expected subject. eav.Judge satisfies it directly; provider configuration
// and caching belong to the adapter supplied at the wiring layer.
type SourceJudge interface {
	JudgeDocument(ctx context.Context, subject eav.Subject, doc eav.Document) (eav.Judgment, error)
}

// Artifact keeps the public cleaned page and its selected raw source together.
// Callers sharing one Artifact across content, verification, and structured
// extraction consumers must treat it as read-only. Source may be nil only when
// an external caller deliberately supplies a response-only artifact; evidence
// and deterministic compilation require it.
type Artifact struct {
	Public *models.ScrapeResponse
	Source *scraper.ScrapeResult
}

// Config controls deterministic time injection in tests.
type Config struct {
	Now                func() time.Time
	CompiledRepository CompiledRepository
	CompileObserver    CompileObserver
	// SafeProxyURL is the process-owned loopback SOCKS5 boundary required by
	// public artifact and multi-source fetching. It is never accepted from a
	// request.
	SafeProxyURL string
	// SourceSlots bounds source work across all concurrent multi requests made
	// through this Service. Zero selects the default of four.
	SourceSlots int
	// SourceJudge enables per-source entity attribution for multi-source
	// requests carrying an expected subject. Nil disables the capability.
	SourceJudge SourceJudge
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
	runner           Runner
	extractor        StructuredExtractor
	signer           ReceiptSigner
	compiled         CompiledRepository
	compileObserver  CompileObserver
	validateCompiled func(json.RawMessage, json.RawMessage) ([]llm.Violation, error)
	mergeMulti       func([]consensus.SourceResult) (consensus.Result, consensus.Materialization, error)
	encodeMulti      func(any) ([]byte, error)
	beforeMultiMerge func()
	now              func() time.Time
	safeProxyURL     string
	sourceSlots      chan struct{}
	sourceJudge      SourceJudge
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
	compiledRepository := cfg.CompiledRepository
	if isNilCompiledRepository(compiledRepository) {
		compiledRepository = nil
	}
	compileObserver := cfg.CompileObserver
	if isNilCompileObserver(compileObserver) {
		compileObserver = nil
	}
	sourceJudge := cfg.SourceJudge
	if isNilSourceJudge(sourceJudge) {
		sourceJudge = nil
	}
	safeProxyURL, err := normalizeMultiSafeProxyURL(cfg.SafeProxyURL)
	if err != nil {
		return nil, err
	}
	sourceSlotCount, err := normalizeMultiSourceSlots(cfg.SourceSlots)
	if err != nil {
		return nil, err
	}
	return &Service{
		runner:           runner,
		extractor:        extractor,
		signer:           signer,
		compiled:         compiledRepository,
		compileObserver:  compileObserver,
		validateCompiled: llm.ValidateAgainstSchema,
		mergeMulti:       consensus.MergeWithMaterialization,
		encodeMulti:      json.Marshal,
		now:              now,
		safeProxyURL:     safeProxyURL,
		sourceSlots:      make(chan struct{}, sourceSlotCount),
		sourceJudge:      sourceJudge,
	}, nil
}

// Extract runs the requested compiled/LLM dispatch. The caller-owned request
// is never mutated and cache is forcibly bypassed so deterministic execution
// and evidence stay tied to one fresh raw source.
func (s *Service) Extract(ctx context.Context, request *models.ExtractRequest) (*models.ExtractResponse, error) {
	startedAt := s.now()
	if request != nil && request.ExpectedSubject != nil {
		return nil, s.operationError(
			invalidExtractRequest("expected_subject requires multi-source extraction"),
			startedAt,
			models.ExtractTimingInfo{},
		)
	}
	prepared, err := prepareRequest(request)
	if err != nil {
		return nil, s.operationError(err, startedAt, models.ExtractTimingInfo{})
	}
	tryCompiled, err := s.prepareDispatch(prepared)
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

	response, err := s.extractPreparedArtifact(ctx, artifact, prepared, startedAt, tryCompiled)
	if err != nil {
		return nil, err
	}
	return response, nil
}

// FetchArtifact exposes one fresh canonical scrape to trusted Go callers. It
// accepts caller-controlled network options and therefore must not be used for
// untrusted URLs such as search-provider results; use FetchPublicArtifact for
// those instead.
func (s *Service) FetchArtifact(ctx context.Context, request *models.ScrapeRequest) (*Artifact, error) {
	if request == nil {
		return nil, models.NewScrapeError(models.ErrCodeInvalidInput, "scrape request is required", nil)
	}
	cloned := *request
	models.ApplyScrapeOptions(&cloned, models.ScrapeOptionsFromRequest(request))
	cloned.MaxAge = 0
	return s.fetchArtifact(ctx, &cloned)
}

// FetchPublicArtifact performs one fresh canonical scrape of an untrusted
// public HTTP(S) URL. The caller controls only the URL and context deadline;
// cache, proxy, browser state, and fetch-size policy are fixed by Service. The
// returned Artifact can be reused by verification, content, and schema
// consumers without another fetch and must be treated as read-only.
func (s *Service) FetchPublicArtifact(ctx context.Context, rawURL string) (*Artifact, error) {
	if ctx == nil {
		return nil, models.NewScrapeError(models.ErrCodeInvalidInput, "public artifact context is required", nil)
	}
	if err := ctx.Err(); err != nil {
		return nil, publicArtifactFetchError(ctx, err)
	}
	canonicalURL, err := normalizePublicArtifactURL(rawURL)
	if err != nil {
		return nil, err
	}
	if s == nil || s.runner == nil || s.safeProxyURL == "" {
		return nil, models.NewScrapeError(models.ErrCodeInternal, "public artifact fetching is unavailable", nil)
	}

	waitForNetworkIdle := true
	artifact, err := s.fetchArtifact(ctx, &models.ScrapeRequest{
		URL:                canonicalURL,
		WaitForNetworkIdle: &waitForNetworkIdle,
		Timeout:            defaultPublicArtifactTimeout,
		ProxyURL:           s.safeProxyURL,
		OutputFormat:       "markdown",
		ExtractMode:        "readability",
		MaxAge:             0,
		MaximumBodyBytes:   maximumExtractArtifactBytes,
	})
	if err != nil {
		return nil, publicArtifactFetchError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, publicArtifactFetchError(ctx, err)
	}
	if err := validatePublicArtifact(artifact); err != nil {
		return nil, err
	}
	return artifact, nil
}

func normalizePublicArtifactURL(rawURL string) (string, error) {
	if err := validateExtractText("public artifact URL", rawURL, maximumExtractURLBytes, false); err != nil {
		return "", models.NewScrapeError(models.ErrCodeInvalidInput, "public artifact URL is invalid", nil)
	}
	canonicalURL, _, err := publicnet.NormalizeHTTPURL(rawURL, nil, false)
	if err != nil || len(canonicalURL) > maximumExtractURLBytes {
		return "", models.NewScrapeError(models.ErrCodeInvalidInput, "public artifact URL is invalid", nil)
	}
	return canonicalURL, nil
}

func publicArtifactFetchError(ctx context.Context, err error) error {
	if (ctx != nil && ctx.Err() != nil) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return models.NewScrapeError(models.ErrCodeTimeout, "public artifact fetch timed out", err)
	}
	code := models.ErrCodeNavigation
	var scrapeError *models.ScrapeError
	if errors.As(err, &scrapeError) && isPublicArtifactRunnerCode(scrapeError.Code) {
		code = scrapeError.Code
	}
	return models.NewScrapeError(code, "public artifact fetch failed", err)
}

func isPublicArtifactRunnerCode(code string) bool {
	switch code {
	case models.ErrCodeTimeout,
		models.ErrCodeNavigation,
		models.ErrCodeReadability,
		models.ErrCodeBrowserCrash,
		models.ErrCodeRateLimited,
		models.ErrCodeUnauthorized,
		models.ErrCodeInternal,
		models.ErrCodeActionFailed,
		models.ErrCodeContentUnusable:
		return true
	default:
		return false
	}
}

func validatePublicArtifact(artifact *Artifact) error {
	if artifact == nil || artifact.Public == nil || artifact.Source == nil || !artifact.Public.Success ||
		strings.TrimSpace(artifact.Source.RawHTML) == "" || artifact.Source.FetchedAt.IsZero() ||
		artifact.Source.StatusCode < 100 || artifact.Source.StatusCode > 599 {
		return models.NewScrapeError(models.ErrCodeInternal, "public artifact result is invalid", nil)
	}
	if len(artifact.Public.Content) > maximumExtractArtifactBytes ||
		len(artifact.Source.RawHTML) > maximumExtractArtifactBytes {
		return models.NewScrapeError(models.ErrCodeNavigation, "public artifact exceeds maximum size", nil)
	}
	if artifact.Public.StatusCode != 0 && artifact.Public.StatusCode != artifact.Source.StatusCode {
		return models.NewScrapeError(models.ErrCodeInternal, "public artifact result is invalid", nil)
	}

	finalURL, _, err := publicnet.NormalizeHTTPURL(artifact.Source.FinalURL, nil, false)
	if err != nil || len(finalURL) > maximumExtractURLBytes {
		return models.NewScrapeError(models.ErrCodeNavigation, "public artifact final URL is invalid", nil)
	}
	for _, candidate := range []string{artifact.Public.FinalURL, artifact.Public.Metadata.SourceURL} {
		if candidate == "" {
			continue
		}
		canonical, _, normalizeErr := publicnet.NormalizeHTTPURL(candidate, nil, false)
		if normalizeErr != nil || canonical != finalURL {
			return models.NewScrapeError(models.ErrCodeInternal, "public artifact result is invalid", nil)
		}
	}

	artifact.Source.FinalURL = finalURL
	artifact.Public.FinalURL = finalURL
	artifact.Public.Metadata.SourceURL = finalURL
	artifact.Public.StatusCode = artifact.Source.StatusCode
	return nil
}

// ExtractArtifact performs structured extraction against an already selected
// canonical artifact without fetching again.
func (s *Service) ExtractArtifact(ctx context.Context, artifact *Artifact, request *models.ExtractRequest) (*models.ExtractResponse, error) {
	startedAt := s.now()
	prepared, err := prepareRequest(request)
	if err != nil {
		return nil, s.operationError(err, startedAt, models.ExtractTimingInfo{})
	}
	tryCompiled, err := s.prepareDispatch(prepared)
	if err != nil {
		return nil, s.operationError(err, startedAt, models.ExtractTimingInfo{})
	}
	return s.extractPreparedArtifact(ctx, artifact, prepared, startedAt, tryCompiled)
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
	tryCompiled bool,
) (*models.ExtractResponse, error) {
	if tryCompiled {
		response, err := s.extractCompiledArtifact(ctx, artifact, request, startedAt)
		if err == nil {
			return response, nil
		}
		if !shouldFallbackToLLM(request, err) {
			return nil, err
		}
	}
	return s.extractLLMArtifact(ctx, artifact, request, startedAt)
}

func (s *Service) extractLLMArtifact(
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
	if len(violations) == 0 {
		s.observeCompilation(ctx, artifact, request)
	}
	return response, nil
}

func (s *Service) extractCompiledArtifact(
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
	if artifact.Source == nil || strings.TrimSpace(artifact.Source.RawHTML) == "" {
		return nil, s.operationError(extractorUnavailable("compiled extraction requires raw HTML", nil), startedAt, baseTiming)
	}
	if request.Evidence {
		if artifact.Source.SnapshotID == "" {
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
	}

	sourceURL := artifact.Source.FinalURL
	if strings.TrimSpace(sourceURL) == "" {
		sourceURL = request.URL
	}
	extractionStartedAt := s.now()
	finishExtraction := func() {
		baseTiming.ExtractionMs = elapsedMilliseconds(extractionStartedAt, s.now())
	}
	key, err := compiler.BuildPageKey(sourceURL, request.Schema, artifact.Source.RawHTML)
	if err != nil {
		finishExtraction()
		return nil, s.operationError(extractorUnavailable("compiled extraction is incompatible with this page", err), startedAt, baseTiming)
	}
	extractor, found, err := s.compiled.Lookup(ctx, key)
	if err != nil {
		finishExtraction()
		return nil, s.compiledInternalOperationError(ctx, err, startedAt, baseTiming)
	}
	if !found {
		finishExtraction()
		return nil, s.operationError(extractorUnavailable("no active compiled extractor matches this page", nil), startedAt, baseTiming)
	}

	data, anchors, err := compiler.Execute(extractor.IR, artifact.Source.RawHTML)
	if err != nil {
		if errors.Is(err, compiler.ErrRequiredField) {
			if _, recordErr := s.compiled.RecordEmpty(ctx, key, extractor.ID); recordErr != nil {
				finishExtraction()
				return nil, s.compiledInternalOperationError(ctx, recordErr, startedAt, baseTiming)
			}
			finishExtraction()
			return nil, s.operationError(extractorUnavailable("compiled extractor did not produce a required field", err), startedAt, baseTiming)
		}
		finishExtraction()
		return nil, s.compiledInternalOperationError(ctx, err, startedAt, baseTiming)
	}
	if err := validateStructuredExtractData(data); err != nil {
		finishExtraction()
		return nil, s.operationError(extractorUnavailable("compiled extractor output exceeds structural limits", nil), startedAt, baseTiming)
	}
	violations, err := s.validateCompiled(request.Schema, data)
	if err != nil {
		finishExtraction()
		return nil, s.compiledInternalOperationError(ctx, err, startedAt, baseTiming)
	}
	if len(violations) > 0 {
		finishExtraction()
		return nil, s.operationError(extractorUnavailable("compiled extractor output violates the requested schema", nil), startedAt, baseTiming)
	}

	response := &models.ExtractResponse{
		Success:  true,
		Data:     append(json.RawMessage(nil), data...),
		Metadata: artifact.Public.Metadata,
		Tokens:   artifact.Public.Tokens,
		Extractor: &models.ExtractorMetadata{
			ID:         extractor.ID,
			Version:    extractor.Version,
			CompiledAt: extractor.CreatedAt,
			Validation: extractor.Validation.Overall,
			Mode:       "compiled",
		},
	}
	if request.Evidence {
		basis, unlocatedRate, evidenceErr := hydrateCompiledEvidence(
			data,
			anchors,
			artifact.Public.Content,
			string(artifact.Source.SnapshotID),
			artifact.Source.FetchedAt,
		)
		if evidenceErr != nil {
			finishExtraction()
			return nil, s.operationError(
				models.NewScrapeError(models.ErrCodeEvidenceUnavailable, "compiled evidence could not be aligned", evidenceErr),
				startedAt,
				baseTiming,
			)
		}
		receiptTokens, signErr := signFieldReceipts(
			data,
			basis,
			sourceURL,
			s.now().UTC(),
			s.signer,
			fmt.Sprintf("%s@%d", extractor.ID, extractor.Version),
		)
		if signErr != nil {
			finishExtraction()
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

	_, err = s.compiled.Touch(ctx, key, extractor.ID)
	if err != nil {
		finishExtraction()
		return nil, s.compiledInternalOperationError(ctx, err, startedAt, baseTiming)
	}
	finishExtraction()
	baseTiming.TotalMs = elapsedMilliseconds(startedAt, s.now())
	response.Timing = baseTiming
	return response, nil
}

func (s *Service) prepareDispatch(request *models.ExtractRequest) (bool, error) {
	hasLLMKey := strings.TrimSpace(request.LLMAPIKey) != ""
	if request.Engine == "llm" {
		if !hasLLMKey {
			return false, models.NewScrapeError(models.ErrCodeInvalidInput, "llm_api_key is required for LLM extraction", nil)
		}
		return false, nil
	}

	eligible := request.CSSSelector == "" && request.OutputFormat == "markdown" && request.ExtractMode == "readability"
	if eligible {
		eligible = compiler.ValidateCompileSchema(request.Schema) == nil
	}
	if eligible && s.compiled != nil {
		return true, nil
	}
	if request.Engine == "auto" && hasLLMKey {
		return false, nil
	}
	return false, extractorUnavailable("compiled extraction is unavailable for this request", nil)
}

func shouldFallbackToLLM(request *models.ExtractRequest, err error) bool {
	if request == nil || request.Engine != "auto" || strings.TrimSpace(request.LLMAPIKey) == "" {
		return false
	}
	var scrapeError *models.ScrapeError
	if errors.As(err, &scrapeError) && scrapeError.Code == models.ErrCodeExtractorUnavailable {
		return true
	}
	return errors.Is(err, errCompiledAttemptInternal)
}

func extractorUnavailable(message string, cause error) error {
	return models.NewScrapeError(models.ErrCodeExtractorUnavailable, message, cause)
}

func isNilCompiledRepository(repository CompiledRepository) bool {
	if repository == nil {
		return true
	}
	value := reflect.ValueOf(repository)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func isNilCompileObserver(observer CompileObserver) bool {
	if observer == nil {
		return true
	}
	value := reflect.ValueOf(observer)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func isNilSourceJudge(judge SourceJudge) bool {
	if judge == nil {
		return true
	}
	value := reflect.ValueOf(judge)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// observeCompilation runs after the response and its timing have been fully
// assembled. Catalog admission is best-effort: neither an observer error nor a
// panic may turn a successful customer extraction into a failure.
func (s *Service) observeCompilation(ctx context.Context, artifact *Artifact, request *models.ExtractRequest) {
	if s == nil || s.compileObserver == nil || artifact == nil || artifact.Source == nil || request == nil {
		return
	}
	defer func() {
		_ = recover()
	}()

	source := artifact.Source
	if request.CSSSelector != "" || request.OutputFormat != "markdown" || request.ExtractMode != "readability" ||
		strings.TrimSpace(source.RawHTML) == "" || strings.TrimSpace(string(source.SnapshotID)) == "" ||
		source.FetchedAt.IsZero() || source.StatusCode < 200 || source.StatusCode >= 300 ||
		compiler.ValidateCompileSchema(request.Schema) != nil {
		return
	}
	sourceURL := source.FinalURL
	if strings.TrimSpace(sourceURL) == "" {
		sourceURL = request.URL
	}
	page, err := compiler.BuildPageKey(sourceURL, request.Schema, source.RawHTML)
	if err != nil {
		return
	}
	_ = s.compileObserver.Observe(
		ctx,
		page,
		string(source.SnapshotID),
		source.FetchedAt,
		append(json.RawMessage(nil), page.Schema...),
	)
}

var errCompiledAttemptInternal = errors.New("extract: compiled attempt internal failure")

type compiledAttemptInternalCause struct {
	cause error
}

func (e *compiledAttemptInternalCause) Error() string {
	return errCompiledAttemptInternal.Error()
}

func (e *compiledAttemptInternalCause) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *compiledAttemptInternalCause) Is(target error) bool {
	return target == errCompiledAttemptInternal
}

func (s *Service) compiledInternalOperationError(
	ctx context.Context,
	cause error,
	startedAt time.Time,
	timing models.ExtractTimingInfo,
) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return s.operationError(ctxErr, startedAt, timing)
	}
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return s.operationError(cause, startedAt, timing)
	}
	return s.operationError(models.NewScrapeError(
		models.ErrCodeInternal,
		"compiled extractor subsystem failed",
		&compiledAttemptInternalCause{cause: cause},
	), startedAt, timing)
}

func hydrateCompiledEvidence(
	data json.RawMessage,
	anchors map[string]evidence.Anchor,
	cleaned string,
	snapshotID string,
	fetchedAt time.Time,
) (models.EvidenceBasis, float64, error) {
	values, err := evidence.LeafValues(data)
	if err != nil {
		return nil, 0, err
	}
	basis := make(models.EvidenceBasis, len(values))
	unlocated := 0
	for field, anchor := range anchors {
		path := strings.ReplaceAll(field, ".", `\.`)
		if _, ok := values[path]; !ok {
			return nil, 0, fmt.Errorf("compiled anchor %q has no output value", field)
		}
		anchor.TextRange = [2]int{}
		located := false
		if anchor.Quote != "" {
			start := strings.Index(cleaned, anchor.Quote)
			if start >= 0 && strings.LastIndex(cleaned, anchor.Quote) == start {
				anchor.TextRange = [2]int{start, start + len(anchor.Quote)}
				located = true
			}
		}
		anchor.SnapshotID = snapshotID
		anchor.FetchedAt = fetchedAt
		anchor.Method = evidence.MethodCompiled
		if !located {
			unlocated++
		}
		basis[path] = anchor
	}
	if len(basis) != len(values) {
		return nil, 0, fmt.Errorf("compiled evidence/value path count mismatch: %d != %d", len(basis), len(values))
	}
	if len(values) == 0 {
		return basis, 0, nil
	}
	return basis, float64(unlocated) / float64(len(values)), nil
}

func prepareRequest(request *models.ExtractRequest) (*models.ExtractRequest, error) {
	if request == nil {
		return nil, models.NewScrapeError(models.ErrCodeInvalidInput, "extract request is required", nil)
	}
	if request.URL == "" || request.Sources != nil {
		return nil, invalidExtractRequest("single-source extraction requires url and forbids sources")
	}
	if err := validateRawExtractRequest(request); err != nil {
		return nil, err
	}
	prepared := *request
	prepared.Schema = append(json.RawMessage(nil), request.Schema...)
	if request.WaitForNetworkIdle != nil {
		value := *request.WaitForNetworkIdle
		prepared.WaitForNetworkIdle = &value
	}
	if request.ExpectedSubject != nil {
		spec := *request.ExpectedSubject
		spec.Name = strings.TrimSpace(spec.Name)
		spec.Hint = strings.TrimSpace(spec.Hint)
		if spec.Name == "" {
			return nil, invalidExtractRequest("expected_subject.name is required")
		}
		if err := validateExtractText("expected_subject.name", spec.Name, models.MaxAnswerSubjectBytes, false); err != nil {
			return nil, err
		}
		if err := validateExtractText("expected_subject.hint", spec.Hint, models.MaxAnswerPredicateBytes, false); err != nil {
			return nil, err
		}
		prepared.ExpectedSubject = &spec
	}
	prepared.URL = strings.TrimSpace(prepared.URL)
	prepared.LLMModel = strings.TrimSpace(prepared.LLMModel)
	prepared.LLMBaseURL = strings.TrimSpace(prepared.LLMBaseURL)
	prepared.Defaults()
	parsed, err := url.ParseRequestURI(prepared.URL)
	if err != nil || parsed.Host == "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, invalidExtractRequest("url must be an absolute http or https URL")
	}
	switch prepared.Engine {
	case "auto", "compiled", "llm":
	default:
		return nil, invalidExtractRequest("engine must be auto, compiled, or llm")
	}
	baseURL, err := normalizeExtractBaseURL(prepared.LLMBaseURL)
	if err != nil {
		return nil, invalidExtractRequest("llm_base_url is invalid")
	}
	prepared.LLMBaseURL = baseURL
	normalizedSchema, err := llm.NormalizeSchema(prepared.Schema)
	if err != nil {
		return nil, invalidExtractRequest("invalid JSON schema")
	}
	if len(normalizedSchema) > maximumExtractSchemaBytes {
		return nil, invalidExtractRequest("normalized JSON schema exceeds maximum size")
	}
	if err := llm.ValidateSchema(normalizedSchema); err != nil {
		return nil, invalidExtractRequest("invalid JSON schema")
	}
	prepared.Schema = normalizedSchema
	return &prepared, nil
}

func validateRawExtractRequest(request *models.ExtractRequest) error {
	if err := validateExtractText("url", request.URL, maximumExtractURLBytes, false); err != nil {
		return err
	}
	if len(request.Schema) == 0 || len(request.Schema) > maximumExtractSchemaBytes || !utf8.Valid(request.Schema) {
		return invalidExtractRequest("schema is invalid or exceeds maximum size")
	}
	if err := validateExtractText("llm_api_key", request.LLMAPIKey, maximumExtractCredentialBytes, false); err != nil {
		return err
	}
	if err := validateExtractText("llm_model", request.LLMModel, maximumExtractModelBytes, true); err != nil {
		return err
	}
	if err := validateExtractText("llm_base_url", request.LLMBaseURL, maximumExtractBaseURLBytes, false); err != nil {
		return err
	}
	return nil
}

func validateExtractText(label, value string, maximumBytes int, rejectWhitespace bool) error {
	if len(value) > maximumBytes || !utf8.ValidString(value) {
		return invalidExtractRequest(label + " is invalid or exceeds maximum size")
	}
	for _, character := range value {
		if unicode.IsControl(character) || rejectWhitespace && unicode.IsSpace(character) {
			return invalidExtractRequest(label + " contains invalid characters")
		}
	}
	return nil
}

func normalizeExtractBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Opaque != "" || parsed.Host == "" || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		strings.Contains(raw, "#") || strings.HasSuffix(parsed.Host, ":") {
		return "", errors.New("invalid provider base URL")
	}
	canonical, _, err := publicnet.NormalizeHTTPURL(raw, nil, false)
	if err != nil {
		return "", errors.New("invalid provider base URL")
	}
	return strings.TrimRight(canonical, "/"), nil
}

func invalidExtractRequest(message string) error {
	return models.NewScrapeError(models.ErrCodeInvalidInput, message, nil)
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
	if len(artifact.Public.Content) > maximumExtractArtifactBytes ||
		artifact.Source != nil && len(artifact.Source.RawHTML) > maximumExtractArtifactBytes {
		return timing, models.NewScrapeError(models.ErrCodeNavigation, "extract artifact exceeds maximum size", nil)
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
	if err := validateStructuredExtractData(result.Data); err != nil {
		return nil, nil, models.NewScrapeError(models.ErrCodeLLMFailure, "structured extractor returned invalid or oversized JSON", nil)
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
	if err := validateStructuredExtractData(repaired.Data); err != nil {
		return nil, nil, models.NewScrapeError(models.ErrCodeLLMFailure, "structured extractor returned invalid or oversized repair JSON", nil)
	}
	repaired.Usage = addLLMUsage(result.Usage, repaired.Usage)

	remaining, err := llm.ValidateAgainstSchema(schema, repaired.Data)
	if err != nil {
		return nil, nil, models.NewScrapeError(models.ErrCodeInvalidInput, "invalid JSON schema", err)
	}
	return repaired, remaining, nil
}

func validateStructuredExtractData(data json.RawMessage) error {
	if len(data) == 0 || len(data) > maximumExtractArtifactBytes {
		return errors.New("structured data is empty or exceeds maximum size")
	}
	_, err := consensus.Merge([]consensus.SourceResult{{
		URL:  extractDataPreflightURL,
		Data: data,
	}})
	return err
}

func signFieldReceipts(
	data json.RawMessage,
	basis models.EvidenceBasis,
	sourceURL string,
	issuedAt time.Time,
	signer ReceiptSigner,
	extractorVersion ...string,
) (map[string]string, error) {
	if signer == nil {
		return nil, errors.New("receipt signer is unavailable")
	}
	values, err := evidence.LeafValues(data)
	if err != nil {
		return nil, fmt.Errorf("decode leaf values: %w", err)
	}
	if len(values) > consensus.MaxLeavesPerSource {
		return nil, fmt.Errorf("evidence leaves exceed %d", consensus.MaxLeavesPerSource)
	}
	if len(values) != len(basis) {
		return nil, fmt.Errorf("evidence/value path count mismatch: %d != %d", len(basis), len(values))
	}
	paths := make([]string, 0, len(values))
	for path := range values {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	metadataRemaining := consensus.MaxTotalMetadataBytes
	reserveMetadata := func(requested int) bool {
		if requested < 0 || requested > metadataRemaining {
			return false
		}
		metadataRemaining -= requested
		return true
	}
	for _, path := range paths {
		if len(path) == 0 || len(path) > consensus.MaxPathBytes || !utf8.ValidString(path) {
			return nil, fmt.Errorf("evidence path is invalid or exceeds %d bytes", consensus.MaxPathBytes)
		}
		anchor, ok := basis[path]
		if !ok {
			return nil, fmt.Errorf("evidence anchor missing for %q", path)
		}
		for _, size := range []int{len(path), len(anchor.Quote), len(anchor.Selector), len(anchor.Method), len(anchor.SnapshotID), 64} {
			if !reserveMetadata(size) {
				return nil, fmt.Errorf("evidence metadata exceeds %d bytes", consensus.MaxTotalMetadataBytes)
			}
		}
	}

	tokens := make(map[string]string, len(paths))
	version := ""
	if len(extractorVersion) > 0 {
		version = extractorVersion[0]
	}
	for _, path := range paths {
		anchor, ok := basis[path]
		if !ok {
			return nil, fmt.Errorf("evidence anchor missing for %q", path)
		}
		token, err := signer.Sign(receipts.Payload{
			URL:              sourceURL,
			Path:             path,
			Value:            values[path],
			Anchor:           anchor,
			ExtractorVersion: version,
			IssuedAt:         issuedAt,
		})
		if err != nil {
			return nil, fmt.Errorf("sign %q: %w", path, err)
		}
		if !utf8.ValidString(token) || len(token) > consensus.MaxReceiptBytes {
			return nil, fmt.Errorf("receipt for %q is invalid or exceeds %d bytes", path, consensus.MaxReceiptBytes)
		}
		for _, size := range []int{len(path), len(token), 16} {
			if !reserveMetadata(size) {
				return nil, fmt.Errorf("evidence metadata exceeds %d bytes", consensus.MaxTotalMetadataBytes)
			}
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
