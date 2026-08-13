package engine

import (
	"bufio"
	"context"
	stdtls "crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/html"
	"golang.org/x/net/proxy"

	"github.com/use-agent/purify/proxypool"
)

// HTTPEngine is a lightweight Layer 1 engine that uses pure net/http.
// It is the fastest option, suitable for static pages that don't need
// JavaScript rendering.
type HTTPEngine struct {
	client          *http.Client
	defaultProxyURL string
	pool            *proxypool.Pool
	clientErr       error
}

const (
	defaultMaximumResponseBodyBytes int64 = 10 << 20
	hardMaximumResponseBodyBytes    int64 = 64 << 20
)

// chromeH1Spec is a Chrome-like TLS ClientHello with ALPN forced to http/1.1
// only. Computed once at init time and reused for every connection.
var chromeH1Spec utls.ClientHelloSpec

func init() {
	spec, err := utls.UTLSIdToSpec(utls.HelloChrome_Auto)
	if err != nil {
		// Fallback: if spec generation fails, use HelloChrome_Auto as-is.
		// (Should never happen with a valid utls version.)
		return
	}
	// Replace h2 with http/1.1 only in the ALPN extension so the server
	// never negotiates HTTP/2 (which Go's http.Transport cannot handle
	// over a utls connection).
	for i, ext := range spec.Extensions {
		if alpn, ok := ext.(*utls.ALPNExtension); ok {
			alpn.AlpnProtocols = []string{"http/1.1"}
			spec.Extensions[i] = alpn
			break
		}
	}
	chromeH1Spec = spec
}

// NewHTTPEngine creates an HTTPEngine with a Chrome-like TLS fingerprint.
// ALPN is locked to http/1.1 to avoid the HTTP/2 framing mismatch that
// occurs when utls negotiates h2 but Go's http.Transport only speaks h1.
// If proxyURL is non-empty, all connections are routed through the proxy
// (SOCKS5 with optional username/password auth is supported).
func NewHTTPEngine(proxyURL string) *HTTPEngine {
	return NewHTTPEngineWithPool(proxyURL, nil)
}

// NewHTTPEngineWithPool builds an engine whose egress rotates across pool when a
// request pins no proxy of its own. The shared client still backs defaultProxyURL
// so a zero- or single-proxy deployment reuses one client for every request; the
// per-request rotation client is built only when the pool holds two or more
// exits. A single-entry pool with no explicit default is promoted to the default
// so it, too, keeps the shared client.
func NewHTTPEngineWithPool(defaultProxyURL string, pool *proxypool.Pool) *HTTPEngine {
	if defaultProxyURL == "" && pool.Len() == 1 {
		defaultProxyURL = pool.Next()
	}
	client, err := newHTTPClient(defaultProxyURL)
	switch {
	case defaultProxyURL != "":
		slog.Info("http_engine: proxy configured", "proxy", redactProxy(defaultProxyURL))
	case pool.Len() > 1:
		slog.Info("http_engine: proxy pool configured", "exits", pool.Len())
	}
	return &HTTPEngine{
		client:          client,
		defaultProxyURL: defaultProxyURL,
		pool:            pool,
		clientErr:       err,
	}
}

func newHTTPClient(proxyURL string) (*http.Client, error) {
	return newHTTPClientWithTLSConfig(proxyURL, nil)
}

func newHTTPClientWithTLSConfig(proxyURL string, tlsConfig *stdtls.Config) (*http.Client, error) {
	if err := validateProxyURL(proxyURL); err != nil {
		return nil, err
	}

	transport := &http.Transport{
		ForceAttemptHTTP2: false,
	}
	if proxyURL == "" {
		transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			var conn net.Conn
			var err error
			dialer := &net.Dialer{Timeout: 10 * time.Second}
			conn, err = dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}

			host, _, _ := net.SplitHostPort(addr)
			tlsConn := utls.UClient(conn, &utls.Config{ServerName: host}, utls.HelloCustom)
			if err := tlsConn.ApplyPreset(&chromeH1Spec); err != nil {
				conn.Close()
				return nil, fmt.Errorf("http_engine: apply tls spec: %w", err)
			}
			if err := tlsConn.HandshakeContext(ctx); err != nil {
				conn.Close()
				return nil, err
			}
			return tlsConn, nil
		}
	} else {
		proxy, _ := url.Parse(proxyURL) // validated above
		transport.Proxy = http.ProxyURL(proxy)
		config := &stdtls.Config{MinVersion: stdtls.VersionTLS12}
		if tlsConfig != nil {
			config = tlsConfig.Clone()
			if config.MinVersion == 0 {
				config.MinVersion = stdtls.VersionTLS12
			}
		}
		// net/http applies ServerName separately for the HTTPS proxy and the
		// tunneled target while preserving certificate verification.
		transport.TLSClientConfig = config
	}

	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}, nil
}

