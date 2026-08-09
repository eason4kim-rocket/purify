package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/use-agent/purify/consensus"
	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/extract"
	"github.com/use-agent/purify/llm"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/receipts"
	"github.com/use-agent/purify/scraper"
	"github.com/use-agent/purify/snapshot"
)

const searchTestSnapshotID = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type searchArtifactPlan struct {
	artifact *extract.Artifact
	err      error
}

type stubSearchArtifactService struct {
	mu sync.Mutex

	plans       map[string]searchArtifactPlan
	fetchHook   func(context.Context, string) (*extract.Artifact, error)
	extractHook func(context.Context, *extract.Artifact, *models.ExtractRequest) (*models.ExtractResponse, error)

	fetchCalls        []string
	extractCalls      int
	extractedArtifact *extract.Artifact
	extractedRequest  *models.ExtractRequest
}

func (service *stubSearchArtifactService) FetchPublicArtifact(ctx context.Context, rawURL string) (*extract.Artifact, error) {
	service.mu.Lock()
	service.fetchCalls = append(service.fetchCalls, rawURL)
	hook := service.fetchHook
	plan := service.plans[rawURL]
	service.mu.Unlock()
	if hook != nil {
		return hook(ctx, rawURL)
	}
	return plan.artifact, plan.err
}

func (service *stubSearchArtifactService) ExtractArtifact(ctx context.Context, artifact *extract.Artifact, request *models.ExtractRequest) (*models.ExtractResponse, error) {
	service.mu.Lock()
	service.extractCalls++
	service.extractedArtifact = artifact
	service.extractedRequest = cloneExtractRequestForSearchTest(request)
	hook := service.extractHook
	service.mu.Unlock()
	if hook == nil {
		return nil, errors.New("unexpected extraction")
	}
	return hook(ctx, artifact, request)
}

func (service *stubSearchArtifactService) snapshot() ([]string, int, *extract.Artifact, *models.ExtractRequest) {
	service.mu.Lock()
	defer service.mu.Unlock()
	return append([]string(nil), service.fetchCalls...), service.extractCalls, service.extractedArtifact, cloneExtractRequestForSearchTest(service.extractedRequest)
}

type stubSearchReceiptSigner struct {
	mu       sync.Mutex
	token    string
	err      error
	panicNow bool
	hook     func(receipts.Payload)
	payloads []receipts.Payload
}

func (signer *stubSearchReceiptSigner) Sign(payload receipts.Payload) (string, error) {
	if signer.panicNow {
		panic("private signer panic")
	}
	payload.Value = bytes.Clone(payload.Value)
	payload.Anchor.Quote = strings.Clone(payload.Anchor.Quote)
	if signer.hook != nil {
		signer.hook(payload)
	}
	signer.mu.Lock()
	signer.payloads = append(signer.payloads, payload)
	token, err := signer.token, signer.err
	signer.mu.Unlock()
	return token, err
}

func (signer *stubSearchReceiptSigner) snapshot() []receipts.Payload {
	signer.mu.Lock()
	defer signer.mu.Unlock()
	return append([]receipts.Payload(nil), signer.payloads...)
}

func TestWithEnrichmentRejectsMissingAndTypedNilDependencies(t *testing.T) {
	provider := &stubSearchProvider{name: "stub"}
	artifacts := &stubSearchArtifactService{}
	signer := &stubSearchReceiptSigner{token: "receipt"}
	var nilArtifacts *stubSearchArtifactService
	var nilSigner *stubSearchReceiptSigner

	for _, option := range []ServiceOption{
		WithEnrichment(nil, signer),
		WithEnrichment(nilArtifacts, signer),
		WithEnrichment(artifacts, nil),
		WithEnrichment(artifacts, nilSigner),
	} {
		if service, err := NewService(provider, option); err == nil || service != nil {
			t.Fatalf("NewService(invalid enrichment) = (%#v, %v), want error", service, err)
		}
	}

	service, err := NewService(provider, WithEnrichment(artifacts, signer))
	if err != nil || !service.enrichmentAvailable() || cap(service.enrichmentSlots) != defaultSearchEnrichmentSlots {
		t.Fatalf("NewService(valid enrichment) = (%#v, %v)", service, err)
	}
}

