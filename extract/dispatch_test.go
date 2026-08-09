package extract

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/use-agent/purify/compiler"
	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/llm"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/receipts"
	"github.com/use-agent/purify/scrape"
)

func TestExtractEngineDispatchMatrix(t *testing.T) {
	tests := []struct {
		name         string
		engine       string
		key          string
		repository   *recordingCompiledRepository
		wantCode     string
		wantLLM      int
		wantLookup   int
		wantTouch    int
		wantFetch    int
		wantCompiled bool
	}{
		{
			name:       "explicit llm preserves direct path",
			engine:     "llm",
			key:        "secret",
			repository: compiledHitRepository(requiredCountIR()),
			wantLLM:    1,
			wantFetch:  1,
		},
		{
			name:       "explicit llm rejects missing key before fetch",
			engine:     "llm",
			wantCode:   models.ErrCodeInvalidInput,
			repository: compiledHitRepository(requiredCountIR()),
		},
		{
			name:         "auto defaults to compiled without key",
			key:          "",
			repository:   compiledHitRepository(requiredCountIR()),
			wantLookup:   1,
			wantTouch:    1,
			wantFetch:    1,
			wantCompiled: true,
		},
		{
			name:         "auto hit with key still makes zero llm calls",
			engine:       "auto",
			key:          "secret",
			repository:   compiledHitRepository(requiredCountIR()),
			wantLookup:   1,
			wantTouch:    1,
			wantFetch:    1,
			wantCompiled: true,
		},
		{
			name:         "strict compiled works without key",
			engine:       "compiled",
			repository:   compiledHitRepository(requiredCountIR()),
			wantLookup:   1,
			wantTouch:    1,
			wantFetch:    1,
			wantCompiled: true,
		},
		{
			name:       "auto miss falls back only with key",
			engine:     "auto",
			key:        "secret",
			repository: &recordingCompiledRepository{},
			wantLLM:    1,
			wantLookup: 1,
			wantFetch:  1,
		},
		{
			name:       "auto miss without key is unavailable",
			engine:     "auto",
			repository: &recordingCompiledRepository{},
			wantCode:   models.ErrCodeExtractorUnavailable,
			wantLookup: 1,
			wantFetch:  1,
		},
		{
			name:       "strict compiled never falls back",
			engine:     "compiled",
			key:        "secret",
			repository: &recordingCompiledRepository{},
			wantCode:   models.ErrCodeExtractorUnavailable,
			wantLookup: 1,
			wantFetch:  1,
		},
		{
			name:      "auto keeps legacy llm behavior without repository",
			engine:    "auto",
			key:       "secret",
			wantLLM:   1,
			wantFetch: 1,
		},
		{
			name:     "auto without repository and key is unavailable before fetch",
			engine:   "auto",
			wantCode: models.ErrCodeExtractorUnavailable,
		},
		{
			name:     "strict compiled without repository is unavailable before fetch",
			engine:   "compiled",
			key:      "secret",
			wantCode: models.ErrCodeExtractorUnavailable,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &recordingRunner{result: compiledScrapeResult()}
			client := &recordingExtractor{initial: &llm.ExtractResult{Data: json.RawMessage(`{"count":3}`)}}
			service := newDispatchService(t, runner, client, nil, test.repository)
			request := validExtractRequest()
			request.Engine = test.engine
			request.LLMAPIKey = test.key

			response, err := service.Extract(context.Background(), request)
			if test.wantCode != "" {
				assertScrapeErrorCode(t, err, test.wantCode)
				if response != nil {
					t.Fatalf("response = %#v, want nil", response)
				}
			} else {
				if err != nil || response == nil || !response.Success {
					t.Fatalf("Extract() = %#v, %v", response, err)
				}
				if got := response.Extractor != nil; got != test.wantCompiled {
					t.Fatalf("compiled metadata present = %v, want %v", got, test.wantCompiled)
				}
				if test.wantCompiled {
					if response.Extractor.ID != testExtractorID || response.Extractor.Mode != "compiled" || response.Extractor.Version != 3 || response.Extractor.Validation != 0.97 {
						t.Fatalf("extractor metadata = %#v", response.Extractor)
					}
					if response.LLMUsage != nil || string(response.Data) != `{"count":3}` {
						t.Fatalf("compiled response = %#v", response)
					}
				}
			}
			if runner.calls != test.wantFetch || client.extractCalls != test.wantLLM {
				t.Fatalf("fetch/LLM calls = %d/%d, want %d/%d", runner.calls, client.extractCalls, test.wantFetch, test.wantLLM)
			}
			if test.repository != nil {
				if test.repository.lookupCalls != test.wantLookup || test.repository.touchCalls != test.wantTouch || test.repository.emptyCalls != 0 || test.repository.recordUseCalls != 0 {
					t.Fatalf("repository calls lookup/touch/empty/use = %d/%d/%d/%d", test.repository.lookupCalls, test.repository.touchCalls, test.repository.emptyCalls, test.repository.recordUseCalls)
				}
			}
		})
	}
}

