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

// TestQueryProximitySuppressesHubPages locks the NEAR tier: a link-directory
// page mentions every term somewhere in its soup, but only the content page
// holds them inside one passage, so the hub must stay out of the results.
func TestQueryProximitySuppressesHubPages(t *testing.T) {
	store := openTestStore(t)
	filler := strings.Repeat("unrelated filler words about many other subjects entirely ", 12)
	mustUpsert(t, store, Page{
		URL: "https://docs.example/status", Root: "docs.example", Title: "HTTP status codes explained",
		Body:      "Every http response carries a status code that tells the client what happened.",
		Lang:      LangEnglish,
		FetchedAt: time.Now(),
	})
	mustUpsert(t, store, Page{
		URL: "https://hub.example/sitemap", Root: "hub.example", Title: "All articles",
		Body:      "http client tutorial " + filler + " status " + filler + " code reference " + filler,
		Lang:      LangEnglish,
		FetchedAt: time.Now(),
	})

	hits, err := store.Query(context.Background(), "http status code", 5)
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if len(hits) != 1 || hits[0].URL != "https://docs.example/status" {
		t.Fatalf("NEAR-tier hits = %#v", hits)
	}
}

// TestQueryPrefersPagesMatchingAllTerms locks the AND-first pass: while any
// page holds every term, pages matching only one common term stay out of the
// results entirely instead of leaking into the head of the ranking.
func TestQueryPrefersPagesMatchingAllTerms(t *testing.T) {
	store := openTestStore(t)
	mustUpsert(t, store, Page{
		URL: "https://k8s.example/restart", Root: "k8s.example", Title: "Kubernetes pod restart policy",
		Body:      "The restart policy for pods controls how kubernetes restarts containers.",
		Lang:      LangEnglish,
		FetchedAt: time.Now(),
	})
	mustUpsert(t, store, Page{
		URL: "https://blog.example/privacy", Root: "blog.example", Title: "Site policy",
		Body: "Our privacy policy explains cookie retention.", Lang: LangEnglish, FetchedAt: time.Now(),
	})

	hits, err := store.Query(context.Background(), "kubernetes restart policy", 5)
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if len(hits) != 1 || hits[0].URL != "https://k8s.example/restart" {
		t.Fatalf("AND-first hits = %#v", hits)
	}
}

// TestQueryAndPassSuppressesOtherLanguageNoise locks the global strictness
// rule: when the preferred language satisfies AND, the other language table
// must not interleave weak single-term matches into the merged ranking.
func TestQueryAndPassSuppressesOtherLanguageNoise(t *testing.T) {
	store := openTestStore(t)
	mustUpsert(t, store, Page{
		URL: "https://k8s.example/restart", Root: "k8s.example", Title: "Kubernetes pod restart policy",
		Body:      "The restart policy for pods controls how kubernetes restarts containers.",
		Lang:      LangEnglish,
		FetchedAt: time.Now(),
	})
	mustUpsert(t, store, Page{
		URL: "https://zh.example/k8s", Root: "example", Title: "Kubernetes 集群入门",
		Body: "这一篇介绍 kubernetes 集群的部署。", Lang: LangChinese, FetchedAt: time.Now(),
	})

	hits, err := store.Query(context.Background(), "kubernetes restart policy", 5)
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	for _, hit := range hits {
		if hit.URL == "https://zh.example/k8s" {
			t.Fatalf("other-language partial match leaked into AND results: %#v", hits)
		}
	}
	if len(hits) != 1 || hits[0].URL != "https://k8s.example/restart" {
		t.Fatalf("AND-first hits = %#v", hits)
	}
}

// TestQueryKeepsPartialMatches locks the OR fallback. When no page holds every
// term, the query degrades to OR so bm25 can still rank partial matches
// instead of returning nothing.
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

// TestReindexRebuildsFromCompressedBodies locks the reason the compressed body
// is kept at all: a tokenizer change must cost one local pass, not a re-crawl.
func TestReindexRebuildsFromCompressedBodies(t *testing.T) {
	store := openTestStore(t)
	mustUpsert(t, store, Page{
		URL: "https://example.com/en", Root: "example.com", Title: "Nginx proxy",
		Body: "configure the nginx reverse proxy read timeout", Lang: LangEnglish, FetchedAt: time.Now(),
	})
	mustUpsert(t, store, Page{
		URL: "https://zh.example/cn", Root: "example", Title: "Redis 配置",
		Body: "这是讲 Redis 配置和价格的文档。", Lang: LangChinese, FetchedAt: time.Now(),
	})

	rebuilt, err := store.Reindex(context.Background())
	if err != nil {
		t.Fatalf("Reindex() error = %v", err)
	}
	if rebuilt != 2 {
		t.Fatalf("rebuilt = %d, want 2", rebuilt)
	}
	for _, query := range []string{"nginx timeout", "配置", "价格"} {
		hits, err := store.Query(context.Background(), query, 5)
		if err != nil || len(hits) == 0 {
			t.Fatalf("after reindex Query(%q) = %#v, %v", query, hits, err)
		}
		if hits[0].Snippet == "" {
			t.Fatalf("after reindex Query(%q) lost its snippet", query)
		}
	}
}

// TestQueryMatchesChineseCompoundParts locks what search-mode segmentation buys
// over character bigrams: a compound is indexed together with its parts, so a
// query for either half reaches the page without matching arbitrary character
// pairs that straddle two words.
func TestQueryMatchesChineseCompoundParts(t *testing.T) {
	store := openTestStore(t)
	mustUpsert(t, store, Page{
		URL: "https://zh.example/pricing", Root: "example", Title: "计费说明",
		Body: "价格按内存容量计费，缓存命中率越高单位成本越低。", Lang: LangChinese, FetchedAt: time.Now(),
	})
	mustUpsert(t, store, Page{
		URL: "https://zh.example/rivers", Root: "example", Title: "河流",
		Body: "这一页讲河流与航运，和计价无关。", Lang: LangChinese, FetchedAt: time.Now(),
	})

	for _, query := range []string{"内存容量", "内存", "容量", "命中率"} {
		hits, err := store.Query(context.Background(), query, 5)
		if err != nil {
			t.Fatalf("Query(%q) error = %v", query, err)
		}
		if len(hits) == 0 || hits[0].URL != "https://zh.example/pricing" {
			t.Fatalf("Query(%q) hits = %#v", query, hits)
		}
	}

	// "存容" spans the boundary of 内存|容量 and is not a word, so a word index
	// must not match it the way a character bigram index would.
	spurious, err := store.Query(context.Background(), "存容", 5)
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if len(spurious) != 0 {
		t.Fatalf("cross-word fragment matched: %#v", spurious)
	}
}

// TestEnglishQueryNeverLoadsChineseDictionary locks the lazy-load claim. Every
// query probes both language tables, so without a Han short-circuit an
// English-only deployment would still pay the dictionary's ~130 MB on its very
// first query.
func TestEnglishQueryNeverLoadsChineseDictionary(t *testing.T) {
	if segmenterLoaded() {
		t.Skip("another test in this package already built the dictionary")
	}
	store := openTestStore(t)
	mustUpsert(t, store, Page{
		URL: "https://example.com/go", Root: "example.com", Title: "Go scheduler",
		Body: "the go scheduler multiplexes goroutines onto threads", Lang: LangEnglish, FetchedAt: time.Now(),
	})
	if _, err := store.Query(context.Background(), "goroutine scheduler threads", 5); err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if segmenterLoaded() {
		t.Fatal("an English-only query loaded the Chinese dictionary")
	}
}
