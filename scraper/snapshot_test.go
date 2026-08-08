package scraper

import (
	"testing"

	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/snapshot"
)

func TestFinalizeScrapePersistsSelectedResult(t *testing.T) {
	store, err := snapshot.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer store.Close()
	scraper := &Scraper{snapshots: store}
	result, err := scraper.finalizeScrape(&models.ScrapeRequest{URL: "https://requested.example"}, &ScrapeResult{
		RawHTML:     "<html><body>captured</body></html>",
		StatusCode:  200,
		FinalURL:    "https://final.example/page",
		EngineUsed:  "http",
		FetchMethod: "http",
		ContentType: "text/html; charset=utf-8",
	})
	if err != nil {
		t.Fatalf("finalizeScrape() error = %v", err)
	}
	if result.SnapshotID == "" || !store.Has(result.SnapshotID) {
		t.Fatalf("SnapshotID = %q, want stored snapshot", result.SnapshotID)
	}
	html, meta, err := store.Get(result.SnapshotID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if string(html) != result.RawHTML || meta.URL != result.FinalURL || meta.Engine != "http" || meta.StatusCode != 200 || meta.ContentType != result.ContentType || meta.FetchedAt.IsZero() {
		t.Fatalf("snapshot html/meta mismatch: html=%q meta=%#v result=%#v", html, meta, result)
	}
}

func TestFinalizeScrapeWithoutStoreDoesNoIO(t *testing.T) {
	scraper := &Scraper{}
	input := &ScrapeResult{RawHTML: "<html></html>", FetchMethod: "browser"}
	result, err := scraper.finalizeScrape(&models.ScrapeRequest{URL: "https://example.com"}, input)
	if err != nil {
		t.Fatalf("finalizeScrape() error = %v", err)
	}
	if result.SnapshotID != "" {
		t.Fatalf("SnapshotID = %q with snapshots disabled", result.SnapshotID)
	}
	if result.FinalURL != "https://example.com" || result.EngineUsed != "browser" || result.ContentType != "text/html" || result.FetchedAt.IsZero() {
		t.Fatalf("result defaults not applied: %#v", result)
	}
}