func TestExtractCompiledPreflightMatrix(t *testing.T) {
	unsupportedSchema := json.RawMessage(`{"type":"object","properties":{"nested":{"type":"object","properties":{"name":{"type":"string"}}}}}`)
	tests := []struct {
		name      string
		engine    string
		key       string
		configure func(*models.ExtractRequest)
		wantCode  string
		wantFetch int
		wantLLM   int
	}{
		{
			name:   "compiled rejects non-default profile",
			engine: "compiled",
			key:    "secret",
			configure: func(request *models.ExtractRequest) {
				request.CSSSelector = "main"
			},
			wantCode: models.ErrCodeExtractorUnavailable,
		},
		{
			name:   "auto with key sends non-default profile to llm",
			engine: "auto",
			key:    "secret",
			configure: func(request *models.ExtractRequest) {
				request.OutputFormat = "text"
			},
			wantFetch: 1,
			wantLLM:   1,
		},
		{
			name:   "auto without key rejects non-default profile",
			engine: "auto",
			configure: func(request *models.ExtractRequest) {
				request.ExtractMode = "raw"
			},
			wantCode: models.ErrCodeExtractorUnavailable,
		},
		{
			name:   "compiled rejects unsupported compiler schema",
			engine: "compiled",
			key:    "secret",
			configure: func(request *models.ExtractRequest) {
				request.Schema = unsupportedSchema
			},
			wantCode: models.ErrCodeExtractorUnavailable,
		},
		{
			name:   "auto with key sends unsupported compiler schema to llm",
			engine: "auto",
			key:    "secret",
			configure: func(request *models.ExtractRequest) {
				request.Schema = unsupportedSchema
			},
			wantFetch: 1,
			wantLLM:   1,
		},
		{
			name:   "auto without key rejects unsupported compiler schema",
			engine: "auto",
			configure: func(request *models.ExtractRequest) {
				request.Schema = unsupportedSchema
			},
			wantCode: models.ErrCodeExtractorUnavailable,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &recordingRunner{result: compiledScrapeResult()}
			client := &recordingExtractor{initial: &llm.ExtractResult{Data: json.RawMessage(`{"count":3}`)}}
			repository := compiledHitRepository(requiredCountIR())
			service := newDispatchService(t, runner, client, nil, repository)
			request := validExtractRequest()
			request.Engine = test.engine
			request.LLMAPIKey = test.key
			test.configure(request)

			response, err := service.Extract(context.Background(), request)
			if test.wantCode != "" {
				assertScrapeErrorCode(t, err, test.wantCode)
			} else if err != nil || response == nil || !response.Success {
				t.Fatalf("Extract() = %#v, %v", response, err)
			}
			if runner.calls != test.wantFetch || client.extractCalls != test.wantLLM || repository.lookupCalls != 0 || repository.touchCalls != 0 || repository.emptyCalls != 0 || repository.recordUseCalls != 0 {
				t.Fatalf("calls fetch/LLM/lookup/touch/empty/use = %d/%d/%d/%d/%d/%d", runner.calls, client.extractCalls, repository.lookupCalls, repository.touchCalls, repository.emptyCalls, repository.recordUseCalls)
			}
		})
	}
}

