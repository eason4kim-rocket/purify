package answer

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/search"
	searchrerank "github.com/use-agent/purify/search/rerank"
)

type answerSearchProvider struct {
	results []search.ProviderResult
	calls   int
	query   search.ProviderQuery
}

func (*answerSearchProvider) Name() string { return "answer-test" }

func (provider *answerSearchProvider) Search(_ context.Context, query search.ProviderQuery) ([]search.ProviderResult, error) {
	provider.calls++
	provider.query = query
	return append([]search.ProviderResult(nil), provider.results...), nil
}

type stubAnswerSearcher struct {
	response *models.SearchResponse
	err      error
	call     func(context.Context, *models.SearchRequest, search.RunOptions) (*models.SearchResponse, error)
	calls    int
	request  *models.SearchRequest
	options  search.RunOptions
}

func (stub *stubAnswerSearcher) SearchWithOptions(
	ctx context.Context,
	request *models.SearchRequest,
	options search.RunOptions,
) (*models.SearchResponse, error) {
	stub.calls++
	stub.request = cloneObservedSearchRequest(request)
	stub.options = options
	if stub.call != nil {
		return stub.call(ctx, request, options)
	}
	return stub.response, stub.err
}

type stubAnswerExtractor struct {
	response *models.MultiExtractResponse
	err      error
	call     func(context.Context, *models.ExtractRequest) (*models.MultiExtractResponse, error)
	calls    int
	request  *models.ExtractRequest
}

func (stub *stubAnswerExtractor) ExtractMulti(ctx context.Context, request *models.ExtractRequest) (*models.MultiExtractResponse, error) {
	stub.calls++
	stub.request = cloneObservedExtractRequest(request)
	if stub.call != nil {
		return stub.call(ctx, request)
	}
	return stub.response, stub.err
}

func TestAnswerKnownComposesFreshSearchAndMultiConsensus(t *testing.T) {
	predicate := `price.v1\gross`
	consensusPath := `price\.v1\gross`
	fetchedAt := time.Date(2026, 11, 2, 10, 9, 0, 0, time.FixedZone("source", 8*60*60))
	now := time.Date(2026, 11, 2, 10, 10, 0, 0, time.FixedZone("offset", 8*60*60))
	searcher := &stubAnswerSearcher{response: successfulSearch(
		"https://one.example.com/fact",
		"https://two.example.net/fact",
	)}
	searcher.response.Query = "anthropic claude-fable-5 " + predicate
	field := knownField(`"$3.00"`, 2, 2, fetchedAt)
	extractorResponse := successfulConsensus(consensusPath, field)
	extractor := &stubAnswerExtractor{response: extractorResponse}
	service, err := NewService(searcher, extractor)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	service.now = func() time.Time { return now }
	request := &models.AnswerRequest{Spec: models.FactSpec{
		Subject:   "  anthropic claude-fable-5  ",
		Predicate: predicate,
	}}

	response, err := service.Answer(context.Background(), request)
	if err != nil {
		t.Fatalf("Answer() error = %v", err)
	}
	if response.Status != models.AnswerStatusKnown || response.Belief == nil || response.Reason != "" || response.Needs != nil {
		t.Fatalf("response = %#v, want known belief", response)
	}
	if string(response.Belief.Value) != `"$3.00"` || response.Belief.Agreement != (models.MultiExtractAgreement{Pages: 2, IndependentRoots: 2}) {
		t.Fatalf("belief = %#v", response.Belief)
	}
	if response.Belief.Confidence != models.AnswerConfidenceMedium || !response.Belief.AsOf.Equal(now.UTC()) {
		t.Fatalf("confidence/as_of = %q/%v", response.Belief.Confidence, response.Belief.AsOf)
	}
	if len(response.Belief.Evidence) != 2 || len(response.Belief.Receipts) != 2 {
		t.Fatalf("evidence/receipts = %#v/%#v", response.Belief.Evidence, response.Belief.Receipts)
	}
	if response.Belief.Evidence[0].FetchedAt.Location() != time.UTC || !response.Belief.Evidence[0].FetchedAt.Equal(fetchedAt) {
		t.Fatalf("evidence fetched_at = %v (%v)", response.Belief.Evidence[0].FetchedAt, response.Belief.Evidence[0].FetchedAt.Location())
	}
	if response.Lease == nil || !response.Lease.ExpiresAt.Equal(now.UTC().Add(24*time.Hour)) ||
		response.Lease.RenewURL != models.DefaultAnswerRenewURL ||
		response.Lease.ConfidenceHalflife != models.DefaultAnswerConfidenceHalflifeSeconds {
		t.Fatalf("lease = %#v", response.Lease)
	}
	if _, err := json.Marshal(response); err != nil {
		t.Fatalf("known response is not JSON-safe: %v", err)
	}
	encoded, err := service.EncodeResponse(context.Background(), response)
	if err != nil || len(encoded) == 0 || len(encoded) > models.MaxAnswerResponseBytes || !json.Valid(encoded) {
		t.Fatalf("EncodeResponse() = (%d bytes, %v)", len(encoded), err)
	}

	if searcher.calls != 1 || !searcher.options.BypassProviderCache {
		t.Fatalf("Search calls/options = %d/%#v", searcher.calls, searcher.options)
	}
	if searcher.request.Query != "anthropic claude-fable-5 "+predicate || searcher.request.Freshness != models.DefaultAnswerFreshness ||
		searcher.request.Limit != models.MaxExtractSources || searcher.request.Timeout != models.DefaultAnswerTimeoutSeconds ||
		searcher.request.Deduplicate == nil || !*searcher.request.Deduplicate || searcher.request.Ranking != "" ||
		searcher.request.ExpectedSubject != nil {
		t.Fatalf("Search request = %#v", searcher.request)
	}
	if extractor.calls != 1 || extractor.request.Engine != "auto" || !extractor.request.Evidence ||
		extractor.request.Timeout != models.DefaultAnswerTimeoutSeconds ||
		extractor.request.LLMAPIKey != "" || extractor.request.LLMBaseURL != "" || extractor.request.ProxyURL != "" {
		t.Fatalf("Extract request = %#v", extractor.request)
	}
	if len(extractor.request.Sources) != 2 || extractor.request.Sources[0] != "https://one.example.com/fact" {
		t.Fatalf("Extract sources = %#v", extractor.request.Sources)
	}
	var schema struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(extractor.request.Schema, &schema); err != nil {
		t.Fatalf("generated schema = %s: %v", extractor.request.Schema, err)
	}
	if schema.Properties[predicate].Type != "string" || len(schema.Required) != 1 || schema.Required[0] != predicate {
		t.Fatalf("generated schema projection = %#v", schema)
	}
	if request.Spec.Freshness != "" || request.Spec.MinIndependentSources != 0 || request.Spec.OnConflict != "" ||
		request.Spec.Subject != "  anthropic claude-fable-5  " {
		t.Fatalf("caller request was mutated: %#v", request)
	}

	mutable := extractorResponse.Consensus.Fields[consensusPath]
	mutable.Value[1] = 'X'
	mutable.Supports[0].Evidence.Quote = "changed"
	mutable.Supports[0].Receipt = "changed"
	if string(response.Belief.Value) != `"$3.00"` || response.Belief.Evidence[0].Quote != "$3.00" ||
		response.Belief.Receipts[response.Belief.Evidence[0].URL] == "changed" {
		t.Fatalf("Answer retained dependency-owned buffers: %#v", response.Belief)
	}
}

func TestAnswerKeepsProviderSearchWhenRerankerIsAbsentOrConfigured(t *testing.T) {
	for _, configured := range []bool{false, true} {
		name := "reranker absent"
		if configured {
			name = "reranker configured"
		}
		t.Run(name, func(t *testing.T) {
			provider := &answerSearchProvider{results: []search.ProviderResult{
				{Rank: 1, URL: "https://one.example.com/fact", Title: "one"},
				{Rank: 2, URL: "https://two.example.net/fact", Title: "two"},
			}}
			scorerCalls := 0
			var options []search.ServiceOption
			if configured {
				options = append(options, search.WithReranker(searchrerank.ScorerFunc(func(context.Context, searchrerank.ScoreRequest) ([]searchrerank.ScoreResult, error) {
					scorerCalls++
					return nil, errors.New("answer must not call the reranker")
				})))
			}
			searchService, err := search.NewService(provider, options...)
			if err != nil {
				t.Fatal(err)
			}
			field := knownField(`"$3.00"`, 2, 2, time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC))
			extractor := &stubAnswerExtractor{response: successfulConsensus("price", field)}
			service, err := NewService(searchService, extractor)
			if err != nil {
				t.Fatal(err)
			}
			response, err := service.Answer(context.Background(), &models.AnswerRequest{Spec: models.FactSpec{
				Subject: "anthropic claude", Predicate: "price", MinIndependentSources: 2,
			}})
			if err != nil || response.Status != models.AnswerStatusKnown {
				t.Fatalf("Answer() = (%#v, %v)", response, err)
			}
			if scorerCalls != 0 || provider.calls != 1 || provider.query.Limit != models.MaxExtractSources {
				t.Fatalf("scorer/provider calls/query = %d/%d/%#v", scorerCalls, provider.calls, provider.query)
			}
			if extractor.calls != 1 || len(extractor.request.Sources) != 2 ||
				extractor.request.Sources[0] != "https://one.example.com/fact" ||
				extractor.request.Sources[1] != "https://two.example.net/fact" {
				t.Fatalf("ExtractMulti calls/sources = %d/%#v", extractor.calls, extractor.request.Sources)
			}
		})
	}
}

