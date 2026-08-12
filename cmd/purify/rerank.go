package main

import (
	"fmt"

	"github.com/use-agent/purify/config"
	"github.com/use-agent/purify/search/rerank"
)

// validateManagedRerankConfig is an early, no-I/O startup gate. R-3 makes
// every enabled profile unconditionally unavailable before syntax or endpoint
// parsing; R-6a authenticated deployment handoff plus an R-6 manifest must
// define any future positive admission path.
func validateManagedRerankConfig(value config.RerankConfig) error {
	if !value.Enabled {
		return nil
	}
	if err := rerank.RequireCertifiedProfile(value.Profile); err != nil {
		return fmt.Errorf("managed reranker profile is unavailable: %w", err)
	}
	return config.ValidateRerankConfig(value)
}