func validateProxyURL(rawURL string) error {
	if rawURL == "" {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("http_engine: parse proxy url: %w", err)
	}
	if u.Host == "" {
		return fmt.Errorf("http_engine: proxy URL has no host")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
		return nil
	default:
		return fmt.Errorf("http_engine: unsupported proxy scheme: %s", u.Scheme)
	}
}

// dialViaProxy creates a TCP connection through a proxy (SOCKS5 or HTTP CONNECT).
func dialViaProxy(ctx context.Context, rawProxyURL, network, addr string) (net.Conn, error) {
	u, err := url.Parse(rawProxyURL)
	if err != nil {
		return nil, fmt.Errorf("http_engine: parse proxy url: %w", err)
	}

	switch u.Scheme {
	case "socks5", "socks5h":
		return dialViaSocks5(ctx, u, network, addr)
	case "http", "https":
		return dialViaHTTPConnect(ctx, u, addr)
	default:
		return nil, fmt.Errorf("http_engine: unsupported proxy scheme: %s", u.Scheme)
	}
}

// dialViaSocks5 connects through a SOCKS5 proxy with optional auth.
func dialViaSocks5(ctx context.Context, u *url.URL, network, addr string) (net.Conn, error) {
	var auth *proxy.Auth
	if u.User != nil {
		pass, _ := u.User.Password()
		auth = &proxy.Auth{
			User:     u.User.Username(),
			Password: pass,
		}
	}

	forward := &net.Dialer{Timeout: 10 * time.Second}
	dialer, err := proxy.SOCKS5("tcp", u.Host, auth, forward)
	if err != nil {
		return nil, fmt.Errorf("http_engine: create socks5 dialer: %w", err)
	}

	if cd, ok := dialer.(proxy.ContextDialer); ok {
		return cd.DialContext(ctx, network, addr)
	}
	return dialer.Dial(network, addr)
}

// dialViaHTTPConnect creates a tunnel through an HTTP proxy using CONNECT.
func dialViaHTTPConnect(ctx context.Context, u *url.URL, addr string) (net.Conn, error) {
	return dialViaHTTPConnectWithTLSConfig(ctx, u, addr, nil)
}

func dialViaHTTPConnectWithTLSConfig(ctx context.Context, u *url.URL, addr string, tlsConfig *stdtls.Config) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, fmt.Errorf("http_engine: connect to http proxy: %w", err)
	}
	connectionOK := false
	defer func() {
		if !connectionOK {
			_ = conn.Close()
		}
	}()

	if u.Scheme == "https" {
		config := &stdtls.Config{ //nolint:gosec -- certificate verification remains enabled.
			MinVersion: stdtls.VersionTLS12,
			ServerName: u.Hostname(),
		}
		if tlsConfig != nil {
			config = tlsConfig.Clone()
			if config.ServerName == "" {
				config.ServerName = u.Hostname()
			}
			if config.MinVersion == 0 {
				config.MinVersion = stdtls.VersionTLS12
			}
		}
		tlsConn := stdtls.Client(conn, config)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("http_engine: TLS handshake with HTTPS proxy: %w", err)
		}
		conn = tlsConn
	}

	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, fmt.Errorf("http_engine: set proxy CONNECT deadline: %w", err)
		}
	}

	// Build CONNECT request with Basic auth.
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)
	if u.User != nil {
		pass, _ := u.User.Password()
		creds := base64.StdEncoding.EncodeToString(
			[]byte(u.User.Username() + ":" + pass))
		req += "Proxy-Authorization: Basic " + creds + "\r\n"
	}
	req += "\r\n"

	if _, err := conn.Write([]byte(req)); err != nil {
		return nil, fmt.Errorf("http_engine: send CONNECT: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
	if err != nil {
		return nil, fmt.Errorf("http_engine: read CONNECT response: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("http_engine: CONNECT rejected: %s", resp.Status)
	}

	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("http_engine: clear proxy CONNECT deadline: %w", err)
	}
	connectionOK = true
	return conn, nil
}

// redactProxy returns a proxy URL with credentials masked for logging.
func redactProxy(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "***"
	}
	if u.User != nil {
		u.User = url.UserPassword("***", "***")
	}
	return u.String()
}

func (e *HTTPEngine) Name() string { return "http" }

// Supports rejects options that require a rendered browser document. A false
// value is an explicit signal to dispatchers to skip HTTP rather than silently
// returning a response that did not honor the request.
func (e *HTTPEngine) Supports(req *FetchRequest) bool {
	if req == nil {
		return false
	}
	return !req.Stealth &&
		!req.RemoveOverlays &&
		!req.BlockAds &&
		len(req.Actions) == 0 &&
		req.CDPURL == ""
}

