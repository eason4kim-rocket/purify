package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	xproxy "golang.org/x/net/proxy"
)

func TestDialExternalHTTPSProxyUsesTLSAndAuth(t *testing.T) {
	authorization := make(chan string, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorization <- request.Header.Get("Proxy-Authorization")
		connection, buffered, err := writer.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer connection.Close()
		_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\nContent-Length: 0\r\n\r\n")
		_ = buffered.Flush()
		payload := make([]byte, 4)
		if _, err := io.ReadFull(buffered, payload); err == nil {
			_, _ = connection.Write(payload)
		}
	}))
	t.Cleanup(server.Close)

	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	parsed, _ := url.Parse(server.URL)
	parsed.User = url.UserPassword("relay-user", "relay-pass")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connection, err := dialExternalHTTPConnectWithTLSConfig(ctx, parsed, "target.test:443", &tls.Config{RootCAs: roots})
	if err != nil {
		t.Fatalf("dialExternalHTTPConnectWithTLSConfig() error = %v", err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte("ping")); err != nil {
		t.Fatalf("tunnel write: %v", err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(connection, echo); err != nil {
		t.Fatalf("tunnel read: %v", err)
	}
	if string(echo) != "ping" {
		t.Fatalf("tunnel echo = %q", echo)
	}
	if got := <-authorization; got != "Basic cmVsYXktdXNlcjpyZWxheS1wYXNz" {
		t.Fatalf("Proxy-Authorization = %q", got)
	}
}

func TestDialExternalHTTPSProxyRejectsUntrustedCertificate(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(server.Close)
	parsed, _ := url.Parse(server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := dialExternalHTTPConnectWithTLSConfig(ctx, parsed, "target.test:443", &tls.Config{RootCAs: x509.NewCertPool()})
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("dial error = %v, want certificate validation failure", err)
	}
}

func TestDialExternalHTTPSProxyHandshakeHonorsContext(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			defer connection.Close()
			time.Sleep(time.Second)
		}
	}()
	parsed, _ := url.Parse("https://" + listener.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = dialExternalHTTPConnect(ctx, parsed, "target.test:443")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("dial error = %v, want context deadline exceeded", err)
	}
}

func TestRelayCloseConcurrentIdempotent(t *testing.T) {
	relay, err := StartRelay("http://127.0.0.1:9")
	if err != nil {
		t.Fatalf("StartRelay() error = %v", err)
	}

	const callers = 64
	results := make(chan error, callers)
	var waitGroup sync.WaitGroup
	for i := 0; i < callers; i++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			results <- relay.Close()
		}()
	}
	waitGroup.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}
	if err := relay.Close(); err != nil {
		t.Fatalf("repeated Close() error = %v", err)
	}
	select {
	case <-relay.done:
	default:
		t.Fatal("relay done channel remains open")
	}
}

func TestRelayCloseTerminatesEstablishedTunnel(t *testing.T) {
	handlerDone := make(chan struct{})
	externalProxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		connection, buffered, err := writer.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer close(handlerDone)
		defer connection.Close()
		_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\nContent-Length: 0\r\n\r\n")
		_ = buffered.Flush()
		buffer := make([]byte, 32)
		for {
			count, readErr := buffered.Read(buffer)
			if count > 0 {
				if _, writeErr := connection.Write(buffer[:count]); writeErr != nil {
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}))
	t.Cleanup(externalProxy.Close)

	relay, err := StartRelay(externalProxy.URL)
	if err != nil {
		t.Fatalf("StartRelay() error = %v", err)
	}
	dialer, err := xproxy.SOCKS5("tcp", relay.Addr(), nil, &net.Dialer{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SOCKS5() error = %v", err)
	}
	connection, err := dialer.Dial("tcp", "target.test:80")
	if err != nil {
		_ = relay.Close()
		t.Fatalf("relay tunnel dial error = %v", err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte("ping")); err != nil {
		t.Fatalf("relay tunnel write: %v", err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(connection, echo); err != nil || string(echo) != "ping" {
		t.Fatalf("relay tunnel echo = %q, error = %v", echo, err)
	}

	started := time.Now()
	if err := relay.Close(); err != nil {
		t.Fatalf("Relay.Close() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("Relay.Close() elapsed = %v, want prompt active-tunnel shutdown", elapsed)
	}
	_ = connection.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := connection.Read(make([]byte, 1)); err == nil {
		t.Fatal("established relay tunnel remained readable after Close")
	}
	select {
	case <-handlerDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("external proxy handler did not exit after Relay.Close")
	}
}
