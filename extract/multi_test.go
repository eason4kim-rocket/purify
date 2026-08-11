package extract

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/use-agent/purify/consensus"
	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/llm"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/receipts"
	"github.com/use-agent/purify/scrape"
	"github.com/use-agent/purify/scraper"
	"github.com/use-agent/purify/simhash"
	"github.com/use-agent/purify/snapshot"
)

func TestExtractMultiCanonicalDedupeCompleteConsensusAndWallTiming(t *testing.T) {
	const proxyURL = "socks5://127.0.0.1:1080"
	const fetchDelay = 40 * time.Millisecond
	plans := map[string]multiRunnerPlan{
		"https://a.example.com/": {
			content: "Ada alpha orchard river copper violet",
			delay:   fetchDelay,
		},
		"https://b.example.net/path": {
			content: "Ada zebra tundra quartz maple silver",
			delay:   fetchDelay,
		},
	}
	runner := &multiRunner{plans: plans}
	extractor := &multiExtractor{plans: map[string]multiExtractorPlan{
		plans["https://a.example.com/"].content:     {initial: json.RawMessage(`{"name":"Ada"}`)},
		plans["https://b.example.net/path"].content: {initial: json.RawMessage(`{"name":"Ada"}`)},
	}}
	signer := &multiSigner{}
	service := newMultiTestService(t, runner, extractor, signer, Config{SafeProxyURL: proxyURL})
	wantsNetwork := false
	request := &models.ExtractRequest{
		Sources: []string{
			"https://B.example.net:443/path#first",
			"https://a.example.com",
			"https://b.example.net/path#duplicate",
		},
		Schema:             multiNameSchema(),
		Engine:             "llm",
		LLMAPIKey:          "byok-secret",
		WaitForNetworkIdle: &wantsNetwork,
		Timeout:            2,
	}
	wantRequest := cloneMultiRequest(request)

	response, err := service.ExtractMulti(context.Background(), request)
	if err != nil {
		t.Fatalf("ExtractMulti() error = %v", err)
	}
	if !reflect.DeepEqual(request, wantRequest) {
		t.Fatalf("caller request mutated:\n got: %#v\nwant: %#v", request, wantRequest)
	}
	if !response.Success || response.Status != models.MultiExtractStatusComplete || string(response.Data) != `{"name":"Ada"}` {
		t.Fatalf("response = %#v", response)
	}
	if len(response.Sources) != 2 || response.Sources[0].URL != "https://a.example.com/" ||
		response.Sources[1].URL != "https://b.example.net/path" {
		t.Fatalf("stable deduplicated sources = %#v", response.Sources)
	}
	for _, source := range response.Sources {
		if !source.Success || source.Status != models.MultiExtractSourceStatusValid ||
			source.FinalURL != source.URL || source.SnapshotID == "" || source.Timing.TotalMs < 30 {
			t.Fatalf("source summary = %#v", source)
		}
	}
	if response.Tokens != (models.TokenInfo{OriginalEstimate: 20, CleanedEstimate: 10, SavingsPercent: 50}) {
		t.Fatalf("tokens = %#v", response.Tokens)
	}
	if response.LLMUsage == nil || *response.LLMUsage != (models.LLMUsage{PromptTokens: 2, CompletionTokens: 2, TotalTokens: 4}) || !response.UsageComplete {
		t.Fatalf("usage = %#v, complete=%v", response.LLMUsage, response.UsageComplete)
	}
	if response.Consensus == nil {
		t.Fatal("consensus is nil")
	}
	field, ok := response.Consensus.Fields["name"]
	if !ok || field.Agreement != (models.MultiExtractAgreement{Pages: 2, IndependentRoots: 2}) || field.Ambiguous {
		t.Fatalf("name consensus = %#v", field)
	}
	summedSourceWall := response.Sources[0].Timing.TotalMs + response.Sources[1].Timing.TotalMs
	if response.Timing.TotalMs <= 0 || response.Timing.TotalMs >= summedSourceWall {
		t.Fatalf("fanout total=%d, summed source wall=%d", response.Timing.TotalMs, summedSourceWall)
	}
	requests, calls, maxActive := runner.snapshot()
	if calls != 2 || maxActive != 2 || len(requests) != 2 {
		t.Fatalf("runner calls/max-active/requests = %d/%d/%d", calls, maxActive, len(requests))
	}
	for _, got := range requests {
		if got.ProxyURL != proxyURL || got.CSSSelector != "" || got.OutputFormat != "markdown" ||
			got.ExtractMode != "readability" || got.Timeout != 2 || got.MaxAge != 0 ||
			got.MaximumBodyBytes != maximumExtractArtifactBytes ||
			got.WaitForNetworkIdle == nil || *got.WaitForNetworkIdle {
			t.Fatalf("forced scrape request = %#v", got)
		}
	}
	params, extractCalls, repairCalls := extractor.snapshot()
	if extractCalls != 2 || repairCalls != 0 {
		t.Fatalf("extract/repair calls = %d/%d", extractCalls, repairCalls)
	}
	for _, got := range params {
		if got.APIKey != "byok-secret" || got.Model != "gpt-4o-mini" || got.BaseURL != "https://api.openai.com/v1" {
			t.Fatalf("BYOK params = %#v", got)
		}
	}
	if signer.calls() != 2 {
		t.Fatalf("receipt calls = %d, want 2", signer.calls())
	}
}

func TestExtractMultiInjectsCleanedTextForAnchoredCoreConsensus(t *testing.T) {
	body := multiNumberedWords("wire", 180) + " Ada " + multiNumberedWords("report", 180)
	firstContent := multiNumberedWords("alpha-nav", 700) + body + multiNumberedWords("alpha-footer", 700)
	secondContent := multiNumberedWords("bravo-nav", 700) + body + multiNumberedWords("bravo-footer", 700)
	if distance := simhash.Distance(simhash.Fingerprint(firstContent), simhash.Fingerprint(secondContent)); distance <= 3 {
		t.Fatalf("fixture legacy distance = %d, want > 3", distance)
	}
	runner := &multiRunner{plans: map[string]multiRunnerPlan{
		"https://a.example.com/": {content: firstContent},
		"https://b.example.net/": {content: secondContent},
	}}
	extractor := &multiExtractor{plans: map[string]multiExtractorPlan{
		firstContent:  {initial: json.RawMessage(`{"name":"Ada"}`)},
		secondContent: {initial: json.RawMessage(`{"name":"Ada"}`)},
	}}
	service := newMultiTestService(t, runner, extractor, &multiSigner{}, Config{SafeProxyURL: "socks5://127.0.0.1:1080"})

	response, err := service.ExtractMulti(context.Background(), multiRequest(
		multiNameSchema(),
		"https://a.example.com",
		"https://b.example.net",
	))
	if err != nil {
		t.Fatalf("ExtractMulti() error = %v", err)
	}
	if response.Consensus == nil {
		t.Fatalf("response consensus is nil: %#v", response)
	}
	field := response.Consensus.Fields["name"]
	if field.Agreement != (models.MultiExtractAgreement{
		Pages:            2,
		IndependentRoots: 1,
		FoldReason:       models.MultiExtractFoldReasonNearDuplicate,
	}) {
		t.Fatalf("agreement = %#v, cleaned text was not used for core folding", field.Agreement)
	}
	reasons := make(map[string]models.MultiExtractFoldReason, len(field.Supports))
	for _, support := range field.Supports {
		reasons[support.URL] = support.FoldReason
	}
	if len(reasons) != 2 || reasons["https://a.example.com/"] != "" ||
		reasons["https://b.example.net/"] != models.MultiExtractFoldReasonNearDuplicate {
		t.Fatalf("support fold reasons = %#v", reasons)
	}
}

func TestProjectMultiConsensusMapsFoldReasons(t *testing.T) {
	input := consensus.Result{Fields: map[string]consensus.FieldConsensus{
		"name": {
			Value: json.RawMessage(`"Ada"`),
			Agreement: consensus.Agreement{
				Pages:            3,
				IndependentRoots: 1,
				FoldReason:       consensus.FoldReasonSameRoot,
			},
			Supports: []consensus.Support{
				{URL: "https://a.example/", Root: "a.example"},
				{URL: "https://b.example/", Root: "b.example", FoldReason: consensus.FoldReasonNearDuplicate},
			},
			Conflicts: []consensus.Conflict{{
				Value: json.RawMessage(`"Grace"`),
				Agreement: consensus.Agreement{
					Pages:            2,
					IndependentRoots: 1,
					FoldReason:       consensus.FoldReasonQuoteLineage,
				},
				Supports: []consensus.Support{{
					URL:        "https://c.example/",
					Root:       "c.example",
					FoldReason: consensus.FoldReasonQuoteLineage,
				}},
			}},
		},
	}}

	projected := projectMultiConsensus(input)
	field := projected.Fields["name"]
	if field.Agreement != (models.MultiExtractAgreement{
		Pages:            3,
		IndependentRoots: 1,
		FoldReason:       models.MultiExtractFoldReasonSameRoot,
	}) || len(field.Supports) != 2 ||
		field.Supports[1].FoldReason != models.MultiExtractFoldReasonNearDuplicate ||
		len(field.Conflicts) != 1 ||
		field.Conflicts[0].Agreement.FoldReason != models.MultiExtractFoldReasonQuoteLineage ||
		len(field.Conflicts[0].Supports) != 1 ||
		field.Conflicts[0].Supports[0].FoldReason != models.MultiExtractFoldReasonQuoteLineage {
		t.Fatalf("projected fold reasons = %#v", field)
	}
}