func TestSearchEnrichmentReusesOneArtifactAndCopiesCompleteExtraction(t *testing.T) {
	requestedURL := "https://source.example/article"
	finalURL := "https://final.example/article"
	fetchedAt := time.Date(2026, time.August, 10, 1, 2, 3, 0, time.UTC)
	artifact := validSearchArtifact(finalURL, "claim Ada", 200, fetchedAt)
	extractionBasis := models.EvidenceBasis{
		"name": {
			Quote:      "Ada",
			TextRange:  [2]int{6, 9},
			Method:     evidence.MethodExact,
			SnapshotID: searchTestSnapshotID,
			FetchedAt:  fetchedAt,
		},
	}
	extractionReceipts := models.FieldReceipts{"name": "field-receipt"}
	zero := 0.0
	extractionResponse := &models.ExtractResponse{
		Success:       true,
		Data:          json.RawMessage(`{"name":"Ada"}`),
		SnapshotID:    searchTestSnapshotID,
		Basis:         &extractionBasis,
		Receipts:      &extractionReceipts,
		UnlocatedRate: &zero,
		Extractor: &models.ExtractorMetadata{
			ID: "extractor-id", Version: 2, CompiledAt: fetchedAt, Validation: 0.98, Mode: "compiled",
		},
		LLMUsage: &models.LLMUsage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5},
	}
	artifacts := &stubSearchArtifactService{
		plans: map[string]searchArtifactPlan{requestedURL: {artifact: artifact}},
		extractHook: func(_ context.Context, gotArtifact *extract.Artifact, _ *models.ExtractRequest) (*models.ExtractResponse, error) {
			if gotArtifact != artifact {
				t.Fatalf("ExtractArtifact artifact = %p, want fetched %p", gotArtifact, artifact)
			}
			return extractionResponse, nil
		},
	}
	signer := &stubSearchReceiptSigner{token: "snippet-receipt"}
	provider := &stubSearchProvider{name: "stub", results: []ProviderResult{{
		Rank: 1, URL: requestedURL, Title: "Title", Snippet: "claim",
	}}}
	service, err := NewService(provider, WithEnrichment(artifacts, signer))
	if err != nil {
		t.Fatal(err)
	}
	schema := json.RawMessage(`{"properties":{"name":{"type":"string"}},"required":["name"],"type":"object"}`)
	wantSchema, err := llm.NormalizeSchema(schema)
	if err != nil {
		t.Fatal(err)
	}
	request := &models.SearchRequest{
		Query: "q", Limit: 1, IncludeContent: true, Verify: true, Schema: schema,
		Engine: "compiled", LLMAPIKey: "caller-key", LLMModel: "caller-model", LLMBaseURL: "https://llm.example/v1",
	}
	response, err := service.Search(context.Background(), request)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if !response.Success || response.Partial || response.DroppedStale != 0 || len(response.Results) != 1 {
		t.Fatalf("response envelope = %#v", response)
	}
	result := response.Results[0]
	if result.FinalURL != finalURL || result.Content != "claim Ada" || result.Verified == nil || !*result.Verified ||
		result.VerificationStatus != models.SearchVerificationVerified || result.Evidence == nil ||
		result.Receipt != "snippet-receipt" || string(result.Data) != `{"name":"Ada"}` ||
		result.Basis == nil || (*result.Basis)["name"].Quote != "Ada" ||
		result.Receipts == nil || (*result.Receipts)["name"] != "field-receipt" ||
		result.UnlocatedRate == nil || *result.UnlocatedRate != 0 || result.Extractor == nil ||
		result.Extractor.ID != "extractor-id" || result.LLMUsage == nil || result.LLMUsage.TotalTokens != 5 ||
		len(result.Errors) != 0 {
		t.Fatalf("enriched result = %#v", result)
	}

	fetchCalls, extractCalls, extractedArtifact, extractedRequest := artifacts.snapshot()
	if !reflect.DeepEqual(fetchCalls, []string{requestedURL}) || extractCalls != 1 || extractedArtifact != artifact {
		t.Fatalf("artifact calls = fetch %#v, extract %d, artifact %p", fetchCalls, extractCalls, extractedArtifact)
	}
	if extractedRequest == nil || extractedRequest.URL != finalURL || !bytes.Equal(extractedRequest.Schema, wantSchema) ||
		extractedRequest.Engine != "compiled" || extractedRequest.LLMAPIKey != "caller-key" ||
		extractedRequest.LLMModel != "caller-model" || extractedRequest.LLMBaseURL != "https://llm.example/v1" ||
		!extractedRequest.Evidence || extractedRequest.ProxyURL != "" || extractedRequest.Sources != nil {
		t.Fatalf("ExtractArtifact request = %#v", extractedRequest)
	}
	if request.Limit != 1 || request.Timeout != 0 || request.Deduplicate != nil || request.LLMAPIKey != "caller-key" ||
		!bytes.Equal(request.Schema, schema) {
		t.Fatalf("Search mutated caller request: %#v", request)
	}
	payloads := signer.snapshot()
	if len(payloads) != 1 || payloads[0].URL != finalURL || payloads[0].Path != "snippet" ||
		string(payloads[0].Value) != `"claim"` || payloads[0].Anchor.SnapshotID != searchTestSnapshotID ||
		!payloads[0].Anchor.FetchedAt.Equal(fetchedAt) || payloads[0].Anchor.Quote != "claim" {
		t.Fatalf("snippet receipt payloads = %#v", payloads)
	}

	// Search owns every value it publishes; mutating a dependency response
	// after return must not rewrite the public result.
	extractionResponse.Data[0] = '['
	changed := (*extractionResponse.Basis)["name"]
	changed.Quote = "changed"
	(*extractionResponse.Basis)["name"] = changed
	(*extractionResponse.Receipts)["name"] = "changed"
	extractionResponse.Extractor.ID = "changed"
	extractionResponse.LLMUsage.TotalTokens = 99
	if string(response.Results[0].Data) != `{"name":"Ada"}` || (*response.Results[0].Basis)["name"].Quote != "Ada" ||
		(*response.Results[0].Receipts)["name"] != "field-receipt" || response.Results[0].Extractor.ID != "extractor-id" ||
		response.Results[0].LLMUsage.TotalTokens != 5 {
		t.Fatalf("dependency mutation escaped into response: %#v", response.Results[0])
	}
}

