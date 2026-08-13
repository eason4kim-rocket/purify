package searchindex

import (
	"context"
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

func mustUpsert(t *testing.T, store *Store, page Page) {
	t.Helper()
	if err := store.Upsert(context.Background(), page); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
}
