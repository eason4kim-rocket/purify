package models

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/use-agent/purify/evidence"
)

func TestSearchResourceContract(t *testing.T) {
	wants := map[string]int{
		"default limit":        DefaultSearchLimit,
		"maximum limit":        MaxSearchLimit,
		"heavy results":        MaxSearchHeavyResults,
		"domains":              MaxSearchDomains,
		"query runes":          MaxSearchQueryRunes,
		"query words":          MaxSearchQueryWords,
		"domain bytes":         MaxSearchDomainBytes,
		"URL bytes":            MaxSearchURLBytes,
		"schema bytes":         MaxSearchSchemaBytes,
		"request bytes":        MaxSearchRequestBytes,
		"response bytes":       MaxSearchResponseBytes,
		"result title bytes":   MaxSearchResultTitleBytes,
		"result snippet bytes": MaxSearchResultSnippetBytes,
		"result content bytes": MaxSearchResultContentBytes,
		"result data bytes":    MaxSearchResultDataBytes,
		"result receipt bytes": MaxSearchResultReceiptBytes,
		"LLM API key bytes":    MaxSearchLLMAPIKeyBytes,
		"LLM model bytes":      MaxSearchLLMModelBytes,
		"LLM base URL bytes":   MaxSearchLLMBaseURLBytes,
		"default timeout":      DefaultSearchTimeoutSeconds,
		"maximum timeout":      MaxSearchTimeoutSeconds,
	}
	expected := map[string]int{
		"default limit":        10,
		"maximum limit":        20,
		"heavy results":        5,
		"domains":              20,
		"query runes":          400,
		"query words":          50,
		"domain bytes":         253,
		"URL bytes":            16 << 10,
		"schema bytes":         512 << 10,
		"request bytes":        1 << 20,
		"response bytes":       32 << 20,
		"result title bytes":   16 << 10,
		"result snippet bytes": 64 << 10,
		"result content bytes": 4 << 20,
		"result data bytes":    4 << 20,
		"LLM API key bytes":    16 << 10,
		"LLM model bytes":      256,
		"LLM base URL bytes":   16 << 10,
		"default timeout":      30,
		"maximum timeout":      120,
	}
	for name, want := range expected {
		if got := wants[name]; got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
	}
	if ErrCodeSearchUnavailable != "SEARCH_UNAVAILABLE" || ErrCodeSearchFailed != "SEARCH_FAILED" {
		t.Fatalf("Search error codes = %q/%q", ErrCodeSearchUnavailable, ErrCodeSearchFailed)
	}
}

func TestSearchRequestDefaultsKeepBaselineLightweight(t *testing.T) {
	var nilRequest *SearchRequest
	nilRequest.Defaults()

	request := &SearchRequest{Query: "purify search"}
	request.Defaults()
	if request.Limit != DefaultSearchLimit || request.Timeout != DefaultSearchTimeoutSeconds ||
		request.Deduplicate == nil || !*request.Deduplicate {
		t.Fatalf("baseline defaults = %#v", request)
	}
	if request.IncludeContent || request.Verify || len(request.Schema) != 0 || request.Engine != "" ||
		request.LLMModel != "" || request.LLMBaseURL != "" || request.LLMAPIKey != "" {
		t.Fatalf("baseline unexpectedly enabled heavy work: %#v", request)
	}
}

func TestResolveSearchDefaultsIsModeAwareWithoutMaterializingRanking(t *testing.T) {
	tests := []struct {
		name        string
		ranking     SearchRankingMode
		limit       int
		timeout     int
		wantRanking SearchRankingMode
		wantLimit   int
		wantTimeout int
	}{
		{name: "omitted provider", wantRanking: SearchRankingProvider, wantLimit: 10, wantTimeout: 30},
		{name: "explicit provider", ranking: SearchRankingProvider, wantRanking: SearchRankingProvider, wantLimit: 10, wantTimeout: 30},
		{name: "relevance", ranking: SearchRankingRelevance, wantRanking: SearchRankingRelevance, wantLimit: 10, wantTimeout: 30},
		{name: "trust", ranking: SearchRankingTrust, wantRanking: SearchRankingTrust, wantLimit: 5, wantTimeout: 60},
		{name: "explicit values", ranking: SearchRankingTrust, limit: 3, timeout: 41, wantRanking: SearchRankingTrust, wantLimit: 3, wantTimeout: 41},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ResolveSearchDefaults(test.ranking, test.limit, test.timeout)
			if got.Ranking != test.wantRanking || got.Limit != test.wantLimit || got.TimeoutSeconds != test.wantTimeout {
				t.Fatalf("ResolveSearchDefaults() = %#v", got)
			}
			request := SearchRequest{Ranking: test.ranking, Limit: test.limit, Timeout: test.timeout}
			request.Defaults()
			if request.Ranking != test.ranking || request.Limit != test.wantLimit || request.Timeout != test.wantTimeout {
				t.Fatalf("Defaults() = %#v", request)
			}
		})
	}
}

