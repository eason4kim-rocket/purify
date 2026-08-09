package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/use-agent/purify/publicnet"
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

func TestDialExternalHTTPProxyRejectsInvalidConnectTarget(t *testing.T) {
	proxyURL, _ := url.Parse("http://127.0.0.1:9")
	for _, target := range []string{
		"target.test:0",
		"target.test:65536",
		"target.test:https",
		"target.test\r\nX-Injected: yes:443",
		"user@target.test:443",
		"target.test/path:443",
		"[not-an-ipv6-host]:443",
	} {
		if _, err := dialExternalHTTPConnect(context.Background(), proxyURL, target); err == nil || !strings.Contains(err.Error(), "invalid CONNECT target") {
			t.Errorf("dialExternalHTTPConnect(%q) error = %v", target, err)
		}
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

func TestStartDirectRelayRejectsNilDialer(t *testing.T) {
	if _, err := StartDirectRelay(nil); err == nil {
		t.Fatal("StartDirectRelay(nil) error = nil")
	}
}

func TestDirectRelayPassesDomainIPv4IPv6AndPortToDialer(t *testing.T) {
	attempts := make(chan string, 3)
	relay, err := StartDirectRelay(func(_ context.Context, network, address string) (net.Conn, error) {
		attempts <- network + " " + address
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			_, _ = io.Copy(server, server)
		}()
		return client, nil
	})
	if err != nil {
		t.Fatalf("StartDirectRelay() error = %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })

	dialer, err := xproxy.SOCKS5("tcp", relay.Addr(), nil, &net.Dialer{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SOCKS5() error = %v", err)
	}
	for _, target := range []string{
		"chrome-target.test:8443",
		"8.8.8.8:53",
		"[2606:4700:4700::1111]:443",
	} {
		connection, dialErr := dialer.Dial("tcp", target)
		if dialErr != nil {
			t.Fatalf("SOCKS Dial(%q) error = %v", target, dialErr)
		}
		if _, writeErr := connection.Write([]byte("ping")); writeErr != nil {
			_ = connection.Close()
			t.Fatalf("SOCKS tunnel write: %v", writeErr)
		}
		echo := make([]byte, 4)
		if _, readErr := io.ReadFull(connection, echo); readErr != nil || string(echo) != "ping" {
			_ = connection.Close()
			t.Fatalf("SOCKS tunnel echo = %q, error = %v", echo, readErr)
		}
		_ = connection.Close()
	}

	for _, want := range []string{
		"tcp chrome-target.test:8443",
		"tcp 8.8.8.8:53",
		"tcp [2606:4700:4700::1111]:443",
	} {
		select {
		case got := <-attempts:
			if got != want {
				t.Fatalf("dial attempt = %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for dial attempt %q", want)
		}
	}
}

func TestDirectRelayAppliesPublicPolicyToEverySOCKSConnect(t *testing.T) {
	resolver := &relaySequenceResolver{answers: [][]netip.Addr{
		{netip.MustParseAddr("1.1.1.1")},
		{netip.MustParseAddr("127.0.0.1")},
	}}
	var rawDials atomic.Int32
	policy := publicnet.NewPolicy(publicnet.Options{
		Resolver: resolver,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			rawDials.Add(1)
			client, server := net.Pipe()
			go func() {
				defer server.Close()
				_, _ = io.Copy(server, server)
			}()
			return client, nil
		},
	})
	relay, err := StartDirectRelay(policy.DialContext)
	if err != nil {
		t.Fatalf("StartDirectRelay() error = %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	dialer, err := xproxy.SOCKS5("tcp", relay.Addr(), nil, &net.Dialer{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SOCKS5() error = %v", err)
	}

	first, err := dialer.Dial("tcp", "rebind.test:443")
	if err != nil {
		t.Fatalf("first SOCKS Dial() error = %v", err)
	}
	_ = first.Close()
	if _, err := dialer.Dial("tcp", "rebind.test:443"); err == nil {
		t.Fatal("second SOCKS Dial() error = nil after private DNS rebinding")
	}
	if got := resolver.calls.Load(); got != 2 {
		t.Fatalf("resolver calls = %d, want one per CONNECT", got)
	}
	if got := rawDials.Load(); got != 1 {
		t.Fatalf("underlying raw dials = %d, want only the public CONNECT", got)
	}
}

func TestDirectRelayCloseCancelsInFlightDialAndIsConcurrentSafe(t *testing.T) {
	dialStarted := make(chan struct{})
	relay, err := StartDirectRelay(func(ctx context.Context, _, _ string) (net.Conn, error) {
		close(dialStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if err != nil {
		t.Fatalf("StartDirectRelay() error = %v", err)
	}
	dialer, err := xproxy.SOCKS5("tcp", relay.Addr(), nil, &net.Dialer{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SOCKS5() error = %v", err)
	}
	dialDone := make(chan error, 1)
	go func() {
		connection, dialErr := dialer.Dial("tcp", "pending.test:443")
		if connection != nil {
			_ = connection.Close()
		}
		dialDone <- dialErr
	}()
	select {
	case <-dialStarted:
	case <-time.After(time.Second):
		t.Fatal("direct dial did not start")
	}

	const callers = 32
	results := make(chan error, callers)
	var waitGroup sync.WaitGroup
	for range callers {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			results <- relay.Close()
		}()
	}
	waitGroup.Wait()
	close(results)
	for closeErr := range results {
		if closeErr != nil {
			t.Errorf("Close() error = %v", closeErr)
		}
	}
	select {
	case dialErr := <-dialDone:
		if dialErr == nil {
			t.Fatal("in-flight SOCKS dial unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight SOCKS dial did not stop after Close")
	}
}

func TestDirectRelayReapsTunnelWhenBrowserSideDisconnects(t *testing.T) {
	remoteClosed := make(chan struct{})
	relay, err := StartDirectRelay(func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer close(remoteClosed)
			defer server.Close()
			_, _ = io.Copy(io.Discard, server)
		}()
		return client, nil
	})
	if err != nil {
		t.Fatalf("StartDirectRelay() error = %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	dialer, err := xproxy.SOCKS5("tcp", relay.Addr(), nil, &net.Dialer{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SOCKS5() error = %v", err)
	}
	connection, err := dialer.Dial("tcp", "public.test:443")
	if err != nil {
		t.Fatalf("SOCKS Dial() error = %v", err)
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("browser-side Close() error = %v", err)
	}
	select {
	case <-remoteClosed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("upstream connection remained open after browser-side disconnect")
	}
}

func TestSOCKSReplyForError(t *testing.T) {
	for _, test := range []struct {
		err  error
		want byte
	}{
		{err: syscall.EACCES, want: 0x02},
		{err: syscall.EPERM, want: 0x02},
		{err: fmt.Errorf("policy: %w", publicnet.ErrNotPublic), want: 0x02},
		{err: syscall.ENETUNREACH, want: 0x03},
		{err: syscall.EHOSTUNREACH, want: 0x04},
		{err: syscall.ECONNREFUSED, want: 0x05},
		{err: fmt.Errorf("wrapped: %w", syscall.ECONNREFUSED), want: 0x05},
		{err: context.DeadlineExceeded, want: 0x01},
	} {
		if got := socksReplyForError(test.err); got != test.want {
			t.Errorf("socksReplyForError(%v) = %#x, want %#x", test.err, got, test.want)
		}
	}
}

type relaySequenceResolver struct {
	mu      sync.Mutex
	answers [][]netip.Addr
	calls   atomic.Int32
}

func (resolver *relaySequenceResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	resolver.calls.Add(1)
	if len(resolver.answers) == 0 {
		return nil, errors.New("no DNS answer")
	}
	answer := append([]netip.Addr(nil), resolver.answers[0]...)
	resolver.answers = resolver.answers[1:]
	return answer, nil
}
