package main

import (
	"errors"
	"os"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/use-agent/purify/api/handler"
	"github.com/use-agent/purify/config"
	"github.com/use-agent/purify/publicnet"
	searchdomain "github.com/use-agent/purify/search"
	"github.com/use-agent/purify/searchindex"
)

var errManagedSearchConfigInvalid = errors.New("managed search configuration is invalid")

// managedSearchRuntime owns the local index handle used by Search.
// Search is request-driven: constructing this runtime starts no background
// work and makes no network request.
type managedSearchRuntime struct {
	service   *searchdomain.Service
	provider  *searchdomain.LocalIndexProvider
	enriched  bool
	closeOnce sync.Once
}

// validateManagedSearchConfig keeps disabled configuration inert. A
// configured path is accepted here; existence and Open happen at runtime
// construction so a missing index leaves Search unavailable instead of
// crashing the rest of the process.
func validateManagedSearchConfig(cfg config.SearchConfig) error {
	path := strings.TrimSpace(cfg.IndexPath)
	if path == "" {
		return nil
	}
	if !utf8.ValidString(path) {
		return errManagedSearchConfigInvalid
	}
	return nil
}

// managedSearchCapabilityEnabled mirrors the router's fail-closed Search
// capability gate. Keeping the production runtime behind the same auth,
// effective-key, and minimum positive burst boundary ensures an unavailable
// route never retains an open index handle. Costlier requests remain gated
// independently by the shared limiter.
func managedSearchCapabilityEnabled(cfg *config.Config) bool {
	if cfg == nil || !cfg.Auth.Enabled || cfg.RateLimit.Burst < handler.MinSearchRequestCost {
		return false
	}
	for _, key := range cfg.Auth.APIKeys {
		if strings.TrimSpace(key) != "" {
			return true
		}
	}
	return false
}

// managedAnswerCapabilityEnabled keeps the in-process Answer/Watch dependency
// graph behind Answer's own fixed admission boundary. Lowering Search's route
// gate to one token must not implicitly enable an internal Answer core that
// the public router would reject.
func managedAnswerCapabilityEnabled(cfg *config.Config) bool {
	if cfg == nil || !cfg.Auth.Enabled || cfg.RateLimit.Burst < handler.MaxAnswerRequestCost {
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
	if err := validateManagedSearchConfig(cfg.Search); err != nil {
		return nil, err
	}
	path := strings.TrimSpace(cfg.Search.IndexPath)
	if path == "" || !managedSearchCapabilityEnabled(cfg) {
		return nil, nil
	}

	info, statErr := os.Stat(path)
	if statErr != nil || info.IsDir() {
		return nil, nil
	}

	store, err := searchindex.Open(path)
	if err != nil {
		return nil, errManagedSearchConfigInvalid
	}
	options := make([]searchdomain.ServiceOption, 0, 1)
	enriched := artifacts != nil && signer != nil
	if enriched {
		enrichmentArtifacts := artifacts
		if cfg.Search.FeedIndex {
			// Enrichment already fetches and cleans these pages, so indexing
			// them costs one queued write and grows coverage where callers
			// actually look.
			enrichmentArtifacts = searchdomain.NewIndexFeeder(artifacts, store)
		}
		options = append(options, searchdomain.WithEnrichment(enrichmentArtifacts, signer))
	}
	provider, err := searchdomain.NewLocalIndexProvider(store)
	if err != nil {
		_ = store.Close()
		return nil, errManagedSearchConfigInvalid
	}
	service, err := searchdomain.NewService(provider, options...)
	if err != nil {
		_ = provider.Close()
		return nil, errManagedSearchConfigInvalid
	}
	return &managedSearchRuntime{service: service, provider: provider, enriched: enriched}, nil
}

// Close releases the index store exactly once. It is safe after partial
// startup, on a nil runtime, and from concurrent shutdown paths.
func (runtime *managedSearchRuntime) Close() {
	if runtime == nil {
		return
	}
	runtime.closeOnce.Do(func() {
		if runtime.provider != nil {
			_ = runtime.provider.Close()
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
