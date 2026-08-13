package search

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/searchindex"
)

func TestNewLocalIndexProviderRejectsNilStore(t *testing.T) {
	if _, err := NewLocalIndexProvider(nil); err != ErrInvalidLocalIndex {
		t.Fatalf("NewLocalIndexProvider(nil) = %v", err)
	}
	var provider *LocalIndexProvider
	if err := provider.Close(); err != nil {
		t.Fatalf("nil Close() = %v", err)
	}
}

func TestLocalIndexProviderSearchRanksMergedHits(t *testing.T) {
	store := openLocalIndexStore(t)
	mustIndexPage(t, store, searchindex.Page{
		URL: "https://en.example/go", Root: "example", Title: "Go language",
		Body: "The Go programming language is compiled.", Lang: searchindex.LangEnglish,
		FetchedAt: time.Unix(1_700_000_000, 0).UTC(),
	})
	mustIndexPage(t, store, searchindex.Page{
		URL: "https://zh.example/go", Root: "example", Title: "Go 语言",
		Body: "Go 编程语言是编译型的。", Lang: searchindex.LangChinese,
		FetchedAt: time.Unix(1_700_000_001, 0).UTC(),
	})
	provider, err := NewLocalIndexProvider(store)
	if err != nil {
		t.Fatal(err)
	}
	if provider.Name() != localIndexProviderName {
		t.Fatalf("Name() = %q", provider.Name())
	}

	results, err := provider.Search(context.Background(), ProviderQuery{
		Text: "Go 编程语言", Limit: 5, Freshness: FreshnessAny,
	})
	if err != nil || len(results) == 0 {
		t.Fatalf("Search() = %#v, %v", results, err)
	}
	if results[0].Rank != 1 || results[0].URL != "https://zh.example/go" || results[0].Score != nil {
		t.Fatalf("first hit = %#v", results[0])
	}
	if results[0].Title == "" || results[0].Snippet == "" {
		t.Fatalf("missing title/snippet = %#v", results[0])
	}
	for index, result := range results {
		if result.Rank != index+1 {
			t.Fatalf("rank[%d] = %d", index, result.Rank)
		}
	}

	truncated, err := provider.Search(context.Background(), ProviderQuery{Text: "Go", Limit: 1})
	if err != nil || len(truncated) != 1 {
		t.Fatalf("limited Search() = %#v, %v", truncated, err)
	}
}

func TestLocalIndexProviderClipsOversizeText(t *testing.T) {
	store := openLocalIndexStore(t)
	mustIndexPage(t, store, searchindex.Page{
		URL: "https://example.com/long", Root: "example.com",
		Title:     strings.Repeat("标题", (MaxProviderTitleBytes/len("标"))+8),
		Body:      strings.Repeat("visible body token ", 80),
		Lang:      searchindex.LangChinese,
		FetchedAt: time.Now().UTC(),
	})
	provider, err := NewLocalIndexProvider(store)
	if err != nil {
		t.Fatal(err)
	}
	results, err := provider.Search(context.Background(), ProviderQuery{Text: "visible body token", Limit: 5})
	if err != nil || len(results) != 1 {
		t.Fatalf("Search() = %#v, %v", results, err)
	}
	if len(results[0].Title) > MaxProviderTitleBytes || len(results[0].Snippet) > MaxProviderSnippetBytes {
		t.Fatalf("clipped bounds title=%d snippet=%d", len(results[0].Title), len(results[0].Snippet))
	}
}

func TestLocalIndexProviderServesSearchService(t *testing.T) {
	store := openLocalIndexStore(t)
	mustIndexPage(t, store, searchindex.Page{
		URL: "https://example.com/go", Root: "example.com", Title: "Go language",
		Body: "The Go programming language is compiled and garbage collected.",
		Lang: searchindex.LangEnglish, FetchedAt: time.Now().UTC(),
	})
	provider, err := NewLocalIndexProvider(store)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(provider)
	if err != nil {
		t.Fatal(err)
	}
	response, err := service.Search(context.Background(), &models.SearchRequest{Query: "Go programming language"})
	if err != nil || response == nil || len(response.Results) == 0 {
		t.Fatalf("Search() = %#v, %v", response, err)
	}
	if response.Results[0].URL != "https://example.com/go" || response.Results[0].Score != nil {
		t.Fatalf("result = %#v", response.Results[0])
	}
}

func TestLocalIndexProviderEmptyQueryReturnsNoHits(t *testing.T) {
	store := openLocalIndexStore(t)
	provider, err := NewLocalIndexProvider(store)
	if err != nil {
		t.Fatal(err)
	}
	results, err := provider.Search(context.Background(), ProviderQuery{Text: "   ", Limit: 5})
	if err != nil || len(results) != 0 {
		t.Fatalf("empty Search() = %#v, %v", results, err)
	}
}

func openLocalIndexStore(t *testing.T) *searchindex.Store {
	t.Helper()
	store, err := searchindex.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func mustIndexPage(t *testing.T, store *searchindex.Store, page searchindex.Page) {
	t.Helper()
	if err := store.Upsert(context.Background(), page); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
}