func TestExtractMultiReturnsAmbiguousWithoutData(t *testing.T) {
	runner := &multiRunner{plans: map[string]multiRunnerPlan{
		"https://one.example.com/": {content: "Ada alpha orchard river copper violet"},
		"https://two.example.net/": {content: "Bob zebra tundra quartz maple silver"},
	}}
	extractor := &multiExtractor{plans: map[string]multiExtractorPlan{
		"Ada alpha orchard river copper violet": {initial: json.RawMessage(`{"name":"Ada"}`)},
		"Bob zebra tundra quartz maple silver":  {initial: json.RawMessage(`{"name":"Bob"}`)},
	}}
	service := newMultiTestService(t, runner, extractor, &multiSigner{}, Config{SafeProxyURL: "socks5://[::1]:1080"})
	response, err := service.ExtractMulti(context.Background(), multiRequest(
		multiNameSchema(),
		"https://two.example.net",
		"https://one.example.com",
	))
	if err != nil {
		t.Fatalf("ExtractMulti() error = %v", err)
	}
	if !response.Success || response.Status != models.MultiExtractStatusAmbiguous || len(response.Data) != 0 || response.Consensus == nil {
		t.Fatalf("response = %#v", response)
	}
	if field := response.Consensus.Fields["name"]; !field.Ambiguous || len(field.Conflicts) != 2 {
		t.Fatalf("ambiguous field = %#v", field)
	}
}

func TestExtractMultiFinalURLDedupeSelectsNewestThenCanonicalTie(t *testing.T) {
	sharedFinalURL := "https://final.example.org/article"
	older := time.Date(2026, time.August, 9, 7, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)
	tests := []struct {
		name          string
		firstFetched  time.Time
		secondFetched time.Time
		wantWinner    string
		wantName      string
	}{
		{
			name:          "newest fetched snapshot",
			firstFetched:  older,
			secondFetched: newer,
			wantWinner:    "https://b.example.net/",
			wantName:      "New",
		},
		{
			name:          "canonical URL breaks fetched-at tie",
			firstFetched:  newer,
			secondFetched: newer,
			wantWinner:    "https://a.example.com/",
			wantName:      "Old",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &multiRunner{plans: map[string]multiRunnerPlan{
				"https://a.example.com/": {
					content:   "Old source article alpha copper violet",
					finalURL:  sharedFinalURL,
					fetchedAt: test.firstFetched,
				},
				"https://b.example.net/": {
					content:   "New source article zebra quartz silver",
					finalURL:  sharedFinalURL,
					fetchedAt: test.secondFetched,
				},
			}}
			extractor := &multiExtractor{plans: map[string]multiExtractorPlan{
				"Old source article alpha copper violet": {initial: json.RawMessage(`{"name":"Old"}`)},
				"New source article zebra quartz silver": {initial: json.RawMessage(`{"name":"New"}`)},
			}}
			service := newMultiTestService(t, runner, extractor, &multiSigner{}, Config{SafeProxyURL: "socks5://127.0.0.1:1080"})
			response, err := service.ExtractMulti(context.Background(), multiRequest(
				multiNameSchema(),
				"https://b.example.net",
				"https://a.example.com",
			))
			if err != nil {
				t.Fatalf("ExtractMulti() error = %v", err)
			}
			if response.Status != models.MultiExtractStatusComplete || string(response.Data) != `{"name":"`+test.wantName+`"}` {
				t.Fatalf("response = %#v", response)
			}
			for _, source := range response.Sources {
				if source.URL == test.wantWinner {
					if !source.Success || source.Status != models.MultiExtractSourceStatusValid || source.DuplicateOf != "" {
						t.Fatalf("winner = %#v", source)
					}
					continue
				}
				if source.Success || source.Status != models.MultiExtractSourceStatusDuplicate ||
					source.DuplicateOf != test.wantWinner || source.Error != nil || source.FinalURL != sharedFinalURL {
					t.Fatalf("duplicate = %#v", source)
				}
			}
		})
	}
}

func TestExtractMultiMaterializedSchemaViolationReturnsSchemaInvalid(t *testing.T) {
	contents := []string{
		"source alpha values 1 0 0 orchard copper violet",
		"source beta values 0 1 0 tundra quartz silver",
		"source gamma values 0 0 1 canyon maple bronze",
	}
	urls := []string{"https://a.example.com/", "https://b.example.net/", "https://c.example.org/"}
	runnerPlans := make(map[string]multiRunnerPlan, len(urls))
	extractorPlans := make(map[string]multiExtractorPlan, len(urls))
	data := []json.RawMessage{json.RawMessage(`[1,0,0]`), json.RawMessage(`[0,1,0]`), json.RawMessage(`[0,0,1]`)}
	for index := range urls {
		runnerPlans[urls[index]] = multiRunnerPlan{content: contents[index]}
		extractorPlans[contents[index]] = multiExtractorPlan{initial: data[index]}
	}
	service := newMultiTestService(
		t,
		&multiRunner{plans: runnerPlans},
		&multiExtractor{plans: extractorPlans},
		&multiSigner{},
		Config{SafeProxyURL: "socks5://127.0.0.1:1080"},
	)
	schema := json.RawMessage(`{"type":"array","minItems":3,"maxItems":3,"contains":{"const":1}}`)
	response, err := service.ExtractMulti(context.Background(), multiRequest(schema, urls...))
	if err != nil {
		t.Fatalf("ExtractMulti() error = %v", err)
	}
	if !response.Success || response.Status != models.MultiExtractStatusSchemaInvalid || len(response.Data) != 0 || len(response.Violations) == 0 {
		t.Fatalf("response = %#v", response)
	}
}

func TestExtractMultiExcludesPartialAndTimeoutButKeepsValidSource(t *testing.T) {
	runner := &multiRunner{plans: map[string]multiRunnerPlan{
		"https://partial.example.net/": {content: "Count bad partial source"},
		"https://timeout.example.org/": {err: context.DeadlineExceeded},
		"https://valid.example.com/":   {content: "Count 3 valid source"},
	}}
	extractor := &multiExtractor{plans: map[string]multiExtractorPlan{
		"Count bad partial source": {
			initial: json.RawMessage(`{"count":"bad"}`),
			repair:  json.RawMessage(`{"count":"still-bad"}`),
		},
		"Count 3 valid source": {initial: json.RawMessage(`{"count":3}`)},
	}}
	service := newMultiTestService(t, runner, extractor, &multiSigner{}, Config{SafeProxyURL: "socks5://127.0.0.1:1080"})
	response, err := service.ExtractMulti(context.Background(), multiRequest(
		multiCountSchema(),
		"https://valid.example.com",
		"https://timeout.example.org",
		"https://partial.example.net",
	))
	if err != nil {
		t.Fatalf("ExtractMulti() error = %v", err)
	}
	if !response.Success || response.Status != models.MultiExtractStatusComplete || string(response.Data) != `{"count":3}` {
		t.Fatalf("response = %#v", response)
	}
	statuses := make(map[string]models.MultiExtractSourceStatus, len(response.Sources))
	for _, source := range response.Sources {
		statuses[source.URL] = source.Status
		if source.Error != nil && (strings.Contains(source.Error.Message, "bad") || strings.Contains(source.Error.Message, "deadline")) {
			t.Fatalf("source error leaked raw detail: %#v", source.Error)
		}
	}
	if statuses["https://partial.example.net/"] != models.MultiExtractSourceStatusPartial ||
		statuses["https://timeout.example.org/"] != models.MultiExtractSourceStatusTimeout ||
		statuses["https://valid.example.com/"] != models.MultiExtractSourceStatusValid || !response.UsageComplete {
		t.Fatalf("statuses=%#v usage_complete=%v", statuses, response.UsageComplete)
	}
}