func TestAnswerPropagatesAgreementFoldReasonsWithoutProjectingSupportReason(t *testing.T) {
	fetchedAt := time.Date(2025, 1, 2, 2, 9, 0, 0, time.UTC)
	field := knownField(`"winner"`, 3, 2, fetchedAt)
	field.Agreement.FoldReason = models.MultiExtractFoldReasonQuoteLineage
	field.Supports[1].FoldReason = models.MultiExtractFoldReasonQuoteLineage
	conflictAgreement := models.MultiExtractAgreement{
		Pages:            2,
		IndependentRoots: 1,
		FoldReason:       models.MultiExtractFoldReasonNearDuplicate,
	}
	conflictSupports := generatedSupports(conflictAgreement, 3, `"other"`)
	conflictSupports[1].FoldReason = models.MultiExtractFoldReasonNearDuplicate
	field.Conflicts = []models.MultiExtractConflict{{
		Value:     json.RawMessage(`"other"`),
		Agreement: conflictAgreement,
		Supports:  conflictSupports,
	}}

	response := runAnswerWithField(t, field, 2)
	if response.Status != models.AnswerStatusKnown || response.Belief == nil {
		t.Fatalf("response = %#v, want known belief", response)
	}
	if response.Belief.Agreement.FoldReason != models.MultiExtractFoldReasonQuoteLineage {
		t.Fatalf("belief agreement = %#v", response.Belief.Agreement)
	}
	if response.Belief.Confidence != models.AnswerConfidenceMedium {
		t.Fatalf("confidence = %q, fold metadata changed the existing roots formula", response.Belief.Confidence)
	}
	if len(response.Conflicts) != 1 ||
		response.Conflicts[0].Agreement.FoldReason != models.MultiExtractFoldReasonNearDuplicate {
		t.Fatalf("conflicts = %#v", response.Conflicts)
	}
	encodedEvidence, err := json.Marshal(response.Belief.Evidence)
	if err != nil {
		t.Fatalf("Marshal(evidence) error = %v", err)
	}
	if strings.Contains(string(encodedEvidence), `"fold_reason"`) {
		t.Fatalf("support fold reason leaked into AnswerEvidence: %s", encodedEvidence)
	}
}

func TestAnswerTiePreservesAlternativeAgreementFoldReason(t *testing.T) {
	response := runAnswerWithField(t, models.MultiExtractFieldConsensus{
		Ambiguous: true,
		Conflicts: []models.MultiExtractConflict{
			{
				Value: json.RawMessage(`"alpha"`),
				Agreement: models.MultiExtractAgreement{
					Pages:            3,
					IndependentRoots: 2,
					FoldReason:       models.MultiExtractFoldReasonNearDuplicate,
				},
			},
			{
				Value: json.RawMessage(`"beta"`),
				Agreement: models.MultiExtractAgreement{
					Pages:            3,
					IndependentRoots: 2,
					FoldReason:       models.MultiExtractFoldReasonQuoteLineage,
				},
			},
		},
	}, 2)
	if response.Status != models.AnswerStatusUnknown || response.Reason != models.AnswerUnknownConflict ||
		response.Closest == nil || string(response.Closest.Value) != `"alpha"` || len(response.Conflicts) != 1 {
		t.Fatalf("response = %#v, want tied unknown", response)
	}
	if response.Conflicts[0].Agreement.FoldReason != models.MultiExtractFoldReasonQuoteLineage {
		t.Fatalf("tie alternative agreement = %#v", response.Conflicts[0].Agreement)
	}
}

func TestValidateAgreementFoldReasonStates(t *testing.T) {
	tests := []struct {
		name      string
		agreement models.MultiExtractAgreement
		wantValid bool
	}{
		{
			name:      "unfolded omits reason",
			agreement: models.MultiExtractAgreement{Pages: 2, IndependentRoots: 2},
			wantValid: true,
		},
		{
			name: "single same-root reason",
			agreement: models.MultiExtractAgreement{
				Pages: 2, IndependentRoots: 1, FoldReason: models.MultiExtractFoldReasonSameRoot,
			},
			wantValid: true,
		},
		{
			name: "single near-duplicate reason",
			agreement: models.MultiExtractAgreement{
				Pages: 2, IndependentRoots: 1, FoldReason: models.MultiExtractFoldReasonNearDuplicate,
			},
			wantValid: true,
		},
		{
			name: "single quote-lineage reason",
			agreement: models.MultiExtractAgreement{
				Pages: 2, IndependentRoots: 1, FoldReason: models.MultiExtractFoldReasonQuoteLineage,
			},
			wantValid: true,
		},
		{
			name:      "mixed reasons omit enum",
			agreement: models.MultiExtractAgreement{Pages: 3, IndependentRoots: 1},
			wantValid: true,
		},
		{
			name: "unfolded cannot claim reason",
			agreement: models.MultiExtractAgreement{
				Pages: 2, IndependentRoots: 2, FoldReason: models.MultiExtractFoldReasonSameRoot,
			},
		},
		{
			name: "unknown reason",
			agreement: models.MultiExtractAgreement{
				Pages: 2, IndependentRoots: 1, FoldReason: models.MultiExtractFoldReason("unknown"),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateAgreement(test.agreement)
			if (err == nil) != test.wantValid {
				t.Fatalf("validateAgreement(%#v) error = %v, want valid=%t", test.agreement, err, test.wantValid)
			}
		})
	}
}

func TestAnswerIndependentRootThresholdAndTieStayUnknown(t *testing.T) {
	tests := []struct {
		name         string
		field        models.MultiExtractFieldConsensus
		minimum      int
		wantReason   models.AnswerUnknownReason
		wantRoots    int
		wantNeeded   int
		wantConflict int
	}{
		{
			name: "insufficient syndicated pages",
			field: models.MultiExtractFieldConsensus{
				Value:     json.RawMessage(`"winner"`),
				Agreement: models.MultiExtractAgreement{Pages: 6, IndependentRoots: 1},
			},
			minimum:    2,
			wantReason: models.AnswerUnknownInsufficient,
			wantRoots:  1,
			wantNeeded: 1,
		},
		{
			name: "page majority cannot break independent tie",
			field: models.MultiExtractFieldConsensus{
				Value:     json.RawMessage(`"six-pages"`),
				Agreement: models.MultiExtractAgreement{Pages: 6, IndependentRoots: 2},
				Conflicts: []models.MultiExtractConflict{{
					Value:     json.RawMessage(`"one-page"`),
					Agreement: models.MultiExtractAgreement{Pages: 2, IndependentRoots: 2},
				}},
			},
			minimum:      2,
			wantReason:   models.AnswerUnknownConflict,
			wantRoots:    2,
			wantNeeded:   1,
			wantConflict: 1,
		},
		{
			name: "materializer ambiguity",
			field: models.MultiExtractFieldConsensus{
				Ambiguous: true,
				Conflicts: []models.MultiExtractConflict{
					{Value: json.RawMessage(`"alpha"`), Agreement: models.MultiExtractAgreement{Pages: 2, IndependentRoots: 2}},
					{Value: json.RawMessage(`"beta"`), Agreement: models.MultiExtractAgreement{Pages: 2, IndependentRoots: 2}},
				},
			},
			minimum:      2,
			wantReason:   models.AnswerUnknownConflict,
			wantRoots:    2,
			wantNeeded:   1,
			wantConflict: 1,
		},
		{
			name: "null conflict still blocks promotion",
			field: models.MultiExtractFieldConsensus{
				Value:     json.RawMessage(`"winner"`),
				Agreement: models.MultiExtractAgreement{Pages: 2, IndependentRoots: 2},
				Conflicts: []models.MultiExtractConflict{{
					Value:     json.RawMessage(`null`),
					Agreement: models.MultiExtractAgreement{Pages: 2, IndependentRoots: 2},
				}},
			},
			minimum:      2,
			wantReason:   models.AnswerUnknownConflict,
			wantRoots:    2,
			wantNeeded:   1,
			wantConflict: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := runAnswerWithField(t, test.field, test.minimum)
			if response.Status != models.AnswerStatusUnknown || response.Belief != nil || response.Lease != nil || response.Reason != test.wantReason {
				t.Fatalf("response = %#v", response)
			}
			if response.Closest == nil || response.Closest.IndependentRoots != test.wantRoots || response.Needs == nil ||
				response.Needs.MoreIndependentSources != test.wantNeeded || len(response.Conflicts) != test.wantConflict {
				t.Fatalf("unknown details = %#v", response)
			}
		})
	}
}