func TestCompiledExecutionHealthMatrix(t *testing.T) {
	tests := []struct {
		name      string
		engine    string
		key       string
		schema    json.RawMessage
		ir        compiler.IR
		wantCode  string
		wantLLM   int
		wantTouch int
		wantEmpty int
		wantData  string
	}{
		{
			name:      "required miss alone enters drift window",
			engine:    "compiled",
			ir:        requiredMissingIR(),
			wantCode:  models.ErrCodeExtractorUnavailable,
			wantEmpty: 1,
		},
		{
			name:      "auto required miss records then falls back",
			engine:    "auto",
			key:       "secret",
			ir:        requiredMissingIR(),
			wantLLM:   1,
			wantEmpty: 1,
			wantData:  `{"count":3}`,
		},
		{
			name:      "optional miss is a successful deterministic use",
			engine:    "compiled",
			schema:    json.RawMessage(`{"type":"object","properties":{"note":{"type":"string"}},"additionalProperties":false}`),
			ir:        optionalMissingIR(),
			wantTouch: 1,
			wantData:  `{}`,
		},
		{
			name:     "corrupt ir is not recorded",
			engine:   "compiled",
			ir:       compiler.IR{Version: 999, Fields: requiredCountIR().Fields},
			wantCode: models.ErrCodeInternal,
		},
		{
			name:     "full schema violation is not recorded",
			engine:   "compiled",
			schema:   json.RawMessage(`{"type":"object","properties":{"count":{"type":"number","minimum":10}},"required":["count"],"additionalProperties":false}`),
			ir:       requiredCountIR(),
			wantCode: models.ErrCodeExtractorUnavailable,
		},
		{
			name:     "auto corrupt ir fails open with key without recording",
			engine:   "auto",
			key:      "secret",
			ir:       compiler.IR{Version: 999, Fields: requiredCountIR().Fields},
			wantLLM:  1,
			wantData: `{"count":3}`,
		},
		{
			name:     "auto corrupt ir without key preserves internal failure",
			engine:   "auto",
			ir:       compiler.IR{Version: 999, Fields: requiredCountIR().Fields},
			wantCode: models.ErrCodeInternal,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &recordingRunner{result: compiledScrapeResult()}
			client := &recordingExtractor{initial: &llm.ExtractResult{Data: json.RawMessage(`{"count":3}`)}}
			repository := compiledHitRepository(test.ir)
			service := newDispatchService(t, runner, client, nil, repository)
			request := validExtractRequest()
			request.Engine = test.engine
			request.LLMAPIKey = test.key
			if test.schema != nil {
				request.Schema = test.schema
			}

			response, err := service.Extract(context.Background(), request)
			if test.wantCode != "" {
				assertScrapeErrorCode(t, err, test.wantCode)
			} else {
				if err != nil || response == nil || !response.Success || string(response.Data) != test.wantData {
					t.Fatalf("Extract() = %#v, %v", response, err)
				}
			}
			if runner.calls != 1 || client.extractCalls != test.wantLLM || repository.lookupCalls != 1 || repository.touchCalls != test.wantTouch || repository.emptyCalls != test.wantEmpty || repository.recordUseCalls != 0 {
				t.Fatalf("calls fetch/LLM/lookup/touch/empty/use = %d/%d/%d/%d/%d/%d", runner.calls, client.extractCalls, repository.lookupCalls, repository.touchCalls, repository.emptyCalls, repository.recordUseCalls)
			}
		})
	}
}

