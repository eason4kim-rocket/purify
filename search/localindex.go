package search

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/searchindex"
)

const localIndexProviderName = "local-index"

// ErrInvalidLocalIndex reports an unusable local index handle.
var ErrInvalidLocalIndex = errors.New("search: invalid local index provider")

// LocalIndexProvider serves baseline candidates from a Purify-owned FTS store.
type LocalIndexProvider struct {
	store *searchindex.Store
}

var _ Provider = (*LocalIndexProvider)(nil)

// NewLocalIndexProvider wraps an open page store. The caller retains Close
// ownership unless Close is invoked on the provider itself.
func NewLocalIndexProvider(store *searchindex.Store) (*LocalIndexProvider, error) {
	if store == nil {
		return nil, ErrInvalidLocalIndex
	}
	return &LocalIndexProvider{store: store}, nil
}

// Name is the internal diagnostic identity for this adapter.
func (provider *LocalIndexProvider) Name() string {
	return localIndexProviderName
}

// Close releases the wrapped store. It is safe on a nil provider.
func (provider *LocalIndexProvider) Close() error {
	if provider == nil || provider.store == nil {
		return nil
	}
	return provider.store.Close()
}

// Search returns rank-merged FTS hits. Score stays nil: SQLite BM25 is not
// in the [0,1] interval that normalizeProviderResults accepts.
func (provider *LocalIndexProvider) Search(ctx context.Context, query ProviderQuery) ([]ProviderResult, error) {
	if provider == nil || provider.store == nil {
		return nil, ErrInvalidLocalIndex
	}
	if ctx == nil {
		return nil, ErrInvalidLocalIndex
	}
	if err := ctx.Err(); err != nil {
		return nil, NewProviderError(ProviderErrorTimeout, 0, err)
	}
	limit := query.Limit
	if limit < 1 {
		limit = 1
	}
	if limit > MaxProviderResults {
		limit = MaxProviderResults
	}
	hits, err := provider.store.Query(ctx, query.Text, limit)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, NewProviderError(ProviderErrorTimeout, 0, err)
		}
		return nil, NewProviderError(ProviderErrorUpstream, 0, err)
	}
	results := make([]ProviderResult, 0, len(hits))
	for _, hit := range hits {
		if len(hit.URL) == 0 || len(hit.URL) > models.MaxSearchURLBytes {
			continue
		}
		results = append(results, ProviderResult{
			Rank:    len(results) + 1,
			Title:   clipProviderText(hit.Title, MaxProviderTitleBytes),
			URL:     hit.URL,
			Snippet: clipProviderText(hit.Snippet, MaxProviderSnippetBytes),
		})
		if len(results) >= limit {
			break
		}
	}
	return results, nil
}

func clipProviderText(raw string, maximum int) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || maximum <= 0 {
		return ""
	}
	var builder strings.Builder
	builder.Grow(min(len(raw), maximum))
	for _, character := range raw {
		if unicode.IsControl(character) {
			continue
		}
		size := utf8.RuneLen(character)
		if size < 0 || builder.Len()+size > maximum {
			break
		}
		builder.WriteRune(character)
	}
	return builder.String()
}