func TestAnswerNeverPromotesNullOrMissingConsensus(t *testing.T) {
	for _, test := range []struct {
		name  string
		field models.MultiExtractFieldConsensus
	}{
		{name: "null", field: models.MultiExtractFieldConsensus{Value: json.RawMessage(`null`)}},
		{name: "empty", field: models.MultiExtractFieldConsensus{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := runAnswerWithField(t, test.field, 2)
			if response.Status != models.AnswerStatusUnknown || response.Belief != nil || response.Closest != nil ||
				response.Reason != models.AnswerUnknownMissingValue || response.Needs == nil || response.Needs.MoreIndependentSources != 2 {
				t.Fatalf("response = %#v", response)
			}
		})
	}
}

func TestAnswerGeneratedStringSchemaRejectsNonStringConsensus(t *testing.T) {
	for _, raw := range []string{`true`, `42`, `3.14`} {
		t.Run(raw, func(t *testing.T) {
			field := models.MultiExtractFieldConsensus{
				Value:     json.RawMessage(raw),
				Agreement: models.MultiExtractAgreement{Pages: 2, IndependentRoots: 2},
			}
			response, err := answerForField(t, field, 2)
			if response != nil || errorCode(err) != models.ErrCodeAnswerFailed {
				t.Fatalf("Answer() = (%#v, %v), want no belief + internal contract failure", response, err)
			}
		})
	}
}

func TestAnswerNoResultsAndNoValidSourcesAreUnknown(t *testing.T) {
	t.Run("no search results", func(t *testing.T) {
		searcher := &stubAnswerSearcher{response: successfulSearch()}
		extractor := &stubAnswerExtractor{}
		service, _ := NewService(searcher, extractor)
		response, err := service.Answer(context.Background(), validAnswerRequest())
		if err != nil || response.Reason != models.AnswerUnknownNoSearchResults || extractor.calls != 0 ||
			response.Needs.MoreIndependentSources != models.DefaultAnswerMinIndependentSources {
			t.Fatalf("Answer() = (%#v, %v), extract calls=%d", response, err, extractor.calls)
		}
	})
	t.Run("no valid extraction source", func(t *testing.T) {
		searcher := &stubAnswerSearcher{response: successfulSearch("https://one.example.com/fact")}
		domainErr := models.NewScrapeError(models.ErrCodeNoValidSource, "provider secret: sk-live", errors.New("credential=secret"))
		extractor := &stubAnswerExtractor{
			response: &models.MultiExtractResponse{Success: false, Sources: []models.MultiExtractSource{}, Error: domainErr.ToDetail()},
			err:      domainErr,
		}
		service, _ := NewService(searcher, extractor)
		response, err := service.Answer(context.Background(), validAnswerRequest())
		if err != nil || response.Reason != models.AnswerUnknownNoValidSources || response.Belief != nil {
			t.Fatalf("Answer() = (%#v, %v)", response, err)
		}
	})
}

func TestAnswerValidatesPredicateAndResourceInputsBeforeDependencies(t *testing.T) {
	tests := []struct {
		name      string
		predicate string
	}{
		{name: "empty", predicate: ""},
		{name: "space", predicate: "price per token"},
		{name: "leading whitespace", predicate: " price"},
		{name: "control", predicate: "price\n"},
		{name: "too long", predicate: strings.Repeat("p", models.MaxAnswerPredicateBytes+1)},
	}

	t.Run("whitespace freshness", func(t *testing.T) {
		searcher := &stubAnswerSearcher{}
		extractor := &stubAnswerExtractor{}
		service, _ := NewService(searcher, extractor)
		request := validAnswerRequest()
		request.Spec.Freshness = "   "
		if _, err := service.Answer(context.Background(), request); errorCode(err) != models.ErrCodeInvalidInput || searcher.calls != 0 {
			t.Fatalf("error/calls = %v/%d", err, searcher.calls)
		}
	})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			searcher := &stubAnswerSearcher{}
			extractor := &stubAnswerExtractor{}
			service, _ := NewService(searcher, extractor)
			request := validAnswerRequest()
			request.Spec.Predicate = test.predicate
			response, err := service.Answer(context.Background(), request)
			if response != nil || errorCode(err) != models.ErrCodeInvalidInput || searcher.calls != 0 || extractor.calls != 0 {
				t.Fatalf("Answer() = (%#v, %v), calls=%d/%d", response, err, searcher.calls, extractor.calls)
			}
		})
	}

	for _, minimum := range []int{-1, 9} {
		searcher := &stubAnswerSearcher{}
		extractor := &stubAnswerExtractor{}
		service, _ := NewService(searcher, extractor)
		request := validAnswerRequest()
		request.Spec.MinIndependentSources = minimum
		if _, err := service.Answer(context.Background(), request); errorCode(err) != models.ErrCodeInvalidInput || searcher.calls != 0 {
			t.Fatalf("minimum %d error/calls = %v/%d", minimum, err, searcher.calls)
		}
	}

	for _, request := range []*models.AnswerRequest{
		{Spec: models.FactSpec{Subject: strings.Repeat("s", models.MaxAnswerSubjectBytes+1), Predicate: "price"}},
		{Spec: models.FactSpec{Subject: "subject", Predicate: "price", Freshness: strings.Repeat("d", models.MaxAnswerFreshnessBytes+1)}},
		{Spec: models.FactSpec{Subject: "subject", Predicate: "price"}, Timeout: -1},
		{Spec: models.FactSpec{Subject: "subject", Predicate: "price"}, Timeout: models.MaxAnswerTimeoutSeconds + 1},
	} {
		searcher := &stubAnswerSearcher{}
		extractor := &stubAnswerExtractor{}
		service, _ := NewService(searcher, extractor)
		if response, err := service.Answer(context.Background(), request); response != nil ||
			errorCode(err) != models.ErrCodeInvalidInput || searcher.calls != 0 || extractor.calls != 0 {
			t.Fatalf("oversized/invalid request = (%#v, %v), calls=%d/%d", response, err, searcher.calls, extractor.calls)
		}
	}
}

func TestAnswerUsesOneNormalizedEndToEndTimeout(t *testing.T) {
	var remaining time.Duration
	searcher := &stubAnswerSearcher{call: func(ctx context.Context, request *models.SearchRequest, _ search.RunOptions) (*models.SearchResponse, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("Search context has no deadline")
		}
		remaining = time.Until(deadline)
		return &models.SearchResponse{Success: true, Query: request.Query, Results: []models.SearchResult{}}, nil
	}}
	extractor := &stubAnswerExtractor{}
	service, _ := NewService(searcher, extractor)
	request := validAnswerRequest()
	request.Timeout = 45
	response, err := service.Answer(context.Background(), request)
	if err != nil || response.Status != models.AnswerStatusUnknown || searcher.request.Timeout != 45 || extractor.calls != 0 ||
		remaining <= 44*time.Second || remaining > 45*time.Second {
		t.Fatalf("Answer() = (%#v, %v), timeout=%d remaining=%v extract=%d", response, err, searcher.request.Timeout, remaining, extractor.calls)
	}
	if request.Timeout != 45 {
		t.Fatalf("caller timeout mutated to %d", request.Timeout)
	}
}

func TestAnswerNoValidSourceRequiresMatchingResponseAndError(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "missing error"},
		{name: "mismatched error", err: models.NewScrapeError(models.ErrCodeInternal, "secret", errors.New("credential"))},
	} {
		t.Run(test.name, func(t *testing.T) {
			searcher := &stubAnswerSearcher{response: successfulSearch("https://one.example.com/fact")}
			extractor := &stubAnswerExtractor{
				response: &models.MultiExtractResponse{
					Success: false,
					Sources: []models.MultiExtractSource{},
					Error:   &models.ErrorDetail{Code: models.ErrCodeNoValidSource, Message: "no valid extraction source"},
				},
				err: test.err,
			}
			service, _ := NewService(searcher, extractor)
			response, err := service.Answer(context.Background(), validAnswerRequest())
			if response != nil || errorCode(err) != models.ErrCodeAnswerFailed || strings.Contains(err.Error(), "credential") {
				t.Fatalf("Answer() = (%#v, %v)", response, err)
			}
		})
	}
}

