// Package search owns provider-neutral query orchestration. Provider adapters
// translate their private wire formats into the bounded types in this package;
// public HTTP and MCP contracts remain in models.
package search

import (
	"context"
	"time"
)

const (
	// MaxProviderResults is the defensive baseline candidate bound regardless
	// of an adapter or upstream returning more than requested.
	MaxProviderResults = 20
	// Provider result metadata is bounded independently of an adapter's wire
	// body reader so in-process fakes and future providers obey the same limits.
	MaxProviderNameBytes     = 64
	MaxProviderTitleBytes    = 16 << 10
	MaxProviderSnippetBytes  = 64 << 10
	MaxProviderMetadataBytes = 2 << 20
)

// ProviderQuery is the complete provider-facing baseline query. Domains are
// intentionally absent: Purify applies canonical domain filtering after a
// provider response so correctness never depends on provider-specific syntax.
type ProviderQuery struct {
	Text      string
	Limit     int
	Freshness Freshness
}

// ProviderResult is one provider-neutral baseline candidate. Rank is one-based
// provider order. Score is nil when a provider does not expose a meaningful
// comparable score; adapters must not synthesize one.
type ProviderResult struct {
	Rank        int
	Score       *float64
	Title       string
	URL         string
	Snippet     string
	PublishedAt *time.Time
}

// Provider executes one bounded web search. A non-nil error invalidates every
// result returned alongside it; providers must not rely on partial-result
// behavior. Name is internal diagnostic identity and must never enter a public
// response or error message.
type Provider interface {
	Name() string
	Search(context.Context, ProviderQuery) ([]ProviderResult, error)
}
