package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newArchiveFixture(t *testing.T, availableJSON, snapshotHTML string) *ArchiveEngine {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/wayback/available", func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("url") == "" {
			t.Errorf("availability request missing url param")
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(availableJSON))
	})
	mux.HandleFunc("/web/", func(writer http.ResponseWriter, request *http.Request) {
		if !strings.Contains(request.URL.Path, "id_/") {
			t.Errorf("snapshot path missing id_ marker: %q", request.URL.Path)
		}
		writer.Header().Set("Content-Type", "text/html")
		_, _ = writer.Write([]byte(snapshotHTML))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return NewArchiveEngine(ArchiveConfig{
		AvailabilityEndpoint: server.URL + "/wayback/available",
		SnapshotBase:         server.URL + "/web",
	})
}

func TestArchiveEngineServesClosestSnapshot(t *testing.T) {
	engine := newArchiveFixture(t,
		`{"archived_snapshots":{"closest":{"available":true,"status":"200","timestamp":"20200101000000"}}}`,
		"<html><head><title>archived</title></head><body>archived content</body></html>")

	result, err := engine.Fetch(context.Background(), &FetchRequest{
		URL:     "https://example.com/page",
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if !strings.Contains(result.HTML, "archived content") {
		t.Fatalf("Fetch() HTML = %q", result.HTML)
	}
	if result.Title != "archived" {
		t.Fatalf("Fetch() title = %q", result.Title)
	}
	if result.EngineName != "wayback-archive" {
		t.Fatalf("Fetch() engine = %q, want wayback-archive provenance", result.EngineName)
	}
	if result.FinalURL != "https://example.com/page" {
		t.Fatalf("Fetch() final URL = %q, want the original URL", result.FinalURL)
	}
}

func TestArchiveEngineFailsWhenNoSnapshot(t *testing.T) {
	engine := newArchiveFixture(t, `{"archived_snapshots":{}}`, "unused")
	_, err := engine.Fetch(context.Background(), &FetchRequest{URL: "https://example.com/missing", Timeout: 5 * time.Second})
	if err == nil {
		t.Fatal("Fetch() error = nil, want a miss when no snapshot exists")
	}
}

func TestArchiveEngineRejectsNon200Snapshot(t *testing.T) {
	engine := newArchiveFixture(t,
		`{"archived_snapshots":{"closest":{"available":true,"status":"404","timestamp":"20200101000000"}}}`,
		"unused")
	_, err := engine.Fetch(context.Background(), &FetchRequest{URL: "https://example.com/page", Timeout: 5 * time.Second})
	if err == nil {
		t.Fatal("Fetch() error = nil, want rejection of a non-200 archived capture")
	}
}

// TestArchiveEngineNeverServesVerifyOrBrowserOptions locks that the archive is
// not evidence and cannot stand in for browser-only fetches.
func TestArchiveEngineNeverServesVerifyOrBrowserOptions(t *testing.T) {
	engine := NewArchiveEngine(ArchiveConfig{})
	for name, req := range map[string]*FetchRequest{
		"observation": {URL: "https://example.com", Mode: FetchModeObservation},
		"stealth":     {URL: "https://example.com", Stealth: true},
		"actions":     {URL: "https://example.com", Actions: []Action{{Type: "click"}}},
		"cdp":         {URL: "https://example.com", CDPURL: "ws://browser.invalid/x"},
	} {
		t.Run(name, func(t *testing.T) {
			if engine.Supports(req) {
				t.Fatalf("Supports(%s) = true, want the archive to decline", name)
			}
			if _, err := engine.Fetch(context.Background(), req); !errors.Is(err, ErrUnsupportedRequest) {
				t.Fatalf("Fetch(%s) error = %v, want ErrUnsupportedRequest", name, err)
			}
		})
	}
}