func TestAnswerRejectsAdversarialSearchAndConsensusProjections(t *testing.T) {
	t.Run("search query mismatch", func(t *testing.T) {
		searcher := &stubAnswerSearcher{response: successfulSearch("https://one.example.com/fact")}
		searcher.response.Query = "different query"
		extractor := &stubAnswerExtractor{}
		service, _ := NewService(searcher, extractor)
		response, err := service.Answer(context.Background(), validAnswerRequest())
		if response != nil || errorCode(err) != models.ErrCodeAnswerFailed || extractor.calls != 0 {
			t.Fatalf("Answer() = (%#v, %v), extract calls=%d", response, err, extractor.calls)
		}
	})

	t.Run("search exceeds requested limit", func(t *testing.T) {
		urls := make([]string, models.MaxExtractSources+1)
		for index := range urls {
			urls[index] = "https://source-" + string(rune('a'+index)) + ".example.com/fact"
		}
		searcher := &stubAnswerSearcher{response: successfulSearch(urls...)}
		extractor := &stubAnswerExtractor{}
		service, _ := NewService(searcher, extractor)
		response, err := service.Answer(context.Background(), validAnswerRequest())
		if response != nil || errorCode(err) != models.ErrCodeAnswerFailed || extractor.calls != 0 {
			t.Fatalf("Answer() = (%#v, %v), extract calls=%d", response, err, extractor.calls)
		}
	})

	t.Run("duplicate canonical search URL", func(t *testing.T) {
		searcher := &stubAnswerSearcher{response: successfulSearch(
			"https://one.example.com/fact",
			"https://one.example.com/fact",
		)}
		extractor := &stubAnswerExtractor{}
		service, _ := NewService(searcher, extractor)
		response, err := service.Answer(context.Background(), validAnswerRequest())
		if response != nil || errorCode(err) != models.ErrCodeAnswerFailed || extractor.calls != 0 {
			t.Fatalf("Answer() = (%#v, %v), extract calls=%d", response, err, extractor.calls)
		}
	})

	t.Run("support outside searched source set", func(t *testing.T) {
		field := knownField(`"value"`, 1, 1, time.Now().UTC())
		response := successfulConsensus("price", field)
		mutated := response.Consensus.Fields["price"]
		mutated.Supports[0].URL = "https://attacker.example.org/fact"
		mutated.Supports[0].Root = "example.org"
		response.Consensus.Fields["price"] = mutated
		searcher := &stubAnswerSearcher{response: successfulSearch("https://one.example.com/fact")}
		extractor := &stubAnswerExtractor{response: response}
		service, _ := NewService(searcher, extractor)
		request := validAnswerRequest()
		request.Spec.MinIndependentSources = 1
		belief, err := service.Answer(context.Background(), request)
		if belief != nil || errorCode(err) != models.ErrCodeAnswerFailed {
			t.Fatalf("Answer() = (%#v, %v)", belief, err)
		}
	})

	t.Run("support root mismatches URL", func(t *testing.T) {
		field := knownField(`"value"`, 1, 1, time.Now().UTC())
		field.Supports[0].Root = "attacker.com"
		response, err := answerForField(t, field, 1)
		if response != nil || errorCode(err) != models.ErrCodeAnswerFailed {
			t.Fatalf("Answer() = (%#v, %v)", response, err)
		}
	})

	t.Run("aggregate data disagrees with winner", func(t *testing.T) {
		field := knownField(`"winner"`, 1, 1, time.Now().UTC())
		extractResponse := successfulConsensus("price", field)
		extractResponse.Data = json.RawMessage(`{"price":"forged"}`)
		searcher := &stubAnswerSearcher{response: successfulSearch("https://one.example.com/fact")}
		extractor := &stubAnswerExtractor{response: extractResponse}
		service, _ := NewService(searcher, extractor)
		request := validAnswerRequest()
		request.Spec.MinIndependentSources = 1
		response, err := service.Answer(context.Background(), request)
		if response != nil || errorCode(err) != models.ErrCodeAnswerFailed {
			t.Fatalf("Answer() = (%#v, %v)", response, err)
		}
	})

	t.Run("schema invalid carries wrong consensus field", func(t *testing.T) {
		field := models.MultiExtractFieldConsensus{}
		extractResponse := successfulConsensus("wrong", field)
		searcher := &stubAnswerSearcher{response: successfulSearch("https://one.example.com/fact")}
		extractor := &stubAnswerExtractor{response: extractResponse}
		service, _ := NewService(searcher, extractor)
		response, err := service.Answer(context.Background(), validAnswerRequest())
		if response != nil || errorCode(err) != models.ErrCodeAnswerFailed {
			t.Fatalf("Answer() = (%#v, %v)", response, err)
		}
	})

	t.Run("schema invalid null carries forged agreement", func(t *testing.T) {
		field := models.MultiExtractFieldConsensus{
			Value:     json.RawMessage(`null`),
			Agreement: models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1},
		}
		response, err := answerForField(t, field, 1)
		if response != nil || errorCode(err) != models.ErrCodeAnswerFailed {
			t.Fatalf("Answer() = (%#v, %v)", response, err)
		}
	})

	t.Run("same support claimed by winner and conflict", func(t *testing.T) {
		field := knownField(`"winner"`, 1, 1, time.Now().UTC())
		field.Conflicts = []models.MultiExtractConflict{{
			Value:     json.RawMessage(`"other"`),
			Agreement: models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1},
			Supports:  append([]models.MultiExtractSupport(nil), field.Supports...),
		}}
		response, err := answerForField(t, field, 1)
		if response != nil || errorCode(err) != models.ErrCodeAnswerFailed {
			t.Fatalf("Answer() = (%#v, %v)", response, err)
		}
	})

	t.Run("conflict canonically repeats winner value", func(t *testing.T) {
		field := models.MultiExtractFieldConsensus{
			Value:     json.RawMessage(`"same"`),
			Agreement: models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1},
			Conflicts: []models.MultiExtractConflict{{
				Value:     json.RawMessage(`"\u0073ame"`),
				Agreement: models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1},
			}},
		}
		response, err := answerForField(t, field, 1)
		if response != nil || errorCode(err) != models.ErrCodeAnswerFailed {
			t.Fatalf("Answer() = (%#v, %v)", response, err)
		}
	})

	t.Run("lower conflicts are out of score order", func(t *testing.T) {
		field := models.MultiExtractFieldConsensus{
			Value:     json.RawMessage(`"winner"`),
			Agreement: models.MultiExtractAgreement{Pages: 3, IndependentRoots: 3},
			Conflicts: []models.MultiExtractConflict{
				{Value: json.RawMessage(`"one"`), Agreement: models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1}},
				{Value: json.RawMessage(`"two"`), Agreement: models.MultiExtractAgreement{Pages: 2, IndependentRoots: 2}},
			},
		}
		response, err := answerForField(t, field, 1)
		if response != nil || errorCode(err) != models.ErrCodeAnswerFailed {
			t.Fatalf("Answer() = (%#v, %v)", response, err)
		}
	})

	t.Run("equal-root conflicts are out of page order", func(t *testing.T) {
		field := models.MultiExtractFieldConsensus{
			Value:     json.RawMessage(`"winner"`),
			Agreement: models.MultiExtractAgreement{Pages: 3, IndependentRoots: 3},
			Conflicts: []models.MultiExtractConflict{
				{Value: json.RawMessage(`"one-page"`), Agreement: models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1}},
				{Value: json.RawMessage(`"two-pages"`), Agreement: models.MultiExtractAgreement{Pages: 2, IndependentRoots: 1}},
			},
		}
		response, err := answerForField(t, field, 1)
		if response != nil || errorCode(err) != models.ErrCodeAnswerFailed {
			t.Fatalf("Answer() = (%#v, %v)", response, err)
		}
	})

	t.Run("ambiguous top candidates do not tie", func(t *testing.T) {
		field := models.MultiExtractFieldConsensus{
			Ambiguous: true,
			Conflicts: []models.MultiExtractConflict{
				{Value: json.RawMessage(`"two"`), Agreement: models.MultiExtractAgreement{Pages: 2, IndependentRoots: 2}},
				{Value: json.RawMessage(`"one"`), Agreement: models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1}},
			},
		}
		response, err := answerForField(t, field, 1)
		if response != nil || errorCode(err) != models.ErrCodeAnswerFailed {
			t.Fatalf("Answer() = (%#v, %v)", response, err)
		}
	})

	t.Run("ambiguous candidates are out of root order", func(t *testing.T) {
		field := models.MultiExtractFieldConsensus{
			Ambiguous: true,
			Conflicts: []models.MultiExtractConflict{
				{Value: json.RawMessage(`"first"`), Agreement: models.MultiExtractAgreement{Pages: 2, IndependentRoots: 2}},
				{Value: json.RawMessage(`"second"`), Agreement: models.MultiExtractAgreement{Pages: 2, IndependentRoots: 2}},
				{Value: json.RawMessage(`"higher"`), Agreement: models.MultiExtractAgreement{Pages: 3, IndependentRoots: 3}},
			},
		}
		response, err := answerForField(t, field, 1)
		if response != nil || errorCode(err) != models.ErrCodeAnswerFailed {
			t.Fatalf("Answer() = (%#v, %v)", response, err)
		}
	})

	t.Run("ambiguous candidates are out of page order", func(t *testing.T) {
		field := models.MultiExtractFieldConsensus{
			Ambiguous: true,
			Conflicts: []models.MultiExtractConflict{
				{Value: json.RawMessage(`"one-page"`), Agreement: models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1}},
				{Value: json.RawMessage(`"two-pages"`), Agreement: models.MultiExtractAgreement{Pages: 2, IndependentRoots: 1}},
			},
		}
		response, err := answerForField(t, field, 1)
		if response != nil || errorCode(err) != models.ErrCodeAnswerFailed {
			t.Fatalf("Answer() = (%#v, %v)", response, err)
		}
	})

	t.Run("ambiguous null top tie cannot promote a lower non-null closest", func(t *testing.T) {
		field := models.MultiExtractFieldConsensus{
			Ambiguous: true,
			Conflicts: []models.MultiExtractConflict{
				{Value: json.RawMessage(`null`), Agreement: models.MultiExtractAgreement{Pages: 2, IndependentRoots: 2}},
				{Value: json.RawMessage(" \n null\t"), Agreement: models.MultiExtractAgreement{Pages: 2, IndependentRoots: 2}},
				{Value: json.RawMessage(`"lower"`), Agreement: models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1}},
			},
		}
		response, err := answerForField(t, field, 1)
		if response != nil || errorCode(err) != models.ErrCodeAnswerFailed {
			t.Fatalf("Answer() = (%#v, %v)", response, err)
		}
	})
}

