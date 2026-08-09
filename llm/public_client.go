package llm

import (
	"crypto/tls"
	"errors"
	"net/http"
	"time"

	"github.com/use-agent/purify/publicnet"
)

// NewPublicHTTPClient builds the production transport for request-scoped and
// managed OpenAI-compatible providers. DNS answers are checked and pinned at
// dial time, ambient proxies are ignored, and no redirect is followed with a
// caller credential or prompt body. http.ErrUseLastResponse deliberately
// returns the 3xx response with its body still owned by the caller.
func NewPublicHTTPClient(policy *publicnet.Policy, timeout time.Duration) (*http.Client, error) {
	if policy == nil {
		return nil, errors.New("llm: public network policy is required")
	}
	if timeout < 0 {
		return nil, errors.New("llm: HTTP timeout cannot be negative")
	}
	if timeout == 0 {
		timeout = defaultHTTPTimeout
	}

	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           policy.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   8,
		MaxConnsPerHost:       32,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12}, //nolint:gosec -- normal certificate verification remains enabled.
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}
