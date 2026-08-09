package discovery

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/use-agent/purify/publicnet"
)

// IPResolver is the DNS boundary used by the safe discovery transport.
type IPResolver = publicnet.Resolver

// DialContextFunc is injectable for deterministic transport tests.
type DialContextFunc = publicnet.DialContextFunc

type safeHTTPClient struct {
	client       *http.Client
	maxRedirects int
	allowPrivate bool
	policy       *publicnet.Policy
}

func newSafeHTTPClient(cfg Config) *safeHTTPClient {
	policy := publicnet.NewPolicy(publicnet.Options{
		AllowPrivateNetworks: cfg.AllowPrivateNetworks,
		Resolver:             cfg.Resolver,
		DialContext:          cfg.DialContext,
	})
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           policy.DialContext,
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
		allowPrivate: cfg.AllowPrivateNetworks,
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
	canonical, parsed, err := normalizeURL(request.URL.String(), nil, client.allowPrivate)
	if err != nil {
		return fmt.Errorf("discovery: reject redirect target: %w", err)
	}
	for _, previous := range via {
		previousCanonical, _, normalizeErr := normalizeURL(previous.URL.String(), nil, client.allowPrivate)
		if normalizeErr == nil && previousCanonical == canonical {
			return errors.New("discovery: redirect cycle detected")
		}
	}
	if _, err := client.policy.Resolve(request.Context(), parsed.Hostname()); err != nil {
		return fmt.Errorf("discovery: reject redirect target: %w", err)
	}
	return nil
}
