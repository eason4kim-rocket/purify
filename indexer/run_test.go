package indexer

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/use-agent/purify/searchindex"
)

func TestRunIndexesDiscoveredPagesAndHonorsRobots(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/robots.txt":
			_, _ = io.WriteString(writer, "User-agent: *\nDisallow: /hidden\n")
		case "/hidden":
			_, _ = io.WriteString(writer, "<html><title>secret</title><body>should not index</body></html>")
		default:
			_, _ = io.WriteString(writer, "<html><title>Hello</title><body>"+strings.Repeat("visible content ", 40)+"</body></html>")
		}
	}))
	t.Cleanup(server.Close)

	store, err := searchindex.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	fetcher, err := NewFetcher(FetcherConfig{AllowPrivateNetworks: true})
	if err != nil {
		t.Fatal(err)
	}
	discoverer := stubDiscoverer{urls: []string{server.URL + "/", server.URL + "/hidden"}}
	stats, err := Run(context.Background(), store, fetcher, discoverer, []string{server.URL + "/"}, RunConfig{
		Workers: 1, BatchSize: 2, MaxPages: 10, AllowPrivate: true,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if stats.Indexed < 1 {
		t.Fatalf("indexed = %#v", stats)
	}
	if stats.RobotsDeny < 1 {
		t.Fatalf("robots deny = %#v", stats)
	}
	hits, err := store.Query(context.Background(), "visible content", 5)
	if err != nil || len(hits) == 0 {
		t.Fatalf("Query() = %#v, %v", hits, err)
	}
	hidden, err := store.Query(context.Background(), "should not index", 5)
	if err != nil {
		t.Fatalf("hidden Query() error = %v", err)
	}
	for _, hit := range hidden {
		if strings.Contains(hit.URL, "/hidden") {
			t.Fatalf("robots-denied URL indexed: %#v", hit)
		}
	}
}
