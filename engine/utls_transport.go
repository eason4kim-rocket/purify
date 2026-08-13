package engine

import (
	"bufio"
	"context"
	stdtls "crypto/tls"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// chromeRoundTripper dials one connection per request, presents a Chrome TLS
// fingerprint via utls, and then speaks whichever protocol ALPN negotiates:
// HTTP/2 when the server offers it, otherwise HTTP/1.1.
//
// It exists because net/http's own HTTP/2 handoff type-asserts *tls.Conn, which
// a utls connection is not — so a utls connection that negotiated h2 would
// otherwise be driven as HTTP/1.1, contradicting the very fingerprint we sent.
// Chrome always offers h2 in its ALPN, so offering only http/1.1 (the previous
// behavior) was itself a JA4 tell.
//
// One connection per request is deliberate: the fetch layer is polite and
// mostly fetches one page per host, so cross-request pooling is a non-goal.
type chromeRoundTripper struct {
	// dialTCP returns a raw TCP connection to the target address. It is the
	// only network boundary, so a proxy dialer can be injected here later
	// without changing the TLS or protocol handling below.
	dialTCP func(ctx context.Context, addr string) (net.Conn, error)
	// tlsConfig optionally overrides RootCAs / ServerName / verification. Nil
	// means system roots with verification on.
	tlsConfig *stdtls.Config
	h2        *http2.Transport
}

func (rt *chromeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	conn, err := rt.dialTCP(req.Context(), canonicalAddr(req.URL))
	if err != nil {
		return nil, ctxErrOr(req.Context(), err)
	}

	// Plain HTTP needs no TLS and never negotiates h2 here.
	if req.URL.Scheme != "https" {
		return roundTripHTTP1(conn, req)
	}

	config := &utls.Config{ServerName: req.URL.Hostname()}
	if rt.tlsConfig != nil {
		if rt.tlsConfig.RootCAs != nil {
			config.RootCAs = rt.tlsConfig.RootCAs
		}
		if rt.tlsConfig.ServerName != "" {
			config.ServerName = rt.tlsConfig.ServerName
		}
		config.InsecureSkipVerify = rt.tlsConfig.InsecureSkipVerify
	}

	// HelloChrome_Auto builds a fresh, correct Chrome ClientHello per connection
	// (its own key shares, GREASE, and randomness). A shared preset spec must
	// never be reused across connections: utls mutates its key material, so the
	// second handshake would fail with a bad record MAC.
	uconn := utls.UClient(conn, config, utls.HelloChrome_Auto)
	if err := uconn.HandshakeContext(req.Context()); err != nil {
		_ = conn.Close()
		return nil, ctxErrOr(req.Context(), err)
	}

	if uconn.ConnectionState().NegotiatedProtocol == http2.NextProtoTLS {
		return rt.roundTripHTTP2(uconn, req)
	}
	return roundTripHTTP1(uconn, req)
}

// roundTripHTTP2 drives one request over a fresh HTTP/2 connection. The client
// connection is closed when the response body is closed, so the connection does
// not leak despite not being pooled.
func (rt *chromeRoundTripper) roundTripHTTP2(conn net.Conn, req *http.Request) (*http.Response, error) {
	clientConn, err := rt.h2.NewClientConn(conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	resp, err := clientConn.RoundTrip(req)
	if err != nil {
		_ = clientConn.Close()
		return nil, ctxErrOr(req.Context(), err)
	}
	resp.Body = &connClosingBody{ReadCloser: resp.Body, closeConn: clientConn.Close}
	return resp, nil
}

// roundTripHTTP1 writes one request and reads its response over conn in
// origin-form, which is correct because conn is already a direct (or tunneled)
// connection to the target rather than a forward proxy.
//
// Cancellation closes conn through context.AfterFunc rather than a raw read
// deadline: AfterFunc runs only after the context is already done, so the
// unblocked read reliably reports context.DeadlineExceeded instead of a bare
// i/o timeout that raced the context timer. The hook stays armed until the body
// is closed so a mid-body timeout still interrupts the stream.
func roundTripHTTP1(conn net.Conn, req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	if err := req.Write(conn); err != nil {
		stop()
		_ = conn.Close()
		return nil, ctxErrOr(ctx, err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		stop()
		_ = conn.Close()
		return nil, ctxErrOr(ctx, err)
	}
	resp.Body = &connClosingBody{ReadCloser: resp.Body, closeConn: func() error {
		stop()
		return conn.Close()
	}}
	return resp, nil
}

// ctxErrOr returns the context error when the request context is already done,
// so a deadline surfaces as context.DeadlineExceeded rather than the raw
// connection deadline that our per-request SetDeadline would otherwise produce.
func ctxErrOr(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

// connClosingBody closes the underlying connection (or HTTP/2 client
// connection) exactly once, after the response body is closed.
type connClosingBody struct {
	io.ReadCloser
	closeConn func() error
	once      sync.Once
}

func (b *connClosingBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() { _ = b.closeConn() })
	return err
}

// canonicalAddr returns host:port for a URL, filling in the scheme default port.
func canonicalAddr(u *url.URL) string {
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return net.JoinHostPort(host, port)
}
