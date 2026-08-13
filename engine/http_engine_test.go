package engine

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/use-agent/purify/proxypool"
)

// TestChromeRoundTripperNegotiatesHTTP2 locks the core of the Chrome-coherent
// fingerprint: when the server offers h2 over ALPN, the direct utls path must
// actually speak HTTP/2 rather than fall back to HTTP/1.1. Offering h2 in the
// ClientHello but then speaking h1 was the JA4 tell this replaces.
func TestChromeRoundTripperNegotiatesHTTP2(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(writer, "<html><body>proto=%s</body></html>", request.Proto)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)

	resp, body := directTLSRoundTrip(t, server)
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("negotiated %s, want HTTP/2", resp.Proto)
	}
	if !strings.Contains(body, "proto=HTTP/2.0") {
		t.Fatalf("server saw %q, want HTTP/2.0", body)
	}
}

// TestChromeRoundTripperFallsBackToHTTP1 covers a server that does not offer h2:
// the same direct utls path must complete the request over HTTP/1.1.
func TestChromeRoundTripperFallsBackToHTTP1(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(writer, "<html><body>proto=%s</body></html>", request.Proto)
	}))
	server.EnableHTTP2 = false
	server.StartTLS()
	t.Cleanup(server.Close)

	resp, body := directTLSRoundTrip(t, server)
	defer resp.Body.Close()
	if resp.ProtoMajor != 1 {
		t.Fatalf("negotiated %s, want HTTP/1.1", resp.Proto)
	}
	if !strings.Contains(body, "proto=HTTP/1.1") {
		t.Fatalf("server saw %q, want HTTP/1.1", body)
	}
}

// TestChromeRoundTripperReusesAcrossConnections guards a real bug: reusing one
// utls ClientHello spec across connections mutates its key material, so the
// second handshake fails with a bad record MAC. Every request must build a
// fresh hello, so many sequential fetches through one client must all succeed.
func TestChromeRoundTripperReusesAcrossConnections(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/html")
		_, _ = writer.Write([]byte("<html><body>ok</body></html>"))
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)

	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	client := &http.Client{Transport: &chromeRoundTripper{
		dialTCP: func(ctx context.Context, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp", addr)
		},
		tlsConfig: &tls.Config{RootCAs: roots},
		h2:        &http2.Transport{},
	}}

	for i := range 5 {
		resp, err := client.Get(server.URL)
		if err != nil {
			t.Fatalf("Get(%d) error = %v", i, err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
}

// directTLSRoundTrip drives one GET through the direct chromeRoundTripper,
// trusting the test server's certificate, and returns the response and body.
func directTLSRoundTrip(t *testing.T, server *httptest.Server) (*http.Response, string) {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &chromeRoundTripper{
		dialTCP: func(ctx context.Context, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp", addr)
		},
		tlsConfig: &tls.Config{RootCAs: roots},
		h2:        &http2.Transport{},
	}
	client := &http.Client{Transport: transport}
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, string(body)
}

func TestHTTPEngineHonorsHeadersCookiesAndDefaultNetworkWait(t *testing.T) {
	wait := true
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		cookie, err := request.Cookie("session")
		if err != nil {
			t.Errorf("request cookie: %v", err)
		}
		fmt.Fprintf(writer, "<html><head><title>fixture</title></head><body>%s|%s</body></html>", request.Header.Get("X-Purify-Test"), cookie.Value)
	}))
	t.Cleanup(server.Close)

	engine := NewHTTPEngine("")
	request := &FetchRequest{
		URL:                server.URL,
		Headers:            map[string]string{"X-Purify-Test": "header-value"},
		Cookies:            []http.Cookie{{Name: "session", Value: "cookie-value"}},
		Timeout:            time.Second,
		WaitForNetworkIdle: &wait,
	}
	if !engine.Supports(request) {
		t.Fatal("default wait_for_network_idle=true must not disable the HTTP-first candidate")
	}

	result, err := engine.Fetch(context.Background(), request)
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if !strings.Contains(result.HTML, "header-value|cookie-value") {
		t.Fatalf("Fetch() HTML = %q, want propagated header and cookie", result.HTML)
	}
	if result.Title != "fixture" || result.FinalURL != server.URL || result.StatusCode != http.StatusOK {
		t.Fatalf("Fetch() metadata = %+v", result)
	}
}

func TestHTTPEngineRequestTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		writer.Header().Set("Content-Type", "text/html")
		_, _ = writer.Write([]byte("<html><body>late</body></html>"))
	}))
	t.Cleanup(server.Close)

	_, err := NewHTTPEngine("").Fetch(context.Background(), &FetchRequest{
		URL:     server.URL,
		Timeout: 25 * time.Millisecond,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Fetch() error = %v, want context deadline exceeded", err)
	}
}

