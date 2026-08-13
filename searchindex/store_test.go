package searchindex

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenCreatesSchemaAndUpsertsWithoutDuplicateHash(t *testing.T) {
	store := openTestStore(t)
	page := Page{
		URL: "https://example.com/a", Root: "example.com", Title: "Alpha",
		Body: "hello world", Lang: LangEnglish, FetchedAt: time.Unix(1_700_000_000, 0).UTC(),
	}
	if err := store.Upsert(context.Background(), page); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	if err := store.Upsert(context.Background(), page); err != nil {
		t.Fatalf("duplicate Upsert() error = %v", err)
	}
	count, err := store.CountPages(context.Background())
	if err != nil || count != 1 {
		t.Fatalf("CountPages() = %d, %v", count, err)
	}

	page.Body = "hello world changed"
	page.ContentHash = ""
	if err := store.Upsert(context.Background(), page); err != nil {
		t.Fatalf("changed Upsert() error = %v", err)
	}
	if count, err = store.CountPages(context.Background()); err != nil || count != 1 {
		t.Fatalf("CountPages after change = %d, %v", count, err)
	}

	page.URL = "https://example.com/b"
	page.Lang = LangChinese
	page.Body = "你好世界"
	page.ContentHash = ""
	if err := store.Upsert(context.Background(), page); err != nil {
		t.Fatalf("second page Upsert() error = %v", err)
	}
	if count, err = store.CountPages(context.Background()); err != nil || count != 2 {
		t.Fatalf("CountPages two pages = %d, %v", count, err)
	}
}

func TestOpenRejectsEmptyPathAndClosedStore(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("Open(\"\") succeeded")
	}
	store := openTestStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(context.Background(), Page{URL: "https://example.com/", Root: "example.com", Lang: LangEnglish}); err != ErrClosed {
		t.Fatalf("Upsert on closed store = %v", err)
	}
}

func TestUpsertManyWritesChangedPagesOnly(t *testing.T) {
	store := openTestStore(t)
	pages := []Page{
		{URL: "https://example.com/a", Root: "example.com", Title: "A", Body: "alpha", Lang: LangEnglish},
		{URL: "https://example.com/b", Root: "example.com", Title: "B", Body: "bravo", Lang: LangEnglish},
	}
	written, err := store.UpsertMany(context.Background(), pages)
	if err != nil || written != 2 {
		t.Fatalf("UpsertMany() = %d, %v", written, err)
	}
	written, err = store.UpsertMany(context.Background(), pages)
	if err != nil || written != 0 {
		t.Fatalf("duplicate UpsertMany() = %d, %v", written, err)
	}
	pages[1].Body = "bravo changed"
	pages[1].ContentHash = ""
	written, err = store.UpsertMany(context.Background(), pages)
	if err != nil || written != 1 {
		t.Fatalf("changed UpsertMany() = %d, %v", written, err)
	}
}

func TestNormalizePageRequiresURLRootAndLang(t *testing.T) {
	if _, err := normalizePage(Page{}); err == nil {
		t.Fatal("empty page accepted")
	}
	if _, err := normalizePage(Page{URL: "https://example.com/", Root: "example.com", Lang: "de"}); err == nil {
		t.Fatal("unknown lang accepted")
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
