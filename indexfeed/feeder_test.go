package indexfeed

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/scrape"
	"github.com/use-agent/purify/scraper"
	"github.com/use-agent/purify/searchindex"
	"github.com/use-agent/purify/snapshot"
)

func TestFeederIndexesFetchedPages(t *testing.T) {
	store := openTestStore(t)
	inner := &runnerStub{}
	feeder := New(inner, store).(*Feeder)

	if _, err := feeder.Run(context.Background(), &models.ScrapeRequest{URL: "https://example.com/doc"}, nil); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if err := feeder.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	hits, err := store.Query(context.Background(), "goroutine scheduler", 5)
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if len(hits) != 1 || hits[0].URL != "https://example.com/doc" {
		t.Fatalf("fetched page was not indexed: %#v", hits)
	}
	if hits[0].Root != "example.com" {
		t.Fatalf("root = %q", hits[0].Root)
	}
}

// TestFeederSkipsUnservablePages locks the filters. A private-network address,
// an error page, or a thin shell must never become searchable content, and a
// failed fetch must not leave a row behind.
func TestFeederSkipsUnservablePages(t *testing.T) {
	store := openTestStore(t)
	inner := &runnerStub{}
	feeder := New(inner, store).(*Feeder)
	ctx := context.Background()
	request := &models.ScrapeRequest{URL: "https://example.com/page"}

	inner.statusCode = 404
	_, _ = feeder.Run(ctx, request, nil)
	inner.statusCode = 200
	inner.failure = true
	_, _ = feeder.Run(ctx, request, nil)
	inner.failure = false
	inner.body = "too short"
	_, _ = feeder.Run(ctx, request, nil)
	inner.body = ""
	inner.url = "http://127.0.0.1:9000/private"
	_, _ = feeder.Run(ctx, request, nil)
	inner.url = ""
	inner.err = errors.New("fetch failed")
	_, _ = feeder.Run(ctx, request, nil)

	if err := feeder.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	count, err := store.CountPages(ctx)
	if err != nil {
		t.Fatalf("CountPages() error = %v", err)
	}
	if count != 0 {
		t.Fatalf("indexed pages = %d, want none", count)
	}
}

// TestFeederNeverBlocksOrFailsTheRequest locks the contract that indexing is a
// side effect: a saturated queue drops pages instead of stalling the caller.
func TestFeederNeverBlocksOrFailsTheRequest(t *testing.T) {
	store := openTestStore(t)
	feeder := New(&runnerStub{}, store).(*Feeder)
	ctx := context.Background()
	for index := range queueDepth * 2 {
		result, err := feeder.Run(ctx, &models.ScrapeRequest{URL: "https://example.com/page"}, nil)
		if err != nil || result == nil {
			t.Fatalf("Run(%d) = %#v, %v", index, result, err)
		}
	}
	if err := feeder.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

// TestFeederSkipsArchivePages locks that a Wayback fallback result never enters
// the index: an archived copy under the live URL would masquerade as a current
// capture.
func TestFeederSkipsArchivePages(t *testing.T) {
	store := openTestStore(t)
	feeder := New(&runnerStub{engineUsed: "wayback-archive"}, store).(*Feeder)
	ctx := context.Background()

	if _, err := feeder.Run(ctx, &models.ScrapeRequest{URL: "https://example.com/doc"}, nil); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if err := feeder.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	count, err := store.CountPages(ctx)
	if err != nil {
		t.Fatalf("CountPages() error = %v", err)
	}
	if count != 0 {
		t.Fatalf("archived page was indexed: count = %d, want 0", count)
	}
}

func TestNewPassesThroughWithoutStore(t *testing.T) {
	inner := &runnerStub{}
	if got := New(inner, nil); got != Runner(inner) {
		t.Fatalf("New(nil store) = %#v, want the inner runner", got)
	}
}

func openTestStore(t *testing.T) *searchindex.Store {
	t.Helper()
	store, err := searchindex.Open(t.TempDir() + "/index.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

type runnerStub struct {
	statusCode int
	body       string
	url        string
	engineUsed string
	failure    bool
	err        error
}

func (stub *runnerStub) Run(_ context.Context, request *models.ScrapeRequest, _ scrape.Observer) (*scrape.Result, error) {
	if stub.err != nil {
		return nil, stub.err
	}
	status := stub.statusCode
	if status == 0 {
		status = 200
	}
	body := stub.body
	if body == "" {
		body = "The Go scheduler multiplexes goroutine execution onto operating system threads. " +
			"A goroutine parked on a channel receive is requeued when the scheduler finds runnable work. " +
			"Work stealing lets an idle processor take runnable goroutines from a busy processor's queue."
	}
	final := stub.url
	if final == "" {
		final = request.URL
	}
	return &scrape.Result{
		Response: &models.ScrapeResponse{
			Success:    !stub.failure,
			StatusCode: status,
			FinalURL:   final,
			Content:    body,
			EngineUsed: stub.engineUsed,
			Metadata:   models.Metadata{Title: "Go scheduler"},
		},
		Source: &scraper.ScrapeResult{
			FinalURL:   final,
			FetchedAt:  time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC),
			SnapshotID: snapshot.ID("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		},
	}, nil
}

// TestFeederFlushesWithoutAFullBatch locks the time-based flush. Batching on
// count alone stranded pages until traffic happened to fill a batch — exactly
// the early low-traffic case — and lost them on a crash.
func TestFeederFlushesWithoutAFullBatch(t *testing.T) {
	store := openTestStore(t)
	feeder := newFeeder(&runnerStub{}, store, 20*time.Millisecond)
	t.Cleanup(func() { _ = feeder.Close() })

	if _, err := feeder.Run(context.Background(), &models.ScrapeRequest{URL: "https://example.com/one"}, nil); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		count, err := store.CountPages(context.Background())
		if err != nil {
			t.Fatalf("CountPages() error = %v", err)
		}
		if count == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("one page never flushed on its own: count = %d", count)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
