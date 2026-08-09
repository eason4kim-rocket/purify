package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/use-agent/purify/publicnet"
	xproxy "golang.org/x/net/proxy"
)

// Relay is a local SOCKS5 proxy (no auth) that forwards all connections
// through an external authenticated proxy. This allows Chrome (which cannot
// handle SOCKS5 auth or HTTP proxy auth without CDP conflicts) to use
// authenticated proxies transparently.
type Relay struct {
	listener  net.Listener
	dial      publicnet.DialContextFunc
	done      chan struct{}
	serveDone chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	closeErr  error
	handlers  sync.WaitGroup
}

// StartRelay creates a local SOCKS5 relay on 127.0.0.1 (random port)
// that forwards connections through the given external proxy URL.
// Supports both socks5:// and http:// external proxies with auth.
func StartRelay(externalProxyURL string) (*Relay, error) {
	return startRelay(func(ctx context.Context, _ string, target string) (net.Conn, error) {
		return dialExternal(ctx, externalProxyURL, target)
	})
}

// StartDirectRelay creates a local SOCKS5 relay whose every CONNECT request is
// passed to dialContext. Callers can inject publicnet.Policy.DialContext to
// enforce fresh DNS validation and literal-IP pinning for browser traffic.
// dialContext must honor context cancellation so Close can stop in-flight
// connection attempts promptly.
func StartDirectRelay(dialContext publicnet.DialContextFunc) (*Relay, error) {
	if dialContext == nil {
		return nil, errors.New("proxy relay: direct dialer is nil")
	}
	return startRelay(dialContext)
}

func startRelay(dialContext publicnet.DialContextFunc) (*Relay, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("proxy relay: listen: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	r := &Relay{
		listener:  listener,
		dial:      dialContext,
		done:      make(chan struct{}),
		serveDone: make(chan struct{}),
		ctx:       ctx,
		cancel:    cancel,
	}

	go r.serve()
	slog.Info("proxy relay started", "addr", r.Addr())
	return r, nil
}

// Addr returns the local listen address (e.g., "127.0.0.1:12345").
func (r *Relay) Addr() string {
	return r.listener.Addr().String()
}

// Close stops the relay.
func (r *Relay) Close() error {
	r.closeOnce.Do(func() {
		r.cancel()
		close(r.done)
		r.closeErr = r.listener.Close()
		<-r.serveDone
		r.handlers.Wait()
	})
	return r.closeErr
}

func (r *Relay) serve() {
	defer close(r.serveDone)
	for {
		conn, err := r.listener.Accept()
		if err != nil {
			select {
			case <-r.done:
				return
			default:
				continue
			}
		}
		r.handlers.Add(1)
		go func() {
			defer r.handlers.Done()
			r.handle(conn)
		}()
	}
}

func (r *Relay) handle(client net.Conn) {
	defer client.Close()
	stopClientClose := context.AfterFunc(r.ctx, func() { _ = client.Close() })
	defer stopClientClose()

	// ── SOCKS5 handshake (no auth) ──────────────────────────────────
	buf := make([]byte, 258)

	// 1. Read greeting: [VER, NMETHODS, METHODS...]
	if _, err := io.ReadFull(client, buf[:2]); err != nil || buf[0] != 0x05 {
		return
	}
	nMethods := int(buf[1])
	if _, err := io.ReadFull(client, buf[:nMethods]); err != nil {
		return
	}

	// 2. Reply: no auth required
	if _, err := client.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	// 3. Read request: [VER, CMD, RSV, ATYP, ...]
	if _, err := io.ReadFull(client, buf[:4]); err != nil {
		return
	}
	if buf[0] != 0x05 || buf[1] != 0x01 { // only CONNECT
		client.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	// Parse target address based on ATYP. Keep host resolution delegated to the
	// injected dialer so domain requests from Chrome cannot bypass its policy.
	var target string
	switch buf[3] {
	case 0x01: // IPv4
		if _, err := io.ReadFull(client, buf[:6]); err != nil {
			return
		}
		host := net.IP(buf[:4]).String()
		target = net.JoinHostPort(host, fmt.Sprint(binary.BigEndian.Uint16(buf[4:6])))
	case 0x03: // Domain
		if _, err := io.ReadFull(client, buf[:1]); err != nil {
			return
		}
		domainLen := int(buf[0])
		if _, err := io.ReadFull(client, buf[:domainLen+2]); err != nil {
			return
		}
		target = net.JoinHostPort(
			string(buf[:domainLen]),
			fmt.Sprint(binary.BigEndian.Uint16(buf[domainLen:domainLen+2])),
		)
	case 0x04: // IPv6
		if _, err := io.ReadFull(client, buf[:18]); err != nil {
			return
		}
		ip := net.IP(buf[:16])
		port := binary.BigEndian.Uint16(buf[16:18])
		target = net.JoinHostPort(ip.String(), fmt.Sprint(port))
	default:
		writeSOCKSReply(client, 0x08)
		return
	}

	// ── Connect through the configured policy or external proxy ─────
	dialCtx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
	remote, err := r.dial(dialCtx, "tcp", target)
	cancel()
	if err != nil {
		slog.Debug("proxy relay: dial failed", "error", err)
		writeSOCKSReply(client, socksReplyForError(err))
		return
	}
	defer remote.Close()
	stopRemoteClose := context.AfterFunc(r.ctx, func() { _ = remote.Close() })
	defer stopRemoteClose()

	// 4. Reply: success
	if !writeSOCKSReply(client, 0x00) {
		return
	}

	// ── Relay data ──────────────────────────────────────────────────
	copyDone := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(remote, client)
		copyDone <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, remote)
		copyDone <- struct{}{}
	}()
	// A half-closed browser connection must not leave the opposite copy and its
	// upstream socket alive until the whole relay shuts down. Closing both ends
	// after either direction finishes makes every tunnel self-reaping.
	<-copyDone
	_ = client.Close()
	_ = remote.Close()
	<-copyDone
}