func TestExtractMultiCapabilityAndNoValidSourceFailures(t *testing.T) {
	validPlans := map[string]multiRunnerPlan{"https://one.example.com/": {content: "Ada source"}}
	validExtractor := &multiExtractor{plans: map[string]multiExtractorPlan{
		"Ada source": {initial: json.RawMessage(`{"name":"Ada"}`)},
	}}
	request := multiRequest(multiNameSchema(), "https://one.example.com")

	t.Run("safe proxy missing", func(t *testing.T) {
		runner := &multiRunner{plans: validPlans}
		service := newMultiTestService(t, runner, validExtractor, &multiSigner{}, Config{})
		response, err := service.ExtractMulti(context.Background(), request)
		assertMultiScrapeCode(t, err, models.ErrCodeMultiSourceUnavailable)
		if response != nil {
			t.Fatalf("response = %#v, want nil", response)
		}
		_, calls, _ := runner.snapshot()
		if calls != 0 {
			t.Fatalf("fetch calls = %d", calls)
		}
	})

	t.Run("signer missing", func(t *testing.T) {
		runner := &multiRunner{plans: validPlans}
		service := newMultiTestService(t, runner, validExtractor, nil, Config{SafeProxyURL: "socks5://127.0.0.1:1080"})
		response, err := service.ExtractMulti(context.Background(), request)
		assertMultiScrapeCode(t, err, models.ErrCodeMultiSourceUnavailable)
		if response != nil {
			t.Fatalf("response = %#v, want nil", response)
		}
	})

	t.Run("snapshot capability missing", func(t *testing.T) {
		runner := &multiRunner{plans: map[string]multiRunnerPlan{
			"https://one.example.com/": {content: "Ada source", missingSnapshot: true},
		}}
		service := newMultiTestService(t, runner, validExtractor, &multiSigner{}, Config{SafeProxyURL: "socks5://127.0.0.1:1080"})
		response, err := service.ExtractMulti(context.Background(), request)
		assertMultiScrapeCode(t, err, models.ErrCodeMultiSourceUnavailable)
		if response == nil || len(response.Sources) != 1 ||
			response.Sources[0].Status != models.MultiExtractSourceStatusEvidenceUnavailable ||
			response.Error == nil || response.Error.Code != models.ErrCodeMultiSourceUnavailable ||
			!response.UsageComplete {
			t.Fatalf("response = %#v", response)
		}
	})

	t.Run("all timeout", func(t *testing.T) {
		runner := &multiRunner{plans: map[string]multiRunnerPlan{
			"https://one.example.com/": {err: context.DeadlineExceeded},
		}}
		service := newMultiTestService(t, runner, validExtractor, &multiSigner{}, Config{SafeProxyURL: "socks5://127.0.0.1:1080"})
		response, err := service.ExtractMulti(context.Background(), request)
		assertMultiScrapeCode(t, err, models.ErrCodeTimeout)
		if response == nil || response.Sources[0].Status != models.MultiExtractSourceStatusTimeout || !response.UsageComplete {
			t.Fatalf("response = %#v", response)
		}
	})

	t.Run("fetch failure is sanitized", func(t *testing.T) {
		const privateError = "credential=private-secret path=/private/run"
		runner := &multiRunner{plans: map[string]multiRunnerPlan{
			"https://one.example.com/": {err: errors.New(privateError)},
		}}
		service := newMultiTestService(t, runner, validExtractor, &multiSigner{}, Config{SafeProxyURL: "socks5://127.0.0.1:1080"})
		response, err := service.ExtractMulti(context.Background(), request)
		assertMultiScrapeCode(t, err, models.ErrCodeNoValidSource)
		if response == nil || !response.UsageComplete {
			t.Fatalf("fetch failure response = %#v", response)
		}
		encoded, marshalErr := json.Marshal(response)
		if marshalErr != nil || strings.Contains(string(encoded), privateError) || strings.Contains(string(encoded), "private-secret") {
			t.Fatalf("response leaked private failure: %s, err=%v", encoded, marshalErr)
		}
	})
}

func TestExtractMultiPreservesStableFetchErrorClassification(t *testing.T) {
	tests := []struct {
		name          string
		sourceCode    string
		wantSource    string
		wantAggregate string
		wantStatus    models.MultiExtractSourceStatus
	}{
		{name: "authentication", sourceCode: models.ErrCodeUnauthorized, wantSource: models.ErrCodeUnauthorized, wantAggregate: models.ErrCodeLLMAuthFailure, wantStatus: models.MultiExtractSourceStatusFetchFailed},
		{name: "rate limit", sourceCode: models.ErrCodeRateLimited, wantSource: models.ErrCodeRateLimited, wantAggregate: models.ErrCodeLLMRateLimited, wantStatus: models.MultiExtractSourceStatusFetchFailed},
		{name: "internal", sourceCode: models.ErrCodeInternal, wantSource: models.ErrCodeInternal, wantAggregate: models.ErrCodeInternal, wantStatus: models.MultiExtractSourceStatusFetchFailed},
		{name: "fetch capability", sourceCode: models.ErrCodeInvalidInput, wantSource: models.ErrCodeMultiSourceUnavailable, wantAggregate: models.ErrCodeMultiSourceUnavailable, wantStatus: models.MultiExtractSourceStatusEvidenceUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const privateDetail = "private fetch credential and filesystem path"
			runner := &multiRunner{plans: map[string]multiRunnerPlan{
				"https://one.example.com/": {err: models.NewScrapeError(test.sourceCode, privateDetail, errors.New(privateDetail))},
			}}
			service := newMultiTestService(
				t,
				runner,
				&multiExtractor{plans: map[string]multiExtractorPlan{}},
				&multiSigner{},
				Config{SafeProxyURL: "socks5://127.0.0.1:1080"},
			)
			response, err := service.ExtractMulti(context.Background(), multiRequest(multiNameSchema(), "https://one.example.com"))
			assertMultiScrapeCode(t, err, test.wantAggregate)
			if response == nil || response.Error == nil || response.Error.Code != test.wantAggregate ||
				len(response.Sources) != 1 || response.Sources[0].Status != test.wantStatus ||
				response.Sources[0].Error == nil || response.Sources[0].Error.Code != test.wantSource ||
				!response.UsageComplete {
				t.Fatalf("response = %#v", response)
			}
			encoded, marshalErr := json.Marshal(response)
			if marshalErr != nil || strings.Contains(string(encoded), privateDetail) {
				t.Fatalf("fetch classification leaked private detail: %s, err=%v", encoded, marshalErr)
			}
		})
	}
}

func TestExtractMultiClassifiesNon2xxBeforeMissingEvidence(t *testing.T) {
	runner := &multiRunner{plans: map[string]multiRunnerPlan{
		"https://one.example.com/": {content: "upstream denied", statusCode: 503, missingSnapshot: true},
	}}
	service := newMultiTestService(
		t,
		runner,
		&multiExtractor{plans: map[string]multiExtractorPlan{}},
		&multiSigner{},
		Config{SafeProxyURL: "socks5://127.0.0.1:1080"},
	)
	response, err := service.ExtractMulti(context.Background(), multiRequest(multiNameSchema(), "https://one.example.com"))
	assertMultiScrapeCode(t, err, models.ErrCodeNoValidSource)
	if response == nil || len(response.Sources) != 1 ||
		response.Sources[0].Status != models.MultiExtractSourceStatusFetchFailed ||
		response.Sources[0].Error == nil || response.Sources[0].Error.Code != models.ErrCodeNavigation ||
		!response.UsageComplete {
		t.Fatalf("response = %#v", response)
	}
}

