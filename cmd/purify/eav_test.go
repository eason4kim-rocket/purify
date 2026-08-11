package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/use-agent/purify/config"
	"github.com/use-agent/purify/llm"
	"github.com/use-agent/purify/publicnet"
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

func TestManagedSourceJudgeRuntimeDisabledIsInert(t *testing.T) {
	runtime, err := newManagedSourceJudgeRuntime(config.EAVConfig{AllowPrivate: true}, nil)
	if err != nil || runtime != nil {
		t.Fatalf("disabled runtime = %#v, %v; want nil, nil", runtime, err)
	}

	cfg := validEAVConfig()
	if runtime, err := newManagedSourceJudgeRuntime(cfg, nil); !errors.Is(err, errManagedEAVUnavailable) || runtime != nil {
		t.Fatalf("enabled runtime without policy = %#v, %v", runtime, err)
	}
}

func TestManagedEAVLoopbackPolicyAndBYOKIsolation(t *testing.T) {
	provider, capture := newEAVProviderServer(t)
	baseURL := provider.URL + "/v1"
	doc := managedEAVTestDocument(provider.URL)

	strictPolicy, err := newManagedEAVPolicy("", false)
	if err != nil {
		t.Fatal(err)
	}
	strictCfg := validEAVConfig()
	strictCfg.BaseURL = baseURL
	strictRuntime, err := newManagedSourceJudgeRuntime(strictCfg, strictPolicy)
	if err != nil {
		t.Fatalf("newManagedSourceJudgeRuntime(strict) error = %v", err)
	}
	t.Cleanup(strictRuntime.Close)
	judgment, err := strictRuntime.JudgeDocument(context.Background(), eav.Subject{Name: "Apple Bank"}, doc)
	if err != nil || judgment.Verdict != eav.VerdictUncertain {
		t.Fatalf("strict loopback judgment = %#v, %v", judgment, err)
	}
	if hits, _, _ := capture.snapshot(); hits != 0 {
		t.Fatalf("strict managed loopback provider hits = %d, want zero", hits)
	}

	privatePolicy, err := newManagedEAVPolicy("", true)
	if err != nil {
		t.Fatal(err)
	}
	privateCfg := validEAVConfig()
	privateCfg.AllowPrivate = true
	privateCfg.BaseURL = baseURL
	privateRuntime, err := newManagedSourceJudgeRuntime(privateCfg, privatePolicy)
	if err != nil {
		t.Fatalf("newManagedSourceJudgeRuntime(private) error = %v", err)
	}
	t.Cleanup(privateRuntime.Close)
	judgment, err = privateRuntime.JudgeDocument(context.Background(), eav.Subject{Name: "Apple Bank"}, doc)
	if err != nil || judgment.Verdict != eav.VerdictMatch {
		t.Fatalf("private loopback judgment = %#v, %v", judgment, err)
	}
	hits, authorizations, bodies := capture.snapshot()
	if hits != 1 || len(authorizations) != 1 || authorizations[0] != "Bearer key" {
		t.Fatalf("private managed provider hits/auth = %d/%#v", hits, authorizations)
	}

	// Recreate the production request-scoped client from the immutable global
	// policy after private managed EAV is enabled. The same target must remain
	// unreachable and the caller credential must never reach the provider.
	requestPolicy, err := newOutboundPolicy("")
	if err != nil {
		t.Fatal(err)
	}
	requestHTTPClient, err := llm.NewPublicHTTPClient(requestPolicy, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(requestHTTPClient.CloseIdleConnections)
	const byokSecret = "byok-must-not-leak"
	_, err = llm.NewClient(requestHTTPClient).Extract(
		context.Background(),
		"request content",
		json.RawMessage(`{"type":"object"}`),
		llm.ExtractParams{APIKey: byokSecret, Model: "request-model", BaseURL: baseURL},
	)
	if !errors.Is(err, publicnet.ErrNotPublic) {
		t.Fatalf("BYOK loopback error = %v, want ErrNotPublic", err)
	}
	if strings.Contains(err.Error(), byokSecret) {
		t.Fatalf("BYOK error leaked credential: %v", err)
	}
	afterHits, afterAuthorizations, afterBodies := capture.snapshot()
	if afterHits != hits || len(afterAuthorizations) != len(authorizations) || len(afterBodies) != len(bodies) {
		t.Fatalf("BYOK reached private provider: hits/auth/bodies = %d/%d/%d", afterHits, len(afterAuthorizations), len(afterBodies))
	}
	for _, value := range append(afterAuthorizations, afterBodies...) {
		if strings.Contains(value, byokSecret) {
			t.Fatal("private provider captured BYOK credential")
		}
	}
}

func TestManagedEAVStrictPolicyReachesPublicProvider(t *testing.T) {
	provider, capture := newEAVProviderServer(t)
	serverAddress := provider.Listener.Addr().String()
	_, rawPort, err := net.SplitHostPort(serverAddress)
	if err != nil {
		t.Fatal(err)
	}
	policy := publicnet.NewPolicy(publicnet.Options{
		Resolver: eavStaticResolver{"provider.test": {netip.MustParseAddr("1.1.1.1")}},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, serverAddress)
		},
	})
	cfg := validEAVConfig()
	cfg.BaseURL = "http://provider.test:" + rawPort + "/v1"
	runtime, err := newManagedSourceJudgeRuntime(cfg, policy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Close)
	judgment, err := runtime.JudgeDocument(
		context.Background(),
		eav.Subject{Name: "Apple Bank"},
		managedEAVTestDocument("http://provider.test:"+rawPort),
	)
	if err != nil || judgment.Verdict != eav.VerdictMatch {
		t.Fatalf("public provider judgment = %#v, %v", judgment, err)
	}
	if hits, _, _ := capture.snapshot(); hits != 1 {
		t.Fatalf("public provider hits = %d, want one", hits)
	}
}

type eavStaticResolver map[string][]netip.Addr

func (resolver eavStaticResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	addresses, ok := resolver[host]
	if !ok {
		return nil, errors.New("unexpected host")
	}
	return append([]netip.Addr(nil), addresses...), nil
}

type eavProviderCapture struct {
	mu             sync.Mutex
	hits           int
	authorizations []string
	bodies         []string
}

func (capture *eavProviderCapture) snapshot() (int, []string, []string) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return capture.hits, append([]string(nil), capture.authorizations...), append([]string(nil), capture.bodies...)
}

func newEAVProviderServer(t *testing.T) (*httptest.Server, *eavProviderCapture) {
	t.Helper()
	capture := &eavProviderCapture{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(writer, "read request", http.StatusBadRequest)
			return
		}
		capture.mu.Lock()
		capture.hits++
		capture.authorizations = append(capture.authorizations, request.Header.Get("Authorization"))
		capture.bodies = append(capture.bodies, string(body))
		capture.mu.Unlock()

		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{
					"content": `{"primary":{"name":"Apple Bank","kind":"organization","aliases":[],"quote":"Apple Bank was founded in 1863."},"reason_if_none":null}`,
				},
			}},
		})
	}))
	t.Cleanup(server.Close)
	return server, capture
}

func managedEAVTestDocument(rawURL string) eav.Document {
	return eav.Document{
		URL:     rawURL,
		Title:   "Apple Bank",
		Cleaned: "Apple Bank was founded in 1863.",
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
