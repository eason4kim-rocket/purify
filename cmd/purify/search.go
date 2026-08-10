package main

import (
	"errors"
	"strings"
	"sync"

	"github.com/use-agent/purify/api/handler"
	"github.com/use-agent/purify/config"
	"github.com/use-agent/purify/publicnet"
	searchdomain "github.com/use-agent/purify/search"
)

var errManagedSearchConfigInvalid = errors.New("managed search configuration is invalid")

// managedSearchRuntime owns the baseline provider's dedicated HTTP transport.
// Search is request-driven: constructing this runtime starts no background
// work and makes no network request.
type managedSearchRuntime struct {
	service   *searchdomain.Service
	provider  *searchdomain.BraveProvider
	enriched  bool
	closeOnce sync.Once
}

// validateManagedSearchConfig keeps disabled configuration inert and delegates
// every configured credential boundary to the provider's canonical validator.
// It deliberately replaces the provider error with a fixed process-level error
// so startup diagnostics can never echo credential material.
func validateManagedSearchConfig(cfg config.SearchConfig) error {
	if cfg.BraveKey == "" {
		return nil
	}
	if err := searchdomain.ValidateBraveAPIKey(cfg.BraveKey); err != nil {
		return errManagedSearchConfigInvalid
	}
	return nil
}

// managedSearchCapabilityEnabled mirrors the router's fail-closed Search
// capability gate. Keeping the production runtime behind the same auth,
// effective-key, and maximum-cost burst boundary ensures an unavailable route
// never retains provider state or its process-owned credential.
func managedSearchCapabilityEnabled(cfg *config.Config) bool {
	if cfg == nil || !cfg.Auth.Enabled || cfg.RateLimit.Burst < handler.MaxSearchRequestCost {
		return false
	}
	for _, key := range cfg.Auth.APIKeys {
		if strings.TrimSpace(key) != "" {
			return true
		}
	}
	return false
}

// newManagedSearchRuntime constructs the request-driven Search runtime.
// Result enrichment (verify, include_content, and schema) is attached only
// when both process-owned dependencies are supplied; a baseline-only runtime
// keeps those request shapes failing closed as SEARCH_UNAVAILABLE.
func newManagedSearchRuntime(
	cfg *config.Config,
	policy *publicnet.Policy,
	artifacts searchdomain.ArtifactService,
	signer searchdomain.ReceiptSigner,
) (*managedSearchRuntime, error) {
	if cfg == nil {
		return nil, nil
	}
	// Validate every non-empty process credential before the capability gate:
	// an unsafe auth/rate configuration must not hide a malformed secret.
	if err := validateManagedSearchConfig(cfg.Search); err != nil {
		return nil, err
	}
	if cfg.Search.BraveKey == "" || !managedSearchCapabilityEnabled(cfg) {
		return nil, nil
	}
	if policy == nil {
		return nil, errManagedSearchConfigInvalid
	}

	options := make([]searchdomain.ServiceOption, 0, 1)
	enriched := artifacts != nil && signer != nil
	if enriched {
		options = append(options, searchdomain.WithEnrichment(artifacts, signer))
	}
	provider, err := searchdomain.NewBraveProvider(cfg.Search.BraveKey, policy)
	if err != nil {
		return nil, errManagedSearchConfigInvalid
	}
	service, err := searchdomain.NewService(provider, options...)
	if err != nil {
		provider.CloseIdleConnections()
		return nil, errManagedSearchConfigInvalid
	}
	return &managedSearchRuntime{service: service, provider: provider, enriched: enriched}, nil
}

// Close releases the provider's idle connection pool exactly once. It is safe
// after partial startup, on a nil runtime, and from concurrent shutdown paths.
func (runtime *managedSearchRuntime) Close() {
	if runtime == nil {
		return
	}
	runtime.closeOnce.Do(func() {
		if runtime.provider != nil {
			runtime.provider.CloseIdleConnections()
		}
	})
}

// managedSearchHandlerService avoids storing a typed nil in the transport
// interface when Search is disabled.
func managedSearchHandlerService(runtime *managedSearchRuntime) handler.SearchService {
	if runtime == nil || runtime.service == nil {
		return nil
	}
	return runtime.service
}