func TestAnswerRejectsMoreThanEightWinnerAndConflictCandidates(t *testing.T) {
	conflicts := make([]models.MultiExtractConflict, models.MaxExtractSources)
	for index := range conflicts {
		conflicts[index] = models.MultiExtractConflict{
			Value:     json.RawMessage(`"value-` + string(rune('a'+index)) + `"`),
			Agreement: models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1},
		}
	}
	_, err := decide(context.Background(), models.MultiExtractFieldConsensus{
		Value:     json.RawMessage(`"winner"`),
		Agreement: models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1},
		Conflicts: conflicts,
	}, 1, map[string]string{})
	if errorCode(err) != models.ErrCodeAnswerFailed {
		t.Fatalf("decide() error = %v", err)
	}
}

func TestAnswerBindsSupportsToUniqueFinalURLSnapshots(t *testing.T) {
	requestedA := "https://origin-a.example.com/fact"
	requestedB := "https://origin-b.example.org/fact"
	finalA := "https://canonical-a.example.net/fact"
	finalB := "https://canonical-b.example.edu/fact"
	snapshotA := testSnapshotID("snapshot-a")
	snapshotB := testSnapshotID("snapshot-b")
	valid := func(requested, finalURL, snapshotID string) models.MultiExtractSource {
		return models.MultiExtractSource{
			URL:        requested,
			FinalURL:   finalURL,
			SnapshotID: snapshotID,
			Success:    true,
			Status:     models.MultiExtractSourceStatusValid,
		}
	}

	allowed, err := allowedConsensusSupports(
		[]string{requestedA, requestedB},
		[]models.MultiExtractSource{valid(requestedA, finalA, snapshotA), valid(requestedB, finalB, snapshotB)},
	)
	if err != nil || len(allowed) != 2 || allowed[finalA] != snapshotA || allowed[finalB] != snapshotB {
		t.Fatalf("allowedConsensusSupports() = (%#v, %v)", allowed, err)
	}
	matching := models.MultiExtractSupport{
		URL: finalA, Root: "example.net", Evidence: &evidence.Anchor{SnapshotID: snapshotA},
	}
	if canonical, err := validateSupportIdentity(matching, allowed); err != nil || canonical != finalA {
		t.Fatalf("validateSupportIdentity(matching) = (%q, %v)", canonical, err)
	}
	mismatched := matching
	mismatched.Evidence = &evidence.Anchor{SnapshotID: snapshotB}
	if canonical, err := validateSupportIdentity(mismatched, allowed); canonical != "" || errorCode(err) != models.ErrCodeAnswerFailed {
		t.Fatalf("validateSupportIdentity(mismatched) = (%q, %v)", canonical, err)
	}

	t.Run("two valid summaries cannot share a final URL", func(t *testing.T) {
		allowed, err := allowedConsensusSupports(
			[]string{requestedA, requestedB},
			[]models.MultiExtractSource{valid(requestedA, finalA, snapshotA), valid(requestedB, finalA, snapshotB)},
		)
		if allowed != nil || errorCode(err) != models.ErrCodeAnswerFailed {
			t.Fatalf("allowedConsensusSupports() = (%#v, %v)", allowed, err)
		}
	})

	t.Run("duplicate requested summary cannot overwrite snapshot", func(t *testing.T) {
		allowed, err := allowedConsensusSupports(
			[]string{requestedA, requestedB},
			[]models.MultiExtractSource{valid(requestedA, finalA, snapshotA), valid(requestedA, finalB, snapshotB)},
		)
		if allowed != nil || errorCode(err) != models.ErrCodeAnswerFailed {
			t.Fatalf("allowedConsensusSupports() = (%#v, %v)", allowed, err)
		}
	})
}

func TestAnswerRejectsOverstatedIndependentSupportRoots(t *testing.T) {
	t.Run("winner", func(t *testing.T) {
		field := knownField(`"winner"`, 2, 2, time.Now().UTC())
		field.Supports[1].URL = "https://two.example.com/fact"
		field.Supports[1].Root = "example.com"
		response, err := answerForField(t, field, 2)
		if response != nil || errorCode(err) != models.ErrCodeAnswerFailed {
			t.Fatalf("Answer() = (%#v, %v)", response, err)
		}
	})

	t.Run("conflict", func(t *testing.T) {
		field := knownField(`"winner"`, 3, 3, time.Now().UTC())
		conflictSupports := generatedSupports(models.MultiExtractAgreement{Pages: 2, IndependentRoots: 2}, 3, `"other"`)
		conflictSupports[1].URL = "https://mirror." + conflictSupports[0].Root + "/fact"
		conflictSupports[1].Root = conflictSupports[0].Root
		field.Conflicts = []models.MultiExtractConflict{{
			Value:     json.RawMessage(`"other"`),
			Agreement: models.MultiExtractAgreement{Pages: 2, IndependentRoots: 2},
			Supports:  conflictSupports,
		}}
		response, err := answerForField(t, field, 3)
		if response != nil || errorCode(err) != models.ErrCodeAnswerFailed {
			t.Fatalf("Answer() = (%#v, %v)", response, err)
		}
	})

	t.Run("encoded belief", func(t *testing.T) {
		response := runAnswerWithField(t, knownField(`"winner"`, 2, 2, time.Now().UTC()), 2)
		oldURL := response.Belief.Evidence[1].URL
		newURL := "https://two.example.com/fact"
		receipt := response.Belief.Receipts[oldURL]
		delete(response.Belief.Receipts, oldURL)
		response.Belief.Receipts[newURL] = receipt
		response.Belief.Evidence[1].URL = newURL
		response.Belief.Evidence[1].Root = "example.com"
		service, _ := NewService(&stubAnswerSearcher{}, &stubAnswerExtractor{})
		if encoded, err := service.EncodeResponse(context.Background(), response); encoded != nil || errorCode(err) != models.ErrCodeAnswerFailed {
			t.Fatalf("EncodeResponse() = (%q, %v)", encoded, err)
		}
	})
}

