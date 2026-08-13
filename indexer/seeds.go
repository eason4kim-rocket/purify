package indexer

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// LoadSeeds reads a JSON array of seed URLs.
func LoadSeeds(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("indexer: read seeds: %w", err)
	}
	var seeds []string
	if err := json.Unmarshal(raw, &seeds); err != nil {
		return nil, fmt.Errorf("indexer: parse seeds: %w", err)
	}
	cleaned := make([]string, 0, len(seeds))
	seen := map[string]struct{}{}
	for _, seed := range seeds {
		seed = strings.TrimSpace(seed)
		if seed == "" {
			continue
		}
		if _, exists := seen[seed]; exists {
			continue
		}
		seen[seed] = struct{}{}
		cleaned = append(cleaned, seed)
	}
	if len(cleaned) == 0 {
		return nil, fmt.Errorf("indexer: seeds file is empty")
	}
	return cleaned, nil
}