func TestCompiledRepositoryErrorMatrix(t *testing.T) {
	sentinel := errors.New("repository unavailable")
	tests := []struct {
		name      string
		engine    string
		key       string
		configure func(*recordingCompiledRepository)
		wantCode  string
		wantLLM   int
		wantTouch int
		wantEmpty int
	}{
		{
			name:      "auto lookup error fails open with key",
			engine:    "auto",
			key:       "secret",
			configure: func(repository *recordingCompiledRepository) { repository.lookupErr = sentinel },
			wantLLM:   1,
		},
		{
			name:      "compiled lookup error fails closed",
			engine:    "compiled",
			key:       "secret",
			configure: func(repository *recordingCompiledRepository) { repository.lookupErr = sentinel },
			wantCode:  models.ErrCodeInternal,
		},
		{
			name:      "auto lookup error without key fails closed",
			engine:    "auto",
			configure: func(repository *recordingCompiledRepository) { repository.lookupErr = sentinel },
			wantCode:  models.ErrCodeInternal,
		},
		{
			name:      "auto lookup timeout never falls back",
			engine:    "auto",
			key:       "secret",
			configure: func(repository *recordingCompiledRepository) { repository.lookupErr = context.DeadlineExceeded },
			wantCode:  models.ErrCodeTimeout,
		},
		{
			name:      "compiled lookup cancellation remains timeout",
			engine:    "compiled",
			configure: func(repository *recordingCompiledRepository) { repository.lookupErr = context.Canceled },
			wantCode:  models.ErrCodeTimeout,
		},
		{
			name:      "auto touch error discards compiled response and falls back",
			engine:    "auto",
			key:       "secret",
			configure: func(repository *recordingCompiledRepository) { repository.touchErr = sentinel },
			wantLLM:   1,
			wantTouch: 1,
		},
		{
			name:      "compiled touch error fails closed",
			engine:    "compiled",
			configure: func(repository *recordingCompiledRepository) { repository.touchErr = sentinel },
			wantCode:  models.ErrCodeInternal,
			wantTouch: 1,
		},
		{
			name:   "auto required-empty recording error falls back",
			engine: "auto",
			key:    "secret",
			configure: func(repository *recordingCompiledRepository) {
				repository.extractor.IR = requiredMissingIR()
				repository.emptyErr = sentinel
			},
			wantLLM:   1,
			wantEmpty: 1,
		},
		{
			name:   "compiled required-empty recording error fails closed",
			engine: "compiled",
			configure: func(repository *recordingCompiledRepository) {
				repository.extractor.IR = requiredMissingIR()
				repository.emptyErr = sentinel
			},
			wantCode:  models.ErrCodeInternal,
			wantEmpty: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &recordingRunner{result: compiledScrapeResult()}
			client := &recordingExtractor{initial: &llm.ExtractResult{Data: json.RawMessage(`{"count":3}`)}}
			repository := compiledHitRepository(requiredCountIR())
			test.configure(repository)
			service := newDispatchService(t, runner, client, nil, repository)
			request := validExtractRequest()
			request.Engine = test.engine
			request.LLMAPIKey = test.key

			response, err := service.Extract(context.Background(), request)
			if test.wantCode != "" {
				assertScrapeErrorCode(t, err, test.wantCode)
				if test.wantCode == models.ErrCodeInternal {
					var scrapeError *models.ScrapeError
					if !errors.As(err, &scrapeError) || scrapeError.Message != "compiled extractor subsystem failed" || strings.Contains(err.Error(), sentinel.Error()) {
						t.Fatalf("internal error leaked cause or message changed: %v", err)
					}
				}
			} else if err != nil || response == nil || !response.Success {
				t.Fatalf("Extract() = %#v, %v", response, err)
			}
			if runner.calls != 1 || client.extractCalls != test.wantLLM || repository.lookupCalls != 1 || repository.touchCalls != test.wantTouch || repository.emptyCalls != test.wantEmpty || repository.recordUseCalls != 0 {
				t.Fatalf("calls fetch/LLM/lookup/touch/empty/use = %d/%d/%d/%d/%d/%d", runner.calls, client.extractCalls, repository.lookupCalls, repository.touchCalls, repository.emptyCalls, repository.recordUseCalls)
			}
		})
	}
}