func TestAnswerRejectsMoreThanEightCandidatePages(t *testing.T) {
	winnerAgreement := models.MultiExtractAgreement{Pages: 5, IndependentRoots: 1}
	conflictAgreement := models.MultiExtractAgreement{Pages: 4, IndependentRoots: 1}
	winnerSupports := generatedSupports(winnerAgreement, 0, `"winner"`)
	conflictSupports := generatedSupports(conflictAgreement, 5, `"other"`)
	allowed := make(map[string]string, len(winnerSupports)+len(conflictSupports))
	for _, supports := range [][]models.MultiExtractSupport{winnerSupports, conflictSupports} {
		for _, support := range supports {
			allowed[support.URL] = support.Evidence.SnapshotID
		}
	}
	_, err := decide(context.Background(), models.MultiExtractFieldConsensus{
		Value:     json.RawMessage(`"winner"`),
		Agreement: winnerAgreement,
		Supports:  winnerSupports,
		Conflicts: []models.MultiExtractConflict{{
			Value:     json.RawMessage(`"other"`),
			Agreement: conflictAgreement,
			Supports:  conflictSupports,
		}},
	}, 1, allowed)
	if errorCode(err) != models.ErrCodeAnswerFailed {
		t.Fatalf("decide() error = %v", err)
	}
}

func TestAnswerAcceptsValidatedRedirectSupportAndDropsFailureDetails(t *testing.T) {
	secret := "sk-provider-should-not-project"
	requested := "https://origin.example.com/fact"
	finalURL := "https://canonical.example.net/fact"
	field := knownField(`"value"`, 1, 1, time.Now().UTC())
	field.Supports[0].URL = finalURL
	field.Supports[0].Root = "example.net"
	searchResponse := successfulSearch(requested, "https://failed.example.org/fact")
	searchResponse.Results[0].Title = secret
	searchResponse.Results[0].Snippet = secret
	extractResponse := successfulConsensus("price", field)
	extractResponse.Sources = []models.MultiExtractSource{
		{
			URL:        requested,
			FinalURL:   finalURL,
			SnapshotID: field.Supports[0].Evidence.SnapshotID,
			Success:    true,
			Status:     models.MultiExtractSourceStatusValid,
		},
		{
			URL:     "https://failed.example.org/fact",
			Success: false,
			Status:  models.MultiExtractSourceStatusExtractionFailed,
			Error:   &models.ErrorDetail{Code: models.ErrCodeLLMFailure, Message: secret},
		},
	}
	searcher := &stubAnswerSearcher{response: searchResponse}
	extractor := &stubAnswerExtractor{response: extractResponse}
	service, _ := NewService(searcher, extractor)
	request := validAnswerRequest()
	request.Spec.MinIndependentSources = 1
	response, err := service.Answer(context.Background(), request)
	if err != nil || response.Belief == nil || response.Belief.Evidence[0].URL != finalURL {
		t.Fatalf("Answer() = (%#v, %v)", response, err)
	}
	encoded, err := json.Marshal(response)
	if err != nil || strings.Contains(string(encoded), secret) {
		t.Fatalf("public response = %s, %v", encoded, err)
	}
}

func TestAnswerAcceptsMissingFieldOnlyFromCleanSchemaInvalidAggregate(t *testing.T) {
	searcher := &stubAnswerSearcher{response: successfulSearch("https://one.example.com/fact")}
	extractor := &stubAnswerExtractor{response: &models.MultiExtractResponse{
		Success: true,
		Status:  models.MultiExtractStatusSchemaInvalid,
		Consensus: &models.MultiExtractConsensus{
			Fields: map[string]models.MultiExtractFieldConsensus{},
		},
		Sources: []models.MultiExtractSource{{
			URL:        "https://one.example.com/fact",
			FinalURL:   "https://one.example.com/fact",
			SnapshotID: testSnapshotID("https://one.example.com/fact"),
			Success:    true,
			Status:     models.MultiExtractSourceStatusValid,
		}},
		Violations: []models.SchemaViolation{{Path: "$", Message: "required value unavailable"}},
	}}
	service, _ := NewService(searcher, extractor)
	response, err := service.Answer(context.Background(), validAnswerRequest())
	if err != nil || response.Status != models.AnswerStatusUnknown || response.Reason != models.AnswerUnknownMissingValue ||
		response.Belief != nil || response.Closest != nil {
		t.Fatalf("Answer() = (%#v, %v)", response, err)
	}
}

func TestAnswerRejectsNonJSONTimes(t *testing.T) {
	t.Run("evidence", func(t *testing.T) {
		field := knownField(`"value"`, 1, 1, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC))
		response, err := answerForField(t, field, 1)
		if response != nil || errorCode(err) != models.ErrCodeAnswerFailed {
			t.Fatalf("Answer() = (%#v, %v)", response, err)
		}
	})
	t.Run("belief and lease", func(t *testing.T) {
		field := knownField(`"value"`, 1, 1, time.Now().UTC())
		field = ensureTestSupports(field)
		urls := supportURLs(field)
		searcher := &stubAnswerSearcher{response: successfulSearch(urls...)}
		extractor := &stubAnswerExtractor{response: successfulConsensus("price", field)}
		service, _ := NewService(searcher, extractor)
		service.now = func() time.Time { return time.Date(9999, 12, 31, 23, 0, 0, 0, time.UTC) }
		request := validAnswerRequest()
		request.Spec.MinIndependentSources = 1
		response, err := service.Answer(context.Background(), request)
		if response != nil || errorCode(err) != models.ErrCodeAnswerFailed {
			t.Fatalf("Answer() = (%#v, %v)", response, err)
		}
	})
}

func TestAnswerRejectsInvalidSnapshotAndFutureEvidence(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*models.MultiExtractFieldConsensus)
	}{
		{
			name: "snapshot",
			mutate: func(field *models.MultiExtractFieldConsensus) {
				field.Supports[0].Evidence.SnapshotID = "sha256:not-a-digest"
			},
		},
		{
			name: "future fetched_at",
			mutate: func(field *models.MultiExtractFieldConsensus) {
				field.Supports[0].Evidence.FetchedAt = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			field := knownField(`"value"`, 1, 1, time.Now().UTC())
			test.mutate(&field)
			response, err := answerForField(t, field, 1)
			if response != nil || errorCode(err) != models.ErrCodeAnswerFailed {
				t.Fatalf("Answer() = (%#v, %v)", response, err)
			}
		})
	}
}

func TestEncodeAnswerResponseRejectsInvalidShapesAndCancellation(t *testing.T) {
	service, _ := NewService(&stubAnswerSearcher{}, &stubAnswerExtractor{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if encoded, err := service.EncodeResponse(ctx, &models.AnswerResponse{}); encoded != nil || errorCode(err) != models.ErrCodeTimeout {
		t.Fatalf("EncodeResponse(canceled) = (%q, %v)", encoded, err)
	}

	tooMany := make([]models.AnswerCandidate, models.MaxExtractSources+1)
	for index := range tooMany {
		tooMany[index] = models.AnswerCandidate{
			Value:     json.RawMessage(`"value"`),
			Agreement: models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1},
		}
	}
	invalid := &models.AnswerResponse{
		Status:    models.AnswerStatusUnknown,
		Belief:    nil,
		Reason:    models.AnswerUnknownConflict,
		Needs:     &models.AnswerNeeds{MoreIndependentSources: 1},
		Conflicts: tooMany,
	}
	if encoded, err := service.EncodeResponse(context.Background(), invalid); encoded != nil || errorCode(err) != models.ErrCodeAnswerFailed {
		t.Fatalf("EncodeResponse(invalid) = (%q, %v)", encoded, err)
	}

	for _, response := range []*models.AnswerResponse{
		{
			Status:  models.AnswerStatusUnknown,
			Belief:  nil,
			Reason:  models.AnswerUnknownNoSearchResults,
			Needs:   &models.AnswerNeeds{MoreIndependentSources: 2},
			Closest: &models.AnswerClosest{Value: json.RawMessage(`"stray"`), IndependentRoots: 1, Note: "stray"},
		},
		{
			Status: models.AnswerStatusUnknown,
			Belief: nil,
			Reason: models.AnswerUnknownInsufficient,
			Needs:  &models.AnswerNeeds{MoreIndependentSources: 1},
		},
		{
			Status:  models.AnswerStatusUnknown,
			Belief:  nil,
			Reason:  models.AnswerUnknownConflict,
			Needs:   &models.AnswerNeeds{MoreIndependentSources: 1},
			Closest: &models.AnswerClosest{Value: json.RawMessage(`"only"`), IndependentRoots: 1, Note: "tie"},
			Conflicts: []models.AnswerCandidate{{
				Value: json.RawMessage(`"\u006fnly"`), Agreement: models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1},
			}},
		},
	} {
		if encoded, err := service.EncodeResponse(context.Background(), response); encoded != nil || errorCode(err) != models.ErrCodeAnswerFailed {
			t.Fatalf("EncodeResponse(reason mismatch) = (%q, %v), response=%#v", encoded, err, response)
		}
	}

	validInsufficient := runAnswerWithField(t, models.MultiExtractFieldConsensus{
		Value:     json.RawMessage(`"closest"`),
		Agreement: models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1},
	}, 2)
	if encoded, err := service.EncodeResponse(context.Background(), validInsufficient); err != nil || !json.Valid(encoded) {
		t.Fatalf("EncodeResponse(valid unknown) = (%q, %v)", encoded, err)
	}
	validConflict := runAnswerWithField(t, models.MultiExtractFieldConsensus{
		Value:     json.RawMessage(`"winner"`),
		Agreement: models.MultiExtractAgreement{Pages: 2, IndependentRoots: 2},
		Conflicts: []models.MultiExtractConflict{{
			Value:     json.RawMessage(`"other"`),
			Agreement: models.MultiExtractAgreement{Pages: 2, IndependentRoots: 2},
		}},
	}, 2)
	if encoded, err := service.EncodeResponse(context.Background(), validConflict); err != nil || !json.Valid(encoded) {
		t.Fatalf("EncodeResponse(valid conflict) = (%q, %v)", encoded, err)
	}
}

