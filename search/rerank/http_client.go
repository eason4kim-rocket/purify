package rerank

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/use-agent/purify/publicnet"
	"golang.org/x/net/idna"
)

const (
	rerankMaximumResponseHeaderBytes = 64 << 10
	rerankMaximumConnectionsPerHost  = 4
)

var (
	errRerankDestinationRejected = errors.New("rerank: destination rejected")
	errRerankRedirectRejected    = errors.New("rerank: redirect rejected")
)

// rerankHTTPClientConfig is intentionally package-private: only the managed
// reranker adapter may construct this process-owned network boundary.
type rerankHTTPClientConfig struct {
	endpoint     *url.URL
	allowPrivate bool
	timeout      time.Duration
	resolver     publicnet.Resolver
	dialContext  publicnet.DialContextFunc
}

// rerankHTTPClient hides the mutable net/http implementation from callers and
// binds every request to the exact endpoint whose network mode was validated.
type rerankHTTPClient struct {
	endpoint string
	client   *http.Client
}

// newRerankHTTPClient returns a client with a dedicated immutable destination
// policy. It never reads ambient proxy variables and never follows redirects.
func newRerankHTTPClient(config rerankHTTPClientConfig) (*rerankHTTPClient, error) {
	mode, err := rerankModeForEndpoint(config.endpoint, config.allowPrivate)
	if err != nil {
		return nil, err
	}
	if config.timeout < time.Second || config.timeout > 10*time.Second {
		return nil, errors.New("rerank: HTTP timeout must be between 1 and 10 seconds")
	}

	resolver := config.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	dialContext := config.dialContext
	if dialContext == nil {
		dialer := &net.Dialer{Timeout: config.timeout, KeepAlive: 30 * time.Second}
		dialContext = dialer.DialContext
	}
	policy := &rerankDestinationPolicy{
		mode:     mode,
		resolver: resolver,
		dial:     dialContext,
	}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            policy.DialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           rerankMaximumConnectionsPerHost,
		MaxIdleConnsPerHost:    rerankMaximumConnectionsPerHost,
		MaxConnsPerHost:        rerankMaximumConnectionsPerHost,
		IdleConnTimeout:        30 * time.Second,
		TLSHandshakeTimeout:    config.timeout,
		ResponseHeaderTimeout:  config.timeout,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: rerankMaximumResponseHeaderBytes,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}
	return &rerankHTTPClient{
		endpoint: config.endpoint.String(),
		client: &http.Client{
			Transport: transport,
			Timeout:   config.timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errRerankRedirectRejected
			},
		},
	}, nil
}

func (client *rerankHTTPClient) Do(request *http.Request) (*http.Response, error) {
	if client == nil || client.client == nil || request == nil || request.URL == nil ||
		request.URL.String() != client.endpoint {
		return nil, errRerankDestinationRejected
	}
	return client.client.Do(request)
}

func (client *rerankHTTPClient) CloseIdleConnections() {
	if client != nil && client.client != nil {
		client.client.CloseIdleConnections()
	}
}

type rerankDestinationMode uint8

const (
	rerankPublicOnly rerankDestinationMode = iota + 1
	rerankPrivateOnly
	rerankPublicOrPrivate
)

func rerankModeForEndpoint(endpoint *url.URL, allowPrivate bool) (rerankDestinationMode, error) {
	if endpoint == nil || !endpoint.IsAbs() || endpoint.Opaque != "" || endpoint.Host == "" ||
		endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery ||
		endpoint.Fragment != "" || endpoint.Path != "/v1/rerank" || endpoint.RawPath != "" {
		return 0, errors.New("rerank: endpoint is invalid")
	}
	switch strings.ToLower(endpoint.Scheme) {
	case "http":
		if !allowPrivate {
			return 0, errors.New("rerank: HTTP endpoint requires private-network opt-in")
		}
		return rerankPrivateOnly, nil
	case "https":
		if allowPrivate {
			return rerankPublicOrPrivate, nil
		}
		return rerankPublicOnly, nil
	default:
		return 0, errors.New("rerank: endpoint scheme must be HTTP or HTTPS")
	}
}