func TestSearchRelevanceJSONIsAdditiveAndProviderShapeStaysExact(t *testing.T) {
	zero := int64(0)
	relevance := 0.75
	encoded, err := json.Marshal(SearchResponse{
		Success: true,
		Query:   "purify",
		Results: []SearchResult{{
			Rank:               1,
			Title:              "Purify",
			URL:                "https://example.com/",
			VerificationStatus: SearchVerificationNotChecked,
			Ranking: &SearchResultRanking{
				ProviderRank:   7,
				RelevanceScore: &relevance,
			},
		}},
		Ranking: &SearchResponseRanking{
			Mode:           SearchRankingRelevance,
			Status:         SearchRankingApplied,
			CandidateCount: 20,
		},
		Timing: SearchTimingInfo{RerankMs: &zero},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"success":true,"query":"purify","results":[{"rank":1,"title":"Purify","url":"https://example.com/","verification_status":"not_checked","ranking":{"provider_rank":7,"relevance_score":0.75}}],"deduplicated":0,"dropped_stale":0,"partial":false,"timing":{"total_ms":0,"provider_ms":0,"enrichment_ms":0,"rerank_ms":0},"ranking":{"mode":"relevance","status":"applied","candidate_count":20}}`
	if string(encoded) != want {
		t.Fatalf("relevance response = %s\nwant               = %s", encoded, want)
	}

	provider, err := json.Marshal(SearchResponse{Success: true, Query: "purify", Results: []SearchResult{}})
	if err != nil {
		t.Fatal(err)
	}
	wantProvider := `{"success":true,"query":"purify","results":[],"deduplicated":0,"dropped_stale":0,"partial":false,"timing":{"total_ms":0,"provider_ms":0,"enrichment_ms":0}}`
	if string(provider) != wantProvider {
		t.Fatalf("provider response changed = %s", provider)
	}
}

func TestSearchRelevanceEnumsAndResourceBounds(t *testing.T) {
	if SearchRankingProvider != "provider" || SearchRankingRelevance != "relevance" || SearchRankingTrust != "trust" {
		t.Fatalf("ranking modes = %q/%q/%q", SearchRankingProvider, SearchRankingRelevance, SearchRankingTrust)
	}
	if SearchRankingApplied != "applied" || SearchRankingDegraded != "degraded" ||
		SearchRankingReasonRerankerFailed != "reranker_failed" {
		t.Fatal("ranking status/reason enums drifted")
	}
	if DefaultSearchTrustLimit != 5 || MaxSearchTrustLimit != 5 || MaxSearchTrustCandidates != 8 || DefaultSearchTrustTimeoutSeconds != 60 {
		t.Fatalf("trust bounds = %d/%d/%d/%d", DefaultSearchTrustLimit, MaxSearchTrustLimit, MaxSearchTrustCandidates, DefaultSearchTrustTimeoutSeconds)
	}
}

func TestSearchRequestDefaultsPreserveExplicitValuesAndDefaultSchemaExtraction(t *testing.T) {
	falseValue := false
	request := &SearchRequest{
		Query:          "purify",
		Limit:          7,
		Deduplicate:    &falseValue,
		IncludeContent: true,
		Verify:         true,
		Schema:         json.RawMessage(`{"type":"object"}`),
		Timeout:        44,
	}
	request.Defaults()
	if request.Limit != 7 || request.Timeout != 44 || request.Deduplicate != &falseValue || *request.Deduplicate ||
		!request.IncludeContent || !request.Verify {
		t.Fatalf("explicit values changed: %#v", request)
	}
	if request.Engine != "auto" || request.LLMModel != "gpt-4o-mini" ||
		request.LLMBaseURL != "https://api.openai.com/v1" {
		t.Fatalf("schema defaults = %#v", request)
	}

	explicit := &SearchRequest{
		Schema:     json.RawMessage(`null`),
		Engine:     "compiled",
		LLMModel:   "model",
		LLMBaseURL: "https://llm.example/v1",
	}
	explicit.Defaults()
	if explicit.Engine != "compiled" || explicit.LLMModel != "model" || explicit.LLMBaseURL != "https://llm.example/v1" {
		t.Fatalf("explicit extraction settings changed: %#v", explicit)
	}
}

func TestSearchRequestJSONPreservesExplicitDeduplicateFalse(t *testing.T) {
	encoded := []byte(`{"query":"purify","deduplicate":false}`)
	var request SearchRequest
	if err := json.Unmarshal(encoded, &request); err != nil {
		t.Fatal(err)
	}
	if request.Deduplicate == nil || *request.Deduplicate {
		t.Fatalf("deduplicate = %#v, want explicit false", request.Deduplicate)
	}
	request.Defaults()
	if *request.Deduplicate {
		t.Fatal("Defaults replaced explicit deduplicate=false")
	}

	minimal, err := json.Marshal(SearchRequest{Query: "purify"})
	if err != nil {
		t.Fatal(err)
	}
	if string(minimal) != `{"query":"purify"}` {
		t.Fatalf("minimal request = %s", minimal)
	}
}

func TestSearchResponseBaselineJSONShape(t *testing.T) {
	encoded, err := json.Marshal(SearchResponse{
		Success: true,
		Query:   "purify",
		Results: []SearchResult{},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"success":true,"query":"purify","results":[],"deduplicated":0,"dropped_stale":0,"partial":false,"timing":{"total_ms":0,"provider_ms":0,"enrichment_ms":0}}`
	if string(encoded) != want {
		t.Fatalf("baseline response = %s\nwant              = %s", encoded, want)
	}

	result, err := json.Marshal(SearchResult{
		Rank:               1,
		Title:              "Purify",
		URL:                "https://example.com/",
		VerificationStatus: SearchVerificationNotChecked,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantResult := `{"rank":1,"title":"Purify","url":"https://example.com/","verification_status":"not_checked"}`
	if string(result) != wantResult {
		t.Fatalf("baseline result = %s\nwant            = %s", result, wantResult)
	}
}

func TestSearchResultJSONPreservesPointerAndEvidenceShapes(t *testing.T) {
	observedAt := time.Date(2026, time.August, 10, 8, 0, 0, 0, time.UTC)
	falseValue := false
	zero := 0.0
	score := 0.75
	basis := EvidenceBasis{}
	receipts := FieldReceipts{}
	encoded, err := json.Marshal(SearchResult{
		Rank:               2,
		Score:              &score,
		Title:              "Result",
		URL:                "https://example.com/source",
		FinalURL:           "https://example.com/final",
		Snippet:            "claim",
		PublishedAt:        &observedAt,
		Content:            "page content",
		Verified:           &falseValue,
		VerificationStatus: SearchVerificationMismatch,
		Evidence: &evidence.Anchor{
			Quote:      "claim",
			TextRange:  [2]int{0, 5},
			Method:     evidence.MethodExact,
			SnapshotID: "sha256:snapshot",
			FetchedAt:  observedAt,
		},
		Receipt:       "receipt-token",
		Data:          json.RawMessage(`null`),
		Basis:         &basis,
		Receipts:      &receipts,
		UnlocatedRate: &zero,
		Extractor: &ExtractorMetadata{
			ID:         "123e4567-e89b-12d3-a456-426614174000",
			Version:    1,
			CompiledAt: observedAt,
			Validation: 1,
			Mode:       "compiled",
		},
		LLMUsage:   &LLMUsage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3},
		Violations: []SchemaViolation{{Path: "/name", Message: "required"}},
		Errors: []SearchResultError{{
			Stage: SearchResultStageVerify, Code: ErrCodeEvidenceUnavailable, Message: "result verification is unavailable",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		"rank", "score", "title", "url", "final_url", "snippet", "published_at", "content",
		"verified", "verification_status", "evidence", "receipt", "data", "basis", "receipts",
		"unlocated_rate", "extractor", "llm_usage", "violations", "errors",
	} {
		if _, ok := document[field]; !ok {
			t.Errorf("full result omitted %q: %s", field, encoded)
		}
	}
	if string(document["verified"]) != "false" || string(document["data"]) != "null" ||
		string(document["basis"]) != "{}" || string(document["receipts"]) != "{}" ||
		string(document["unlocated_rate"]) != "0" {
		t.Fatalf("pointer/null shapes changed: %s", encoded)
	}
}

func TestSearchErrorResponseKeepsStableEnvelope(t *testing.T) {
	encoded, err := json.Marshal(SearchResponse{
		Success: false,
		Results: []SearchResult{},
		Error: &ErrorDetail{
			Code:    ErrCodeSearchUnavailable,
			Message: "search is unavailable",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	if string(document["results"]) != "[]" || string(document["success"]) != "false" {
		t.Fatalf("error envelope = %s", encoded)
	}
	if _, ok := document["error"]; !ok {
		t.Fatalf("error envelope omitted detail: %s", encoded)
	}
}

func TestValidSearchResultEnrichmentFlowLocksWorkerStageOrder(t *testing.T) {
	falseValue := false
	trueValue := true
	verified := SearchResult{
		FinalURL: "https://example.com/final", Verified: &trueValue, VerificationStatus: SearchVerificationVerified,
		Evidence: &evidence.Anchor{Quote: "q", TextRange: [2]int{0, 1}, Method: evidence.MethodExact}, Receipt: "receipt",
	}
	unavailable := SearchResult{
		FinalURL: "https://example.com/final", Verified: &falseValue, VerificationStatus: SearchVerificationUnavailable,
		Errors: []SearchResultError{{
			Stage: SearchResultStageVerify, Code: ErrCodeEvidenceUnavailable, Message: "result verification is unavailable",
		}},
	}
	timeout := SearchResult{
		FinalURL: "https://example.com/final", Verified: &falseValue, VerificationStatus: SearchVerificationUnavailable,
		Errors: []SearchResultError{{
			Stage: SearchResultStageVerify, Code: ErrCodeTimeout, Message: "result verification timed out",
		}},
	}
	allHeavy := SearchEnrichmentRequirements{IncludeContent: true, Verify: true, Extract: true, AttemptRequired: true}
	tests := []struct {
		name         string
		requirements SearchEnrichmentRequirements
		result       SearchResult
		want         bool
	}{
		{name: "shifted unattempted tail", requirements: SearchEnrichmentRequirements{}, result: SearchResult{VerificationStatus: SearchVerificationNotChecked}, want: true},
		{name: "required attempt omitted", requirements: SearchEnrichmentRequirements{AttemptRequired: true}, result: SearchResult{VerificationStatus: SearchVerificationNotChecked}},
		{name: "final URL alone", requirements: SearchEnrichmentRequirements{Extract: true}, result: SearchResult{FinalURL: "https://example.com/final", VerificationStatus: SearchVerificationNotChecked}},
		{name: "pre-artifact fetch failure", requirements: allHeavy, result: SearchResult{VerificationStatus: SearchVerificationNotChecked, Errors: []SearchResultError{{
			Stage: SearchResultStageFetch, Code: ErrCodeNavigation, Message: "result fetch failed",
		}}}, want: true},
		{name: "verified then content fetch failure", requirements: allHeavy, result: func() SearchResult {
			result := verified
			result.Errors = []SearchResultError{{Stage: SearchResultStageFetch, Code: ErrCodeTimeout, Message: "result fetch timed out"}}
			return result
		}(), want: true},
		{name: "nonterminal verify failure continues through content and extract failure", requirements: allHeavy, result: func() SearchResult {
			result := unavailable
			result.Content = "content"
			result.Errors = append(result.Errors, SearchResultError{Stage: SearchResultStageExtract, Code: ErrCodeInternal, Message: "result extraction failed"})
			return result
		}(), want: true},
		{name: "nonterminal verify failure cannot skip content", requirements: allHeavy, result: unavailable},
		{name: "verification timeout terminates", requirements: allHeavy, result: timeout, want: true},
		{name: "verification timeout cannot publish content", requirements: allHeavy, result: func() SearchResult {
			result := timeout
			result.Content = "late"
			return result
		}()},
		{name: "successful heavy flow", requirements: allHeavy, result: func() SearchResult {
			result := verified
			result.Content = "content"
			result.Data = json.RawMessage(`null`)
			return result
		}(), want: true},
		{name: "authoritative partial extraction", requirements: allHeavy, result: func() SearchResult {
			result := verified
			result.Content = "content"
			result.Data = json.RawMessage(`null`)
			result.Violations = []SchemaViolation{{Path: "$", Message: "type mismatch"}}
			result.Errors = []SearchResultError{{
				Stage: SearchResultStageExtract, Code: ErrCodeLLMFailure, Message: "result extraction was partial",
			}}
			return result
		}(), want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ValidSearchResultEnrichmentFlow(test.requirements, test.result); got != test.want {
				t.Fatalf("ValidSearchResultEnrichmentFlow() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestSearchResultEnrichmentAttemptRequiredUsesConservativeRemovalBound(t *testing.T) {
	for _, test := range []struct {
		name                              string
		limit, index, deduplicated, stale int
		want                              bool
	}{
		{name: "small public pool", limit: 5, index: 4, deduplicated: 99, stale: 99, want: true},
		{name: "first default result", limit: 10, index: 0, want: true},
		{name: "fifth default result", limit: 10, index: 4, want: true},
		{name: "sixth default result", limit: 10, index: 5},
		{name: "four removals still prove first", limit: 10, index: 0, deduplicated: 2, stale: 2, want: true},
		{name: "five removals may expose tail", limit: 10, index: 0, deduplicated: 3, stale: 2},
		{name: "invalid counters", limit: 10, index: 0, deduplicated: -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := SearchResultEnrichmentAttemptRequired(test.limit, test.index, test.deduplicated, test.stale); got != test.want {
				t.Fatalf("SearchResultEnrichmentAttemptRequired() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestSearchModelFieldClassificationStaysCurrent(t *testing.T) {
	assertSearchFields(t, reflect.TypeOf(SearchRequest{}), []string{
		"Query", "Limit", "Domains", "Freshness", "Ranking", "ExpectedSubject", "IncludeContent", "Verify", "Deduplicate",
		"Schema", "Engine", "LLMAPIKey", "LLMModel", "LLMBaseURL", "Timeout",
	})
	assertSearchFields(t, reflect.TypeOf(SearchResult{}), []string{
		"Rank", "Score", "Title", "URL", "FinalURL", "Snippet", "PublishedAt", "Content",
		"Verified", "VerificationStatus", "Evidence", "Receipt", "Data", "Basis", "Receipts",
		"UnlocatedRate", "Extractor", "LLMUsage", "Violations", "Errors", "Ranking",
	})
	assertSearchFields(t, reflect.TypeOf(SearchResponse{}), []string{
		"Success", "Query", "Results", "Deduplicated", "DroppedStale", "Partial", "Timing", "Ranking", "Error",
	})
	assertSearchFields(t, reflect.TypeOf(SearchTimingInfo{}), []string{"TotalMs", "ProviderMs", "EnrichmentMs", "RerankMs"})
	assertSearchFields(t, reflect.TypeOf(SearchResultRanking{}), []string{"ProviderRank", "RelevanceScore"})
	assertSearchFields(t, reflect.TypeOf(SearchResponseRanking{}), []string{
		"Mode", "Status", "DegradedReason", "CandidateCount",
	})
}

func assertSearchFields(t *testing.T, structure reflect.Type, expected []string) {
	t.Helper()
	actual := make([]string, structure.NumField())
	for index := 0; index < structure.NumField(); index++ {
		actual[index] = structure.Field(index).Name
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("%s fields changed: got %v, want %v", structure.Name(), actual, expected)
	}
}
