package indexer

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// LoadSeeds reads a JSON array of seed URLs.
func LoadSeeds(path string) ([]string, error) {
	return loadURLList(path, "seeds")
}

// LoadPriorityURLs reads a JSON array of exact URLs to lease first.
func LoadPriorityURLs(path string) ([]string, error) {
	return loadURLList(path, "priority URLs")
}

func loadURLList(path, label string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("indexer: read %s: %w", label, err)
	}
	var urls []string
	if err := json.Unmarshal(raw, &urls); err != nil {
		return nil, fmt.Errorf("indexer: parse %s: %w", label, err)
	}
	cleaned := make([]string, 0, len(urls))
	seen := map[string]struct{}{}
	for _, rawURL := range urls {
		rawURL = strings.TrimSpace(rawURL)
		if rawURL == "" {
			continue
		}
		if _, exists := seen[rawURL]; exists {
			continue
		}
		seen[rawURL] = struct{}{}
		cleaned = append(cleaned, rawURL)
	}
	if len(cleaned) == 0 {
		return nil, fmt.Errorf("indexer: %s file is empty", label)
	}
	return cleaned, nil
}