func TestSearchEnrichmentDropsStaleCollapsesAliasesAndDoesNotBackfill(t *testing.T) {
	requested := []string{
		"https://one.example/a", "https://two.example/b", "https://three.example/c",
		"https://four.example/d", "https://five.example/e", "https://six.example/f",
	}
	sharedFinal := "https://canonical.example/story"
	fetchedAt := time.Date(2026, time.August, 10, 2, 0, 0, 0, time.UTC)
	plans := map[string]searchArtifactPlan{
		requested[0]: {artifact: validSearchArtifact(sharedFinal, "winner", 200, fetchedAt)},
		requested[1]: {artifact: validSearchArtifact(sharedFinal, "", 200, fetchedAt)},
		requested[2]: {artifact: validSearchArtifact("https://three.example/gone", "", 404, fetchedAt)},
		requested[3]: {artifact: validSearchArtifact("https://four.example/gone", "", 410, fetchedAt)},
		requested[4]: {err: models.NewScrapeError(models.ErrCodeRateLimited, "private quota detail", errors.New("secret"))},
		requested[5]: {artifact: validSearchArtifact("https://six.example/final", "must not fetch", 200, fetchedAt)},
	}
	providerResults := make([]ProviderResult, len(requested))
	for index, rawURL := range requested {
		providerResults[index] = ProviderResult{Rank: index + 1, URL: rawURL, Title: string(rune('A' + index))}
	}
	provider := &stubSearchProvider{name: "stub", results: providerResults}
	artifacts := &stubSearchArtifactService{plans: plans}
	signer := &stubSearchReceiptSigner{token: "receipt"}
	service, _ := NewService(provider, WithEnrichment(artifacts, signer))
	deduplicate := false
	response, err := service.Search(context.Background(), &models.SearchRequest{
		Query: "q", Limit: 6, Deduplicate: &deduplicate, IncludeContent: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.DroppedStale != 2 || response.Deduplicated != 1 || !response.Partial || len(response.Results) != 3 {
		t.Fatalf("response envelope = %#v", response)
	}
	if response.Results[0].URL != requested[0] || response.Results[0].FinalURL != sharedFinal || response.Results[0].Content != "winner" ||
		response.Results[1].URL != requested[4] || response.Results[2].URL != requested[5] {
		t.Fatalf("kept results = %#v", response.Results)
	}
	for index, result := range response.Results {
		if result.Rank != index+1 {
			t.Fatalf("result[%d].rank = %d", index, result.Rank)
		}
	}
	if len(response.Results[1].Errors) != 1 || response.Results[1].Errors[0] != (models.SearchResultError{
		Stage: models.SearchResultStageFetch, Code: models.ErrCodeRateLimited, Message: "result fetch was rate limited",
	}) {
		t.Fatalf("rate-limited result errors = %#v", response.Results[1].Errors)
	}
	if response.Results[2].FinalURL != "" || response.Results[2].Content != "" ||
		response.Results[2].VerificationStatus != models.SearchVerificationNotChecked {
		t.Fatalf("sixth result was enriched: %#v", response.Results[2])
	}
	fetchCalls, _, _, _ := artifacts.snapshot()
	sort.Strings(fetchCalls)
	wantCalls := append([]string(nil), requested[:models.MaxSearchHeavyResults]...)
	sort.Strings(wantCalls)
	if !reflect.DeepEqual(fetchCalls, wantCalls) {
		t.Fatalf("fetch calls = %#v, want top five %#v", fetchCalls, wantCalls)
	}

	// The losing alias' content error is discarded with the alias itself and
	// therefore cannot make an otherwise successful response partial.
	provider = &stubSearchProvider{name: "stub", results: providerResults[:2]}
	artifacts = &stubSearchArtifactService{plans: plans}
	service, _ = NewService(provider, WithEnrichment(artifacts, signer))
	response, err = service.Search(context.Background(), &models.SearchRequest{
		Query: "alias only", Limit: 2, Deduplicate: &deduplicate, IncludeContent: true,
	})
	if err != nil || response.Partial || response.Deduplicated != 1 || len(response.Results) != 1 {
		t.Fatalf("alias-only response = (%#v, %v)", response, err)
	}
}

func TestSearchVerificationMismatchUnavailableAndPanicArePerResult(t *testing.T) {
	fetchedAt := time.Date(2026, time.August, 10, 3, 0, 0, 0, time.UTC)
	requested := []string{"https://one.example/", "https://two.example/", "https://three.example/"}
	badSnapshot := validSearchArtifact(requested[2], "present", 200, fetchedAt)
	badSnapshot.Source.SnapshotID = "sha256:bad"
	provider := &stubSearchProvider{name: "stub", results: []ProviderResult{
		{Rank: 1, URL: requested[0], Snippet: "missing"},
		{Rank: 2, URL: requested[1], Snippet: ""},
		{Rank: 3, URL: requested[2], Snippet: "present"},
	}}
	artifacts := &stubSearchArtifactService{plans: map[string]searchArtifactPlan{
		requested[0]: {artifact: validSearchArtifact(requested[0], "other page content", 200, fetchedAt)},
		requested[1]: {artifact: validSearchArtifact(requested[1], "page content", 200, fetchedAt)},
		requested[2]: {artifact: badSnapshot},
	}}
	service, _ := NewService(provider, WithEnrichment(artifacts, &stubSearchReceiptSigner{token: "receipt"}))
	response, err := service.Search(context.Background(), &models.SearchRequest{Query: "q", Limit: 3, Verify: true})
	if err != nil || !response.Partial || len(response.Results) != 3 {
		t.Fatalf("Search() = (%#v, %v)", response, err)
	}
	if response.Results[0].Verified == nil || *response.Results[0].Verified ||
		response.Results[0].VerificationStatus != models.SearchVerificationMismatch || len(response.Results[0].Errors) != 0 {
		t.Fatalf("mismatch result = %#v", response.Results[0])
	}
	for _, index := range []int{1, 2} {
		result := response.Results[index]
		if result.Verified == nil || *result.Verified || result.VerificationStatus != models.SearchVerificationUnavailable ||
			len(result.Errors) != 1 || result.Errors[0].Stage != models.SearchResultStageVerify ||
			result.Errors[0].Code != models.ErrCodeEvidenceUnavailable || result.Errors[0].Message != "result verification is unavailable" {
			t.Fatalf("unavailable result[%d] = %#v", index, result)
		}
	}

	panicSigner := &stubSearchReceiptSigner{panicNow: true}
	provider = &stubSearchProvider{name: "stub", results: []ProviderResult{{Rank: 1, URL: requested[0], Snippet: "present"}}}
	artifacts = &stubSearchArtifactService{plans: map[string]searchArtifactPlan{
		requested[0]: {artifact: validSearchArtifact(requested[0], "present", 200, fetchedAt)},
	}}
	service, _ = NewService(provider, WithEnrichment(artifacts, panicSigner))
	response, err = service.Search(context.Background(), &models.SearchRequest{Query: "panic", Limit: 1, Verify: true})
	if err != nil || !response.Partial || len(response.Results[0].Errors) != 1 ||
		response.Results[0].Errors[0].Code != models.ErrCodeEvidenceUnavailable ||
		strings.Contains(response.Results[0].Errors[0].Message, "panic") {
		t.Fatalf("signer panic response = (%#v, %v)", response, err)
	}
}

func TestSearchEnrichmentRecoversDependencyPanicsAndRejectsLateFetches(t *testing.T) {
	requestedURL := "https://example.com/page"
	fetchedAt := time.Date(2026, time.August, 10, 3, 30, 0, 0, time.UTC)
	provider := &stubSearchProvider{name: "stub", results: []ProviderResult{{Rank: 1, URL: requestedURL}}}

	artifacts := &stubSearchArtifactService{fetchHook: func(context.Context, string) (*extract.Artifact, error) {
		panic("private fetch panic")
	}}
	service, _ := NewService(provider, WithEnrichment(artifacts, &stubSearchReceiptSigner{token: "receipt"}))
	response, err := service.Search(context.Background(), &models.SearchRequest{Query: "panic", Limit: 1, IncludeContent: true})
	if err != nil || !response.Partial || len(response.Results) != 1 || len(response.Results[0].Errors) != 1 ||
		response.Results[0].Errors[0] != (models.SearchResultError{
			Stage: models.SearchResultStageFetch, Code: models.ErrCodeNavigation, Message: "result fetch failed",
		}) {
		t.Fatalf("fetch panic response = (%#v, %v)", response, err)
	}

	artifacts = &stubSearchArtifactService{fetchHook: func(ctx context.Context, _ string) (*extract.Artifact, error) {
		<-ctx.Done()
		// A broken dependency must not make work completed after the child
		// deadline observable to Search.
		return validSearchArtifact(requestedURL, "late content", 200, fetchedAt), nil
	}}
	service, _ = NewService(provider, WithEnrichment(artifacts, &stubSearchReceiptSigner{token: "receipt"}))
	service.enrichmentTimeout = time.Millisecond
	response, err = service.Search(context.Background(), &models.SearchRequest{Query: "late", Limit: 1, IncludeContent: true})
	if err != nil || !response.Partial || response.Results[0].FinalURL != "" || response.Results[0].Content != "" ||
		len(response.Results[0].Errors) != 1 || response.Results[0].Errors[0].Code != models.ErrCodeTimeout ||
		response.Results[0].Errors[0].Message != "result fetch timed out" {
		t.Fatalf("late fetch response = (%#v, %v)", response, err)
	}
}

func TestSearchEnrichmentStopsAfterVerificationTimeoutAndRejectsLateExtraction(t *testing.T) {
	requestedURL := "https://example.com/page"
	fetchedAt := time.Date(2026, time.August, 10, 3, 45, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	artifact := validSearchArtifact(requestedURL, "claim", 200, fetchedAt)
	schema := json.RawMessage(`{"type":"string"}`)

	t.Run("verification timeout stops content and extraction", func(t *testing.T) {
		artifacts := &stubSearchArtifactService{plans: map[string]searchArtifactPlan{
			requestedURL: {artifact: artifact},
		}}
		ctx, cancel := context.WithCancel(context.Background())
		signer := &stubSearchReceiptSigner{token: "receipt", hook: func(receipts.Payload) { cancel() }}
		service, _ := NewService(&stubSearchProvider{name: "stub"}, WithEnrichment(artifacts, signer))
		outcome := service.runEnrichmentWorker(
			ctx,
			make(chan struct{}, defaultSearchEnrichmentSlots),
			models.SearchResult{
				Rank: 1, URL: requestedURL, Snippet: "claim", VerificationStatus: models.SearchVerificationNotChecked,
			},
			preparedSearchRequest{
				requiresEnrichment: true, verify: true, includeContent: true, schema: schema,
				engine: "compiled", llmModel: "model", llmBaseURL: "https://llm.example/v1",
			},
		)
		_, extractCalls, _, _ := artifacts.snapshot()
		if !outcome.partial || extractCalls != 0 || outcome.result.Content != "" || outcome.result.Data != nil ||
			outcome.result.Verified == nil || *outcome.result.Verified ||
			outcome.result.VerificationStatus != models.SearchVerificationUnavailable ||
			len(outcome.result.Errors) != 1 || outcome.result.Errors[0].Stage != models.SearchResultStageVerify ||
			outcome.result.Errors[0].Code != models.ErrCodeTimeout {
			t.Fatalf("canceled verification outcome = %#v, extract calls=%d", outcome, extractCalls)
		}
	})

	t.Run("late extraction is never published", func(t *testing.T) {
		enteredWithError := make(chan error, 1)
		artifacts := &stubSearchArtifactService{
			plans: map[string]searchArtifactPlan{requestedURL: {artifact: artifact}},
			extractHook: func(ctx context.Context, _ *extract.Artifact, _ *models.ExtractRequest) (*models.ExtractResponse, error) {
				enteredWithError <- ctx.Err()
				<-ctx.Done()
				return &models.ExtractResponse{Success: true, Data: json.RawMessage(`"late"`)}, nil
			},
		}
		provider := &stubSearchProvider{name: "stub", results: []ProviderResult{{Rank: 1, URL: requestedURL}}}
		service, _ := NewService(provider, WithEnrichment(artifacts, &stubSearchReceiptSigner{token: "receipt"}))
		service.enrichmentTimeout = 20 * time.Millisecond
		response, err := service.Search(context.Background(), &models.SearchRequest{
			Query: "late extraction", Limit: 1, Schema: schema, Engine: "compiled",
		})
		if err != nil {
			t.Fatalf("Search() error = %v", err)
		}
		if entryErr := <-enteredWithError; entryErr != nil {
			t.Fatalf("ExtractArtifact entered with canceled context: %v", entryErr)
		}
		result := response.Results[0]
		if !response.Partial || result.Data != nil || result.Basis != nil || result.Receipts != nil ||
			result.UnlocatedRate != nil || len(result.Errors) != 1 ||
			result.Errors[0].Stage != models.SearchResultStageExtract || result.Errors[0].Code != models.ErrCodeTimeout {
			t.Fatalf("late extraction response = %#v", response)
		}
	})
}

func TestSearchSchemaPreservesJSONNullAndPartialBestEffort(t *testing.T) {
	requestedURL := "https://example.com/null"
	fetchedAt := time.Date(2026, time.August, 10, 4, 0, 0, 0, time.UTC)
	artifact := validSearchArtifact(requestedURL, "null", 200, fetchedAt)
	basis := models.EvidenceBasis{
		"$": {Quote: "null", TextRange: [2]int{0, 4}, Method: evidence.MethodExact, SnapshotID: searchTestSnapshotID, FetchedAt: fetchedAt},
	}
	tokens := models.FieldReceipts{"$": "field-receipt"}
	rate := 0.0
	schema := json.RawMessage(`{"type":"string"}`)
	violations, err := llm.ValidateAgainstSchema(schema, json.RawMessage(`null`))
	if err != nil || len(violations) == 0 {
		t.Fatalf("schema fixture violations = %#v, %v", violations, err)
	}
	extractionResponse := &models.ExtractResponse{
		Success: true, Data: json.RawMessage(`null`), Partial: true,
		Violations: violations,
		SnapshotID: searchTestSnapshotID, Basis: &basis, Receipts: &tokens, UnlocatedRate: &rate,
	}
	artifacts := &stubSearchArtifactService{
		plans: map[string]searchArtifactPlan{requestedURL: {artifact: artifact}},
		extractHook: func(context.Context, *extract.Artifact, *models.ExtractRequest) (*models.ExtractResponse, error) {
			return extractionResponse, nil
		},
	}
	provider := &stubSearchProvider{name: "stub", results: []ProviderResult{{Rank: 1, URL: requestedURL}}}
	service, _ := NewService(provider, WithEnrichment(artifacts, &stubSearchReceiptSigner{token: "receipt"}))
	response, err := service.Search(context.Background(), &models.SearchRequest{
		Query: "q", Limit: 1, Schema: schema, Engine: "compiled",
	})
	if err != nil || !response.Partial || string(response.Results[0].Data) != "null" ||
		response.Results[0].Basis == nil || response.Results[0].Receipts == nil ||
		len(response.Results[0].Violations) != 1 || len(response.Results[0].Errors) != 1 ||
		response.Results[0].Errors[0] != (models.SearchResultError{
			Stage: models.SearchResultStageExtract, Code: models.ErrCodeLLMFailure, Message: "result extraction was partial",
		}) {
		t.Fatalf("partial null response = (%#v, %v)", response, err)
	}
}

func TestCopySearchExtractionIsAtomicAndAcceptsUnlocatedCompiledEvidence(t *testing.T) {
	fetchedAt := time.Date(2026, time.August, 10, 4, 30, 0, 0, time.UTC)
	artifact := validSearchArtifact("https://example.com/", "other content", 200, fetchedAt)
	basis := models.EvidenceBasis{
		"name": {
			Quote: "Ada", Method: evidence.MethodCompiled,
			SnapshotID: searchTestSnapshotID, FetchedAt: fetchedAt,
		},
	}
	tokens := models.FieldReceipts{"name": "receipt"}
	rate := 1.0
	valid := &models.ExtractResponse{
		Success: true, Data: json.RawMessage(`{"name":"Ada"}`), SnapshotID: searchTestSnapshotID,
		Basis: &basis, Receipts: &tokens, UnlocatedRate: &rate,
	}
	schema := json.RawMessage(`{"properties":{"name":{"type":"string"}},"required":["name"],"type":"object"}`)
	var result models.SearchResult
	if partial, err := copySearchExtraction(
		context.Background(), &result, artifact, artifact.Source.FinalURL, fetchedAt, schema, valid,
	); err != nil || partial || result.Basis == nil ||
		(*result.Basis)["name"].Method != evidence.MethodCompiled || (*result.Basis)["name"].TextRange != [2]int{} {
		t.Fatalf("copySearchExtraction(compiled unlocated) = (%#v, %v, partial=%v)", result, err, partial)
	}

	invalid := *valid
	invalid.LLMUsage = &models.LLMUsage{PromptTokens: -1}
	result = models.SearchResult{}
	if _, err := copySearchExtraction(
		context.Background(), &result, artifact, artifact.Source.FinalURL, fetchedAt, schema, &invalid,
	); err == nil {
		t.Fatal("copySearchExtraction(invalid usage) unexpectedly succeeded")
	}
	if result.Data != nil || result.Basis != nil || result.Receipts != nil || result.UnlocatedRate != nil ||
		result.Extractor != nil || result.LLMUsage != nil || result.Violations != nil {
		t.Fatalf("failed extraction copy published partial fields: %#v", result)
	}
}

func TestCopySearchExtractionRejectsUnboundedOrAmbiguousData(t *testing.T) {
	fetchedAt := time.Date(2026, time.August, 10, 4, 45, 0, 0, time.UTC)
	artifact := validSearchArtifact("https://example.com/", "content", 200, fetchedAt)
	emptyBasis := models.EvidenceBasis{}
	emptyReceipts := models.FieldReceipts{}
	zero := 0.0
	deep := strings.Repeat("[", consensus.MaxJSONDepth+1) + "0" + strings.Repeat("]", consensus.MaxJSONDepth+1)
	nodes := "[" + strings.Repeat("{},", consensus.MaxJSONNodesPerSource) + "{}]"
	leaves := "[" + strings.Repeat("0,", consensus.MaxLeavesPerSource) + "0]"
	longPath := `{"` + strings.Repeat("p", consensus.MaxPathBytes+1) + `":0}`
	longNumber := "1" + strings.Repeat("0", consensus.MaxNumberBytes)
	tests := []struct {
		name string
		data json.RawMessage
	}{
		{name: "depth", data: json.RawMessage(deep)},
		{name: "nodes", data: json.RawMessage(nodes)},
		{name: "leaves", data: json.RawMessage(leaves)},
		{name: "path", data: json.RawMessage(longPath)},
		{name: "number", data: json.RawMessage(longNumber)},
		{name: "duplicate key", data: json.RawMessage(`{"a":1,"a":2}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := &models.ExtractResponse{
				Success: true, Data: test.data, SnapshotID: searchTestSnapshotID,
				Basis: &emptyBasis, Receipts: &emptyReceipts, UnlocatedRate: &zero,
			}
			var result models.SearchResult
			if _, err := copySearchExtraction(
				context.Background(), &result, artifact, artifact.Source.FinalURL, fetchedAt,
				json.RawMessage(`{}`), response,
			); err == nil {
				t.Fatalf("copySearchExtraction(%s) unexpectedly succeeded", test.name)
			}
			if result.Data != nil || result.Basis != nil || result.Receipts != nil {
				t.Fatalf("rejected %s data was published: %#v", test.name, result)
			}
		})
	}
}

func TestCopySearchExtractionRequiresExactEvidencePathsAndRate(t *testing.T) {
	fetchedAt := time.Date(2026, time.August, 10, 4, 50, 0, 0, time.UTC)
	artifact := validSearchArtifact("https://example.com/", "Ada", 200, fetchedAt)
	schema := json.RawMessage(`{"properties":{"name":{"type":"string"}},"required":["name"],"type":"object"}`)
	baseBasis := models.EvidenceBasis{
		"name": {Quote: "Ada", TextRange: [2]int{0, 3}, Method: evidence.MethodExact, SnapshotID: searchTestSnapshotID, FetchedAt: fetchedAt},
	}
	baseReceipts := models.FieldReceipts{"name": "receipt"}
	zero := 0.0

	t.Run("receipt path set", func(t *testing.T) {
		wrongReceipts := models.FieldReceipts{"other": "receipt"}
		response := &models.ExtractResponse{
			Success: true, Data: json.RawMessage(`{"name":"Ada"}`), SnapshotID: searchTestSnapshotID,
			Basis: &baseBasis, Receipts: &wrongReceipts, UnlocatedRate: &zero,
		}
		var result models.SearchResult
		if _, err := copySearchExtraction(
			context.Background(), &result, artifact, artifact.Source.FinalURL, fetchedAt, schema, response,
		); err == nil {
			t.Fatal("mismatched receipt path set unexpectedly succeeded")
		}
	})

	t.Run("actual unlocated rate", func(t *testing.T) {
		unlocatedBasis := models.EvidenceBasis{
			"name": {Quote: "Ada", Method: evidence.MethodCompiled, SnapshotID: searchTestSnapshotID, FetchedAt: fetchedAt},
		}
		response := &models.ExtractResponse{
			Success: true, Data: json.RawMessage(`{"name":"Ada"}`), SnapshotID: searchTestSnapshotID,
			Basis: &unlocatedBasis, Receipts: &baseReceipts, UnlocatedRate: &zero,
		}
		var result models.SearchResult
		if _, err := copySearchExtraction(
			context.Background(), &result, artifact, artifact.Source.FinalURL, fetchedAt, schema, response,
		); err == nil {
			t.Fatal("incorrect unlocated rate unexpectedly succeeded")
		}
	})

	t.Run("invalid located range", func(t *testing.T) {
		badBasis := models.EvidenceBasis{
			"name": {Quote: "Ada", TextRange: [2]int{1, 4}, Method: evidence.MethodExact, SnapshotID: searchTestSnapshotID, FetchedAt: fetchedAt},
		}
		response := &models.ExtractResponse{
			Success: true, Data: json.RawMessage(`{"name":"Ada"}`), SnapshotID: searchTestSnapshotID,
			Basis: &badBasis, Receipts: &baseReceipts, UnlocatedRate: &zero,
		}
		var result models.SearchResult
		if _, err := copySearchExtraction(
			context.Background(), &result, artifact, artifact.Source.FinalURL, fetchedAt, schema, response,
		); err == nil {
			t.Fatal("invalid located range unexpectedly succeeded")
		}
	})
}

func TestSearchRejectsSchemaInconsistentExtractionPerResult(t *testing.T) {
	requestedURL := "https://example.com/schema"
	fetchedAt := time.Date(2026, time.August, 10, 4, 55, 0, 0, time.UTC)
	artifact := validSearchArtifact(requestedURL, "null", 200, fetchedAt)
	basis := models.EvidenceBasis{
		"$": {Quote: "null", TextRange: [2]int{0, 4}, Method: evidence.MethodExact, SnapshotID: searchTestSnapshotID, FetchedAt: fetchedAt},
	}
	receiptTokens := models.FieldReceipts{"$": "receipt"}
	zero := 0.0
	artifacts := &stubSearchArtifactService{
		plans: map[string]searchArtifactPlan{requestedURL: {artifact: artifact}},
		extractHook: func(context.Context, *extract.Artifact, *models.ExtractRequest) (*models.ExtractResponse, error) {
			return &models.ExtractResponse{
				Success: true, Data: json.RawMessage(`null`), Partial: false, SnapshotID: searchTestSnapshotID,
				Basis: &basis, Receipts: &receiptTokens, UnlocatedRate: &zero,
			}, nil
		},
	}
	provider := &stubSearchProvider{name: "stub", results: []ProviderResult{{Rank: 1, URL: requestedURL}}}
	service, _ := NewService(provider, WithEnrichment(artifacts, &stubSearchReceiptSigner{token: "receipt"}))
	response, err := service.Search(context.Background(), &models.SearchRequest{
		Query: "schema", Limit: 1, Schema: json.RawMessage(`{"type":"string"}`), Engine: "compiled",
	})
	if err != nil || !response.Partial || len(response.Results) != 1 || response.Results[0].Data != nil ||
		response.Results[0].Basis != nil || response.Results[0].Receipts != nil ||
		len(response.Results[0].Errors) != 1 || response.Results[0].Errors[0].Stage != models.SearchResultStageExtract ||
		response.Results[0].Errors[0].Code != models.ErrCodeInternal {
		t.Fatalf("schema-inconsistent response = (%#v, %v)", response, err)
	}
}

func TestCopySearchExtractionRejectsInconsistentOrOverflowingUsage(t *testing.T) {
	fetchedAt := time.Date(2026, time.August, 10, 4, 58, 0, 0, time.UTC)
	artifact := validSearchArtifact("https://example.com/", "Ada", 200, fetchedAt)
	basis := models.EvidenceBasis{
		"name": {Quote: "Ada", TextRange: [2]int{0, 3}, Method: evidence.MethodExact, SnapshotID: searchTestSnapshotID, FetchedAt: fetchedAt},
	}
	receiptsByPath := models.FieldReceipts{"name": "receipt"}
	zero := 0.0
	schema := json.RawMessage(`{"properties":{"name":{"type":"string"}},"required":["name"],"type":"object"}`)
	maximumInt := int(^uint(0) >> 1)
	for _, usage := range []models.LLMUsage{
		{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 4},
		{PromptTokens: maximumInt, CompletionTokens: 1, TotalTokens: maximumInt},
	} {
		response := &models.ExtractResponse{
			Success: true, Data: json.RawMessage(`{"name":"Ada"}`), SnapshotID: searchTestSnapshotID,
			Basis: &basis, Receipts: &receiptsByPath, UnlocatedRate: &zero, LLMUsage: &usage,
		}
		var result models.SearchResult
		if _, err := copySearchExtraction(
			context.Background(), &result, artifact, artifact.Source.FinalURL, fetchedAt, schema, response,
		); err == nil {
			t.Fatalf("usage %#v unexpectedly succeeded", usage)
		}
		if result.Data != nil || result.LLMUsage != nil {
			t.Fatalf("invalid usage published extraction: %#v", result)
		}
	}
}

func TestSearchSnippetReceiptUsesPublishedCanonicalFinalURLAndTime(t *testing.T) {
	requestedURL := "https://source.example.com/article"
	rawFinalURL := "HTTPS://Final.Example.COM:443/path#fragment"
	wantFinalURL := "https://final.example.com/path"
	fetchedAt := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	artifact := validSearchArtifact(rawFinalURL, "claim", 200, fetchedAt)
	artifacts := &stubSearchArtifactService{plans: map[string]searchArtifactPlan{requestedURL: {artifact: artifact}}}
	signer := &stubSearchReceiptSigner{token: "receipt"}
	provider := &stubSearchProvider{name: "stub", results: []ProviderResult{{Rank: 1, URL: requestedURL, Snippet: "claim"}}}
	service, _ := NewService(provider, WithEnrichment(artifacts, signer))
	response, err := service.Search(context.Background(), &models.SearchRequest{Query: "q", Limit: 1, Verify: true})
	if err != nil || response.Partial || len(response.Results) != 1 {
		t.Fatalf("Search() = (%#v, %v)", response, err)
	}
	result := response.Results[0]
	payloads := signer.snapshot()
	if result.FinalURL != wantFinalURL || result.Evidence == nil || result.Evidence.FetchedAt.Location() != time.UTC ||
		!result.Evidence.FetchedAt.Equal(fetchedAt) || len(payloads) != 1 || payloads[0].URL != wantFinalURL ||
		payloads[0].Anchor.FetchedAt.Location() != time.UTC {
		t.Fatalf("canonical receipt/result = result %#v, payloads %#v", result, payloads)
	}
}

func TestSearchExtractionClonesMethodAndCanonicalizesTimes(t *testing.T) {
	requestedURL := "https://example.com/time"
	fetchedAt := time.Date(2026, time.August, 10, 13, 0, 0, 123, time.FixedZone("UTC+8", 8*60*60))
	compiledAt := time.Date(2026, time.August, 9, 9, 0, 0, 456, time.FixedZone("UTC-7", -7*60*60))
	artifact := validSearchArtifact(requestedURL, "Ada", 200, fetchedAt)
	methodBacking := suffixOfLargeBacking("exact")
	basis := models.EvidenceBasis{
		"name": {
			Quote: "Ada", TextRange: [2]int{0, 3}, Method: evidence.Method(methodBacking),
			SnapshotID: searchTestSnapshotID, FetchedAt: fetchedAt,
		},
	}
	receiptTokens := models.FieldReceipts{"name": "receipt"}
	zero := 0.0
	extractionResponse := &models.ExtractResponse{
		Success: true, Data: json.RawMessage(`{"name":"Ada"}`), SnapshotID: searchTestSnapshotID,
		Basis: &basis, Receipts: &receiptTokens, UnlocatedRate: &zero,
		Extractor: &models.ExtractorMetadata{
			ID: "extractor", Version: 1, CompiledAt: compiledAt, Validation: 1, Mode: "compiled",
		},
	}
	artifacts := &stubSearchArtifactService{
		plans: map[string]searchArtifactPlan{requestedURL: {artifact: artifact}},
		extractHook: func(context.Context, *extract.Artifact, *models.ExtractRequest) (*models.ExtractResponse, error) {
			return extractionResponse, nil
		},
	}
	provider := &stubSearchProvider{name: "stub", results: []ProviderResult{{Rank: 1, URL: requestedURL}}}
	service, _ := NewService(provider, WithEnrichment(artifacts, &stubSearchReceiptSigner{token: "receipt"}))
	response, err := service.Search(context.Background(), &models.SearchRequest{
		Query: "time", Limit: 1,
		Schema: json.RawMessage(`{"properties":{"name":{"type":"string"}},"required":["name"],"type":"object"}`),
		Engine: "compiled",
	})
	if err != nil || response.Partial || response.Results[0].Basis == nil || response.Results[0].Extractor == nil {
		t.Fatalf("Search() = (%#v, %v)", response, err)
	}
	anchor := (*response.Results[0].Basis)["name"]
	if anchor.FetchedAt.Location() != time.UTC || !anchor.FetchedAt.Equal(fetchedAt) ||
		response.Results[0].Extractor.CompiledAt.Location() != time.UTC ||
		!response.Results[0].Extractor.CompiledAt.Equal(compiledAt) {
		t.Fatalf("canonical times = anchor %v, extractor %v", anchor.FetchedAt, response.Results[0].Extractor.CompiledAt)
	}
	assertDetachedString(t, "anchor method", string(anchor.Method), methodBacking)
}

func TestSearchRejectsUnencodableObservationTimesPerResult(t *testing.T) {
	requestedURL := "https://example.com/bad-time"
	goodTime := time.Date(2026, time.August, 10, 5, 0, 0, 0, time.UTC)
	badTime := time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC)
	provider := &stubSearchProvider{name: "stub", results: []ProviderResult{{Rank: 1, URL: requestedURL}}}

	t.Run("artifact fetched_at", func(t *testing.T) {
		artifacts := &stubSearchArtifactService{plans: map[string]searchArtifactPlan{
			requestedURL: {artifact: validSearchArtifact(requestedURL, "content", 200, badTime)},
		}}
		service, _ := NewService(provider, WithEnrichment(artifacts, &stubSearchReceiptSigner{token: "receipt"}))
		response, err := service.Search(context.Background(), &models.SearchRequest{Query: "q", Limit: 1, IncludeContent: true})
		if err != nil || !response.Partial || response.Results[0].Content != "" || len(response.Results[0].Errors) != 1 ||
			response.Results[0].Errors[0].Stage != models.SearchResultStageFetch {
			t.Fatalf("bad artifact time response = (%#v, %v)", response, err)
		}
		if _, err := json.Marshal(response); err != nil {
			t.Fatalf("bad artifact time poisoned response JSON: %v", err)
		}
	})

	for _, field := range []string{"anchor", "extractor"} {
		t.Run(field, func(t *testing.T) {
			artifact := validSearchArtifact(requestedURL, "Ada", 200, goodTime)
			basisTime := goodTime
			compiledTime := goodTime
			if field == "anchor" {
				basisTime = badTime
			} else {
				compiledTime = badTime
			}
			basis := models.EvidenceBasis{
				"name": {Quote: "Ada", TextRange: [2]int{0, 3}, Method: evidence.MethodExact, SnapshotID: searchTestSnapshotID, FetchedAt: basisTime},
			}
			receiptsByPath := models.FieldReceipts{"name": "receipt"}
			zero := 0.0
			extractionResponse := &models.ExtractResponse{
				Success: true, Data: json.RawMessage(`{"name":"Ada"}`), SnapshotID: searchTestSnapshotID,
				Basis: &basis, Receipts: &receiptsByPath, UnlocatedRate: &zero,
				Extractor: &models.ExtractorMetadata{
					ID: "extractor", Version: 1, CompiledAt: compiledTime, Validation: 1, Mode: "compiled",
				},
			}
			artifacts := &stubSearchArtifactService{
				plans: map[string]searchArtifactPlan{requestedURL: {artifact: artifact}},
				extractHook: func(context.Context, *extract.Artifact, *models.ExtractRequest) (*models.ExtractResponse, error) {
					return extractionResponse, nil
				},
			}
			service, _ := NewService(provider, WithEnrichment(artifacts, &stubSearchReceiptSigner{token: "receipt"}))
			response, err := service.Search(context.Background(), &models.SearchRequest{
				Query: field, Limit: 1,
				Schema: json.RawMessage(`{"properties":{"name":{"type":"string"}},"required":["name"],"type":"object"}`),
				Engine: "compiled",
			})
			if err != nil || !response.Partial || response.Results[0].Data != nil ||
				len(response.Results[0].Errors) != 1 || response.Results[0].Errors[0].Stage != models.SearchResultStageExtract {
				t.Fatalf("bad %s time response = (%#v, %v)", field, response, err)
			}
			if _, err := json.Marshal(response); err != nil {
				t.Fatalf("bad %s time poisoned response JSON: %v", field, err)
			}
		})
	}
}

func TestSearchEnrichmentUsesSharedFourSlotsAcrossRequests(t *testing.T) {
	entered := make(chan string, 12)
	release := make(chan struct{})
	fetchedAt := time.Date(2026, time.August, 10, 5, 0, 0, 0, time.UTC)
	artifacts := &stubSearchArtifactService{fetchHook: func(ctx context.Context, rawURL string) (*extract.Artifact, error) {
		entered <- rawURL
		select {
		case <-release:
			return validSearchArtifact(rawURL, "content", 200, fetchedAt), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	providerResults := make([]ProviderResult, 5)
	for index := range providerResults {
		providerResults[index] = ProviderResult{Rank: index + 1, URL: "https://example.com/" + string(rune('a'+index))}
	}
	provider := &stubSearchProvider{name: "stub", results: providerResults}
	service, _ := NewService(provider, WithEnrichment(artifacts, &stubSearchReceiptSigner{token: "receipt"}))

	errorsByRequest := make(chan error, 2)
	for _, query := range []string{"first", "second"} {
		query := query
		go func() {
			_, err := service.Search(context.Background(), &models.SearchRequest{Query: query, Limit: 5, IncludeContent: true})
			errorsByRequest <- err
		}()
	}
	for range defaultSearchEnrichmentSlots {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("four enrichment workers did not enter")
		}
	}
	select {
	case rawURL := <-entered:
		t.Fatalf("fifth worker entered shared four-slot boundary for %s", rawURL)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	for range 2 {
		if err := <-errorsByRequest; err != nil {
			t.Fatalf("Search() error = %v", err)
		}
	}
	if calls, _, _, _ := artifacts.snapshot(); len(calls) != 10 {
		t.Fatalf("fetch calls = %d, want ten top-five calls", len(calls))
	}
}

func TestSearchEnrichmentStableErrorMapping(t *testing.T) {
	tests := []struct {
		name string
		got  models.SearchResultError
		want models.SearchResultError
	}{
		{
			name: "fetch unknown",
			got:  fetchResultError(context.Background(), models.NewScrapeError("PRIVATE", "secret", nil)),
			want: models.SearchResultError{Stage: models.SearchResultStageFetch, Code: models.ErrCodeNavigation, Message: "result fetch failed"},
		},
		{
			name: "fetch timeout",
			got:  fetchResultError(context.Background(), context.DeadlineExceeded),
			want: models.SearchResultError{Stage: models.SearchResultStageFetch, Code: models.ErrCodeTimeout, Message: "result fetch timed out"},
		},
		{
			name: "verify unknown",
			got:  verifyResultError(context.Background(), errors.New("secret")),
			want: models.SearchResultError{Stage: models.SearchResultStageVerify, Code: models.ErrCodeEvidenceUnavailable, Message: "result verification is unavailable"},
		},
		{
			name: "extract unavailable",
			got:  extractResultError(context.Background(), models.NewScrapeError(models.ErrCodeExtractorUnavailable, "secret", nil)),
			want: models.SearchResultError{Stage: models.SearchResultStageExtract, Code: models.ErrCodeExtractorUnavailable, Message: "result extractor is unavailable"},
		},
		{
			name: "extract auth",
			got:  extractResultError(context.Background(), models.NewScrapeError(models.ErrCodeLLMAuthFailure, "secret", nil)),
			want: models.SearchResultError{Stage: models.SearchResultStageExtract, Code: models.ErrCodeLLMAuthFailure, Message: "result extraction authentication failed"},
		},
		{
			name: "extract unknown",
			got:  extractResultError(context.Background(), errors.New("secret")),
			want: models.SearchResultError{Stage: models.SearchResultStageExtract, Code: models.ErrCodeInternal, Message: "result extraction failed"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.got != test.want {
				t.Fatalf("error = %#v, want %#v", test.got, test.want)
			}
		})
	}
}

func validSearchArtifact(finalURL, content string, statusCode int, fetchedAt time.Time) *extract.Artifact {
	return &extract.Artifact{
		Public: &models.ScrapeResponse{
			Success: true, StatusCode: statusCode, FinalURL: finalURL, Content: content,
			Metadata: models.Metadata{Title: "Page", SourceURL: finalURL},
		},
		Source: &scraper.ScrapeResult{
			RawHTML:    "<html><body><p>" + content + "</p></body></html>",
			StatusCode: statusCode, FinalURL: finalURL, FetchedAt: fetchedAt,
			SnapshotID: snapshot.ID(searchTestSnapshotID),
		},
	}
}

func cloneExtractRequestForSearchTest(source *models.ExtractRequest) *models.ExtractRequest {
	if source == nil {
		return nil
	}
	cloned := *source
	cloned.Schema = bytes.Clone(source.Schema)
	cloned.Sources = append([]string(nil), source.Sources...)
	if source.WaitForNetworkIdle != nil {
		value := *source.WaitForNetworkIdle
		cloned.WaitForNetworkIdle = &value
	}
	return &cloned
}
