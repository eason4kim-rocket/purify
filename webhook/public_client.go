package webhook

import (
	"crypto/tls"
	"errors"
	"net/http"
	"time"

	"github.com/use-agent/purify/publicnet"
)

const DefaultOutboxHTTPTimeout = 10 * time.Second

// NewPublicHTTPClient builds the production HTTP boundary for durable webhook
// delivery. DNS is resolved and pinned by policy at every dial, ambient proxy
// variables are ignored, and redirects are returned to RawDeliverer instead
// of forwarding a signed payload or secret to another target.
func NewPublicHTTPClient(policy *publicnet.Policy, timeout time.Duration) (*http.Client, error) {
	if policy == nil {
		return nil, errors.New("webhook: public network policy is required")
	}
	if timeout < 0 {
		return nil, errors.New("webhook: HTTP timeout cannot be negative")
	}
	if timeout == 0 {
		timeout = DefaultOutboxHTTPTimeout
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           policy.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12}, //nolint:gosec -- certificate verification remains enabled.
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}