func (e *HTTPEngine) Fetch(ctx context.Context, req *FetchRequest) (*FetchResult, error) {
	if req == nil {
		return nil, fmt.Errorf("http_engine: nil fetch request")
	}
	if !e.Supports(req) {
		return nil, fmt.Errorf("%w: http engine cannot honor browser-only options", ErrUnsupportedRequest)
	}

	fetchCtx, cancel := requestContext(ctx, req.Timeout)
	defer cancel()

	maximumBodyBytes, err := responseBodyLimit(req.MaximumBodyBytes)
	if err != nil {
		return nil, err
	}
	if req.Mode != FetchModeDefault && req.Mode != FetchModeObservation {
		return nil, fmt.Errorf("http_engine: invalid fetch mode %d", req.Mode)
	}

	client, cleanup, err := e.clientForRequest(req.ProxyURL, req.CheckRedirect)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	httpReq, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, req.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("http_engine: build request: %w", err)
	}

	// Simulate browser-like headers.
	httpReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36")
	httpReq.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	httpReq.Header.Set("Accept-Language", "en-US,en;q=0.9")
	httpReq.Header.Set("Accept-Encoding", "identity") // no compression for simplicity

	// Apply custom headers (override defaults if provided).
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}

	// Apply cookies.
	for i := range req.Cookies {
		httpReq.AddCookie(&req.Cookies[i])
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http_engine: do request: %w", err)
	}
	defer resp.Body.Close()

	body, err := readResponseBody(resp.Body, maximumBodyBytes)
	if err != nil {
		return nil, err
	}

	bodyStr := string(body)

	// If the response isn't successful HTML, treat it as a failure so the
	// dispatcher can escalate to a browser engine.
	ct := resp.Header.Get("Content-Type")
	if req.Mode == FetchModeDefault && (resp.StatusCode >= 400 || !isHTMLContentType(ct)) {
		return nil, fmt.Errorf("http_engine: non-html or error status %d (content-type: %s)", resp.StatusCode, ct)
	}

	title := extractTitle(bodyStr)
	finalURL := resp.Request.URL.String()

	return &FetchResult{
		HTML:        bodyStr,
		Title:       title,
		StatusCode:  resp.StatusCode,
		FinalURL:    finalURL,
		EngineName:  e.Name(),
		ContentType: ct,
	}, nil
}

// clientForRequest returns an immutable client for the selected proxy. The
// shared default client is reused only when no override is requested; override
// clients and transports are request-local, making concurrent proxy selection
// race-free.
func (e *HTTPEngine) clientForRequest(proxyOverride string, checkRedirect func(*http.Request, []*http.Request) error) (*http.Client, func(), error) {
	effectiveProxy := proxyOverride
	if effectiveProxy == "" {
		if e.pool.Len() > 1 {
			// Two or more exits: round-robin a per-request client below.
			effectiveProxy = e.pool.Next()
		} else {
			// Zero or one exit: the shared client already covers the default.
			effectiveProxy = e.defaultProxyURL
		}
	}
	if effectiveProxy == "" || effectiveProxy == e.defaultProxyURL {
		if e.clientErr != nil {
			return nil, func() {}, e.clientErr
		}
		if checkRedirect != nil {
			client := *e.client
			client.CheckRedirect = checkRedirect
			return &client, func() {}, nil
		}
		return e.client, func() {}, nil
	}
	client, err := newHTTPClient(effectiveProxy)
	if err != nil {
		return nil, func() {}, err
	}
	if checkRedirect != nil {
		client.CheckRedirect = checkRedirect
	}
	return client, client.CloseIdleConnections, nil
}

func responseBodyLimit(requested int64) (int64, error) {
	if requested < 0 || requested > hardMaximumResponseBodyBytes {
		return 0, fmt.Errorf("http_engine: maximum body bytes must be zero or between 1 and %d", hardMaximumResponseBodyBytes)
	}
	if requested == 0 {
		return defaultMaximumResponseBodyBytes, nil
	}
	return requested, nil
}

func readResponseBody(body io.Reader, maximumBytes int64) ([]byte, error) {
	contents, err := io.ReadAll(io.LimitReader(body, maximumBytes+1))
	if err != nil {
		return nil, fmt.Errorf("http_engine: read body: %w", err)
	}
	if int64(len(contents)) > maximumBytes {
		return nil, fmt.Errorf("%w: maximum is %d bytes", ErrResponseBodyTooLarge, maximumBytes)
	}
	return contents, nil
}

// isHTMLContentType returns true if the content-type header looks like HTML.
func isHTMLContentType(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.Contains(ct, "text/html") || strings.Contains(ct, "application/xhtml+xml")
}

// extractTitle uses the Go HTML tokenizer to find the first <title> element.
func extractTitle(htmlStr string) string {
	tokenizer := html.NewTokenizer(strings.NewReader(htmlStr))
	inTitle := false
	for {
		tt := tokenizer.Next()
		switch tt {
		case html.ErrorToken:
			return ""
		case html.StartTagToken:
			tn, _ := tokenizer.TagName()
			if string(tn) == "title" {
				inTitle = true
			}
		case html.TextToken:
			if inTitle {
				return strings.TrimSpace(string(tokenizer.Text()))
			}
		case html.EndTagToken:
			if inTitle {
				return ""
			}
		}
	}
}