func writeSOCKSReply(connection net.Conn, reply byte) bool {
	_, err := connection.Write([]byte{0x05, reply, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	return err == nil
}

func socksReplyForError(err error) byte {
	switch {
	case errors.Is(err, publicnet.ErrNotPublic), errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return 0x02 // connection not allowed by ruleset
	case errors.Is(err, syscall.ENETUNREACH):
		return 0x03 // network unreachable
	case errors.Is(err, syscall.EHOSTUNREACH):
		return 0x04 // host unreachable
	case errors.Is(err, syscall.ECONNREFUSED):
		return 0x05 // connection refused
	default:
		return 0x01 // general SOCKS server failure
	}
}

// dialExternal connects to the target through the external proxy.
func dialExternal(ctx context.Context, proxyURL, target string) (net.Conn, error) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, err
	}

	switch u.Scheme {
	case "socks5", "socks5h":
		return dialExternalSocks5(ctx, u, target)
	case "http", "https":
		return dialExternalHTTPConnect(ctx, u, target)
	default:
		return nil, fmt.Errorf("unsupported proxy scheme: %s", u.Scheme)
	}
}

func dialExternalSocks5(ctx context.Context, u *url.URL, target string) (net.Conn, error) {
	var auth *xproxy.Auth
	if u.User != nil {
		pass, _ := u.User.Password()
		auth = &xproxy.Auth{User: u.User.Username(), Password: pass}
	}
	forward := &net.Dialer{Timeout: 10 * time.Second}
	dialer, err := xproxy.SOCKS5("tcp", u.Host, auth, forward)
	if err != nil {
		return nil, err
	}
	if contextDialer, ok := dialer.(xproxy.ContextDialer); ok {
		return contextDialer.DialContext(ctx, "tcp", target)
	}
	return dialer.Dial("tcp", target)
}

func dialExternalHTTPConnect(ctx context.Context, u *url.URL, target string) (net.Conn, error) {
	return dialExternalHTTPConnectWithTLSConfig(ctx, u, target, nil)
}

func dialExternalHTTPConnectWithTLSConfig(ctx context.Context, u *url.URL, target string, tlsConfig *tls.Config) (net.Conn, error) {
	if err := validateHTTPConnectTarget(target); err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, fmt.Errorf("connect to proxy: %w", err)
	}
	connectionOK := false
	defer func() {
		if !connectionOK {
			_ = conn.Close()
		}
	}()

	if u.Scheme == "https" {
		config := &tls.Config{ //nolint:gosec -- certificate verification remains enabled.
			MinVersion: tls.VersionTLS12,
			ServerName: u.Hostname(),
		}
		if tlsConfig != nil {
			config = tlsConfig.Clone()
			if config.ServerName == "" {
				config.ServerName = u.Hostname()
			}
			if config.MinVersion == 0 {
				config.MinVersion = tls.VersionTLS12
			}
		}
		tlsConn := tls.Client(conn, config)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("TLS handshake with HTTPS proxy: %w", err)
		}
		conn = tlsConn
	}

	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, fmt.Errorf("set proxy CONNECT deadline: %w", err)
		}
	}

	// Build CONNECT request with Basic auth
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if u.User != nil {
		pass, _ := u.User.Password()
		creds := base64.StdEncoding.EncodeToString(
			[]byte(u.User.Username() + ":" + pass))
		req += "Proxy-Authorization: Basic " + creds + "\r\n"
	}
	req += "\r\n"

	if _, err := conn.Write([]byte(req)); err != nil {
		return nil, fmt.Errorf("send CONNECT: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
	if err != nil {
		return nil, fmt.Errorf("read CONNECT response: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("CONNECT rejected: %s", resp.Status)
	}

	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("clear proxy CONNECT deadline: %w", err)
	}
	connectionOK = true
	return conn, nil
}

func validateHTTPConnectTarget(target string) error {
	host, rawPort, err := net.SplitHostPort(target)
	if err != nil || host == "" || rawPort == "" || strings.Trim(rawPort, "0123456789") != "" {
		return errors.New("proxy relay: invalid CONNECT target")
	}
	port, err := strconv.ParseUint(rawPort, 10, 16)
	if err != nil || port == 0 {
		return errors.New("proxy relay: invalid CONNECT target")
	}
	if (strings.HasPrefix(target, "[") || strings.Contains(host, ":")) && net.ParseIP(host) == nil {
		return errors.New("proxy relay: invalid CONNECT target")
	}
	for _, character := range host {
		if unicode.IsControl(character) || unicode.IsSpace(character) || strings.ContainsRune("/?#\\[]@", character) {
			return errors.New("proxy relay: invalid CONNECT target")
		}
	}
	return nil
}
