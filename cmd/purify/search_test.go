package main

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/use-agent/purify/api/handler"
	"github.com/use-agent/purify/config"
	"github.com/use-agent/purify/extract"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/publicnet"
	"github.com/use-agent/purify/receipts"
	"github.com/use-agent/purify/searchindex"
)

type recordingSearchResolver struct {
	calls atomic.Int64
}

func (resolver *recordingSearchResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	resolver.calls.Add(1)
	return nil, errors.New("unexpected Search construction lookup")
}

func TestValidateManagedSearchConfigDisabledAndValid(t *testing.T) {
	if err := validateManagedSearchConfig(config.SearchConfig{}); err != nil {
		t.Fatalf("disabled validation error = %v", err)
	}
	if err := validateManagedSearchConfig(config.SearchConfig{IndexPath: "/tmp/index.db"}); err != nil {
		t.Fatalf("valid validation error = %v", err)
	}
	if err := validateManagedSearchConfig(config.SearchConfig{IndexPath: "\xff"}); !errors.Is(err, errManagedSearchConfigInvalid) {
		t.Fatalf("invalid path validation error = %v", err)
	}
}

func TestNewManagedSearchRuntimeDisabledIsInert(t *testing.T) {
	cfg := &config.Config{
		Auth:      config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}},
		RateLimit: config.RateLimitConfig{Burst: handler.MaxSearchRequestCost},
	}
	runtime, err := newManagedSearchRuntime(cfg, nil, nil, nil, nil)
	if err != nil || runtime != nil {
		t.Fatalf("disabled newManagedSearchRuntime() = %#v, %v", runtime, err)
	}
	if service := managedSearchHandlerService(runtime); service != nil {
		t.Fatalf("disabled handler service = %#v, want genuine nil", service)
	}
	(*managedSearchRuntime)(nil).Close()
}

func TestNewManagedSearchRuntimeMissingIndexIsInert(t *testing.T) {
	cfg := &config.Config{
		Search:    config.SearchConfig{IndexPath: filepath.Join(t.TempDir(), "missing.db")},
		Auth:      config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}},
		RateLimit: config.RateLimitConfig{Burst: handler.MaxSearchRequestCost},
	}
	runtime, err := newManagedSearchRuntime(cfg, nil, nil, nil, nil)
	if err != nil || runtime != nil {
		t.Fatalf("missing-index newManagedSearchRuntime() = %#v, %v", runtime, err)
	}
}

func TestNewManagedSearchRuntimeRejectsCorruptIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.db")
	if err := os.WriteFile(path, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Search:    config.SearchConfig{IndexPath: path},
		Auth:      config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}},
		RateLimit: config.RateLimitConfig{Burst: handler.MaxSearchRequestCost},
	}
	// Opening the index moved ahead of the Search runtime so every fetch in the
	// process can feed it, so the corrupt-file rejection lives there now.
	store, err := openSearchIndexStore(cfg.Search)
	if store != nil || !errors.Is(err, errManagedSearchConfigInvalid) {
		t.Fatalf("corrupt-index openSearchIndexStore() = %#v, %v", store, err)
	}
	runtime, err := newManagedSearchRuntime(cfg, nil, nil, nil, nil)
	if runtime != nil || err != nil {
		t.Fatalf("corrupt-index newManagedSearchRuntime() = %#v, %v", runtime, err)
	}
}

func TestNewManagedSearchRuntimeUnsafeCapabilityGateIsInert(t *testing.T) {
	tests := []struct {
		name  string
		auth  config.AuthConfig
		burst int
	}{
		{
			name:  "auth disabled",
			auth:  config.AuthConfig{Enabled: false, APIKeys: []string{"required-secret"}},
			burst: handler.MaxSearchRequestCost,
		},
		{
			name:  "empty API key set",
			auth:  config.AuthConfig{Enabled: true},
			burst: handler.MaxSearchRequestCost,
		},
		{
			name:  "all blank API keys",
			auth:  config.AuthConfig{Enabled: true, APIKeys: []string{"", " \t"}},
			burst: handler.MaxSearchRequestCost,
		},
		{
			name:  "burst zero",
			auth:  config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}},
			burst: 0,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{
				Search:    config.SearchConfig{IndexPath: filepath.Join(t.TempDir(), "index.db")},
				Auth:      test.auth,
				RateLimit: config.RateLimitConfig{Burst: test.burst},
			}
			runtime, err := newManagedSearchRuntime(cfg, nil, nil, nil, nil)
			if err != nil || runtime != nil {
				t.Fatalf("unsafe newManagedSearchRuntime() = %#v, %v", runtime, err)
			}
			if service := managedSearchHandlerService(runtime); service != nil {
				t.Fatalf("unsafe handler service = %#v, want genuine nil", service)
			}
		})
	}
}

func TestManagedSearchCapabilityUsesMinimumPositiveBurst(t *testing.T) {
	for _, test := range []struct {
		burst int
		want  bool
	}{
		{burst: 0, want: false},
		{burst: 1, want: true},
		{burst: 22, want: true},
		{burst: 58, want: true},
		{burst: 59, want: true},
	} {
		cfg := &config.Config{
			Auth:      config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}},
			RateLimit: config.RateLimitConfig{Burst: test.burst},
		}
		if got := managedSearchCapabilityEnabled(cfg); got != test.want {
			t.Fatalf("managedSearchCapabilityEnabled(burst=%d) = %t, want %t", test.burst, got, test.want)
		}
	}
}

