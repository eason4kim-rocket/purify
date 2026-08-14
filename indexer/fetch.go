package indexer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/use-agent/purify/publicnet"
)

const (
	defaultUserAgent     = "PurifyIndex/1.0 (+https://github.com/Easonliuliang/purify)"
	defaultFetchTimeout  = 15 * time.Second
	defaultMaxPageBytes  = 2 << 20
	defaultMaxRedirects  = 5
	shortBodyRenderRunes = 200
)

var (
	ErrNotModified = errors.New("indexer: not modified")
	ErrFetchFailed = errors.New("indexer: fetch failed")
	ErrTooLarge    = errors.New("indexer: page exceeds byte limit")
	ErrUnindexable = errors.New("indexer: content type is not indexable")
)

// indexableContentType reports whether a response can become an index row.
// Only markup and plain text qualify: package blobs, media, and archive
// metadata otherwise arrive as empty-bodied rows that match nothing and
// spend the crawl budget. Plain text stays because RFCs are served as it.
// A missing header falls back to sniffing the body.
func indexableContentType(header string, body []byte) bool {
	mediaType := strings.ToLower(strings.TrimSpace(header))
	if at := strings.IndexByte(mediaType, ';'); at >= 0 {
		mediaType = strings.TrimSpace(mediaType[:at])
	}
	if mediaType == "" || mediaType == "application/octet-stream" {
		sniffed := http.DetectContentType(body)
		if at := strings.IndexByte(sniffed, ';'); at >= 0 {
			sniffed = sniffed[:at]
		}
		mediaType = strings.ToLower(strings.TrimSpace(sniffed))
	}
	switch mediaType {
	case "text/html", "application/xhtml+xml", "text/plain", "text/markdown":
		return true
	}
	return false
}

// FetchResult is one lightweight HTTP page.
type FetchResult struct {
	URL         string
	Status      int
	Body        []byte
	ETag        string
	LastMod     string
	ContentType string
	FetchedAt   time.Time
}

// Fetcher performs bounded, public-only page GETs with conditional headers.
type Fetcher struct {
	client       *http.Client
	userAgent    string
	maxBytes     int64
	now          func() time.Time
	allowPrivate bool
}

// FetcherConfig bounds one indexer HTTP client.
type FetcherConfig struct {
	Timeout              time.Duration
	MaxBytes             int64
	UserAgent            string
	AllowPrivateNetworks bool
	Policy               *publicnet.Policy
}

// NewFetcher builds a publicnet-backed client. Tests may allow private nets.
func NewFetcher(cfg FetcherConfig) (*Fetcher, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultFetchTimeout
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = defaultMaxPageBytes
	}
	if strings.TrimSpace(cfg.UserAgent) == "" {
		cfg.UserAgent = defaultUserAgent
	}
	policy := cfg.Policy
	if policy == nil {
		policy = publicnet.NewPolicy(publicnet.Options{AllowPrivateNetworks: cfg.AllowPrivateNetworks})
	}
	transport := &http.Transport{
		Proxy:               nil,
		DialContext:         policy.DialContext,
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &Fetcher{
		client: &http.Client{
			Transport: transport,
			Timeout:   cfg.Timeout,
			CheckRedirect: func(_ *http.Request, via []*http.Request) error {
				if len(via) >= defaultMaxRedirects {
					return errors.New("indexer: too many redirects")
				}
				return nil
			},
		},
		userAgent:    cfg.UserAgent,
		maxBytes:     cfg.MaxBytes,
		now:          time.Now,
		allowPrivate: cfg.AllowPrivateNetworks,
	}, nil
}

// Get fetches url. etag and lastMod enable 304 short-circuit.
func (f *Fetcher) Get(ctx context.Context, rawURL, etag, lastMod string) (FetchResult, error) {
	if ctx == nil {
		return FetchResult{}, fmt.Errorf("%w: context is required", ErrFetchFailed)
	}
	canonical, _, err := publicnet.NormalizeHTTPURL(rawURL, nil, f.allowPrivate)
	if err != nil {
		return FetchResult{}, fmt.Errorf("%w: %v", ErrFetchFailed, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, canonical, nil)
	if err != nil {
		return FetchResult{}, fmt.Errorf("%w: %v", ErrFetchFailed, err)
	}
	request.Header.Set("User-Agent", f.userAgent)
	if etag != "" {
		request.Header.Set("If-None-Match", etag)
	}
	if lastMod != "" {
		request.Header.Set("If-Modified-Since", lastMod)
	}
	response, err := f.client.Do(request)
	if err != nil {
		return FetchResult{}, fmt.Errorf("%w: %v", ErrFetchFailed, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified {
		return FetchResult{URL: canonical, Status: http.StatusNotModified, FetchedAt: f.now().UTC()}, ErrNotModified
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return FetchResult{URL: canonical, Status: response.StatusCode, FetchedAt: f.now().UTC()},
			fmt.Errorf("%w: status %d", ErrFetchFailed, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, f.maxBytes+1))
	if err != nil {
		return FetchResult{}, fmt.Errorf("%w: %v", ErrFetchFailed, err)
	}
	if int64(len(body)) > f.maxBytes {
		return FetchResult{URL: canonical, Status: response.StatusCode}, ErrTooLarge
	}
	return FetchResult{
		URL:         canonical,
		Status:      response.StatusCode,
		Body:        body,
		ETag:        response.Header.Get("ETag"),
		LastMod:     response.Header.Get("Last-Modified"),
		ContentType: response.Header.Get("Content-Type"),
		FetchedAt:   f.now().UTC(),
	}, nil
}

// NeedsRender reports that cleaned text is too short for a useful index row.
func NeedsRender(body string) bool {
	return utf8.RuneCountInString(strings.TrimSpace(body)) < shortBodyRenderRunes
}

// UserAgent returns the fetcher identity string.
func (f *Fetcher) UserAgent() string {
	if f == nil || f.userAgent == "" {
		return defaultUserAgent
	}
	return f.userAgent
}
