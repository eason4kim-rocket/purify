package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/use-agent/purify/config"
	"github.com/use-agent/purify/llm"
	"github.com/use-agent/purify/verify/eav"
)

type eavExtractorStub struct {
	calls   int
	content string
	schema  json.RawMessage
	reply   string
	err     error
}

func (stub *eavExtractorStub) Extract(
	_ context.Context,
	content string,
	schema json.RawMessage,
	_ llm.ExtractParams,
) (*llm.ExtractResult, error) {
	stub.calls++
	stub.content = content
	stub.schema = schema
	if stub.err != nil {
		return nil, stub.err
	}
	return &llm.ExtractResult{Data: json.RawMessage(stub.reply)}, nil
}

func (stub *eavExtractorStub) ExtractWithRepair(
	_ context.Context,
	_ string,
	_ json.RawMessage,
	_ json.RawMessage,
	_ []llm.Violation,
	_ llm.ExtractParams,
) (*llm.ExtractResult, error) {
	return nil, errors.New("repair is not part of the eav adapter")
}

func validEAVConfig() config.EAVConfig {
	return config.EAVConfig{
		Enabled:        true,
		RefereeEnabled: true,
		CacheEntries:   4,
		APIKey:         "key",
		Model:          "gpt-4o-mini",
		BaseURL:        "https://api.openai.com/v1",
	}
}

func TestNewManagedSourceJudge(t *testing.T) {
	if judge, err := newManagedSourceJudge(config.EAVConfig{}, &eavExtractorStub{}); err != nil || judge != nil {
		t.Fatalf("disabled configuration must yield a nil judge, got (%v, %v)", judge, err)
	}
	missingKey := validEAVConfig()
	missingKey.APIKey = "   "
	if _, err := newManagedSourceJudge(missingKey, &eavExtractorStub{}); !errors.Is(err, config.ErrInvalidEAVConfig) {
		t.Fatalf("a missing credential must fail validation, got %v", err)
	}
	if _, err := newManagedSourceJudge(validEAVConfig(), nil); !errors.Is(err, errManagedEAVUnavailable) {
		t.Fatalf("a missing extractor must fail closed, got %v", err)
	}
	judge, err := newManagedSourceJudge(validEAVConfig(), &eavExtractorStub{reply: `{"primary":null,"reason_if_none":null}`})
	if err != nil || judge == nil {
		t.Fatalf("valid configuration must build a judge, got (%v, %v)", judge, err)
	}
}

func TestManagedEntityExtractor(t *testing.T) {
	stub := &eavExtractorStub{
		reply: `{"primary":{"name":"Apple Bank","kind":"organization","aliases":[],` +
			`"quote":"Apple Bank was founded in 1863."},"reason_if_none":null}`,
	}
	adapter := &managedEntityExtractor{extractor: stub, params: llm.ExtractParams{APIKey: "key"}}
	doc := eav.Document{
		URL:     "https://applebank.example/about",
		Title:   "Apple Bank - About",
		Cleaned: "Apple Bank was founded in 1863.",
	}
	slate := []eav.Candidate{{Surface: "Apple Bank", Signal: eav.SignalTitle, Quote: "Apple Bank"}}
	got, err := adapter.ExtractEntities(context.Background(), doc, slate)
	if err != nil {
		t.Fatalf("ExtractEntities returned error: %v", err)
	}
	if got.Primary == nil || got.Primary.Name != "Apple Bank" {
		t.Fatalf("decoded primary is wrong: %#v", got.Primary)
	}
	if string(stub.schema) != eav.ExtractionReplySchema {
		t.Fatal("the adapter must send the eav extraction schema")
	}
	for _, fragment := range []string{eav.ExtractionSystemPrompt, "CANDIDATES:", "Apple Bank - About"} {
		if !strings.Contains(stub.content, fragment) {
			t.Fatalf("adapter content is missing %q", fragment)
		}
	}

	failing := &managedEntityExtractor{extractor: &eavExtractorStub{err: errors.New("HTTP 500")}, params: llm.ExtractParams{}}
	if _, err := failing.ExtractEntities(context.Background(), doc, slate); err == nil {
		t.Fatal("provider failures must surface as errors for the gate to degrade")
	}
}

