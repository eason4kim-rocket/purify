package extract

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/use-agent/purify/compiler"
	"github.com/use-agent/purify/llm"
	"github.com/use-agent/purify/models"
)

func TestCompleteLLMSuccessObservesCompilerForDirectAndFallbackPaths(t *testing.T) {
	for _, engine := range []string{"llm", "auto"} {
		t.Run(engine, func(t *testing.T) {
			result := successfulScrapeResult()
			observer := &recordingCompileObserver{}
			client := &recordingExtractor{initial: &llm.ExtractResult{Data: json.RawMessage(`{"count":3}`)}}
			var repository CompiledRepository
			if engine == "auto" {
				repository = &recordingCompiledRepository{}
				client.initial = &llm.ExtractResult{Data: json.RawMessage(`{"count":"three"}`)}
				client.repaired = &llm.ExtractResult{Data: json.RawMessage(`{"count":3}`)}
			}
			service, err := NewService(&recordingRunner{result: result}, client, nil, Config{
				CompiledRepository: repository,
				CompileObserver:    observer,
			})
			if err != nil {
				t.Fatalf("NewService() error = %v", err)
			}
			request := validExtractRequest()
			request.Engine = engine
			request.LLMAPIKey = "request-secret"
			request.LLMModel = "request-model"
			request.LLMBaseURL = "https://request-provider.example/v1"

			response, err := service.Extract(context.Background(), request)
			if err != nil || response == nil || !response.Success || response.Partial {
				t.Fatalf("Extract() = %#v, %v", response, err)
			}
			if observer.calls != 1 {
				t.Fatalf("observer calls = %d, want 1", observer.calls)
			}
			if engine == "auto" && client.repairCalls != 1 {
				t.Fatalf("auto fallback repair calls = %d, want 1", client.repairCalls)
			}
			if observer.snapshotID != string(result.Source.SnapshotID) || !observer.fetchedAt.Equal(result.Source.FetchedAt) {
				t.Fatalf("observer provenance = %q / %s", observer.snapshotID, observer.fetchedAt)
			}
			if observer.page.URL != result.Source.FinalURL || observer.page.PageHash == "" ||
				observer.page.SchemaHash == "" || observer.page.TemplateSimHash == 0 ||
				string(observer.schema) != string(observer.page.Schema) {
				t.Fatalf("observer page/schema = %#v / %s", observer.page, observer.schema)
			}
			boundary := strings.Join([]string{observer.page.URL, observer.page.Host, observer.page.PageHash,
				observer.page.SchemaHash, string(observer.schema), observer.snapshotID}, "\n")
			for _, forbidden := range []string{"request-secret", "request-model", "request-provider.example",
				result.Response.Content, string(response.Data)} {
				if strings.Contains(boundary, forbidden) {
					t.Fatalf("observer boundary received forbidden value %q", forbidden)
				}
			}
		})
	}
}

func TestTypedNilCompilerObserverIsDisabled(t *testing.T) {
	var observer *recordingCompileObserver
	service, err := NewService(
		&recordingRunner{result: successfulScrapeResult()},
		&recordingExtractor{initial: &llm.ExtractResult{Data: json.RawMessage(`{"count":3}`)}},
		nil,
		Config{CompileObserver: observer},
	)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	request := validExtractRequest()
	request.Engine = "llm"
	response, err := service.Extract(context.Background(), request)
	if err != nil || response == nil || !response.Success || service.compileObserver != nil {
		t.Fatalf("Extract() = %#v, %v; observer = %#v", response, err, service.compileObserver)
	}
}

