package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/use-agent/purify/discovery"
	"github.com/use-agent/purify/searchindex"
)

func TestLoadSeedsDeduplicatesAndRejectsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seeds.json")
	if err := os.WriteFile(path, []byte(`[" https://a.example/ ", "https://a.example/", "https://b.example/"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	seeds, err := LoadSeeds(path)
	if err != nil || len(seeds) != 2 || seeds[0] != "https://a.example/" {
		t.Fatalf("LoadSeeds() = %#v, %v", seeds, err)
	}
	empty := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(empty, []byte(`["", " "]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSeeds(empty); err == nil {
		t.Fatal("empty seeds accepted")
	}
}

func TestSeedFrontierEnqueuesDiscoveredURLs(t *testing.T) {
	store, err := searchindex.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	discoverer := stubDiscoverer{urls: []string{"https://a.example/docs", "https://a.example/about"}}
	n, err := SeedFrontier(context.Background(), store, discoverer, []string{"https://a.example/"}, true)
	if err != nil || n != 3 {
		t.Fatalf("SeedFrontier() = %d, %v", n, err)
	}
	n, err = SeedFrontier(context.Background(), store, discoverer, []string{"https://a.example/"}, true)
	if err != nil || n != 0 {
		t.Fatalf("second SeedFrontier() = %d, %v", n, err)
	}
}

// TestSeedFrontierSkipsForeignLocalePaths locks the crawl-budget guard:
// sitemaps of multi-locale documentation sites hand back every translation of
// every page, and only en/zh variants may reach the frontier. Technical paths
// that merely look like locale tags ("/js/", uppercase "/TR/") must survive.
func TestSeedFrontierSkipsForeignLocalePaths(t *testing.T) {
	store, err := searchindex.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	discoverer := stubDiscoverer{urls: []string{
		"https://a.example/de/docs/Web",
		"https://a.example/pt-BR/docs/Web",
		"https://a.example/ja/",
		"https://a.example/en-US/docs/Web",
		"https://a.example/zh-cn/3/tutorial",
		"https://a.example/js/js-window.html",
		"https://a.example/TR/html52/",
	}}
	n, err := SeedFrontier(context.Background(), store, discoverer, []string{"https://a.example/"}, true)
	if err != nil {
		t.Fatalf("SeedFrontier() error = %v", err)
	}
	// Seed + en-US + zh-cn + js + TR; de, pt-BR, and ja stay out.
	if n != 5 {
		t.Fatalf("SeedFrontier() = %d, want 5", n)
	}
}

type stubDiscoverer struct{ urls []string }

func (s stubDiscoverer) Discover(context.Context, string) (*discovery.Result, error) {
	return &discovery.Result{URLs: append([]string(nil), s.urls...)}, nil
}