func TestValidateMultiSourceEvidenceRequiresFullyLocatedAnchors(t *testing.T) {
	for _, method := range []evidence.Method{
		evidence.MethodExact,
		evidence.MethodNormalized,
		evidence.MethodFuzzy,
		evidence.MethodCompiled,
	} {
		t.Run("accepts "+string(method), func(t *testing.T) {
			artifact, response := validMultiEvidenceFixture(method)
			if err := validateMultiSourceEvidence(artifact, response); err != nil {
				t.Fatalf("validateMultiSourceEvidence() error = %v", err)
			}
		})
	}
	t.Run("accepts equal fetched instant in another timezone", func(t *testing.T) {
		artifact, response := validMultiEvidenceFixture(evidence.MethodExact)
		artifact.Source.FetchedAt = artifact.Source.FetchedAt.In(time.FixedZone("offset", 8*60*60))
		if err := validateMultiSourceEvidence(artifact, response); err != nil {
			t.Fatalf("validateMultiSourceEvidence() error = %v", err)
		}
	})

	tests := []struct {
		name string
		edit func(*Artifact, *models.ExtractResponse)
	}{
		{name: "missing public artifact", edit: func(artifact *Artifact, _ *models.ExtractResponse) { artifact.Public = nil }},
		{name: "zero source fetched at", edit: func(artifact *Artifact, _ *models.ExtractResponse) { artifact.Source.FetchedAt = time.Time{} }},
		{name: "nil unlocated rate", edit: func(_ *Artifact, response *models.ExtractResponse) { response.UnlocatedRate = nil }},
		{name: "nonzero unlocated rate", edit: func(_ *Artifact, response *models.ExtractResponse) { value := 0.25; response.UnlocatedRate = &value }},
		{name: "negative unlocated rate", edit: func(_ *Artifact, response *models.ExtractResponse) { value := -0.1; response.UnlocatedRate = &value }},
		{name: "above one unlocated rate", edit: func(_ *Artifact, response *models.ExtractResponse) { value := 1.1; response.UnlocatedRate = &value }},
		{name: "NaN unlocated rate", edit: func(_ *Artifact, response *models.ExtractResponse) {
			value := math.NaN()
			response.UnlocatedRate = &value
		}},
		{name: "positive infinity unlocated rate", edit: func(_ *Artifact, response *models.ExtractResponse) {
			value := math.Inf(1)
			response.UnlocatedRate = &value
		}},
		{name: "negative infinity unlocated rate", edit: func(_ *Artifact, response *models.ExtractResponse) {
			value := math.Inf(-1)
			response.UnlocatedRate = &value
		}},
		{name: "unlocated method", edit: func(_ *Artifact, response *models.ExtractResponse) {
			(*response.Basis)["name"] = multiEvidenceAnchor(response, evidence.MethodUnlocated)
		}},
		{name: "unknown method", edit: func(_ *Artifact, response *models.ExtractResponse) {
			(*response.Basis)["name"] = multiEvidenceAnchor(response, evidence.Method("future"))
		}},
		{name: "compiled empty quote", edit: func(_ *Artifact, response *models.ExtractResponse) {
			anchor := multiEvidenceAnchor(response, evidence.MethodCompiled)
			anchor.Quote = ""
			(*response.Basis)["name"] = anchor
		}},
		{name: "compiled zero range", edit: func(_ *Artifact, response *models.ExtractResponse) {
			anchor := multiEvidenceAnchor(response, evidence.MethodCompiled)
			anchor.TextRange = [2]int{}
			(*response.Basis)["name"] = anchor
		}},
		{name: "compiled negative range", edit: func(_ *Artifact, response *models.ExtractResponse) {
			anchor := multiEvidenceAnchor(response, evidence.MethodCompiled)
			anchor.TextRange = [2]int{-1, 2}
			(*response.Basis)["name"] = anchor
		}},
		{name: "compiled out of range", edit: func(artifact *Artifact, response *models.ExtractResponse) {
			anchor := multiEvidenceAnchor(response, evidence.MethodCompiled)
			anchor.TextRange = [2]int{0, len(artifact.Public.Content) + 1}
			(*response.Basis)["name"] = anchor
		}},
		{name: "compiled mismatched range", edit: func(_ *Artifact, response *models.ExtractResponse) {
			anchor := multiEvidenceAnchor(response, evidence.MethodCompiled)
			anchor.Quote = "Bob"
			(*response.Basis)["name"] = anchor
		}},
		{name: "snapshot mismatch", edit: func(_ *Artifact, response *models.ExtractResponse) {
			anchor := multiEvidenceAnchor(response, evidence.MethodExact)
			anchor.SnapshotID = "sha256:other"
			(*response.Basis)["name"] = anchor
		}},
		{name: "zero fetched at", edit: func(_ *Artifact, response *models.ExtractResponse) {
			anchor := multiEvidenceAnchor(response, evidence.MethodExact)
			anchor.FetchedAt = time.Time{}
			(*response.Basis)["name"] = anchor
		}},
		{name: "mismatched fetched at", edit: func(_ *Artifact, response *models.ExtractResponse) {
			anchor := multiEvidenceAnchor(response, evidence.MethodExact)
			anchor.FetchedAt = anchor.FetchedAt.Add(time.Second)
			(*response.Basis)["name"] = anchor
		}},
		{name: "empty receipt", edit: func(_ *Artifact, response *models.ExtractResponse) { (*response.Receipts)["name"] = "" }},
	}
	for _, test := range tests {
		t.Run("rejects "+test.name, func(t *testing.T) {
			artifact, response := validMultiEvidenceFixture(evidence.MethodExact)
			test.edit(artifact, response)
			if err := validateMultiSourceEvidence(artifact, response); err == nil {
				t.Fatal("validateMultiSourceEvidence() succeeded")
			}
		})
	}
}

func TestExtractMultiExcludesUnlocatedEvidenceFromConsensusAgreement(t *testing.T) {
	runner := &multiRunner{plans: map[string]multiRunnerPlan{
		"https://located.example.com/":   {content: "Ada alpha orchard river copper violet"},
		"https://unlocated.example.net/": {content: "Bob zebra tundra quartz maple silver"},
	}}
	extractor := &multiExtractor{plans: map[string]multiExtractorPlan{
		"Ada alpha orchard river copper violet": {initial: json.RawMessage(`{"name":"Ada"}`)},
		"Bob zebra tundra quartz maple silver":  {initial: json.RawMessage(`{"name":"Ada"}`)},
	}}
	signer := &multiSigner{}
	service := newMultiTestService(t, runner, extractor, signer, Config{SafeProxyURL: "socks5://127.0.0.1:1080"})
	response, err := service.ExtractMulti(context.Background(), multiRequest(
		multiNameSchema(),
		"https://unlocated.example.net",
		"https://located.example.com",
	))
	if err != nil {
		t.Fatalf("ExtractMulti() error = %v", err)
	}
	if !response.Success || response.Status != models.MultiExtractStatusComplete || response.Consensus == nil ||
		len(response.Sources) != 2 || signer.calls() != 2 {
		t.Fatalf("response/signer calls = %#v/%d", response, signer.calls())
	}
	field := response.Consensus.Fields["name"]
	if field.Agreement != (models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1}) {
		t.Fatalf("agreement = %#v", field.Agreement)
	}
	statuses := make(map[string]models.MultiExtractSourceStatus, len(response.Sources))
	for _, source := range response.Sources {
		statuses[source.URL] = source.Status
	}
	if statuses["https://located.example.com/"] != models.MultiExtractSourceStatusValid ||
		statuses["https://unlocated.example.net/"] != models.MultiExtractSourceStatusEvidenceUnavailable {
		t.Fatalf("source statuses = %#v", statuses)
	}
}

func TestExtractMultiExcludesConsensusMetadataOverflowWithoutPoisoningGoodSources(t *testing.T) {
	oversizedQuote := strings.Repeat("q", consensus.MaxEvidenceQuoteBytes+1)
	oversizedData, err := json.Marshal(map[string]string{"name": oversizedQuote})
	if err != nil {
		t.Fatalf("marshal oversized quote data: %v", err)
	}
	runner := &multiRunner{plans: map[string]multiRunnerPlan{
		"https://good.example.com/": {content: "Ada alpha orchard river copper violet"},
		"https://bad.example.net/":  {content: oversizedQuote},
	}}
	extractor := &multiExtractor{plans: map[string]multiExtractorPlan{
		"Ada alpha orchard river copper violet": {initial: json.RawMessage(`{"name":"Ada"}`)},
		oversizedQuote:                          {initial: oversizedData},
	}}
	signer := &multiSigner{}
	service := newMultiTestService(t, runner, extractor, signer, Config{SafeProxyURL: "socks5://127.0.0.1:1080"})
	response, err := service.ExtractMulti(context.Background(), multiRequest(
		multiNameSchema(),
		"https://bad.example.net",
		"https://good.example.com",
	))
	if err != nil {
		t.Fatalf("ExtractMulti() error = %v", err)
	}
	if !response.Success || response.Status != models.MultiExtractStatusComplete || string(response.Data) != `{"name":"Ada"}` ||
		response.Consensus == nil || len(response.Sources) != 2 || signer.calls() != 2 {
		t.Fatalf("response/signer calls = %#v/%d", response, signer.calls())
	}
	field := response.Consensus.Fields["name"]
	if field.Agreement != (models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1}) {
		t.Fatalf("agreement = %#v", field.Agreement)
	}
	for _, source := range response.Sources {
		switch source.URL {
		case "https://good.example.com/":
			if !source.Success || source.Status != models.MultiExtractSourceStatusValid || source.Error != nil {
				t.Fatalf("good source = %#v", source)
			}
		case "https://bad.example.net/":
			if source.Success || source.Status != models.MultiExtractSourceStatusEvidenceUnavailable ||
				source.Error == nil || source.Error.Code != models.ErrCodeEvidenceUnavailable ||
				source.Error.Message != "source evidence is unavailable" {
				t.Fatalf("bad source = %#v", source)
			}
		default:
			t.Fatalf("unexpected source = %#v", source)
		}
	}
}