func TestManagedEntityReferee(t *testing.T) {
	stub := &eavExtractorStub{reply: `{"answer":"different","quote":"Apple Bank was founded in 1863."}`}
	adapter := &managedEntityReferee{extractor: stub, params: llm.ExtractParams{APIKey: "key"}}
	verdict, err := adapter.SameReferent(
		context.Background(),
		eav.Subject{Name: "Apple", Hint: "consumer electronics"},
		eav.Entity{Name: "Apple Bank", Kind: eav.KindOrganization},
		eav.Document{Title: "Apple Bank - About", Cleaned: "Apple Bank was founded in 1863."},
	)
	if err != nil || verdict.Answer != eav.RefereeDifferent {
		t.Fatalf("referee decode is wrong: (%#v, %v)", verdict, err)
	}
	if string(stub.schema) != eav.RefereeReplySchema {
		t.Fatal("the adapter must send the eav referee schema")
	}
	for _, fragment := range []string{eav.RefereeSystemPrompt, "SUBJECT: Apple", "HINT: consumer electronics"} {
		if !strings.Contains(stub.content, fragment) {
			t.Fatalf("referee content is missing %q", fragment)
		}
	}
}

func TestCachingEntityExtractor(t *testing.T) {
	stub := &eavExtractorStub{
		reply: `{"primary":{"name":"Apple Bank","kind":"organization","aliases":["ABNK"],` +
			`"quote":"Apple Bank was founded in 1863."},"reason_if_none":null}`,
	}
	cache := newCachingEntityExtractor(
		&managedEntityExtractor{extractor: stub, params: llm.ExtractParams{APIKey: "key"}},
		2,
	)
	doc := eav.Document{URL: "https://a.example/", Title: "Apple Bank", Cleaned: "Apple Bank was founded in 1863."}

	first, err := cache.ExtractEntities(context.Background(), doc, nil)
	if err != nil || first.Primary == nil {
		t.Fatalf("first extraction failed: (%#v, %v)", first, err)
	}
	second, err := cache.ExtractEntities(context.Background(), doc, nil)
	if err != nil || second.Primary == nil {
		t.Fatalf("cached extraction failed: (%#v, %v)", second, err)
	}
	if stub.calls != 1 {
		t.Fatalf("identical content must hit the cache, provider calls = %d", stub.calls)
	}
	// The cached value must be isolated: mutating one result cannot leak
	// into the next read.
	second.Primary.Name = "mutated"
	second.Primary.Aliases[0] = "mutated"
	third, err := cache.ExtractEntities(context.Background(), doc, nil)
	if err != nil || third.Primary == nil || third.Primary.Name != "Apple Bank" || third.Primary.Aliases[0] != "ABNK" {
		t.Fatalf("cache must deep-clone results, got %#v", third.Primary)
	}

	changed := doc
	changed.Cleaned = "Apple Bank moved its headquarters."
	if _, err := cache.ExtractEntities(context.Background(), changed, nil); err != nil {
		t.Fatalf("changed content extraction failed: %v", err)
	}
	if stub.calls != 2 {
		t.Fatalf("changed content must miss the cache, provider calls = %d", stub.calls)
	}

	// Errors are never cached.
	failingStub := &eavExtractorStub{err: errors.New("HTTP 500")}
	failing := newCachingEntityExtractor(
		&managedEntityExtractor{extractor: failingStub, params: llm.ExtractParams{}},
		2,
	)
	if _, err := failing.ExtractEntities(context.Background(), doc, nil); err == nil {
		t.Fatal("provider failure must surface")
	}
	failingStub.err = nil
	failingStub.reply = `{"primary":null,"reason_if_none":"list page"}`
	if _, err := failing.ExtractEntities(context.Background(), doc, nil); err != nil {
		t.Fatalf("recovery after failure must extract, got %v", err)
	}
	if failingStub.calls != 2 {
		t.Fatalf("failures must not be cached, provider calls = %d", failingStub.calls)
	}
}