func TestAnswerConflictNeedsRemainingThresholdAndEncodes(t *testing.T) {
	response := runAnswerWithField(t, models.MultiExtractFieldConsensus{
		Value:     json.RawMessage(`"winner"`),
		Agreement: models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1},
		Conflicts: []models.MultiExtractConflict{{
			Value:     json.RawMessage(`"other"`),
			Agreement: models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1},
		}},
	}, models.MaxAnswerMinIndependentSources)
	if response.Status != models.AnswerStatusUnknown || response.Reason != models.AnswerUnknownConflict ||
		response.Needs == nil || response.Needs.MoreIndependentSources != 7 || response.Closest == nil ||
		response.Closest.IndependentRoots != 1 || string(response.Closest.Value) != `"winner"` ||
		len(response.Conflicts) != 1 || string(response.Conflicts[0].Value) != `"other"` ||
		response.Conflicts[0].Agreement.IndependentRoots != response.Closest.IndependentRoots {
		t.Fatalf("Answer() = %#v", response)
	}
	service, _ := NewService(&stubAnswerSearcher{}, &stubAnswerExtractor{})
	if encoded, err := service.EncodeResponse(context.Background(), response); err != nil || !json.Valid(encoded) {
		t.Fatalf("EncodeResponse() = (%q, %v)", encoded, err)
	}
}

func TestEncodeAnswerResponseEnforcesCanonicalCandidateIntegrity(t *testing.T) {
	service, _ := NewService(&stubAnswerSearcher{}, &stubAnswerExtractor{})
	assertRejected := func(t *testing.T, response *models.AnswerResponse) {
		t.Helper()
		if encoded, err := service.EncodeResponse(context.Background(), response); encoded != nil || errorCode(err) != models.ErrCodeAnswerFailed {
			t.Fatalf("EncodeResponse() = (%q, %v), response=%#v", encoded, err, response)
		}
	}

	t.Run("known conflict canonically repeats winner", func(t *testing.T) {
		response := runAnswerWithField(t, knownField(`"same"`, 2, 2, time.Now().UTC()), 2)
		response.Conflicts = []models.AnswerCandidate{{
			Value:     json.RawMessage(`"\u0073ame"`),
			Agreement: models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1},
		}}
		assertRejected(t, response)
	})

	t.Run("canonical null alternatives repeat", func(t *testing.T) {
		assertRejected(t, &models.AnswerResponse{
			Status:  models.AnswerStatusUnknown,
			Belief:  nil,
			Reason:  models.AnswerUnknownConflict,
			Needs:   &models.AnswerNeeds{MoreIndependentSources: 1},
			Closest: &models.AnswerClosest{Value: json.RawMessage(`"winner"`), IndependentRoots: 1, Note: "tie"},
			Conflicts: []models.AnswerCandidate{
				{Value: json.RawMessage(`null`), Agreement: models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1}},
				{Value: json.RawMessage(" \n null\t"), Agreement: models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1}},
			},
		})
	})

	t.Run("alternatives are out of page order", func(t *testing.T) {
		assertRejected(t, &models.AnswerResponse{
			Status:  models.AnswerStatusUnknown,
			Belief:  nil,
			Reason:  models.AnswerUnknownConflict,
			Needs:   &models.AnswerNeeds{MoreIndependentSources: 1},
			Closest: &models.AnswerClosest{Value: json.RawMessage(`"winner"`), IndependentRoots: 2, Note: "tie"},
			Conflicts: []models.AnswerCandidate{
				{Value: json.RawMessage(`"tie"`), Agreement: models.MultiExtractAgreement{Pages: 2, IndependentRoots: 2}},
				{Value: json.RawMessage(`"one-page"`), Agreement: models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1}},
				{Value: json.RawMessage(`"two-pages"`), Agreement: models.MultiExtractAgreement{Pages: 2, IndependentRoots: 1}},
			},
		})
	})

	t.Run("necessary candidate pages exceed source fanout", func(t *testing.T) {
		assertRejected(t, &models.AnswerResponse{
			Status:  models.AnswerStatusUnknown,
			Belief:  nil,
			Reason:  models.AnswerUnknownConflict,
			Needs:   &models.AnswerNeeds{MoreIndependentSources: 1},
			Closest: &models.AnswerClosest{Value: json.RawMessage(`"winner"`), IndependentRoots: 4, Note: "tie"},
			Conflicts: []models.AnswerCandidate{{
				Value: json.RawMessage(`"other"`), Agreement: models.MultiExtractAgreement{Pages: 5, IndependentRoots: 4},
			}},
		})
	})
}

func TestCloneScalarCanonicalizesNull(t *testing.T) {
	for _, raw := range []json.RawMessage{json.RawMessage(`null`), json.RawMessage(" \n null\t")} {
		value, isNull, err := cloneScalar(context.Background(), raw)
		if err != nil || !isNull || string(value) != "null" {
			t.Fatalf("cloneScalar(%q) = (%q, %t, %v)", raw, value, isNull, err)
		}
	}
}

func TestAnswerCancellationWinsOverLateDependencySuccess(t *testing.T) {
	t.Run("pre canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		searcher := &stubAnswerSearcher{}
		extractor := &stubAnswerExtractor{}
		service, _ := NewService(searcher, extractor)
		response, err := service.Answer(ctx, validAnswerRequest())
		if response != nil || errorCode(err) != models.ErrCodeTimeout || searcher.calls != 0 || extractor.calls != 0 {
			t.Fatalf("Answer() = (%#v, %v), calls=%d/%d", response, err, searcher.calls, extractor.calls)
		}
	})
	t.Run("late search success", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		searcher := &stubAnswerSearcher{call: func(context.Context, *models.SearchRequest, search.RunOptions) (*models.SearchResponse, error) {
			cancel()
			return successfulSearch("https://one.example.com/fact"), nil
		}}
		extractor := &stubAnswerExtractor{}
		service, _ := NewService(searcher, extractor)
		response, err := service.Answer(ctx, validAnswerRequest())
		if response != nil || errorCode(err) != models.ErrCodeTimeout || extractor.calls != 0 {
			t.Fatalf("Answer() = (%#v, %v), extract calls=%d", response, err, extractor.calls)
		}
	})
	t.Run("late extraction success", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		searcher := &stubAnswerSearcher{response: successfulSearch("https://one.example.com/fact")}
		extractor := &stubAnswerExtractor{call: func(context.Context, *models.ExtractRequest) (*models.MultiExtractResponse, error) {
			cancel()
			return successfulConsensus("price", knownField(`"$3"`, 1, 1, time.Now().UTC())), nil
		}}
		service, _ := NewService(searcher, extractor)
		response, err := service.Answer(ctx, validAnswerRequest())
		if response != nil || errorCode(err) != models.ErrCodeTimeout {
			t.Fatalf("Answer() = (%#v, %v)", response, err)
		}
	})
}

func TestAnswerRedactsDependencyErrorsAndRejectsTypedNil(t *testing.T) {
	secret := "sk-provider-super-secret"
	searcher := &stubAnswerSearcher{err: models.NewScrapeError(models.ErrCodeSearchFailed, "upstream failed", errors.New(secret))}
	extractor := &stubAnswerExtractor{}
	service, _ := NewService(searcher, extractor)
	_, err := service.Answer(context.Background(), validAnswerRequest())
	if errorCode(err) != models.ErrCodeAnswerFailed || strings.Contains(err.Error(), secret) {
		t.Fatalf("error = %v", err)
	}

	var nilSearcher *stubAnswerSearcher
	if service, err := NewService(nilSearcher, extractor); err == nil || service != nil {
		t.Fatalf("NewService(typed nil searcher) = (%#v, %v)", service, err)
	}
	var nilExtractor *stubAnswerExtractor
	if service, err := NewService(&stubAnswerSearcher{}, nilExtractor); err == nil || service != nil {
		t.Fatalf("NewService(typed nil extractor) = (%#v, %v)", service, err)
	}
}