func TestManagedAnswerCapabilityKeepsItsIndependentBurstBoundary(t *testing.T) {
	for _, test := range []struct {
		burst int
		want  bool
	}{
		{burst: 0, want: false},
		{burst: 1, want: false},
		{burst: handler.MaxAnswerRequestCost - 1, want: false},
		{burst: handler.MaxAnswerRequestCost, want: true},
		{burst: 22, want: true},
		{burst: 59, want: true},
	} {
		cfg := &config.Config{
			Auth:      config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}},
			RateLimit: config.RateLimitConfig{Burst: test.burst},
		}
		if got := managedAnswerCapabilityEnabled(cfg); got != test.want {
			t.Fatalf("managedAnswerCapabilityEnabled(burst=%d) = %t, want %t", test.burst, got, test.want)
		}
	}
	for _, cfg := range []*config.Config{
		nil,
		{Auth: config.AuthConfig{Enabled: false, APIKeys: []string{"required-secret"}}, RateLimit: config.RateLimitConfig{Burst: 59}},
		{Auth: config.AuthConfig{Enabled: true, APIKeys: []string{"", " \t"}}, RateLimit: config.RateLimitConfig{Burst: 59}},
	} {
		if managedAnswerCapabilityEnabled(cfg) {
			t.Fatalf("unsafe Answer capability enabled for %#v", cfg)
		}
	}
}

func TestManagedSearchRuntimeConstructsWithoutNetworkAndClosesConcurrently(t *testing.T) {
	resolver := &recordingSearchResolver{}
	var dialCalls atomic.Int64
	policy := publicnet.NewPolicy(publicnet.Options{
		Resolver: resolver,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			dialCalls.Add(1)
			return nil, errors.New("unexpected Search construction dial")
		},
	})
	cfg := &config.Config{
		Search:    config.SearchConfig{IndexPath: writeTestSearchIndex(t)},
		Auth:      config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}},
		RateLimit: config.RateLimitConfig{Burst: handler.MinSearchRequestCost},
	}
	store, err := openSearchIndexStore(cfg.Search)
	if err != nil || store == nil {
		t.Fatalf("openSearchIndexStore() = %#v, %v", store, err)
	}
	t.Cleanup(func() { _ = store.Close() })
	runtime, err := newManagedSearchRuntime(cfg, policy, store, nil, nil)
	if err != nil {
		t.Fatalf("newManagedSearchRuntime() error = %v", err)
	}
	if runtime == nil || runtime.provider == nil || runtime.service == nil {
		t.Fatalf("runtime = %#v", runtime)
	}
	if service := managedSearchHandlerService(runtime); service == nil {
		t.Fatal("configured handler service = nil")
	}
	if resolver.calls.Load() != 0 || dialCalls.Load() != 0 {
		t.Fatalf("construction performed network work: lookups=%d dials=%d", resolver.calls.Load(), dialCalls.Load())
	}

	var callers sync.WaitGroup
	for range 32 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			runtime.Close()
		}()
	}
	callers.Wait()
	runtime.Close()
}

type stubSearchArtifactService struct{}

func (stubSearchArtifactService) FetchPublicArtifact(context.Context, string) (*extract.Artifact, error) {
	return nil, errors.New("unexpected construction fetch")
}

func (stubSearchArtifactService) ExtractArtifact(context.Context, *extract.Artifact, *models.ExtractRequest) (*models.ExtractResponse, error) {
	return nil, errors.New("unexpected construction extraction")
}

type stubSearchReceiptSigner struct{}

func (stubSearchReceiptSigner) Sign(receipts.Payload) (string, error) {
	return "", errors.New("unexpected construction signing")
}

func writeTestSearchIndex(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index.db")
	store, err := searchindex.Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	return path
}

func TestManagedSearchRuntimeEnrichmentFollowsSuppliedDependencies(t *testing.T) {
	policy := publicnet.NewPolicy(publicnet.Options{
		Resolver: &recordingSearchResolver{},
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("unexpected Search construction dial")
		},
	})
	cfg := &config.Config{
		Search:    config.SearchConfig{IndexPath: writeTestSearchIndex(t)},
		Auth:      config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}},
		RateLimit: config.RateLimitConfig{Burst: handler.MaxSearchRequestCost},
	}

	// Each runtime closes the store it was handed, so every case opens its own.
	baseline, err := newManagedSearchRuntime(cfg, policy, mustOpenTestIndex(t, cfg), nil, nil)
	if err != nil || baseline == nil || baseline.enriched {
		t.Fatalf("baseline runtime = %#v, %v", baseline, err)
	}
	baseline.Close()

	partial, err := newManagedSearchRuntime(cfg, policy, mustOpenTestIndex(t, cfg), stubSearchArtifactService{}, nil)
	if err != nil || partial == nil || partial.enriched {
		t.Fatalf("partial-dependency runtime = %#v, %v", partial, err)
	}
	partial.Close()

	enriched, err := newManagedSearchRuntime(cfg, policy, mustOpenTestIndex(t, cfg), stubSearchArtifactService{}, stubSearchReceiptSigner{})
	if err != nil || enriched == nil || !enriched.enriched || enriched.service == nil {
		t.Fatalf("enriched runtime = %#v, %v", enriched, err)
	}
	enriched.Close()
}

func mustOpenTestIndex(t *testing.T, cfg *config.Config) *searchindex.Store {
	t.Helper()
	store, err := openSearchIndexStore(cfg.Search)
	if err != nil || store == nil {
		t.Fatalf("openSearchIndexStore() = %#v, %v", store, err)
	}
	return store
}