func TestHTTPEngineObservationPreservesDefinitiveStatuses(t *testing.T) {
	for _, statusCode := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "text/plain")
				writer.WriteHeader(statusCode)
				_, _ = writer.Write([]byte("definitive observation"))
			}))
			t.Cleanup(server.Close)

			engine := NewHTTPEngine("")
			result, err := engine.Fetch(context.Background(), &FetchRequest{
				URL:              server.URL,
				Mode:             FetchModeObservation,
				MaximumBodyBytes: 1 << 10,
			})
			if err != nil {
				t.Fatalf("observation Fetch() error = %v", err)
			}
			if result.StatusCode != statusCode || result.HTML != "definitive observation" || result.ContentType != "text/plain" {
				t.Fatalf("observation result = %#v", result)
			}

			if _, err := engine.Fetch(context.Background(), &FetchRequest{URL: server.URL}); err == nil {
				t.Fatal("default Fetch() accepted an error status")
			}
		})
	}
}

func TestHTTPEngineRejectsOversizedBodyWithoutTruncation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/html")
		_, _ = writer.Write([]byte("12345"))
	}))
	t.Cleanup(server.Close)

	engine := NewHTTPEngine("")
	if _, err := engine.Fetch(context.Background(), &FetchRequest{
		URL: server.URL, MaximumBodyBytes: 4,
	}); !errors.Is(err, ErrResponseBodyTooLarge) {
		t.Fatalf("Fetch(oversized) error = %v, want ErrResponseBodyTooLarge", err)
	}
	result, err := engine.Fetch(context.Background(), &FetchRequest{
		URL: server.URL, MaximumBodyBytes: 5,
	})
	if err != nil {
		t.Fatalf("Fetch(exact limit) error = %v", err)
	}
	if result.HTML != "12345" {
		t.Fatalf("Fetch(exact limit) HTML = %q", result.HTML)
	}
}

func TestHTTPEngineUsesRequestLocalRedirectPolicy(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		destinationCalls.Add(1)
		writer.Header().Set("Content-Type", "text/html")
		_, _ = writer.Write([]byte("destination"))
	}))
	t.Cleanup(destination.Close)
	redirect := httptest.NewServer(http.RedirectHandler(destination.URL, http.StatusFound))
	t.Cleanup(redirect.Close)

	engine := NewHTTPEngine("")
	rejected := errors.New("redirect rejected")
	_, err := engine.Fetch(context.Background(), &FetchRequest{
		URL: redirect.URL,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return rejected
		},
	})
	if !errors.Is(err, rejected) {
		t.Fatalf("Fetch(rejected redirect) error = %v", err)
	}
	if destinationCalls.Load() != 0 {
		t.Fatalf("destination calls after rejected redirect = %d", destinationCalls.Load())
	}

	result, err := engine.Fetch(context.Background(), &FetchRequest{URL: redirect.URL})
	if err != nil {
		t.Fatalf("Fetch(default redirect) error = %v", err)
	}
	if result.HTML != "destination" || destinationCalls.Load() != 1 {
		t.Fatalf("default redirect result = %#v, calls = %d", result, destinationCalls.Load())
	}
}

func TestHTTPEngineConcurrentPerRequestProxySelection(t *testing.T) {
	proxyA := proxyFixture(t, "proxy-a")
	proxyB := proxyFixture(t, "proxy-b")
	engine := NewHTTPEngine(proxyA.URL)

	type testCase struct {
		name      string
		override  string
		wantProxy string
	}
	tests := []testCase{
		{name: "default", wantProxy: "proxy-a"},
		{name: "override", override: proxyB.URL, wantProxy: "proxy-b"},
	}

	const attempts = 24
	var waitGroup sync.WaitGroup
	errorsChannel := make(chan error, attempts*len(tests))
	for attempt := 0; attempt < attempts; attempt++ {
		for _, test := range tests {
			test := test
			waitGroup.Add(1)
			go func() {
				defer waitGroup.Done()
				result, err := engine.Fetch(context.Background(), &FetchRequest{
					URL:      "http://purify-proxy-target.invalid/page",
					ProxyURL: test.override,
					Timeout:  time.Second,
				})
				if err != nil {
					errorsChannel <- fmt.Errorf("%s Fetch(): %w", test.name, err)
					return
				}
				if !strings.Contains(result.HTML, test.wantProxy) {
					errorsChannel <- fmt.Errorf("%s HTML = %q, want %q", test.name, result.HTML, test.wantProxy)
				}
			}()
		}
	}
	waitGroup.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		t.Error(err)
	}
}

