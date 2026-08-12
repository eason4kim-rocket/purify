package rerank

import (
	"context"
	"crypto/tls"
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
)

func TestNewHTTPClientValidatesModeAndBuildsHardenedShape(t *testing.T) {
	base := rerankHTTPClientConfig{
		endpoint: rerankTestURL("https://rerank.example.test/v1/rerank"),
		timeout:  5 * time.Second,
		resolver: rerankStaticResolver{
			"rerank.example.test": {netip.MustParseAddr("1.1.1.1")},
		},
		dialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("unused")
		},
	}
	for _, test := range []struct {
		name    string
		options rerankHTTPClientConfig
	}{
		{name: "missing endpoint", options: withHTTPClientEndpoint(base, "")},
		{name: "unsupported scheme", options: withHTTPClientEndpoint(base, "ftp://rerank.example.test/v1/rerank")},
		{name: "HTTP without private opt in", options: withHTTPClientEndpoint(base, "http://rerank.internal/v1/rerank")},
		{name: "timeout below minimum", options: withHTTPClientTimeout(base, time.Second-time.Nanosecond)},
		{name: "timeout above maximum", options: withHTTPClientTimeout(base, 10*time.Second+time.Nanosecond)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := newRerankHTTPClient(test.options); err == nil {
				t.Fatal("newRerankHTTPClient() error = nil")
			}
		})
	}

	client, err := newRerankHTTPClient(base)
	if err != nil {
		t.Fatalf("newRerankHTTPClient() error = %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)
	if client.client.Timeout != 5*time.Second {
		t.Fatalf("client timeout = %s, want 5s", client.client.Timeout)
	}
	transport, ok := client.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("transport Proxy is non-nil")
	}
	if transport.DialContext == nil {
		t.Fatal("transport DialContext is nil")
	}
	if transport.MaxConnsPerHost != 4 || transport.MaxIdleConnsPerHost != 4 {
		t.Fatalf("connection caps = active %d idle %d, want 4/4", transport.MaxConnsPerHost, transport.MaxIdleConnsPerHost)
	}
	if transport.MaxResponseHeaderBytes != 64<<10 {
		t.Fatalf("MaxResponseHeaderBytes = %d, want %d", transport.MaxResponseHeaderBytes, 64<<10)
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("TLS minimum = %#v, want TLS 1.2", transport.TLSClientConfig)
	}
	if client.client.CheckRedirect == nil {
		t.Fatal("CheckRedirect is nil")
	}
	if redirectErr := client.client.CheckRedirect(&http.Request{}, nil); !errors.Is(redirectErr, errRerankRedirectRejected) {
		t.Fatalf("CheckRedirect() error = %v, want errRerankRedirectRejected", redirectErr)
	}
}

