package main

import (
	"fmt"

	"github.com/use-agent/purify/config"
	"github.com/use-agent/purify/search/rerank"
)

// validateManagedRerankConfig is an early, no-I/O startup gate. Enabled
// configuration must first name a profile in the immutable R-6 registry;
// only an admitted profile proceeds to syntax validation. R-3 intentionally
// ships that registry empty, so it never parses an endpoint at startup.
func validateManagedRerankConfig(value config.RerankConfig) error {
	if !value.Enabled {
		return nil
	}
	if err := rerank.RequireCertifiedProfile(value.Profile); err != nil {
		return fmt.Errorf("managed reranker profile is unavailable: %w", err)
	}
	return config.ValidateRerankConfig(value)
}
