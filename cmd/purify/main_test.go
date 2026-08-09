package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/use-agent/purify/config"
	"github.com/use-agent/purify/engine"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/scraper"
)

type boundedRodScraperStub struct {
	maximumBodyBytes int64
	request          *models.ScrapeRequest
	result           *scraper.ScrapeResult
}

func (stub *boundedRodScraperStub) DoScrapeRodBounded(_ context.Context, request *models.ScrapeRequest, maximumBodyBytes int64) (*scraper.ScrapeResult, error) {
	stub.request = request
	stub.maximumBodyBytes = maximumBodyBytes
	return stub.result, nil
}

func TestNewRodFetchPassesMaximumBodyBytesIntoScraper(t *testing.T) {
	stub := &boundedRodScraperStub{result: &scraper.ScrapeResult{
		RawHTML:     "<html>bounded</html>",
		Title:       "bounded",
		StatusCode:  http.StatusOK,
		FinalURL:    "https://example.test/final",
		ContentType: "text/html",
	}}
	fetch := newRodFetch(stub)
	result, err := fetch(context.Background(), &engine.FetchRequest{
		URL:              "https://example.test/start",
		Timeout:          2 * time.Second,
		MaximumBodyBytes: 12345,
	})
	if err != nil {
		t.Fatalf("newRodFetch callback error = %v", err)
	}
	if stub.maximumBodyBytes != 12345 {
		t.Fatalf("DoScrapeRodBounded maximum = %d", stub.maximumBodyBytes)
	}
	if stub.request == nil || stub.request.URL != "https://example.test/start" {
		t.Fatalf("mapped scrape request = %#v", stub.request)
	}
	if result.HTML != stub.result.RawHTML || result.FinalURL != stub.result.FinalURL {
		t.Fatalf("mapped fetch result = %#v", result)
	}
}

func TestOpenSnapshotStoreDisabledDoesNotWrite(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "must-not-exist")
	store, err := openSnapshotStore(config.StorageConfig{DataDir: dataDir, SnapshotEnabled: false})
	if err != nil {
		t.Fatalf("openSnapshotStore() error = %v", err)
	}
	if store != nil {
		t.Fatal("openSnapshotStore() returned a store while disabled")
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("disabled snapshot setup touched disk: stat error = %v", err)
	}
}

func TestNewOutboundPolicyValidatesConfiguredProxy(t *testing.T) {
	policy, err := newOutboundPolicy("")
	if err != nil {
		t.Fatalf("newOutboundPolicy(direct) error = %v", err)
	}
	if policy == nil {
		t.Fatal("newOutboundPolicy(direct) returned nil")
	}

	for _, proxyURL := range []string{
		"not a proxy URL",
		"ftp://proxy.example:21",
		"http://proxy.example:0",
		"socks5://:1080",
	} {
		t.Run(proxyURL, func(t *testing.T) {
			if policy, err := newOutboundPolicy(proxyURL); err == nil || policy != nil {
				t.Fatalf("newOutboundPolicy(%q) = (%#v, %v), want nil + error", proxyURL, policy, err)
			}
		})
	}
}

func TestVerifyRevisitTimeoutUsesExistingScraperBounds(t *testing.T) {
	for _, test := range []struct {
		name   string
		config config.ScraperConfig
		want   time.Duration
	}{
		{name: "default", config: config.ScraperConfig{DefaultTimeout: 30 * time.Second, MaxTimeout: 120 * time.Second}, want: 30 * time.Second},
		{name: "zero default", config: config.ScraperConfig{}, want: 30 * time.Second},
		{name: "negative default", config: config.ScraperConfig{DefaultTimeout: -time.Second}, want: 30 * time.Second},
		{name: "scraper maximum", config: config.ScraperConfig{DefaultTimeout: 30 * time.Second, MaxTimeout: 5 * time.Second}, want: 5 * time.Second},
		{name: "verification hard maximum", config: config.ScraperConfig{DefaultTimeout: 5 * time.Minute}, want: 120 * time.Second},
		{name: "both maxima", config: config.ScraperConfig{DefaultTimeout: 5 * time.Minute, MaxTimeout: 3 * time.Minute}, want: 120 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := verifyRevisitTimeout(test.config); got != test.want {
				t.Fatalf("verifyRevisitTimeout() = %s, want %s", got, test.want)
			}
		})
	}
}

func TestServeUntilShutdownDrainsNormally(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})}
	t.Cleanup(func() { _ = server.Close() })

	quit := make(chan os.Signal, 1)
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serveUntilShutdown(server, listener, quit, time.Second)
	}()

	client := localHTTPClient(2 * time.Second)
	t.Cleanup(client.CloseIdleConnections)
	response, err := client.Get("http://" + listener.Addr().String())
	if err != nil {
		t.Fatalf("GET running test server: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}

	quit <- syscall.SIGTERM
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("serveUntilShutdown() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("normal shutdown did not return")
	}
}

func TestListenAndServeUntilShutdownReturnsListenFailure(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	server := &http.Server{Addr: occupied.Addr().String(), Handler: http.NotFoundHandler()}
	err = listenAndServeUntilShutdown(server, nil, time.Second)
	if err == nil {
		t.Fatal("listenAndServeUntilShutdown() error = nil")
	}
	var operationError *net.OpError
	if !errors.As(err, &operationError) || !strings.Contains(err.Error(), "listen on") {
		t.Fatalf("listen error = %v, want wrapped net.OpError", err)
	}
}

func TestServeUntilShutdownReturnsServeFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	server := &http.Server{Handler: http.NotFoundHandler()}
	err = serveUntilShutdown(server, listener, nil, time.Second)
	if err == nil {
		t.Fatal("serveUntilShutdown() error = nil")
	}
	if !errors.Is(err, net.ErrClosed) || !strings.Contains(err.Error(), "serve HTTP") {
		t.Fatalf("serve error = %v, want wrapped net.ErrClosed", err)
	}
}

func TestServeUntilShutdownForceClosesAfterDeadline(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	requestStarted := make(chan struct{})
	requestCanceled := make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(requestStarted)
		<-request.Context().Done()
		close(requestCanceled)
	})}
	t.Cleanup(func() { _ = server.Close() })

	quit := make(chan os.Signal, 1)
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serveUntilShutdown(server, listener, quit, 50*time.Millisecond)
	}()

	client := localHTTPClient(2 * time.Second)
	t.Cleanup(client.CloseIdleConnections)
	requestDone := make(chan error, 1)
	go func() {
		response, requestErr := client.Get("http://" + listener.Addr().String())
		if response != nil {
			response.Body.Close()
		}
		requestDone <- requestErr
	}()

	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("active request did not reach handler")
	}
	quit <- syscall.SIGTERM

	select {
	case err := <-serveDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown error = %v, want context deadline exceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("forced shutdown did not return")
	}

	select {
	case <-requestCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("server.Close did not cancel the active request context")
	}
	select {
	case <-requestDone:
	case <-time.After(2 * time.Second):
		t.Fatal("force-closed client request did not return")
	}
}

func localHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: nil,
		},
	}
}