// TestHTTPEngineRotatesProxyPool locks the round-robin egress: with two or more
// exits and no per-request override, successive fetches must alternate proxies
// so traffic is not pinned to one IP.
func TestHTTPEngineRotatesProxyPool(t *testing.T) {
	proxyA := proxyFixture(t, "proxy-a")
	proxyB := proxyFixture(t, "proxy-b")
	engine := NewHTTPEngineWithPool("", proxypool.New([]string{proxyA.URL, proxyB.URL}))

	want := []string{"proxy-a", "proxy-b", "proxy-a", "proxy-b"}
	for i, marker := range want {
		result, err := engine.Fetch(context.Background(), &FetchRequest{
			URL:     "http://purify-proxy-target.invalid/page",
			Timeout: time.Second,
		})
		if err != nil {
			t.Fatalf("Fetch(%d) error = %v", i, err)
		}
		if !strings.Contains(result.HTML, marker) {
			t.Fatalf("Fetch(%d) served by wrong exit: HTML = %q, want %q", i, result.HTML, marker)
		}
	}
}

// TestHTTPEngineSingleEntryPoolKeepsSharedClient guards the fast path: one exit
// must behave exactly like a single default proxy, never rotating.
func TestHTTPEngineSingleEntryPoolKeepsSharedClient(t *testing.T) {
	proxyA := proxyFixture(t, "proxy-a")
	engine := NewHTTPEngineWithPool("", proxypool.New([]string{proxyA.URL}))
	if engine.defaultProxyURL != proxyA.URL {
		t.Fatalf("single-entry pool was not promoted to default: %q", engine.defaultProxyURL)
	}
	for i := range 3 {
		result, err := engine.Fetch(context.Background(), &FetchRequest{
			URL:     "http://purify-proxy-target.invalid/page",
			Timeout: time.Second,
		})
		if err != nil {
			t.Fatalf("Fetch(%d) error = %v", i, err)
		}
		if !strings.Contains(result.HTML, "proxy-a") {
			t.Fatalf("Fetch(%d) HTML = %q, want proxy-a", i, result.HTML)
		}
	}
}

func proxyFixture(t *testing.T, marker string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Host != "purify-proxy-target.invalid" {
			t.Errorf("proxy target host = %q", request.URL.Host)
		}
		writer.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(writer, "<html><body>%s</body></html>", marker)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestHTTPEngineSupportsBrowserOnlyOptions(t *testing.T) {
	wait := true
	engine := NewHTTPEngine("")
	if !engine.Supports(&FetchRequest{WaitForNetworkIdle: &wait}) {
		t.Fatal("network-idle preference applies to browser candidates and must preserve HTTP-first")
	}
	for name, request := range map[string]*FetchRequest{
		"stealth":         {Stealth: true},
		"remove overlays": {RemoveOverlays: true},
		"block ads":       {BlockAds: true},
		"actions":         {Actions: []Action{{Type: "click"}}},
		"CDP":             {CDPURL: "ws://browser.invalid/devtools/browser/test"},
	} {
		t.Run(name, func(t *testing.T) {
			if engine.Supports(request) {
				t.Fatalf("Supports(%s) = true, want explicit browser-only skip", name)
			}
			_, err := engine.Fetch(context.Background(), request)
			if !errors.Is(err, ErrUnsupportedRequest) {
				t.Fatalf("Fetch(%s) error = %v, want ErrUnsupportedRequest", name, err)
			}
		})
	}
}

func TestDialViaHTTPSProxyUsesTLSValidationSNIAndAuth(t *testing.T) {
	proxy := newTLSConnectProxy(t)
	parsed, err := url.Parse(proxy.url)
	if err != nil {
		t.Fatal(err)
	}
	parsed.User = url.UserPassword("proxy-user", "proxy-pass")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := dialViaHTTPConnectWithTLSConfig(ctx, parsed, "target.test:443", &tls.Config{RootCAs: proxy.roots})
	if err != nil {
		t.Fatalf("dialViaHTTPConnectWithTLSConfig() error = %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("tunnel write: %v", err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(conn, echo); err != nil {
		t.Fatalf("tunnel read: %v", err)
	}
	if string(echo) != "ping" {
		t.Fatalf("tunnel echo = %q", echo)
	}
	if got := <-proxy.sni; got != "localhost" {
		t.Fatalf("HTTPS proxy SNI = %q, want localhost", got)
	}
	if got := <-proxy.authorization; got != "Basic cHJveHktdXNlcjpwcm94eS1wYXNz" {
		t.Fatalf("Proxy-Authorization = %q", got)
	}
}

func TestHTTPEngineFetchesHTTPSViaVerifiedHTTPSProxy(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/html")
		_, _ = writer.Write([]byte("<html><body>verified-https-proxy</body></html>"))
	}))
	t.Cleanup(target.Close)
	proxy := newTLSForwardProxy(t)
	roots := proxy.roots.Clone()
	roots.AddCert(target.Certificate())
	parsedProxy, _ := url.Parse(proxy.url)
	parsedProxy.User = url.UserPassword("proxy-user", "proxy-pass")
	client, err := newHTTPClientWithTLSConfig(parsedProxy.String(), &tls.Config{RootCAs: roots})
	if err != nil {
		t.Fatalf("newHTTPClientWithTLSConfig() error = %v", err)
	}
	engine := &HTTPEngine{client: client, defaultProxyURL: parsedProxy.String()}
	result, err := engine.Fetch(context.Background(), &FetchRequest{URL: target.URL, Timeout: time.Second})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if !strings.Contains(result.HTML, "verified-https-proxy") {
		t.Fatalf("Fetch() HTML = %q", result.HTML)
	}
	if got := <-proxy.sni; got != "localhost" {
		t.Fatalf("HTTPS proxy SNI = %q, want localhost", got)
	}
	if got := <-proxy.authorization; got != "Basic cHJveHktdXNlcjpwcm94eS1wYXNz" {
		t.Fatalf("Proxy-Authorization = %q", got)
	}
}