func TestExtractMultiPreservesStableSourceAndAggregateErrorClassification(t *testing.T) {
	tests := []struct {
		name          string
		sourceCode    string
		wantAggregate string
		wantMessage   string
	}{
		{name: "authentication", sourceCode: models.ErrCodeLLMAuthFailure, wantAggregate: models.ErrCodeLLMAuthFailure, wantMessage: "source authentication failed"},
		{name: "rate limit", sourceCode: models.ErrCodeLLMRateLimited, wantAggregate: models.ErrCodeLLMRateLimited, wantMessage: "source rate limit exceeded"},
		{name: "compiled unavailable", sourceCode: models.ErrCodeExtractorUnavailable, wantAggregate: models.ErrCodeExtractorUnavailable, wantMessage: "source extractor is unavailable"},
		{name: "internal", sourceCode: models.ErrCodeInternal, wantAggregate: models.ErrCodeInternal, wantMessage: "source extraction failed"},
		{name: "ordinary LLM failure", sourceCode: models.ErrCodeLLMFailure, wantAggregate: models.ErrCodeNoValidSource, wantMessage: "source extraction failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const privateDetail = "private provider credential and filesystem path"
			runner := &multiRunner{plans: map[string]multiRunnerPlan{
				"https://one.example.com/": {content: "Ada source"},
			}}
			extractor := &multiExtractor{plans: map[string]multiExtractorPlan{
				"Ada source": {err: models.NewScrapeError(test.sourceCode, privateDetail, errors.New(privateDetail))},
			}}
			service := newMultiTestService(t, runner, extractor, &multiSigner{}, Config{SafeProxyURL: "socks5://127.0.0.1:1080"})
			response, err := service.ExtractMulti(context.Background(), multiRequest(multiNameSchema(), "https://one.example.com"))
			assertMultiScrapeCode(t, err, test.wantAggregate)
			if response == nil || response.Error == nil || response.Error.Code != test.wantAggregate ||
				len(response.Sources) != 1 || response.Sources[0].Error == nil ||
				response.Sources[0].Error.Code != test.sourceCode || response.Sources[0].Error.Message != test.wantMessage ||
				response.UsageComplete {
				t.Fatalf("response = %#v", response)
			}
			encoded, marshalErr := json.Marshal(response)
			if marshalErr != nil || strings.Contains(string(encoded), privateDetail) {
				t.Fatalf("classification leaked private detail: %s, err=%v", encoded, marshalErr)
			}
		})
	}
}

func TestExtractMultiRejectsUnsafeProfilesTimeoutsAndDispatchBeforeFetch(t *testing.T) {
	runner := &multiRunner{plans: map[string]multiRunnerPlan{"https://one.example.com/": {content: "Ada source"}}}
	extractor := &multiExtractor{plans: map[string]multiExtractorPlan{"Ada source": {initial: json.RawMessage(`{"name":"Ada"}`)}}}
	service := newMultiTestService(t, runner, extractor, &multiSigner{}, Config{SafeProxyURL: "socks5://127.0.0.1:1080"})
	base := multiRequest(multiNameSchema(), "https://one.example.com")
	tests := []struct {
		name string
		edit func(*models.ExtractRequest)
		code string
	}{
		{name: "url and sources", edit: func(r *models.ExtractRequest) { r.URL = "https://other.example.com" }, code: models.ErrCodeInvalidInput},
		{name: "caller proxy", edit: func(r *models.ExtractRequest) { r.ProxyURL = "socks5://127.0.0.1:9999" }, code: models.ErrCodeInvalidInput},
		{name: "selector", edit: func(r *models.ExtractRequest) { r.CSSSelector = "main" }, code: models.ErrCodeInvalidInput},
		{name: "output", edit: func(r *models.ExtractRequest) { r.OutputFormat = "text" }, code: models.ErrCodeInvalidInput},
		{name: "mode", edit: func(r *models.ExtractRequest) { r.ExtractMode = "raw" }, code: models.ErrCodeInvalidInput},
		{name: "timeout negative", edit: func(r *models.ExtractRequest) { r.Timeout = -1 }, code: models.ErrCodeInvalidInput},
		{name: "timeout too large", edit: func(r *models.ExtractRequest) { r.Timeout = 121 }, code: models.ErrCodeInvalidInput},
		{name: "private literal source", edit: func(r *models.ExtractRequest) { r.Sources = []string{"http://127.0.0.1"} }, code: models.ErrCodeInvalidInput},
		{name: "llm key missing", edit: func(r *models.ExtractRequest) { r.LLMAPIKey = "" }, code: models.ErrCodeInvalidInput},
		{name: "compiled unavailable", edit: func(r *models.ExtractRequest) { r.Engine = "compiled"; r.LLMAPIKey = "" }, code: models.ErrCodeExtractorUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := cloneMultiRequest(base)
			test.edit(request)
			_, err := service.ExtractMulti(context.Background(), request)
			assertMultiScrapeCode(t, err, test.code)
		})
	}
	_, calls, _ := runner.snapshot()
	if calls != 0 {
		t.Fatalf("invalid/unsupported requests fetched %d sources", calls)
	}
}

func TestExtractMultiGlobalSlotsNeverExceedFourAcrossRequests(t *testing.T) {
	plans := make(map[string]multiRunnerPlan)
	extractorPlans := make(map[string]multiExtractorPlan)
	urls := make([]string, 4)
	for index := range urls {
		urls[index] = "https://source" + string(rune('a'+index)) + ".example.com/"
		content := "Ada shared source " + string(rune('a'+index))
		plans[urls[index]] = multiRunnerPlan{content: content, delay: 30 * time.Millisecond}
		extractorPlans[content] = multiExtractorPlan{initial: json.RawMessage(`{"name":"Ada"}`)}
	}
	runner := &multiRunner{plans: plans}
	service := newMultiTestService(
		t,
		runner,
		&multiExtractor{plans: extractorPlans},
		&multiSigner{},
		Config{SafeProxyURL: "socks5://127.0.0.1:1080"},
	)
	start := make(chan struct{})
	errorsByRequest := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := service.ExtractMulti(context.Background(), multiRequest(multiNameSchema(), urls...))
			errorsByRequest <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-errorsByRequest; err != nil {
			t.Fatalf("ExtractMulti() error = %v", err)
		}
	}
	_, calls, maxActive := runner.snapshot()
	if calls != 8 || maxActive != defaultMultiSourceSlots {
		t.Fatalf("calls/max-active = %d/%d, want 8/%d", calls, maxActive, defaultMultiSourceSlots)
	}
}

func TestExtractMultiDeadlineBeforeAndDuringMergeReturnsSummaries(t *testing.T) {
	newService := func(t *testing.T) *Service {
		t.Helper()
		return newMultiTestService(
			t,
			&multiRunner{plans: map[string]multiRunnerPlan{
				"https://one.example.com/": {content: "Ada source"},
			}},
			&multiExtractor{plans: map[string]multiExtractorPlan{
				"Ada source": {initial: json.RawMessage(`{"name":"Ada"}`)},
			}},
			&multiSigner{},
			Config{SafeProxyURL: "socks5://127.0.0.1:1080"},
		)
	}
	assertTimeoutResponse := func(t *testing.T, response *models.MultiExtractResponse, err error) {
		t.Helper()
		assertMultiScrapeCode(t, err, models.ErrCodeTimeout)
		if response == nil || response.Success || response.Error == nil || response.Error.Code != models.ErrCodeTimeout ||
			len(response.Sources) != 1 || response.Sources[0].Status != models.MultiExtractSourceStatusValid ||
			response.Sources[0].SnapshotID == "" || len(response.Data) != 0 || response.Consensus != nil {
			t.Fatalf("timeout response = %#v", response)
		}
	}

	t.Run("canceled before merge", func(t *testing.T) {
		service := newService(t)
		ctx, cancel := context.WithCancel(context.Background())
		service.beforeMultiMerge = cancel
		response, err := service.ExtractMulti(ctx, multiRequest(multiNameSchema(), "https://one.example.com"))
		assertTimeoutResponse(t, response, err)
	})

	t.Run("deadline during uncancelable merge", func(t *testing.T) {
		service := newService(t)
		service.mergeMulti = func(results []consensus.SourceResult) (consensus.Result, consensus.Materialization, error) {
			time.Sleep(30 * time.Millisecond)
			return consensus.MergeWithMaterialization(results)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		response, err := service.ExtractMulti(ctx, multiRequest(multiNameSchema(), "https://one.example.com"))
		assertTimeoutResponse(t, response, err)
	})
}

func TestExtractMultiMergeStageSharesFourGlobalSlots(t *testing.T) {
	plans := make(map[string]multiRunnerPlan, 5)
	extractorPlans := make(map[string]multiExtractorPlan, 5)
	urls := make([]string, 5)
	for index := range urls {
		urls[index] = "https://merge" + string(rune('a'+index)) + ".example.com/"
		content := "Ada merge source " + string(rune('a'+index))
		plans[urls[index]] = multiRunnerPlan{content: content}
		extractorPlans[content] = multiExtractorPlan{initial: json.RawMessage(`{"name":"Ada"}`)}
	}
	service := newMultiTestService(
		t,
		&multiRunner{plans: plans},
		&multiExtractor{plans: extractorPlans},
		&multiSigner{},
		Config{SafeProxyURL: "socks5://127.0.0.1:1080"},
	)
	entered := make(chan struct{}, len(urls))
	release := make(chan struct{})
	service.mergeMulti = func(results []consensus.SourceResult) (consensus.Result, consensus.Materialization, error) {
		entered <- struct{}{}
		<-release
		return consensus.MergeWithMaterialization(results)
	}
	errorsByRequest := make(chan error, len(urls))
	for _, sourceURL := range urls {
		sourceURL := sourceURL
		go func() {
			_, err := service.ExtractMulti(context.Background(), multiRequest(multiNameSchema(), sourceURL))
			errorsByRequest <- err
		}()
	}
	for range defaultMultiSourceSlots {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("four merge stages did not enter")
		}
	}
	select {
	case <-entered:
		t.Fatal("fifth merge entered before a global slot was released")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	for range urls {
		if err := <-errorsByRequest; err != nil {
			t.Fatalf("ExtractMulti() error = %v", err)
		}
	}
}

func TestMultiConfigurationRejectsNonLoopbackProxyAndMoreThanFourSlots(t *testing.T) {
	valid := map[string]string{
		"socks5://127.0.0.1:1080": "socks5://127.0.0.1:1080",
		"socks5://[::1]:1080":     "socks5://[::1]:1080",
	}
	for raw, want := range valid {
		if got, err := normalizeMultiSafeProxyURL(raw); err != nil || got != want {
			t.Fatalf("normalizeMultiSafeProxyURL(%q) = %q, %v", raw, got, err)
		}
	}
	invalid := []string{
		"http://127.0.0.1:1080",
		"socks5://localhost:1080",
		"socks5://8.8.8.8:1080",
		"socks5://user:pass@127.0.0.1:1080",
		"socks5://127.0.0.1:1080/path",
		"socks5://127.0.0.1:1080?q=1",
		"socks5://127.0.0.1",
	}
	for _, raw := range invalid {
		if _, err := normalizeMultiSafeProxyURL(raw); err == nil {
			t.Fatalf("normalizeMultiSafeProxyURL(%q) succeeded", raw)
		}
	}
	if got, err := normalizeMultiSourceSlots(0); err != nil || got != 4 {
		t.Fatalf("normalizeMultiSourceSlots(0) = %d, %v", got, err)
	}
	if _, err := normalizeMultiSourceSlots(5); err == nil {
		t.Fatal("normalizeMultiSourceSlots(5) succeeded")
	}
}

func TestMultiMetricsRejectInconsistentValuesAndOverflow(t *testing.T) {
	if err := validateMultiSourceMetrics(models.MultiExtractSource{
		Tokens: models.TokenInfo{OriginalEstimate: 1, CleanedEstimate: 2},
	}); err == nil {
		t.Fatal("cleaned tokens above original were accepted")
	}
	if err := validateMultiSourceMetrics(models.MultiExtractSource{
		LLMUsage: &models.LLMUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 3},
	}); err == nil {
		t.Fatal("inconsistent LLM usage was accepted")
	}
	if _, _, _, _, err := aggregateMultiMetrics([]multiSourceOutcome{
		{summary: models.MultiExtractSource{Tokens: models.TokenInfo{OriginalEstimate: math.MaxInt}}},
		{summary: models.MultiExtractSource{Tokens: models.TokenInfo{OriginalEstimate: 1}}},
	}); err == nil {
		t.Fatal("token overflow was accepted")
	}
}

