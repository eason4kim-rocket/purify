package indexer

import (
	"context"
	"fmt"
	"strings"

	"github.com/use-agent/purify/searchindex"
)

// MaxPriorityURLs keeps exact operator targets bounded and below SQLite's
// parameter limit when the store selects them in one ordered query.
const MaxPriorityURLs = searchindex.MaxPreferredLeaseURLs

// PreparePriorityFrontier validates and canonicalizes exact operator targets,
// inserts missing rows, and reopens completed or exhausted rows. It deliberately
// applies the same public-network and junk admission rules as ordinary links.
func PreparePriorityFrontier(ctx context.Context, store *searchindex.Store, rawURLs []string, allowPrivate bool) ([]string, int, error) {
	if store == nil {
		return nil, 0, fmt.Errorf("indexer: priority frontier store is required")
	}
	if len(rawURLs) == 0 {
		return nil, 0, nil
	}
	if len(rawURLs) > MaxPriorityURLs {
		return nil, 0, fmt.Errorf("indexer: priority URL count exceeds %d", MaxPriorityURLs)
	}

	urls := make([]string, 0, len(rawURLs))
	items := make([]searchindex.FrontierItem, 0, len(rawURLs))
	seen := make(map[string]struct{}, len(rawURLs))
	for index, rawURL := range rawURLs {
		rawURL = strings.TrimSpace(rawURL)
		if rawURL == "" {
			continue
		}
		canonical, root, err := pageRoot(rawURL, allowPrivate)
		if err != nil {
			return nil, 0, fmt.Errorf("indexer: priority URL %d: %w", index+1, err)
		}
		if junkURL(canonical) {
			return nil, 0, fmt.Errorf("indexer: priority URL %d is not indexable", index+1)
		}
		if _, exists := seen[canonical]; exists {
			continue
		}
		seen[canonical] = struct{}{}
		urls = append(urls, canonical)
		items = append(items, searchindex.FrontierItem{URL: canonical, Root: root})
	}
	if len(urls) == 0 {
		return nil, 0, fmt.Errorf("indexer: priority URL list is empty")
	}
	inserted, err := store.Enqueue(ctx, items)
	if err != nil {
		return nil, 0, err
	}
	if _, err := store.Requeue(ctx, urls); err != nil {
		return nil, 0, err
	}
	return urls, inserted, nil
}