func runAnswerWithField(t *testing.T, field models.MultiExtractFieldConsensus, minimum int) *models.AnswerResponse {
	t.Helper()
	response, err := answerForField(t, field, minimum)
	if err != nil {
		t.Fatalf("Answer() error = %v", err)
	}
	return response
}

func answerForField(t *testing.T, field models.MultiExtractFieldConsensus, minimum int) (*models.AnswerResponse, error) {
	t.Helper()
	field = ensureTestSupports(field)
	urls := supportURLs(field)
	if len(urls) == 0 {
		urls = []string{"https://one.example.com/fact"}
	}
	searcher := &stubAnswerSearcher{response: successfulSearch(urls...)}
	extractor := &stubAnswerExtractor{response: successfulConsensus("price", field)}
	service, err := NewService(searcher, extractor)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	request := validAnswerRequest()
	request.Spec.MinIndependentSources = minimum
	return service.Answer(context.Background(), request)
}

func successfulSearch(urls ...string) *models.SearchResponse {
	results := make([]models.SearchResult, len(urls))
	for index, rawURL := range urls {
		results[index] = models.SearchResult{Rank: index + 1, URL: rawURL, VerificationStatus: models.SearchVerificationNotChecked}
	}
	return &models.SearchResponse{Success: true, Query: "anthropic claude price", Results: results}
}

func successfulConsensus(path string, field models.MultiExtractFieldConsensus) *models.MultiExtractResponse {
	urls := supportURLs(field)
	if len(urls) == 0 {
		urls = []string{"https://one.example.com/fact"}
	}
	snapshotByURL := make(map[string]string, len(urls))
	addSnapshots := func(supports []models.MultiExtractSupport) {
		for _, support := range supports {
			if support.Evidence != nil {
				snapshotByURL[support.URL] = support.Evidence.SnapshotID
			}
		}
	}
	addSnapshots(field.Supports)
	for _, conflict := range field.Conflicts {
		addSnapshots(conflict.Supports)
	}
	sources := make([]models.MultiExtractSource, len(urls))
	for index, rawURL := range urls {
		snapshotID := snapshotByURL[rawURL]
		if snapshotID == "" {
			snapshotID = testSnapshotID(rawURL)
		}
		sources[index] = models.MultiExtractSource{
			URL:        rawURL,
			FinalURL:   rawURL,
			SnapshotID: snapshotID,
			Success:    true,
			Status:     models.MultiExtractSourceStatusValid,
		}
	}
	response := &models.MultiExtractResponse{
		Success: true,
		Status:  models.MultiExtractStatusComplete,
		Consensus: &models.MultiExtractConsensus{Fields: map[string]models.MultiExtractFieldConsensus{
			path: field,
		}},
		Sources: sources,
	}
	switch {
	case field.Ambiguous:
		response.Status = models.MultiExtractStatusAmbiguous
	case len(field.Value) == 0 || string(field.Value) == "null":
		response.Status = models.MultiExtractStatusSchemaInvalid
		response.Violations = []models.SchemaViolation{{Path: "$", Message: "required value unavailable"}}
	default:
		property := strings.ReplaceAll(path, `\.`, ".")
		response.Data, _ = json.Marshal(map[string]json.RawMessage{
			property: append(json.RawMessage(nil), field.Value...),
		})
	}
	return response
}

func knownField(rawValue string, pages, roots int, fetchedAt time.Time) models.MultiExtractFieldConsensus {
	urls := []string{"https://one.example.com/fact", "https://two.example.net/fact", "https://three.example.org/fact"}
	rootsByURL := []string{"example.com", "example.net", "example.org"}
	supports := make([]models.MultiExtractSupport, pages)
	for index := range supports {
		quote := strings.Trim(rawValue, `"`)
		supports[index] = models.MultiExtractSupport{
			URL:     urls[index],
			Root:    rootsByURL[index],
			Receipt: "receipt-" + rootsByURL[index],
			Evidence: &evidence.Anchor{
				Quote:      quote,
				TextRange:  [2]int{0, len(quote)},
				Selector:   "#price",
				Method:     evidence.MethodExact,
				SnapshotID: testSnapshotID("snapshot-" + rootsByURL[index]),
				FetchedAt:  fetchedAt,
			},
		}
	}
	return models.MultiExtractFieldConsensus{
		Value:     json.RawMessage(rawValue),
		Agreement: models.MultiExtractAgreement{Pages: pages, IndependentRoots: roots},
		Supports:  supports,
	}
}

func ensureTestSupports(field models.MultiExtractFieldConsensus) models.MultiExtractFieldConsensus {
	next := 0
	if !field.Ambiguous && len(field.Value) > 0 && field.Agreement.Pages > 0 && len(field.Supports) == 0 {
		field.Supports = generatedSupports(field.Agreement, next, string(field.Value))
		next += field.Agreement.Pages
	}
	for index := range field.Conflicts {
		if len(field.Conflicts[index].Supports) == 0 && field.Conflicts[index].Agreement.Pages > 0 {
			field.Conflicts[index].Supports = generatedSupports(field.Conflicts[index].Agreement, next, string(field.Conflicts[index].Value))
			next += field.Conflicts[index].Agreement.Pages
		}
	}
	return field
}

func generatedSupports(agreement models.MultiExtractAgreement, offset int, rawValue string) []models.MultiExtractSupport {
	quote := strings.Trim(rawValue, `"`)
	if quote == "" {
		quote = "null"
	}
	supports := make([]models.MultiExtractSupport, agreement.Pages)
	for index := range supports {
		rootIndex := 0
		if agreement.IndependentRoots > 0 {
			rootIndex = index % agreement.IndependentRoots
		}
		root := "root-" + string(rune('a'+offset+rootIndex)) + ".com"
		host := "source-" + string(rune('a'+offset+index)) + "." + root
		supports[index] = models.MultiExtractSupport{
			URL:     "https://" + host + "/fact",
			Root:    root,
			Receipt: "receipt-" + host,
			Evidence: &evidence.Anchor{
				Quote:      quote,
				TextRange:  [2]int{0, len(quote)},
				Selector:   "#fact",
				Method:     evidence.MethodExact,
				SnapshotID: testSnapshotID(host),
				FetchedAt:  time.Date(2025, 1, 2, 2, 9, 0, 0, time.UTC),
			},
		}
	}
	return supports
}

func supportURLs(field models.MultiExtractFieldConsensus) []string {
	seen := make(map[string]struct{})
	urls := make([]string, 0, models.MaxExtractSources)
	add := func(supports []models.MultiExtractSupport) {
		for _, support := range supports {
			if _, exists := seen[support.URL]; exists {
				continue
			}
			seen[support.URL] = struct{}{}
			urls = append(urls, support.URL)
		}
	}
	add(field.Supports)
	for _, conflict := range field.Conflicts {
		add(conflict.Supports)
	}
	return urls
}

func testSnapshotID(seed string) string {
	digest := sha256.Sum256([]byte(seed))
	const hexadecimal = "0123456789abcdef"
	encoded := make([]byte, len(digest)*2)
	for index, value := range digest {
		encoded[index*2] = hexadecimal[value>>4]
		encoded[index*2+1] = hexadecimal[value&0x0f]
	}
	return "sha256:" + string(encoded)
}

func validAnswerRequest() *models.AnswerRequest {
	return &models.AnswerRequest{Spec: models.FactSpec{Subject: "anthropic claude", Predicate: "price"}}
}

func cloneObservedSearchRequest(input *models.SearchRequest) *models.SearchRequest {
	if input == nil {
		return nil
	}
	output := *input
	output.Query = strings.Clone(input.Query)
	output.Freshness = strings.Clone(input.Freshness)
	output.Domains = append([]string(nil), input.Domains...)
	output.Schema = append(json.RawMessage(nil), input.Schema...)
	if input.Deduplicate != nil {
		value := *input.Deduplicate
		output.Deduplicate = &value
	}
	return &output
}

func cloneObservedExtractRequest(input *models.ExtractRequest) *models.ExtractRequest {
	if input == nil {
		return nil
	}
	output := *input
	output.Sources = append([]string(nil), input.Sources...)
	output.Schema = append(json.RawMessage(nil), input.Schema...)
	return &output
}

func errorCode(err error) string {
	var scrapeError *models.ScrapeError
	if !errors.As(err, &scrapeError) {
		return ""
	}
	return scrapeError.Code
}
