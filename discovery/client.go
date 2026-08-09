package discovery

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// IPResolver is the DNS boundary used by the safe discovery transport.
type IPResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

// DialContextFunc is injectable for deterministic transport tests.
type DialContextFunc func(context.Context, string, string) (net.Conn, error)

type safeHTTPClient struct {
	client       *http.Client
	maxRedirects int
	policy       targetPolicy
}

type targetPolicy struct {
	allowPrivate bool
	resolver     IPResolver
	dial         DialContextFunc
}

func newSafeHTTPClient(cfg Config) *safeHTTPClient {
	resolver := cfg.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	dial := cfg.DialContext
	if dial == nil {
		networkDialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		dial = networkDialer.DialContext
	}
	policy := targetPolicy{allowPrivate: cfg.AllowPrivateNetworks, resolver: resolver, dial: dial}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           policy.dialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: cfg.Timeout,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12}, //nolint:gosec -- verification remains enabled.
	}
	safe := &safeHTTPClient{
		maxRedirects: cfg.MaxRedirects,
		policy:       policy,
	}
	safe.client = &http.Client{
		Transport: transport,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			return safe.checkRedirect(request, via)
		},
	}
	return safe
}

func (client *safeHTTPClient) Do(request *http.Request) (*http.Response, error) {
	return client.client.Do(request)
}

func (client *safeHTTPClient) checkRedirect(request *http.Request, via []*http.Request) error {
	if len(via) > client.maxRedirects {
		return fmt.Errorf("discovery: redirect limit %d exceeded", client.maxRedirects)
	}
	canonical, parsed, err := normalizeURL(request.URL.String(), nil, client.policy.allowPrivate)
	if err != nil {
		return fmt.Errorf("discovery: reject redirect target: %w", err)
	}
	for _, previous := range via {
		previousCanonical, _, normalizeErr := normalizeURL(previous.URL.String(), nil, client.policy.allowPrivate)
		if normalizeErr == nil && previousCanonical == canonical {
			return errors.New("discovery: redirect cycle detected")
		}
	}
	if _, err := client.policy.resolve(request.Context(), parsed.Hostname()); err != nil {
		return fmt.Errorf("discovery: reject redirect target: %w", err)
	}
	return nil
}

func (policy targetPolicy) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("discovery: parse dial address: %w", err)
	}
	addresses, err := policy.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	var failures []error
	for _, candidate := range addresses {
		connection, dialErr := policy.dial(ctx, network, net.JoinHostPort(candidate.String(), port))
		if dialErr == nil {
			return connection, nil
		}
		failures = append(failures, dialErr)
		if ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("discovery: dial %s: %w", address, errors.Join(failures...))
}

func (policy targetPolicy) resolve(ctx context.Context, hostname string) ([]netip.Addr, error) {
	hostname = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(hostname)), ".")
	if hostname == "" {
		return nil, errors.New("target host is empty")
	}
	if address, err := netip.ParseAddr(hostname); err == nil {
		address = address.Unmap()
		if !policy.allowPrivate && !isPublicAddress(address) {
			return nil, fmt.Errorf("target address %s is private, local, or reserved", address)
		}
		return []netip.Addr{address}, nil
	}
	resolved, err := policy.resolver.LookupNetIP(ctx, "ip", hostname)
	if err != nil {
		return nil, fmt.Errorf("resolve target host %q: %w", hostname, err)
	}
	addresses := make([]netip.Addr, 0, len(resolved))
	seen := make(map[netip.Addr]struct{}, len(resolved))
	for _, value := range resolved {
		if !value.IsValid() {
			return nil, fmt.Errorf("resolver returned an invalid address for %q", hostname)
		}
		address := value.Unmap()
		if !policy.allowPrivate && !isPublicAddress(address) {
			return nil, fmt.Errorf("target host %q resolves to private, local, or reserved address %s", hostname, address)
		}
		if _, duplicate := seen[address]; duplicate {
			continue
		}
		seen[address] = struct{}{}
		addresses = append(addresses, address)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("target host %q resolved to no addresses", hostname)
	}
	return addresses, nil
}