func TestStructuredExtractDataPreflightMatchesConsensusBounds(t *testing.T) {
	exactPrefix := `{"value":"`
	exactSuffix := `"}`
	exact := json.RawMessage(exactPrefix + strings.Repeat("x", maximumExtractArtifactBytes-len(exactPrefix)-len(exactSuffix)) + exactSuffix)
	tests := []struct {
		name    string
		data    json.RawMessage
		wantErr bool
	}{
		{name: "exact four MiB", data: exact},
		{name: "four MiB plus one", data: append(append(json.RawMessage(nil), exact...), ' '), wantErr: true},
		{name: "duplicate key", data: json.RawMessage(`{"value":1,"value":2}`), wantErr: true},
		{name: "depth", data: nestedMultiJSON(consensus.MaxJSONDepth + 1), wantErr: true},
		{name: "nodes", data: multiNestedZeroArray(10_001), wantErr: true},
		{name: "leaves", data: multiZeroArray(consensus.MaxLeavesPerSource + 1), wantErr: true},
		{name: "path", data: json.RawMessage(`{"` + strings.Repeat("p", consensus.MaxPathBytes+1) + `":1}`), wantErr: true},
		{name: "malformed", data: json.RawMessage(`{"value":`), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateStructuredExtractData(test.data)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateStructuredExtractData() error = %v, wantErr=%v", err, test.wantErr)
			}
		})
	}
}

func TestExtractAndExtractArtifactDirectCallsEnforceSingleTargetXOR(t *testing.T) {
	runner := &multiRunner{plans: map[string]multiRunnerPlan{
		"https://one.example.com/": {content: "Ada source"},
	}}
	extractor := &multiExtractor{plans: map[string]multiExtractorPlan{
		"Ada source": {initial: json.RawMessage(`{"name":"Ada"}`)},
	}}
	service := newMultiTestService(t, runner, extractor, &multiSigner{}, Config{})
	result := successfulScrapeResult()
	artifact := &Artifact{Public: result.Response, Source: result.Source}
	tests := []struct {
		name    string
		request *models.ExtractRequest
	}{
		{name: "both", request: &models.ExtractRequest{URL: "https://one.example.com", Sources: []string{"https://two.example.net"}, Schema: multiNameSchema(), Engine: "llm", LLMAPIKey: "secret"}},
		{name: "sources only", request: &models.ExtractRequest{Sources: []string{"https://one.example.com"}, Schema: multiNameSchema(), Engine: "llm", LLMAPIKey: "secret"}},
		{name: "neither", request: &models.ExtractRequest{Schema: multiNameSchema(), Engine: "llm", LLMAPIKey: "secret"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := service.Extract(context.Background(), test.request)
			assertMultiScrapeCode(t, err, models.ErrCodeInvalidInput)
			_, err = service.ExtractArtifact(context.Background(), artifact, test.request)
			assertMultiScrapeCode(t, err, models.ErrCodeInvalidInput)
		})
	}
	_, calls, _ := runner.snapshot()
	_, extractCalls, repairCalls := extractor.snapshot()
	if calls != 0 || extractCalls != 0 || repairCalls != 0 {
		t.Fatalf("invalid direct calls reached dependencies: fetch/extract/repair=%d/%d/%d", calls, extractCalls, repairCalls)
	}
}

func TestExtractArtifactRejectsOversizedArtifactBeforeExtraction(t *testing.T) {
	for _, test := range []struct {
		name       string
		contentLen int
		rawLen     int
		wantErr    bool
	}{
		{name: "cleaned exact", contentLen: maximumExtractArtifactBytes, rawLen: 16},
		{name: "raw exact", contentLen: 16, rawLen: maximumExtractArtifactBytes},
		{name: "cleaned plus one", contentLen: maximumExtractArtifactBytes + 1, rawLen: 16, wantErr: true},
		{name: "raw plus one", contentLen: 16, rawLen: maximumExtractArtifactBytes + 1, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := successfulScrapeResult()
			result.Response.Content = strings.Repeat("c", test.contentLen)
			result.Source.RawHTML = strings.Repeat("r", test.rawLen)
			client := &recordingExtractor{initial: &llm.ExtractResult{Data: json.RawMessage(`{"count":3}`)}}
			service := newTestService(t, &recordingRunner{}, client, nil)
			response, err := service.ExtractArtifact(context.Background(), &Artifact{Public: result.Response, Source: result.Source}, validExtractRequest())
			if test.wantErr {
				assertMultiScrapeCode(t, err, models.ErrCodeNavigation)
				if response != nil || client.extractCalls != 0 {
					t.Fatalf("oversized artifact response/calls = %#v/%d", response, client.extractCalls)
				}
				return
			}
			if err != nil || response == nil || !response.Success || client.extractCalls != 1 {
				t.Fatalf("exact-boundary response/error/calls = %#v/%v/%d", response, err, client.extractCalls)
			}
		})
	}
}

func TestExtractArtifactRejectsStructurallyInvalidLLMDataBeforeEvidence(t *testing.T) {
	tests := []struct {
		name        string
		initial     json.RawMessage
		repaired    json.RawMessage
		wantExtract int
		wantRepair  int
	}{
		{
			name:        "initial output",
			initial:     json.RawMessage(`{"count":1,"count":2}`),
			wantExtract: 1,
		},
		{
			name:        "repair output",
			initial:     json.RawMessage(`{"count":"invalid"}`),
			repaired:    nestedMultiJSON(consensus.MaxJSONDepth + 1),
			wantExtract: 1,
			wantRepair:  1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := successfulScrapeResult()
			client := &recordingExtractor{
				initial:  &llm.ExtractResult{Data: test.initial},
				repaired: &llm.ExtractResult{Data: test.repaired},
			}
			signer := &recordingSigner{}
			service := newTestService(t, &recordingRunner{}, client, signer)
			request := validExtractRequest()
			request.Evidence = true
			response, err := service.ExtractArtifact(
				context.Background(),
				&Artifact{Public: result.Response, Source: result.Source},
				request,
			)
			assertMultiScrapeCode(t, err, models.ErrCodeLLMFailure)
			if response != nil || client.extractCalls != test.wantExtract || client.repairCalls != test.wantRepair || len(signer.payloads) != 0 {
				t.Fatalf("response/extract/repair/sign = %#v/%d/%d/%d", response, client.extractCalls, client.repairCalls, len(signer.payloads))
			}
		})
	}
}