func TestDialViaHTTPSProxyRejectsUntrustedCertificate(t *testing.T) {
	proxy := newTLSConnectProxy(t)
	parsed, _ := url.Parse(proxy.url)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := dialViaHTTPConnectWithTLSConfig(ctx, parsed, "target.test:443", &tls.Config{RootCAs: x509.NewCertPool()})
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("dial error = %v, want certificate validation failure", err)
	}
}

func TestDialViaHTTPSProxyHandshakeHonorsContext(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			defer conn.Close()
			time.Sleep(time.Second)
		}
	}()
	parsed, _ := url.Parse("https://" + listener.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = dialViaHTTPConnect(ctx, parsed, "target.test:443")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("dial error = %v, want context deadline exceeded", err)
	}
}

type tlsConnectProxy struct {
	url           string
	roots         *x509.CertPool
	sni           chan string
	authorization chan string
}

func newTLSForwardProxy(t *testing.T) tlsConnectProxy {
	t.Helper()
	certificate, root := localhostCertificate(t)
	sni := make(chan string, 4)
	authorization := make(chan string, 4)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorization <- request.Header.Get("Proxy-Authorization")
		destination, err := net.DialTimeout("tcp", request.Host, time.Second)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadGateway)
			return
		}
		connection, buffered, err := writer.(http.Hijacker).Hijack()
		if err != nil {
			_ = destination.Close()
			t.Errorf("hijack: %v", err)
			return
		}
		defer connection.Close()
		defer destination.Close()
		_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\nContent-Length: 0\r\n\r\n")
		_ = buffered.Flush()
		var waitGroup sync.WaitGroup
		waitGroup.Add(2)
		go func() { defer waitGroup.Done(); _, _ = io.Copy(destination, buffered) }()
		go func() { defer waitGroup.Done(); _, _ = io.Copy(connection, destination) }()
		waitGroup.Wait()
	}))
	server.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{certificate},
		GetConfigForClient: func(info *tls.ClientHelloInfo) (*tls.Config, error) {
			sni <- info.ServerName
			return nil, nil
		},
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	parsed, _ := url.Parse(server.URL)
	_, port, _ := net.SplitHostPort(parsed.Host)
	parsed.Host = net.JoinHostPort("localhost", port)
	return tlsConnectProxy{url: parsed.String(), roots: root, sni: sni, authorization: authorization}
}

func newTLSConnectProxy(t *testing.T) tlsConnectProxy {
	t.Helper()
	certificate, root := localhostCertificate(t)
	sni := make(chan string, 4)
	authorization := make(chan string, 4)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
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
	server.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{certificate},
		GetConfigForClient: func(info *tls.ClientHelloInfo) (*tls.Config, error) {
			sni <- info.ServerName
			return nil, nil
		},
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	parsed, _ := url.Parse(server.URL)
	_, port, _ := net.SplitHostPort(parsed.Host)
	parsed.Host = net.JoinHostPort("localhost", port)
	return tlsConnectProxy{url: parsed.String(), roots: root, sni: sni, authorization: authorization}
}

func localhostCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost test proxy"},
		DNSNames:              []string{"localhost"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots.AddCert(parsed)
	return certificate, roots
}
