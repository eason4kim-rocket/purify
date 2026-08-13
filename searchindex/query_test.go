package searchindex

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestQueryPrefersMatchingLanguageAndMergesByRank(t *testing.T) {
	store := openTestStore(t)
	mustUpsert(t, store, Page{
		URL: "https://en.example/go", Root: "example", Title: "Go language",
		Body: "The Go programming language is compiled.", Lang: LangEnglish, FetchedAt: time.Now(),
	})
	mustUpsert(t, store, Page{
		URL: "https://zh.example/go", Root: "example", Title: "Go 语言",
		Body: "Go 编程语言是编译型的。", Lang: LangChinese, FetchedAt: time.Now(),
	})
	mustUpsert(t, store, Page{
		URL: "https://en.example/other", Root: "example", Title: "Other",
		Body: "unrelated english document about rivers", Lang: LangEnglish, FetchedAt: time.Now(),
	})

	hits, err := store.Query(context.Background(), "Go 编程语言", 5)
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("Query returned no hits")
	}
	if hits[0].Lang != LangChinese || hits[0].URL != "https://zh.example/go" {
		t.Fatalf("preferred first hit = %#v", hits[0])
	}

	english, err := store.Query(context.Background(), "Go programming language", 5)
	if err != nil || len(english) == 0 || english[0].URL != "https://en.example/go" {
		t.Fatalf("english Query() = %#v, %v", english, err)
	}
}

func TestQueryEscapesFTSOperators(t *testing.T) {
	store := openTestStore(t)
	mustUpsert(t, store, Page{
		URL: "https://example.com/and", Root: "example.com", Title: "AND OR",
		Body: "token about apples", Lang: LangEnglish, FetchedAt: time.Now(),
	})
	hits, err := store.Query(context.Background(), `AND OR "apples"`, 5)
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if len(hits) != 1 || hits[0].URL != "https://example.com/and" {
		t.Fatalf("escaped query hits = %#v", hits)
	}
}

// TestQueryKeepsPartialMatches locks OR semantics. Joining phrases with
// whitespace means AND in FTS5, which made ordinary multi-word queries return
// nothing and made bm25's partial-match ranking unreachable.
func TestQueryKeepsPartialMatches(t *testing.T) {
	store := openTestStore(t)
	mustUpsert(t, store, Page{
		URL: "https://example.com/nginx", Root: "example.com", Title: "Nginx reverse proxy",
		Body:      "How to configure the nginx reverse proxy read timeout for upstream servers.",
		Lang:      LangEnglish,
		FetchedAt: time.Now(),
	})
	mustUpsert(t, store, Page{
		URL: "https://example.com/rivers", Root: "example.com", Title: "Rivers",
		Body: "An unrelated document about rivers and boats.", Lang: LangEnglish, FetchedAt: time.Now(),
	})

	// Only "nginx" is present anywhere; under AND this returned zero hits.
	hits, err := store.Query(context.Background(), "nginx kubernetes helm", 5)
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if len(hits) != 1 || hits[0].URL != "https://example.com/nginx" {
		t.Fatalf("partial-match hits = %#v", hits)
	}

	// A page matching more terms must outrank one matching fewer.
	ranked, err := store.Query(context.Background(), "nginx proxy timeout rivers", 5)
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if len(ranked) != 2 || ranked[0].URL != "https://example.com/nginx" {
		t.Fatalf("bm25 partial ranking = %#v", ranked)
	}
}

// TestQueryMatchesTwoCharacterChinese locks the bigram rewrite. The trigram
// tokenizer needs three characters, so it silently dropped the two-character
// words that dominate real Chinese queries.
func TestQueryMatchesTwoCharacterChinese(t *testing.T) {
	store := openTestStore(t)
	mustUpsert(t, store, Page{
		URL: "https://zh.example/redis", Root: "example", Title: "Redis 配置指南",
		Body:      "这是一篇讲 Redis 配置和价格的中文文档，包含缓存超时设置。",
		Lang:      LangChinese,
		FetchedAt: time.Now(),
	})
	mustUpsert(t, store, Page{
		URL: "https://zh.example/other", Root: "example", Title: "无关页面",
		Body: "这一页讲别的东西，完全无关。", Lang: LangChinese, FetchedAt: time.Now(),
	})

	for _, query := range []string{"配置", "价格", "缓存"} {
		hits, err := store.Query(context.Background(), query, 5)
		if err != nil {
			t.Fatalf("Query(%q) error = %v", query, err)
		}
		if len(hits) == 0 || hits[0].URL != "https://zh.example/redis" {
			t.Fatalf("Query(%q) hits = %#v", query, hits)
		}
		// The snippet is cut from the stored body, so it must be original text.
		if hits[0].Snippet == "" || !strings.Contains(hits[0].Snippet, query) {
			t.Fatalf("Query(%q) snippet = %q", query, hits[0].Snippet)
		}
	}
}

// TestUpsertReplacesRewrittenChinesePostings locks delete/insert symmetry: an
// external-content delete replays indexed tokens, so a delete built from raw
// text would strand the bigram postings and keep serving the stale page.
func TestUpsertReplacesRewrittenChinesePostings(t *testing.T) {
	store := openTestStore(t)
	page := Page{
		URL: "https://zh.example/doc", Root: "example", Title: "旧标题",
		Body: "这里讲的是缓存策略。", Lang: LangChinese, FetchedAt: time.Now(),
	}
	mustUpsert(t, store, page)
	page.Title = "新标题"
	page.Body = "这里改成讲价格模型。"
	page.ContentHash = ""
	mustUpsert(t, store, page)

	stale, err := store.Query(context.Background(), "缓存", 5)
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("stale bigram postings survived: %#v", stale)
	}
	fresh, err := store.Query(context.Background(), "价格", 5)
	if err != nil || len(fresh) != 1 {
		t.Fatalf("refreshed Query() = %#v, %v", fresh, err)
	}
}

func mustUpsert(t *testing.T, store *Store, page Page) {
	t.Helper()
	if err := store.Upsert(context.Background(), page); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
}