func TestHTTPPolicyDestinationMatrixRejectsBeforeDial(t *testing.T) {
	tests := []struct {
		name         string
		endpoint     string
		allowPrivate bool
		host         string
		answers      []netip.Addr
		wantTarget   string
		wantReject   bool
	}{
		{
			name:       "HTTPS public-only accepts public",
			endpoint:   "https://public.test/v1/rerank",
			host:       "public.test",
			answers:    rerankAddresses("1.1.1.1", "2606:4700:4700::1111"),
			wantTarget: "1.1.1.1:443",
		},
		{
			name:       "HTTPS public-only rejects private",
			endpoint:   "https://private.test/v1/rerank",
			host:       "private.test",
			answers:    rerankAddresses("10.0.0.1"),
			wantReject: true,
		},
		{
			name:         "HTTPS opted-in accepts public",
			endpoint:     "https://public.test/v1/rerank",
			allowPrivate: true,
			host:         "public.test",
			answers:      rerankAddresses("8.8.8.8"),
			wantTarget:   "8.8.8.8:443",
		},
		{
			name:         "HTTPS opted-in accepts RFC1918",
			endpoint:     "https://private.test/v1/rerank",
			allowPrivate: true,
			host:         "private.test",
			answers:      rerankAddresses("192.168.2.3"),
			wantTarget:   "192.168.2.3:443",
		},
		{
			name:         "HTTPS opted-in accepts ULA",
			endpoint:     "https://private.test/v1/rerank",
			allowPrivate: true,
			host:         "private.test",
			answers:      rerankAddresses("fd00::2"),
			wantTarget:   "[fd00::2]:443",
		},
		{
			name:         "HTTPS opted-in rejects mixed classes",
			endpoint:     "https://mixed.test/v1/rerank",
			allowPrivate: true,
			host:         "mixed.test",
			answers:      rerankAddresses("1.1.1.1", "127.0.0.1"),
			wantReject:   true,
		},
		{
			name:         "HTTP accepts loopback",
			endpoint:     "http://private.test/v1/rerank",
			allowPrivate: true,
			host:         "private.test",
			answers:      rerankAddresses("127.0.0.1"),
			wantTarget:   "127.0.0.1:80",
		},
		{
			name:         "HTTP rejects public",
			endpoint:     "http://public.test/v1/rerank",
			allowPrivate: true,
			host:         "public.test",
			answers:      rerankAddresses("1.1.1.1"),
			wantReject:   true,
		},
		{
			name:         "HTTP rejects mixed private and public",
			endpoint:     "http://mixed.test/v1/rerank",
			allowPrivate: true,
			host:         "mixed.test",
			answers:      rerankAddresses("10.0.0.2", "1.1.1.1"),
			wantReject:   true,
		},
		{
			name:         "HTTP rejects link-local",
			endpoint:     "http://link.test/v1/rerank",
			allowPrivate: true,
			host:         "link.test",
			answers:      rerankAddresses("169.254.169.254"),
			wantReject:   true,
		},
		{
			name:         "HTTPS opted-in rejects reserved",
			endpoint:     "https://reserved.test/v1/rerank",
			allowPrivate: true,
			host:         "reserved.test",
			answers:      rerankAddresses("192.0.2.1"),
			wantReject:   true,
		},
	}
	for _, address := range []string{
		"192.88.99.1",
		"64:ff9b:1::1",
		"64:ff9b::a00:1",
		"100:0:0:1::1",
		"2001:2::1",
		"2002::1",
		"3ffe::1",
		"3fff::1",
		"4000::1",
		"5f00::1",
		"fec0::1",
	} {
		tests = append(tests, struct {
			name         string
			endpoint     string
			allowPrivate bool
			host         string
			answers      []netip.Addr
			wantTarget   string
			wantReject   bool
		}{
			name:       "HTTPS rejects IANA special-purpose " + address,
			endpoint:   "https://special.test/v1/rerank",
			host:       "special.test",
			answers:    rerankAddresses(address),
			wantReject: true,
		})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var dialed atomic.Int32
			var target string
			client, err := newRerankHTTPClient(rerankHTTPClientConfig{
				endpoint:     rerankTestURL(test.endpoint),
				allowPrivate: test.allowPrivate,
				timeout:      time.Second,
				resolver:     rerankStaticResolver{test.host: test.answers},
				dialContext: func(_ context.Context, _ string, address string) (net.Conn, error) {
					dialed.Add(1)
					target = address
					return rerankClosedPipe(), nil
				},
			})
			if err != nil {
				t.Fatalf("newRerankHTTPClient() error = %v", err)
			}
			t.Cleanup(client.CloseIdleConnections)
			transport := client.client.Transport.(*http.Transport)
			port := "443"
			if strings.HasPrefix(test.endpoint, "http://") {
				port = "80"
			}
			connection, dialErr := transport.DialContext(context.Background(), "tcp", net.JoinHostPort(test.host, port))
			if test.wantReject {
				if !errors.Is(dialErr, errRerankDestinationRejected) {
					t.Fatalf("DialContext() error = %v, want errRerankDestinationRejected", dialErr)
				}
				if got := dialed.Load(); got != 0 {
					t.Fatalf("underlying dials = %d, want zero", got)
				}
				return
			}
			if dialErr != nil {
				t.Fatalf("DialContext() error = %v", dialErr)
			}
			_ = connection.Close()
			if target != test.wantTarget {
				t.Fatalf("underlying target = %q, want %q", target, test.wantTarget)
			}
		})
	}
}

