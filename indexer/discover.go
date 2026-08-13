package indexer

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/use-agent/purify/discovery"
	"github.com/use-agent/purify/publicnet"
	"github.com/use-agent/purify/searchindex"
	"golang.org/x/net/publicsuffix"
)

// Discoverer expands a seed URL into a bounded set of same-site URLs.
type Discoverer interface {
	Discover(context.Context, string) (*discovery.Result, error)
}

// SeedFrontier runs discovery for each seed and enqueues unique URLs.
func SeedFrontier(ctx context.Context, store *searchindex.Store, discoverer Discoverer, seeds []string, allowPrivate bool) (int, error) {
	if store == nil || discoverer == nil {
		return 0, fmt.Errorf("indexer: store and discoverer are required")
	}
	inserted := 0
	for _, seed := range seeds {
		if err := ctx.Err(); err != nil {
			return inserted, err
		}
		result, err := discoverer.Discover(ctx, seed)
		if err != nil && result == nil {
			return inserted, err
		}
		items := make([]searchindex.FrontierItem, 0, len(result.URLs)+1)
		for _, rawURL := range append([]string{seed}, result.URLs...) {
			canonical, root, normErr := pageRoot(rawURL, allowPrivate)
			if normErr != nil {
				continue
			}
			items = append(items, searchindex.FrontierItem{URL: canonical, Root: root})
		}
		n, enqueueErr := store.Enqueue(ctx, items)
		if enqueueErr != nil {
			return inserted, enqueueErr
		}
		inserted += n
	}
	return inserted, nil
}

func pageRoot(rawURL string, allowPrivate bool) (string, string, error) {
	canonical, parsed, err := publicnet.NormalizeHTTPURL(rawURL, nil, allowPrivate)
	if err != nil {
		return "", "", err
	}
	host := strings.ToLower(parsed.Hostname())
	root, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		root = host
	}
	if parsed.Path == "" {
		canonical = strings.TrimRight(canonical, "/")
		if u, perr := url.Parse(canonical); perr == nil && u.Path == "" {
			canonical += "/"
		}
	}
	return canonical, strings.ToLower(root), nil
}