func TestCompilerObservationRequiresCompleteDefaultProfileFreshLLMSuccess(t *testing.T) {
	tests := []struct {
		name         string
		configure    func(*models.ExtractRequest, *recordingExtractor, *recordingRunner)
		repository   CompiledRepository
		wantPartial  bool
		wantCompiled bool
	}{
		{
			name: "partial after repair",
			configure: func(_ *models.ExtractRequest, client *recordingExtractor, _ *recordingRunner) {
				client.initial = &llm.ExtractResult{Data: json.RawMessage(`{"count":"three"}`)}
				client.repaired = &llm.ExtractResult{Data: json.RawMessage(`{"count":"still-three"}`)}
			},
			wantPartial: true,
		},
		{name: "compiled response", repository: compiledHitRepository(requiredCountIR()), wantCompiled: true, configure: func(request *models.ExtractRequest, _ *recordingExtractor, runner *recordingRunner) {
			request.Engine = "compiled"
			request.LLMAPIKey = ""
			runner.result = compiledScrapeResult()
		}},
		{name: "selector profile", configure: func(request *models.ExtractRequest, _ *recordingExtractor, _ *recordingRunner) {
			request.CSSSelector = "main"
		}},
		{name: "HTML profile", configure: func(request *models.ExtractRequest, _ *recordingExtractor, _ *recordingRunner) {
			request.OutputFormat = "html"
		}},
		{name: "raw profile", configure: func(request *models.ExtractRequest, _ *recordingExtractor, _ *recordingRunner) {
			request.ExtractMode = "raw"
		}},
		{name: "response-only artifact", configure: func(_ *models.ExtractRequest, _ *recordingExtractor, runner *recordingRunner) {
			runner.result.Source = nil
		}},
		{name: "missing snapshot", configure: func(_ *models.ExtractRequest, _ *recordingExtractor, runner *recordingRunner) {
			runner.result.Source.SnapshotID = ""
		}},
		{name: "missing raw HTML", configure: func(_ *models.ExtractRequest, _ *recordingExtractor, runner *recordingRunner) {
			runner.result.Source.RawHTML = ""
		}},
		{name: "missing fetch time", configure: func(_ *models.ExtractRequest, _ *recordingExtractor, runner *recordingRunner) {
			runner.result.Source.FetchedAt = time.Time{}
		}},
		{name: "unusable source status", configure: func(_ *models.ExtractRequest, _ *recordingExtractor, runner *recordingRunner) {
			runner.result.Source.StatusCode = 503
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &recordingRunner{result: successfulScrapeResult()}
			client := &recordingExtractor{initial: &llm.ExtractResult{Data: json.RawMessage(`{"count":3}`)}}
			request := validExtractRequest()
			request.Engine = "llm"
			test.configure(request, client, runner)
			observer := &recordingCompileObserver{}
			service, err := NewService(runner, client, nil, Config{
				CompiledRepository: test.repository,
				CompileObserver:    observer,
			})
			if err != nil {
				t.Fatalf("NewService() error = %v", err)
			}
			response, err := service.Extract(context.Background(), request)
			if err != nil || response == nil || !response.Success || response.Partial != test.wantPartial || (response.Extractor != nil) != test.wantCompiled {
				t.Fatalf("Extract() = %#v, %v", response, err)
			}
			if observer.calls != 0 {
				t.Fatalf("observer calls = %d, want 0", observer.calls)
			}
		})
	}
}

func TestCompilerObserverFailureAndPanicCannotReverseSuccessfulResponse(t *testing.T) {
	for _, test := range []struct {
		name     string
		observer *recordingCompileObserver
	}{
		{name: "error", observer: &recordingCompileObserver{err: errors.New("catalog secret detail")}},
		{name: "panic", observer: &recordingCompileObserver{panicValue: "observer panic secret"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
			test.observer.onObserve = func() { current = current.Add(time.Hour) }
			service, err := NewService(
				&recordingRunner{result: successfulScrapeResult()},
				&recordingExtractor{initial: &llm.ExtractResult{Data: json.RawMessage(`{"count":3}`)}},
				nil,
				Config{Now: func() time.Time { return current }, CompileObserver: test.observer},
			)
			if err != nil {
				t.Fatalf("NewService() error = %v", err)
			}
			request := validExtractRequest()
			request.Engine = "llm"
			response, err := service.Extract(context.Background(), request)
			if err != nil || response == nil || !response.Success || response.Timing.TotalMs != 0 {
				t.Fatalf("Extract() = %#v, %v", response, err)
			}
			if test.observer.calls != 1 || !current.Equal(time.Date(2026, time.August, 9, 13, 0, 0, 0, time.UTC)) {
				t.Fatalf("observer calls/time = %d/%s", test.observer.calls, current)
			}
		})
	}
}

func TestCompilerObservationRunsAfterEvidenceSigning(t *testing.T) {
	signer := &recordingSigner{}
	observer := &recordingCompileObserver{}
	observer.onObserve = func() {
		if len(signer.payloads) == 0 {
			t.Fatal("observer ran before evidence receipts were signed")
		}
	}
	service, err := NewService(
		&recordingRunner{result: successfulScrapeResult()},
		&recordingExtractor{initial: &llm.ExtractResult{Data: json.RawMessage(`{"count":3}`)}},
		signer,
		Config{CompileObserver: observer},
	)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	request := validExtractRequest()
	request.Engine = "llm"
	request.Evidence = true
	response, err := service.Extract(context.Background(), request)
	if err != nil || response == nil || response.Receipts == nil || observer.calls != 1 {
		t.Fatalf("Extract() = %#v, %v; observer calls = %d", response, err, observer.calls)
	}
}

type recordingCompileObserver struct {
	calls      int
	page       compiler.PageKey
	snapshotID string
	fetchedAt  time.Time
	schema     json.RawMessage
	err        error
	panicValue any
	onObserve  func()
}

func (observer *recordingCompileObserver) Observe(
	_ context.Context,
	page compiler.PageKey,
	snapshotID string,
	fetchedAt time.Time,
	schema json.RawMessage,
) error {
	observer.calls++
	observer.page = page
	observer.page.Schema = append(json.RawMessage(nil), page.Schema...)
	observer.snapshotID = snapshotID
	observer.fetchedAt = fetchedAt
	observer.schema = append(json.RawMessage(nil), schema...)
	if observer.onObserve != nil {
		observer.onObserve()
	}
	if observer.panicValue != nil {
		panic(observer.panicValue)
	}
	return observer.err
}