func TestEncodeMultiResponseSharesGlobalFourSlots(t *testing.T) {
	service := newMultiTestService(
		t,
		&multiRunner{plans: map[string]multiRunnerPlan{}},
		&multiExtractor{plans: map[string]multiExtractorPlan{}},
		&multiSigner{},
		Config{},
	)
	response := &models.MultiExtractResponse{Success: true, Sources: []models.MultiExtractSource{}, UsageComplete: true}
	want, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	got, err := service.EncodeMultiResponse(context.Background(), response)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("EncodeMultiResponse() = %s, %v; want %s", got, err, want)
	}

	entered := make(chan struct{}, 5)
	release := make(chan struct{})
	service.encodeMulti = func(value any) ([]byte, error) {
		entered <- struct{}{}
		<-release
		return json.Marshal(value)
	}
	errorsByCall := make(chan error, 5)
	for range 5 {
		go func() {
			_, err := service.EncodeMultiResponse(context.Background(), response)
			errorsByCall <- err
		}()
	}
	for range defaultMultiSourceSlots {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("four encoders did not enter")
		}
	}
	select {
	case <-entered:
		t.Fatal("fifth encoder entered before a shared slot was released")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	for range 5 {
		if err := <-errorsByCall; err != nil {
			t.Fatalf("EncodeMultiResponse() error = %v", err)
		}
	}
}

func TestEncodeMultiResponsePrefersDeadlineOverEncodingFailure(t *testing.T) {
	service := newMultiTestService(
		t,
		&multiRunner{plans: map[string]multiRunnerPlan{}},
		&multiExtractor{plans: map[string]multiExtractorPlan{}},
		&multiSigner{},
		Config{},
	)
	ctx, cancel := context.WithCancel(context.Background())
	service.encodeMulti = func(any) ([]byte, error) {
		cancel()
		return nil, errors.New("private encoding failure")
	}
	_, err := service.EncodeMultiResponse(ctx, &models.MultiExtractResponse{Sources: []models.MultiExtractSource{}})
	assertMultiScrapeCode(t, err, models.ErrCodeTimeout)
}

func TestEncodeMultiResponseExactWholeResponseBudget(t *testing.T) {
	service := newMultiTestService(
		t,
		&multiRunner{plans: map[string]multiRunnerPlan{}},
		&multiExtractor{plans: map[string]multiExtractorPlan{}},
		&multiSigner{},
		Config{},
	)

	t.Run("exact limit succeeds", func(t *testing.T) {
		response := exactSizedMultiResponse(t, models.MaxMultiExtractResponseBytes)
		encoded, err := service.EncodeMultiResponse(context.Background(), response)
		if err != nil || len(encoded) != models.MaxMultiExtractResponseBytes {
			t.Fatalf("EncodeMultiResponse() bytes/error = %d/%v", len(encoded), err)
		}
	})

	t.Run("one byte over fails without output", func(t *testing.T) {
		response := exactSizedMultiResponse(t, models.MaxMultiExtractResponseBytes+1)
		encoded, err := service.EncodeMultiResponse(context.Background(), response)
		if encoded != nil {
			t.Fatalf("EncodeMultiResponse() returned %d partial bytes", len(encoded))
		}
		assertMultiScrapeCode(t, err, models.ErrCodeInternal)
		if !strings.Contains(err.Error(), "output budget") {
			t.Fatalf("EncodeMultiResponse() error = %v", err)
		}
	})
}

func TestValidateMultiResponseSizeAccountsForFoldReasons(t *testing.T) {
	for _, test := range []struct {
		name    string
		size    int
		wantErr bool
	}{
		{name: "exact limit", size: models.MaxMultiExtractResponseBytes},
		{name: "one byte over", size: models.MaxMultiExtractResponseBytes + 1, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := exactSizedMultiResponseWithFoldReasons(t, test.size)
			encoded, err := json.Marshal(response)
			if err != nil || len(encoded) != test.size {
				t.Fatalf("Marshal() bytes/error = %d/%v, want %d/nil", len(encoded), err, test.size)
			}
			err = validateMultiResponseSize(response)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateMultiResponseSize() error = %v, wantErr=%t", err, test.wantErr)
			}
		})
	}
}

func TestSignFieldReceiptsFailsBeforeUnboundedMaterialization(t *testing.T) {
	t.Run("leaf count", func(t *testing.T) {
		document := make(map[string]int, 10_001)
		for index := 0; index <= 10_000; index++ {
			document["field"+strconv.Itoa(index)] = index
		}
		data, err := json.Marshal(document)
		if err != nil {
			t.Fatalf("marshal leaf fixture: %v", err)
		}
		_, err = signFieldReceipts(data, nil, "https://example.com/", time.Now(), &multiSigner{})
		if err == nil || !strings.Contains(err.Error(), "leaves exceed") {
			t.Fatalf("signFieldReceipts() error = %v", err)
		}
	})

	t.Run("single receipt", func(t *testing.T) {
		data := json.RawMessage(`{"value":"Ada"}`)
		basis := models.EvidenceBasis{"value": {SnapshotID: "sha256:test", FetchedAt: time.Now()}}
		signer := &multiSigner{token: strings.Repeat("r", (2<<20)+1)}
		if _, err := signFieldReceipts(data, basis, "https://example.com/", time.Now(), signer); err == nil ||
			!strings.Contains(err.Error(), "receipt") {
			t.Fatalf("signFieldReceipts() error = %v", err)
		}
	})

	t.Run("aggregate evidence", func(t *testing.T) {
		document := make(map[string]string, 17)
		basis := make(models.EvidenceBasis, 17)
		for index := 0; index < 17; index++ {
			path := "field" + string(rune('a'+index))
			document[path] = "Ada"
			basis[path] = evidence.Anchor{SnapshotID: "sha256:test", FetchedAt: time.Now()}
		}
		data, err := json.Marshal(document)
		if err != nil {
			t.Fatalf("marshal evidence fixture: %v", err)
		}
		signer := &multiSigner{token: strings.Repeat("r", 2<<20)}
		if _, err := signFieldReceipts(data, basis, "https://example.com/", time.Now(), signer); err == nil ||
			!strings.Contains(err.Error(), "metadata exceeds") {
			t.Fatalf("signFieldReceipts() error = %v", err)
		}
		if signer.calls() >= len(document) {
			t.Fatalf("signer calls = %d, wanted fail-fast before all %d receipts", signer.calls(), len(document))
		}
	})
}

type multiRunnerPlan struct {
	content         string
	finalURL        string
	delay           time.Duration
	err             error
	missingSnapshot bool
	statusCode      int
	fetchedAt       time.Time
}

type multiRunner struct {
	mu        sync.Mutex
	plans     map[string]multiRunnerPlan
	requests  []*models.ScrapeRequest
	calls     int
	active    int
	maxActive int
}

