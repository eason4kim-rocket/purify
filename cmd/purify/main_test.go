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
)

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