func TestCompiledValidatorInternalFailureMatrix(t *testing.T) {
	sentinel := errors.New("validator implementation detail")
	tests := []struct {
		name     string
		engine   string
		key      string
		wantCode string
		wantLLM  int
	}{
		{name: "compiled preserves validator internal failure", engine: "compiled", key: "secret", wantCode: models.ErrCodeInternal},
		{name: "auto without key preserves validator internal failure", engine: "auto", wantCode: models.ErrCodeInternal},
		{name: "auto with key falls back from marked validator failure", engine: "auto", key: "secret", wantLLM: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &recordingRunner{result: compiledScrapeResult()}
			client := &recordingExtractor{initial: &llm.ExtractResult{Data: json.RawMessage(`{"count":3}`)}}
			repository := compiledHitRepository(requiredCountIR())
			service := newDispatchService(t, runner, client, nil, repository)
			service.validateCompiled = func(json.RawMessage, json.RawMessage) ([]llm.Violation, error) {
				return nil, sentinel
			}
			request := validExtractRequest()
			request.Engine = test.engine
			request.LLMAPIKey = test.key

			response, err := service.Extract(context.Background(), request)
			if test.wantCode != "" {
				assertScrapeErrorCode(t, err, test.wantCode)
				var scrapeError *models.ScrapeError
				if !errors.As(err, &scrapeError) || scrapeError.Message != "compiled extractor subsystem failed" || strings.Contains(err.Error(), sentinel.Error()) {
					t.Fatalf("validator error leaked cause or message changed: %v", err)
				}
			} else if err != nil || response == nil || !response.Success || response.Extractor != nil {
				t.Fatalf("Extract() = %#v, %v", response, err)
			}
			if runner.calls != 1 || repository.lookupCalls != 1 || repository.touchCalls != 0 || repository.emptyCalls != 0 || repository.recordUseCalls != 0 || client.extractCalls != test.wantLLM {
				t.Fatalf("calls fetch/lookup/touch/empty/use/LLM = %d/%d/%d/%d/%d/%d", runner.calls, repository.lookupCalls, repository.touchCalls, repository.emptyCalls, repository.recordUseCalls, client.extractCalls)
			}
		})
	}
}