func TestHTTPPolicyClassifiesLiteralTargetsWithoutDNS(t *testing.T) {
	resolver := rerankResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		t.Fatal("literal target unexpectedly resolved through DNS")
		return nil, nil
	})
	var dialed atomic.Int32
	client, err := newRerankHTTPClient(rerankHTTPClientConfig{
		endpoint:     rerankTestURL("http://127.0.0.1/v1/rerank"),
		allowPrivate: true,
		timeout:      time.Second,
		resolver:     resolver,
		dialContext: func(context.Context, string, string) (net.Conn, error) {
			dialed.Add(1)
			return rerankClosedPipe(), nil
		},
	})
	if err != nil {
		t.Fatalf("newRerankHTTPClient() error = %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)
	transport := client.client.Transport.(*http.Transport)
	connection, err := transport.DialContext(context.Background(), "tcp", "127.0.0.1:80")
	if err != nil {
		t.Fatalf("private literal DialContext() error = %v", err)
	}
	_ = connection.Close()
	if _, err = transport.DialContext(context.Background(), "tcp", "8.8.8.8:80"); !errors.Is(err, errRerankDestinationRejected) {
		t.Fatalf("public literal DialContext() error = %v, want errRerankDestinationRejected", err)
	}
	if _, err = transport.DialContext(context.Background(), "tcp", "[fe80::1]:80"); !errors.Is(err, errRerankDestinationRejected) {
		t.Fatalf("link-local literal DialContext() error = %v, want errRerankDestinationRejected", err)
	}
	if got := dialed.Load(); got != 1 {
		t.Fatalf("underlying dials = %d, want 1", got)
	}

	for _, address := range []string{
		"192.88.99.1",
		"64:ff9b:1::1",
		"64:ff9b::a00:1",
		"100:0:0:1::1",
		"2001:2::1",
		"2002::1",
		"3ffe::1",
		"3fff::1",
		"4000::1",
		"5f00::1",
		"fec0::1",
	} {
		t.Run("special-purpose "+address, func(t *testing.T) {
			host := address
			if strings.ContainsRune(host, ':') {
				host = "[" + host + "]"
			}
			var specialDials atomic.Int32
			specialClient, err := newRerankHTTPClient(rerankHTTPClientConfig{
				endpoint: rerankTestURL("https://" + host + "/v1/rerank"),
				timeout:  time.Second,
				resolver: resolver,
				dialContext: func(context.Context, string, string) (net.Conn, error) {
					specialDials.Add(1)
					return rerankClosedPipe(), nil
				},
			})
			if err != nil {
				t.Fatalf("newRerankHTTPClient() error = %v", err)
			}
			t.Cleanup(specialClient.CloseIdleConnections)
			specialTransport := specialClient.client.Transport.(*http.Transport)
			if _, err := specialTransport.DialContext(context.Background(), "tcp", net.JoinHostPort(address, "443")); !errors.Is(err, errRerankDestinationRejected) {
				t.Fatalf("DialContext() error = %v, want errRerankDestinationRejected", err)
			}
			if got := specialDials.Load(); got != 0 {
				t.Fatalf("underlying dials = %d, want zero", got)
			}
		})
	}
}

func TestHTTPPolicyResolvesEveryDialPinsLiteralAndRejectsPrivateToPublicRebind(t *testing.T) {
	resolver := &rerankSequenceResolver{answers: [][]netip.Addr{
		rerankAddresses("10.0.0.8"),
		rerankAddresses("1.1.1.1"),
	}}
	var mu sync.Mutex
	var targets []string
	client, err := newRerankHTTPClient(rerankHTTPClientConfig{
		endpoint:     rerankTestURL("http://rebind.test/v1/rerank"),
		allowPrivate: true,
		timeout:      time.Second,
		resolver:     resolver,
		dialContext: func(_ context.Context, _ string, address string) (net.Conn, error) {
			mu.Lock()
			targets = append(targets, address)
			mu.Unlock()
			return rerankClosedPipe(), nil
		},
	})
	if err != nil {
		t.Fatalf("newRerankHTTPClient() error = %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)
	transport := client.client.Transport.(*http.Transport)
	connection, err := transport.DialContext(context.Background(), "tcp", "rebind.test:80")
	if err != nil {
		t.Fatalf("first DialContext() error = %v", err)
	}
	_ = connection.Close()
	if _, err = transport.DialContext(context.Background(), "tcp", "rebind.test:80"); !errors.Is(err, errRerankDestinationRejected) {
		t.Fatalf("second DialContext() error = %v, want errRerankDestinationRejected", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(targets) != fmt.Sprint([]string{"10.0.0.8:80"}) {
		t.Fatalf("underlying targets = %v, want private literal only", targets)
	}
	if resolver.calls.Load() != 2 {
		t.Fatalf("resolver calls = %d, want 2", resolver.calls.Load())
	}
}

func TestHTTPPolicyTriesOnlyValidatedLiteralCandidates(t *testing.T) {
	var targets []string
	client, err := newRerankHTTPClient(rerankHTTPClientConfig{
		endpoint: rerankTestURL("https://fallback.test/v1/rerank"),
		timeout:  time.Second,
		resolver: rerankStaticResolver{
			"fallback.test": rerankAddresses("1.1.1.1", "2606:4700:4700::1111"),
		},
		dialContext: func(_ context.Context, _ string, address string) (net.Conn, error) {
			targets = append(targets, address)
			if len(targets) == 1 {
				return nil, syscall.ECONNREFUSED
			}
			return rerankClosedPipe(), nil
		},
	})
	if err != nil {
		t.Fatalf("newRerankHTTPClient() error = %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)
	transport := client.client.Transport.(*http.Transport)
	connection, err := transport.DialContext(context.Background(), "tcp", "fallback.test:443")
	if err != nil {
		t.Fatalf("DialContext() error = %v", err)
	}
	_ = connection.Close()
	want := []string{"1.1.1.1:443", "[2606:4700:4700::1111]:443"}
	if fmt.Sprint(targets) != fmt.Sprint(want) {
		t.Fatalf("underlying targets = %v, want %v", targets, want)
	}
}

func TestHTTPClientIgnoresAmbientProxyAndRejectsRedirectBeforeCredentialForward(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")

	var sinkRequests atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		sinkRequests.Add(1)
	}))
	defer sink.Close()

	credential := "Bearer process-rerank-secret"
	var sourceCredential string
	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		sourceCredential = request.Header.Get("Authorization")
		http.Redirect(writer, request, sink.URL+"/v1/rerank", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	sourceURL, err := url.Parse(source.URL)
	if err != nil {
		t.Fatalf("parse source URL: %v", err)
	}
	endpoint := "http://reranker.internal:" + sourceURL.Port() + "/v1/rerank"
	client, err := newRerankHTTPClient(rerankHTTPClientConfig{
		endpoint:     rerankTestURL(endpoint),
		allowPrivate: true,
		timeout:      2 * time.Second,
		resolver: rerankStaticResolver{
			"reranker.internal": rerankAddresses("127.0.0.1"),
		},
		dialContext: (&net.Dialer{}).DialContext,
	})
	if err != nil {
		t.Fatalf("newRerankHTTPClient() error = %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	request.Header.Set("Authorization", credential)
	response, requestErr := client.Do(request)
	if response != nil {
		_ = response.Body.Close()
	}
	if !errors.Is(requestErr, errRerankRedirectRejected) {
		t.Fatalf("Do() error = %v, want errRerankRedirectRejected", requestErr)
	}
	if sourceCredential != credential {
		t.Fatalf("source credential = %q, want configured credential", sourceCredential)
	}
	if got := sinkRequests.Load(); got != 0 {
		t.Fatalf("redirect sink requests = %d, want zero", got)
	}
}

func TestHTTPClientCloseIdleConnectionsClosesPooledConnection(t *testing.T) {
	closed := make(chan struct{}, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, "ok")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	server.Start()
	defer server.Close()

	endpoint := server.URL + "/v1/rerank"
	client, err := newRerankHTTPClient(rerankHTTPClientConfig{
		endpoint:     rerankTestURL(endpoint),
		allowPrivate: true,
		timeout:      2 * time.Second,
	})
	if err != nil {
		t.Fatalf("newRerankHTTPClient() error = %v", err)
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	client.CloseIdleConnections()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("idle connection was not closed")
	}
}

func withHTTPClientEndpoint(source rerankHTTPClientConfig, endpoint string) rerankHTTPClientConfig {
	if endpoint == "" {
		source.endpoint = nil
	} else {
		source.endpoint = rerankTestURL(endpoint)
	}
	return source
}

func withHTTPClientTimeout(source rerankHTTPClientConfig, timeout time.Duration) rerankHTTPClientConfig {
	source.timeout = timeout
	return source
}

func rerankTestURL(raw string) *url.URL {
	parsed, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return parsed
}

func rerankAddresses(values ...string) []netip.Addr {
	addresses := make([]netip.Addr, len(values))
	for index, value := range values {
		addresses[index] = netip.MustParseAddr(value)
	}
	return addresses
}

type rerankStaticResolver map[string][]netip.Addr

func (resolver rerankStaticResolver) LookupNetIP(_ context.Context, _, hostname string) ([]netip.Addr, error) {
	addresses, exists := resolver[strings.ToLower(hostname)]
	if !exists {
		return nil, fmt.Errorf("no DNS fixture for %s", hostname)
	}
	return append([]netip.Addr(nil), addresses...), nil
}

type rerankResolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (function rerankResolverFunc) LookupNetIP(ctx context.Context, network, hostname string) ([]netip.Addr, error) {
	return function(ctx, network, hostname)
}

type rerankSequenceResolver struct {
	mu      sync.Mutex
	answers [][]netip.Addr
	calls   atomic.Int32
}

func (resolver *rerankSequenceResolver) LookupNetIP(_ context.Context, _, _ string) ([]netip.Addr, error) {
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

func rerankClosedPipe() net.Conn {
	client, server := net.Pipe()
	_ = server.Close()
	return client
}

var _ publicnet.Resolver = rerankStaticResolver{}
