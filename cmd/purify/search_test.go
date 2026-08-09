package main

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/use-agent/purify/api/handler"
	"github.com/use-agent/purify/config"
	"github.com/use-agent/purify/publicnet"
)

type recordingSearchResolver struct {
	calls atomic.Int64
}

func (resolver *recordingSearchResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	resolver.calls.Add(1)
	return nil, errors.New("unexpected Search construction lookup")
}

func TestValidateManagedSearchConfigDisabledValidAndRedacted(t *testing.T) {
	if err := validateManagedSearchConfig(config.SearchConfig{}); err != nil {
		t.Fatalf("disabled validation error = %v", err)
	}
	if err := validateManagedSearchConfig(config.SearchConfig{BraveKey: "process-key"}); err != nil {
		t.Fatalf("valid validation error = %v", err)
	}

	for _, key := range []string{
		" secret with spaces ",
		"secret\nvalue",
		"\xff",
		strings.Repeat("k", (16<<10)+1),
	} {
		err := validateManagedSearchConfig(config.SearchConfig{BraveKey: key})
		if !errors.Is(err, errManagedSearchConfigInvalid) {
			t.Fatalf("validateManagedSearchConfig() error = %v", err)
		}
		if strings.Contains(err.Error(), key) {
			t.Fatalf("validation error leaked credential: %v", err)
		}
	}
}

func TestNewManagedSearchRuntimeDisabledIsInert(t *testing.T) {
	cfg := &config.Config{
		Auth:      config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}},
		RateLimit: config.RateLimitConfig{Burst: handler.MaxSearchRequestCost},
	}
	runtime, err := newManagedSearchRuntime(cfg, nil)
	if err != nil || runtime != nil {
		t.Fatalf("disabled newManagedSearchRuntime() = %#v, %v", runtime, err)
	}
	if service := managedSearchHandlerService(runtime); service != nil {
		t.Fatalf("disabled handler service = %#v, want genuine nil", service)
	}
	(*managedSearchRuntime)(nil).Close()
}

func TestNewManagedSearchRuntimeRejectsInvalidConfigurationWithoutCredentialLeak(t *testing.T) {
	for _, test := range []struct {
		name   string
		config *config.Config
		policy *publicnet.Policy
	}{
		{
			name: "invalid key is not hidden by unsafe gate",
			config: &config.Config{
				Search: config.SearchConfig{BraveKey: "private key"},
			},
		},
		{
			name: "missing policy",
			config: &config.Config{
				Search:    config.SearchConfig{BraveKey: "private-key"},
				Auth:      config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}},
				RateLimit: config.RateLimitConfig{Burst: handler.MaxSearchRequestCost},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, err := newManagedSearchRuntime(test.config, test.policy)
			if runtime != nil || !errors.Is(err, errManagedSearchConfigInvalid) {
				t.Fatalf("newManagedSearchRuntime() = %#v, %v", runtime, err)
			}
			if strings.Contains(err.Error(), test.config.Search.BraveKey) {
				t.Fatalf("runtime error leaked credential: %v", err)
			}
		})
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
			name:  "burst N minus one",
			auth:  config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}},
			burst: handler.MaxSearchRequestCost - 1,
		},
		{
			name:  "historical default burst",
			auth:  config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}},
			burst: 10,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{
				Search:    config.SearchConfig{BraveKey: "process-key"},
				Auth:      test.auth,
				RateLimit: config.RateLimitConfig{Burst: test.burst},
			}
			runtime, err := newManagedSearchRuntime(cfg, nil)
			if err != nil || runtime != nil {
				t.Fatalf("unsafe newManagedSearchRuntime() = %#v, %v", runtime, err)
			}
			if service := managedSearchHandlerService(runtime); service != nil {
				t.Fatalf("unsafe handler service = %#v, want genuine nil", service)
			}
		})
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
		Search:    config.SearchConfig{BraveKey: "process-key"},
		Auth:      config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}},
		RateLimit: config.RateLimitConfig{Burst: handler.MaxSearchRequestCost},
	}
	runtime, err := newManagedSearchRuntime(cfg, policy)
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