func TestCompiledExtractionTimingIncludesRepositoryValidationAndTouch(t *testing.T) {
	current := time.Date(2026, time.August, 9, 8, 0, 0, 0, time.UTC)
	repository := compiledHitRepository(requiredCountIR())
	repository.lookupHook = func() { current = current.Add(7 * time.Millisecond) }
	repository.touchHook = func() { current = current.Add(11 * time.Millisecond) }
	runner := &recordingRunner{result: compiledScrapeResult()}
	client := &recordingExtractor{}
	service, err := NewService(runner, client, nil, Config{
		CompiledRepository: repository,
		Now:                func() time.Time { return current },
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	service.validateCompiled = func(schema, data json.RawMessage) ([]llm.Violation, error) {
		current = current.Add(13 * time.Millisecond)
		return llm.ValidateAgainstSchema(schema, data)
	}
	request := validExtractRequest()
	request.Engine = "compiled"
	request.LLMAPIKey = ""

	response, err := service.Extract(context.Background(), request)
	if err != nil || response == nil {
		t.Fatalf("Extract() = %#v, %v", response, err)
	}
	if response.Timing.ExtractionMs != 31 {
		t.Fatalf("ExtractionMs = %d, want lookup(7)+validation(13)+touch(11)", response.Timing.ExtractionMs)
	}
}

func TestCompiledEvidenceUsesSameArtifactAndPreservesRuleAnchor(t *testing.T) {
	result := compiledScrapeResult()
	result.Response.Content = "Count: 3"
	result.Source.RawHTML = `<html><body><main><span class="count">3</span></main></body></html>`
	runner := &recordingRunner{result: result}
	client := &recordingExtractor{initial: &llm.ExtractResult{Data: json.RawMessage(`{"count":999}`)}}
	repository := compiledHitRepository(requiredCountIR())
	seed := make([]byte, ed25519.SeedSize)
	for index := range seed {
		seed[index] = byte(index + 1)
	}
	signer, signerErr := receipts.NewSigner(ed25519.NewKeyFromSeed(seed))
	if signerErr != nil {
		t.Fatalf("NewSigner() error = %v", signerErr)
	}
	service := newDispatchService(t, runner, client, signer, repository)
	request := validExtractRequest()
	request.Engine = "auto"
	request.LLMAPIKey = "secret"
	request.Evidence = true

	response, err := service.Extract(context.Background(), request)
	if err != nil || response == nil || response.Basis == nil || response.Receipts == nil || response.UnlocatedRate == nil {
		t.Fatalf("Extract() = %#v, %v", response, err)
	}
	anchor := (*response.Basis)["count"]
	if anchor.Method != evidence.MethodCompiled || anchor.Selector != ".count" || anchor.Quote != "3" || anchor.TextRange != [2]int{7, 8} || anchor.SnapshotID != string(result.Source.SnapshotID) || !anchor.FetchedAt.Equal(result.Source.FetchedAt) {
		t.Fatalf("compiled anchor = %#v", anchor)
	}
	token := (*response.Receipts)["count"]
	payload, verifyErr := signer.Verify(token)
	if verifyErr != nil {
		t.Fatalf("Verify(compiled receipt) error = %v", verifyErr)
	}
	if *response.UnlocatedRate != 0 || payload.ExtractorVersion != testExtractorID+"@3" || payload.Anchor != anchor || payload.Path != "count" || string(payload.Value) != "3" {
		t.Fatalf("evidence/receipt = %#v / %#v", response, payload)
	}
	if runner.calls != 1 || client.extractCalls != 0 || repository.lookupCalls != 1 || repository.touchCalls != 1 || repository.emptyCalls != 0 || repository.recordUseCalls != 0 {
		t.Fatalf("calls fetch/LLM/lookup/touch/empty/use = %d/%d/%d/%d/%d/%d", runner.calls, client.extractCalls, repository.lookupCalls, repository.touchCalls, repository.emptyCalls, repository.recordUseCalls)
	}
	if repository.lookupKey.URL != result.Source.FinalURL || !strings.Contains(repository.lookupKey.URL, "/final") {
		t.Fatalf("lookup key URL = %q, want final artifact URL", repository.lookupKey.URL)
	}
}

func TestHydrateCompiledEvidenceLeavesUnprovableRangesUnlocated(t *testing.T) {
	fetchedAt := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		data    json.RawMessage
		quote   string
		cleaned string
	}{
		{
			name:    "attribute value is absent from cleaned text",
			data:    json.RawMessage(`{"released":"2026-08-09T06:30:00Z"}`),
			quote:   "August 9, 2026 14:30 +08:00",
			cleaned: "Launch date",
		},
		{
			name:    "duplicate quote has no trustworthy global range",
			data:    json.RawMessage(`{"released":"Same"}`),
			quote:   "Same",
			cleaned: "Same and Same",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			basis, rate, err := hydrateCompiledEvidence(
				test.data,
				map[string]evidence.Anchor{"released": {Quote: test.quote, Selector: "time.release", Method: evidence.MethodCompiled}},
				test.cleaned,
				"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				fetchedAt,
			)
			if err != nil {
				t.Fatalf("hydrateCompiledEvidence(): %v", err)
			}
			anchor := basis["released"]
			if rate != 1 || anchor.TextRange != [2]int{} || anchor.Quote != test.quote || anchor.Selector != "time.release" || anchor.Method != evidence.MethodCompiled {
				t.Fatalf("basis/rate = %#v / %v", basis, rate)
			}
		})
	}
}

