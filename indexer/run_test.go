package indexer

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/use-agent/purify/searchindex"
)

// TestRunFetchesDistinctHostsConcurrently locks the worker fan-out. The loop
// used to be sequential with Workers ignored, so one host's crawl-delay stalled
// the whole crawl and throughput collapsed to one page per delay.
func TestRunFetchesDistinctHostsConcurrently(t *testing.T) {
	var mu sync.Mutex
	inFlight, peak := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/robots.txt" {
			_, _ = io.WriteString(writer, "User-agent: *\nCrawl-delay: 5\n")
			return
		}
		mu.Lock()
		inFlight++
		peak = max(peak, inFlight)
		mu.Unlock()
		time.Sleep(150 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		_, _ = io.WriteString(writer, "<html><title>Page</title><body>"+strings.Repeat("indexable words ", 40)+"</body></html>")
	}))
	t.Cleanup(server.Close)

	store, err := searchindex.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// All URLs point at one test server, but each carries its own registrable
	// domain so the frontier hands them to different workers. A five-second
	// crawl-delay makes any cross-host serialization impossible to miss.
	items := make([]searchindex.FrontierItem, 0, 4)
	for index := range 4 {
		items = append(items, searchindex.FrontierItem{
			URL:  fmt.Sprintf("%s/page-%d", server.URL, index),
			Root: fmt.Sprintf("host-%d.example", index),
		})
	}
	if _, err := store.Enqueue(context.Background(), items); err != nil {
		t.Fatal(err)
	}

	fetcher, err := NewFetcher(FetcherConfig{AllowPrivateNetworks: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stats, err := Run(ctx, store, fetcher, stubDiscoverer{}, nil, RunConfig{
		Workers: 4, BatchSize: 4, MaxPages: 10, AllowPrivate: true,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if stats.Indexed != 4 {
		t.Fatalf("indexed = %#v", stats)
	}
	mu.Lock()
	observed := peak
	mu.Unlock()
	if observed < 2 {
		t.Fatalf("peak concurrent fetches = %d, want the workers to overlap across hosts", observed)
	}
}

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
