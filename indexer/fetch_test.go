package indexer

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetcherHonorsConditionalGetAndByteLimit(t *testing.T) {
	var sawNoneMatch bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("User-Agent") != defaultUserAgent {
			t.Errorf("User-Agent = %q", request.Header.Get("User-Agent"))
		}
		if request.Header.Get("If-None-Match") == `"abc"` {
			sawNoneMatch = true
			writer.WriteHeader(http.StatusNotModified)
			return
		}
		writer.Header().Set("ETag", `"abc"`)
		writer.Header().Set("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
		_, _ = io.WriteString(writer, "<html>ok</html>")
	}))
	t.Cleanup(server.Close)

	fetcher, err := NewFetcher(FetcherConfig{AllowPrivateNetworks: true})
	if err != nil {
		t.Fatal(err)
	}
	first, err := fetcher.Get(context.Background(), server.URL+"/", "", "")
	if err != nil || first.Status != http.StatusOK || !strings.Contains(string(first.Body), "ok") {
		t.Fatalf("first Get() = %#v, %v", first, err)
	}
	_, err = fetcher.Get(context.Background(), server.URL+"/", first.ETag, first.LastMod)
	if err != ErrNotModified || !sawNoneMatch {
		t.Fatalf("conditional Get() = %v, saw=%v", err, sawNoneMatch)
	}

	limited, err := NewFetcher(FetcherConfig{AllowPrivateNetworks: true, MaxBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := limited.Get(context.Background(), server.URL+"/", "", ""); err != ErrTooLarge {
		t.Fatalf("byte limit error = %v", err)
	}
}

func TestFetcherRejectsPrivateByDefault(t *testing.T) {
	fetcher, err := NewFetcher(FetcherConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fetcher.Get(context.Background(), "http://127.0.0.1/", "", ""); err == nil {
		t.Fatal("loopback fetch succeeded without AllowPrivateNetworks")
	}
}

func TestNeedsRenderMarksShortBodies(t *testing.T) {
	if !NeedsRender("tiny") {
		t.Fatal("short body not marked")
	}
	if NeedsRender(strings.Repeat("word ", 80)) {
		t.Fatal("long body marked for render")
	}
}