const testExtractorID = "123e4567-e89b-12d3-a456-426614174000"

type recordingCompiledRepository struct {
	extractor compiler.Extractor
	found     bool
	lookupErr error
	touchErr  error
	emptyErr  error

	lookupCalls    int
	touchCalls     int
	emptyCalls     int
	recordUseCalls int
	lookupKey      compiler.PageKey
	lookupHook     func()
	touchHook      func()
}

func (repository *recordingCompiledRepository) Lookup(_ context.Context, key compiler.PageKey) (compiler.Extractor, bool, error) {
	repository.lookupCalls++
	repository.lookupKey = key
	if repository.lookupHook != nil {
		repository.lookupHook()
	}
	return repository.extractor, repository.found, repository.lookupErr
}

func (repository *recordingCompiledRepository) Touch(_ context.Context, _ compiler.PageKey, _ string) (compiler.Extractor, error) {
	repository.touchCalls++
	if repository.touchHook != nil {
		repository.touchHook()
	}
	if repository.touchErr != nil {
		return compiler.Extractor{}, repository.touchErr
	}
	return repository.extractor, nil
}

func (repository *recordingCompiledRepository) RecordEmpty(_ context.Context, _ compiler.PageKey, _ string) (compiler.Extractor, error) {
	repository.emptyCalls++
	if repository.emptyErr != nil {
		return compiler.Extractor{}, repository.emptyErr
	}
	return repository.extractor, nil
}

func (repository *recordingCompiledRepository) RecordUse(_ context.Context, _ compiler.PageKey, _ string, _ compiler.UseOutcome) (compiler.Extractor, error) {
	repository.recordUseCalls++
	return repository.extractor, nil
}

func compiledHitRepository(ir compiler.IR) *recordingCompiledRepository {
	compiledAt := time.Date(2026, time.August, 9, 8, 0, 0, 0, time.UTC)
	return &recordingCompiledRepository{
		found: true,
		extractor: compiler.Extractor{
			ID:         testExtractorID,
			IR:         ir,
			Version:    3,
			Validation: compiler.ValidationReport{Overall: 0.97, Threshold: compiler.ValidationThreshold, CanEnable: true},
			State:      compiler.StateActive,
			CreatedAt:  compiledAt,
		},
	}
}

func requiredCountIR() compiler.IR {
	return compiler.IR{Version: compiler.CurrentIRVersion, Fields: []compiler.FieldRule{{
		Name: "count", Selector: ".count", Transforms: []string{"trim", "parse_number"}, Type: compiler.TypeNumber, Required: true,
	}}}
}

func requiredMissingIR() compiler.IR {
	return compiler.IR{Version: compiler.CurrentIRVersion, Fields: []compiler.FieldRule{{
		Name: "count", Selector: ".missing", Type: compiler.TypeNumber, Required: true,
	}}}
}

func optionalMissingIR() compiler.IR {
	return compiler.IR{Version: compiler.CurrentIRVersion, Fields: []compiler.FieldRule{{
		Name: "note", Selector: ".missing", Type: compiler.TypeString,
	}}}
}

func compiledScrapeResult() *scrape.Result {
	result := successfulScrapeResult()
	result.Response.Content = "Count: 3"
	result.Source.RawHTML = `<html><body><main><span class="count">3</span></main></body></html>`
	return result
}

func newDispatchService(
	t *testing.T,
	runner Runner,
	client StructuredExtractor,
	signer ReceiptSigner,
	repository CompiledRepository,
) *Service {
	t.Helper()
	service, err := NewService(runner, client, signer, Config{CompiledRepository: repository})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return service
}
