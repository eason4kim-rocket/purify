package search

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/use-agent/purify/extract"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/scraper"
	"github.com/use-agent/purify/searchindex"
	"github.com/use-agent/purify/snapshot"
)

func TestIndexFeederIndexesServedPages(t *testing.T) {
	store := openFeedTestStore(t)
	inner := &feedArtifactStub{}
	feeder := NewIndexFeeder(inner, store)
	closer, ok := feeder.(*IndexFeeder)
	if !ok {
		t.Fatal("NewIndexFeeder did not wrap the service")
	}

	if _, err := feeder.FetchPublicArtifact(context.Background(), "https://example.com/doc"); err != nil {
		t.Fatalf("FetchPublicArtifact() error = %v", err)
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	hits, err := store.Query(context.Background(), "goroutine scheduler", 5)
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if len(hits) != 1 || hits[0].URL != "https://example.com/doc" {
		t.Fatalf("served page was not indexed: %#v", hits)
	}
	if hits[0].Root != "example.com" {
		t.Fatalf("root = %q", hits[0].Root)
	}
}

// TestIndexFeederSkipsUnservablePages locks the filters. A private-network or
// error page must never become searchable content, and a fetch failure must not
// leave a partial row behind.
func TestIndexFeederSkipsUnservablePages(t *testing.T) {
	store := openFeedTestStore(t)
	inner := &feedArtifactStub{}
	feeder := NewIndexFeeder(inner, store).(*IndexFeeder)

	inner.statusCode = 404
	_, _ = feeder.FetchPublicArtifact(context.Background(), "https://example.com/missing")
	inner.statusCode = 200
	inner.body = "too short"
	_, _ = feeder.FetchPublicArtifact(context.Background(), "https://example.com/thin")
	inner.body = ""
	inner.url = "http://127.0.0.1:9000/private"
	_, _ = feeder.FetchPublicArtifact(context.Background(), "http://127.0.0.1:9000/private")
	inner.url = ""
	inner.err = errors.New("fetch failed")
	_, _ = feeder.FetchPublicArtifact(context.Background(), "https://example.com/broken")
	if err := feeder.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	count, err := store.CountPages(context.Background())
	if err != nil {
		t.Fatalf("CountPages() error = %v", err)
	}
	if count != 0 {
		t.Fatalf("indexed pages = %d, want none", count)
	}
}

func TestNewIndexFeederPassesThroughWithoutStore(t *testing.T) {
	inner := &feedArtifactStub{}
	if got := NewIndexFeeder(inner, nil); got != ArtifactService(inner) {
		t.Fatalf("NewIndexFeeder(nil store) = %#v, want the inner service", got)
	}
}

func openFeedTestStore(t *testing.T) *searchindex.Store {
	t.Helper()
	store, err := searchindex.Open(t.TempDir() + "/index.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

type feedArtifactStub struct {
	statusCode int
	body       string
	url        string
	err        error
}

func (stub *feedArtifactStub) FetchPublicArtifact(_ context.Context, rawURL string) (*extract.Artifact, error) {
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
			"Work stealing lets an idle processor take runnable goroutines from a busy processor's local queue, " +
			"which keeps every thread fed without a global lock on the run queue."
	}
	final := stub.url
	if final == "" {
		final = rawURL
	}
	return &extract.Artifact{
		Public: &models.ScrapeResponse{
			StatusCode: status,
			FinalURL:   final,
			Content:    body,
			Metadata:   models.Metadata{Title: "Go scheduler"},
		},
		Source: &scraper.ScrapeResult{
			FinalURL:   final,
			FetchedAt:  time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC),
			SnapshotID: snapshot.ID("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		},
	}, nil
}

func (stub *feedArtifactStub) ExtractArtifact(context.Context, *extract.Artifact, *models.ExtractRequest) (*models.ExtractResponse, error) {
	return &models.ExtractResponse{Success: true}, nil
}