func (runner *multiRunner) Run(ctx context.Context, request *models.ScrapeRequest, _ scrape.Observer) (*scrape.Result, error) {
	cloned := cloneMultiScrapeRequest(request)
	runner.mu.Lock()
	runner.calls++
	runner.requests = append(runner.requests, cloned)
	runner.active++
	if runner.active > runner.maxActive {
		runner.maxActive = runner.active
	}
	plan, ok := runner.plans[request.URL]
	runner.mu.Unlock()
	defer func() {
		runner.mu.Lock()
		runner.active--
		runner.mu.Unlock()
	}()
	if !ok {
		return nil, errors.New("private missing runner plan")
	}
	if plan.delay > 0 {
		timer := time.NewTimer(plan.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if plan.err != nil {
		return nil, plan.err
	}
	finalURL := plan.finalURL
	if finalURL == "" {
		finalURL = request.URL
	}
	statusCode := plan.statusCode
	if statusCode == 0 {
		statusCode = 200
	}
	snapshotID := snapshot.ID("sha256:" + strings.Repeat("a", 64))
	if plan.missingSnapshot {
		snapshotID = ""
	}
	fetchedAt := plan.fetchedAt
	if fetchedAt.IsZero() {
		fetchedAt = time.Date(2026, time.August, 9, 8, 0, 0, 0, time.UTC)
	}
	return &scrape.Result{
		Response: &models.ScrapeResponse{
			Success: true,
			Content: plan.content,
			Tokens:  models.TokenInfo{OriginalEstimate: 10, CleanedEstimate: 5, SavingsPercent: 50},
			Timing:  models.TimingInfo{NavigationMs: 4, CleaningMs: 2, TotalMs: 6},
		},
		Source: &scraper.ScrapeResult{
			RawHTML:    "<html><body>" + plan.content + "</body></html>",
			FinalURL:   finalURL,
			SnapshotID: snapshotID,
			FetchedAt:  fetchedAt,
			StatusCode: statusCode,
		},
	}, nil
}

func (runner *multiRunner) snapshot() ([]*models.ScrapeRequest, int, int) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	requests := make([]*models.ScrapeRequest, len(runner.requests))
	for index, request := range runner.requests {
		requests[index] = cloneMultiScrapeRequest(request)
	}
	return requests, runner.calls, runner.maxActive
}

type multiExtractorPlan struct {
	initial json.RawMessage
	repair  json.RawMessage
	err     error
}

type multiExtractor struct {
	mu           sync.Mutex
	plans        map[string]multiExtractorPlan
	params       []llm.ExtractParams
	extractCalls int
	repairCalls  int
}

func (extractor *multiExtractor) Extract(_ context.Context, content string, _ json.RawMessage, params llm.ExtractParams) (*llm.ExtractResult, error) {
	extractor.mu.Lock()
	defer extractor.mu.Unlock()
	extractor.extractCalls++
	extractor.params = append(extractor.params, params)
	plan, ok := extractor.plans[content]
	if !ok {
		return nil, errors.New("private missing extractor plan")
	}
	if plan.err != nil {
		return nil, plan.err
	}
	return &llm.ExtractResult{
		Data:  append(json.RawMessage(nil), plan.initial...),
		Usage: &models.LLMUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}, nil
}

func (extractor *multiExtractor) ExtractWithRepair(_ context.Context, content string, _ json.RawMessage, _ json.RawMessage, _ []llm.Violation, params llm.ExtractParams) (*llm.ExtractResult, error) {
	extractor.mu.Lock()
	defer extractor.mu.Unlock()
	extractor.repairCalls++
	extractor.params = append(extractor.params, params)
	plan, ok := extractor.plans[content]
	if !ok || len(plan.repair) == 0 {
		return nil, errors.New("private missing repair plan")
	}
	return &llm.ExtractResult{Data: append(json.RawMessage(nil), plan.repair...)}, nil
}

func (extractor *multiExtractor) snapshot() ([]llm.ExtractParams, int, int) {
	extractor.mu.Lock()
	defer extractor.mu.Unlock()
	return append([]llm.ExtractParams(nil), extractor.params...), extractor.extractCalls, extractor.repairCalls
}

type multiSigner struct {
	mu      sync.Mutex
	count   int
	token   string
	err     error
	payload []receipts.Payload
}

func (signer *multiSigner) Sign(payload receipts.Payload) (string, error) {
	signer.mu.Lock()
	defer signer.mu.Unlock()
	signer.count++
	signer.payload = append(signer.payload, payload)
	if signer.err != nil {
		return "", signer.err
	}
	if signer.token != "" {
		return signer.token, nil
	}
	return "signed:" + payload.Path, nil
}

func (signer *multiSigner) calls() int {
	signer.mu.Lock()
	defer signer.mu.Unlock()
	return signer.count
}

func newMultiTestService(t *testing.T, runner Runner, extractor StructuredExtractor, signer ReceiptSigner, config Config) *Service {
	t.Helper()
	service, err := NewService(runner, extractor, signer, config)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return service
}

func multiRequest(schema json.RawMessage, urls ...string) *models.ExtractRequest {
	return &models.ExtractRequest{
		Sources:   append([]string(nil), urls...),
		Schema:    append(json.RawMessage(nil), schema...),
		Engine:    "llm",
		LLMAPIKey: "byok-secret",
		Timeout:   2,
	}
}

func multiNameSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}`)
}

func multiCountSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"],"additionalProperties":false}`)
}

func validMultiEvidenceFixture(method evidence.Method) (*Artifact, *models.ExtractResponse) {
	content := "prefix Ada suffix"
	start := strings.Index(content, "Ada")
	fetchedAt := time.Date(2026, time.August, 9, 8, 0, 0, 0, time.UTC)
	snapshotID := snapshot.ID("sha256:" + strings.Repeat("e", 64))
	rate := 0.0
	basis := models.EvidenceBasis{
		"name": {
			Quote:      "Ada",
			TextRange:  [2]int{start, start + len("Ada")},
			Method:     method,
			SnapshotID: string(snapshotID),
			FetchedAt:  fetchedAt,
		},
	}
	receiptTokens := models.FieldReceipts{"name": "signed:name"}
	return &Artifact{
			Public: &models.ScrapeResponse{Success: true, Content: content},
			Source: &scraper.ScrapeResult{
				RawHTML:    "<html><body>" + content + "</body></html>",
				FinalURL:   "https://example.com/",
				SnapshotID: snapshotID,
				FetchedAt:  fetchedAt,
				StatusCode: 200,
			},
		}, &models.ExtractResponse{
			Success:       true,
			Data:          json.RawMessage(`{"name":"Ada"}`),
			SnapshotID:    string(snapshotID),
			UnlocatedRate: &rate,
			Basis:         &basis,
			Receipts:      &receiptTokens,
		}
}

func multiEvidenceAnchor(response *models.ExtractResponse, method evidence.Method) evidence.Anchor {
	anchor := (*response.Basis)["name"]
	anchor.Method = method
	return anchor
}

func exactSizedMultiResponse(t *testing.T, size int) *models.MultiExtractResponse {
	t.Helper()
	response := &models.MultiExtractResponse{
		Success:       false,
		Sources:       []models.MultiExtractSource{},
		UsageComplete: true,
		Error: &models.ErrorDetail{
			Code: models.ErrCodeInternal,
		},
	}
	baseline, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal multi response baseline: %v", err)
	}
	fillerBytes := size - len(baseline)
	if fillerBytes < 0 {
		t.Fatalf("response size %d is below baseline %d", size, len(baseline))
	}
	response.Error.Message = strings.Repeat("x", fillerBytes)
	return response
}

func exactSizedMultiResponseWithFoldReasons(t *testing.T, size int) *models.MultiExtractResponse {
	t.Helper()
	response := &models.MultiExtractResponse{
		Success: false,
		Consensus: &models.MultiExtractConsensus{Fields: map[string]models.MultiExtractFieldConsensus{
			"name": {
				Agreement: models.MultiExtractAgreement{
					Pages:            3,
					IndependentRoots: 1,
					FoldReason:       models.MultiExtractFoldReasonSameRoot,
				},
				Supports: []models.MultiExtractSupport{{
					URL:        "https://b.example/",
					Root:       "b.example",
					FoldReason: models.MultiExtractFoldReasonNearDuplicate,
				}},
				Conflicts: []models.MultiExtractConflict{{
					Value: json.RawMessage(`"Grace"`),
					Agreement: models.MultiExtractAgreement{
						Pages:            2,
						IndependentRoots: 1,
						FoldReason:       models.MultiExtractFoldReasonQuoteLineage,
					},
				}},
			},
		}},
		Sources:       []models.MultiExtractSource{},
		UsageComplete: true,
		Error:         &models.ErrorDetail{Code: models.ErrCodeInternal},
	}
	baseline, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal fold-reason response baseline: %v", err)
	}
	fillerBytes := size - len(baseline)
	if fillerBytes < 0 {
		t.Fatalf("response size %d is below baseline %d", size, len(baseline))
	}
	response.Error.Message = strings.Repeat("x", fillerBytes)
	return response
}

func multiNumberedWords(prefix string, count int) string {
	var builder strings.Builder
	for index := range count {
		builder.WriteString(prefix)
		builder.WriteByte('-')
		builder.WriteString(strconv.Itoa(index))
		builder.WriteByte(' ')
	}
	return builder.String()
}

func nestedMultiJSON(depth int) json.RawMessage {
	return json.RawMessage(strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth))
}

func multiNestedZeroArray(count int) json.RawMessage {
	var builder strings.Builder
	builder.Grow(count*4 + 2)
	builder.WriteByte('[')
	for index := 0; index < count; index++ {
		if index > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString("[0]")
	}
	builder.WriteByte(']')
	return json.RawMessage(builder.String())
}

func multiZeroArray(count int) json.RawMessage {
	var builder strings.Builder
	builder.Grow(count*2 + 1)
	builder.WriteByte('[')
	for index := 0; index < count; index++ {
		if index > 0 {
			builder.WriteByte(',')
		}
		builder.WriteByte('0')
	}
	builder.WriteByte(']')
	return json.RawMessage(builder.String())
}

func cloneMultiRequest(input *models.ExtractRequest) *models.ExtractRequest {
	if input == nil {
		return nil
	}
	output := *input
	output.Sources = append([]string(nil), input.Sources...)
	output.Schema = append(json.RawMessage(nil), input.Schema...)
	if input.WaitForNetworkIdle != nil {
		value := *input.WaitForNetworkIdle
		output.WaitForNetworkIdle = &value
	}
	return &output
}

func cloneMultiScrapeRequest(input *models.ScrapeRequest) *models.ScrapeRequest {
	if input == nil {
		return nil
	}
	output := *input
	models.ApplyScrapeOptions(&output, models.ScrapeOptionsFromRequest(input))
	return &output
}

func assertMultiScrapeCode(t *testing.T, err error, want string) {
	t.Helper()
	var scrapeError *models.ScrapeError
	if !errors.As(err, &scrapeError) || scrapeError.Code != want {
		t.Fatalf("error = %v, want ScrapeError code %q", err, want)
	}
}