// rerankDestinationPolicy validates a complete fresh DNS answer and passes
// only validated literal addresses to its underlying dialer. It contains no
// mutable classification state and is safe for concurrent use.
type rerankDestinationPolicy struct {
	mode     rerankDestinationMode
	resolver publicnet.Resolver
	dial     publicnet.DialContextFunc
}

func (policy *rerankDestinationPolicy) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if policy == nil || policy.resolver == nil || policy.dial == nil {
		return nil, errors.New("rerank: destination policy is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, errors.New("rerank: unsupported network")
	}
	host, rawPort, err := net.SplitHostPort(address)
	if err != nil || rawPort == "" || strings.Trim(rawPort, "0123456789") != "" {
		return nil, errors.New("rerank: invalid destination port")
	}
	portNumber, err := strconv.ParseUint(rawPort, 10, 16)
	if err != nil || portNumber == 0 {
		return nil, errors.New("rerank: invalid destination port")
	}
	port := strconv.FormatUint(portNumber, 10)

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
	if err := ctx.Err(); err != nil {
		failures = append(failures, err)
	}
	return nil, fmt.Errorf("rerank: dial failed: %w", errors.Join(failures...))
}

func (policy *rerankDestinationPolicy) resolve(ctx context.Context, hostname string) ([]netip.Addr, error) {
	hostname = strings.TrimSuffix(strings.ToLower(hostname), ".")
	if hostname == "" {
		return nil, errRerankDestinationRejected
	}
	if literal, err := netip.ParseAddr(hostname); err == nil {
		if literal.Zone() != "" {
			return nil, errRerankDestinationRejected
		}
		literal = literal.Unmap()
		if !policy.accepts([]netip.Addr{literal}) {
			return nil, errRerankDestinationRejected
		}
		return []netip.Addr{literal}, nil
	}

	canonicalHost, err := idna.Lookup.ToASCII(hostname)
	if err != nil {
		return nil, errRerankDestinationRejected
	}
	canonicalHost = strings.TrimSuffix(strings.ToLower(canonicalHost), ".")
	if canonicalHost == "" || strings.Contains(canonicalHost, "%") {
		return nil, errRerankDestinationRejected
	}
	resolved, err := policy.resolver.LookupNetIP(ctx, "ip", canonicalHost)
	if err != nil {
		return nil, errors.New("rerank: resolve destination failed")
	}
	addresses := make([]netip.Addr, 0, len(resolved))
	seen := make(map[netip.Addr]struct{}, len(resolved))
	for _, value := range resolved {
		if !value.IsValid() || value.Zone() != "" {
			return nil, errRerankDestinationRejected
		}
		address := value.Unmap()
		if _, duplicate := seen[address]; duplicate {
			continue
		}
		seen[address] = struct{}{}
		addresses = append(addresses, address)
	}
	if len(addresses) == 0 || !policy.accepts(addresses) {
		return nil, errRerankDestinationRejected
	}
	return addresses, nil
}

type rerankAddressClass uint8

const (
	rerankInvalidAddress rerankAddressClass = iota
	rerankPublicAddress
	rerankPrivateAddress
)

func (policy *rerankDestinationPolicy) accepts(addresses []netip.Addr) bool {
	class := rerankInvalidAddress
	for _, address := range addresses {
		candidateClass := classifyRerankAddress(address)
		if candidateClass == rerankInvalidAddress || class != rerankInvalidAddress && class != candidateClass {
			return false
		}
		class = candidateClass
	}
	switch policy.mode {
	case rerankPublicOnly:
		return class == rerankPublicAddress
	case rerankPrivateOnly:
		return class == rerankPrivateAddress
	case rerankPublicOrPrivate:
		return class == rerankPublicAddress || class == rerankPrivateAddress
	default:
		return false
	}
}

func classifyRerankAddress(address netip.Addr) rerankAddressClass {
	address = address.Unmap()
	if !address.IsValid() || address.Zone() != "" {
		return rerankInvalidAddress
	}
	if address.IsLoopback() || address.IsPrivate() {
		return rerankPrivateAddress
	}
	if publicnet.IsPublicAddress(address) {
		return rerankPublicAddress
	}
	return rerankInvalidAddress
}
