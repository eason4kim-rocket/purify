package search

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/publicnet"
)

const (
	braveProviderName         = "brave"
	braveSearchEndpoint       = "https://api.search.brave.com/res/v1/web/search"
	braveDefaultHTTPTimeout   = 8 * time.Second
	braveMaximumResponseBytes = MaxProviderMetadataBytes
	braveMaximumAPIKeyBytes   = 16 << 10
)

var (
	// ErrInvalidBraveConfig reports an unusable process-owned provider
	// configuration without echoing its credential.
	ErrInvalidBraveConfig = errors.New("search: invalid Brave provider configuration")
	// ErrInvalidBraveQuery is a defensive provider-boundary error. The Search
	// service normally rejects these values before selecting a provider.
	ErrInvalidBraveQuery = errors.New("search: invalid Brave provider query")
)

// BraveProvider maps Brave's private Web Search wire format into the
// provider-neutral Search contract. Its endpoint and HTTP transport are not
// caller configurable so the process credential cannot be redirected.
type BraveProvider struct {
	apiKey string
	client *http.Client
}

var _ Provider = (*BraveProvider)(nil)

// NewBraveProvider constructs the production Brave adapter with a dedicated
// public-only client. Ambient proxies are ignored, every new connection uses
// the policy's validated literal-IP dialer, and redirects are never followed.
func NewBraveProvider(apiKey string, policy *publicnet.Policy) (*BraveProvider, error) {
	if err := ValidateBraveAPIKey(apiKey); err != nil {
		return nil, err
	}
	client, err := newBraveHTTPClient(policy, braveDefaultHTTPTimeout)
	if err != nil {
		return nil, err
	}
	return &BraveProvider{apiKey: apiKey, client: client}, nil
}

// ValidateBraveAPIKey applies the process credential's inclusive byte bound
// and rejects every character unsafe for an HTTP header. The returned error
// never contains any part of the credential.
func ValidateBraveAPIKey(apiKey string) error {
	if apiKey == "" || len(apiKey) > braveMaximumAPIKeyBytes || !utf8.ValidString(apiKey) {
		return ErrInvalidBraveConfig
	}
	for _, character := range apiKey {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return ErrInvalidBraveConfig
		}
	}
	return nil
}

func newBraveHTTPClient(policy *publicnet.Policy, timeout time.Duration) (*http.Client, error) {
	if policy == nil || timeout <= 0 {
		return nil, ErrInvalidBraveConfig
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           policy.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   4,
		MaxConnsPerHost:       8,
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

func (provider *BraveProvider) Name() string {
	return braveProviderName
}

// CloseIdleConnections releases this provider's dedicated transport pool.
// It is safe to call during shutdown and on a nil or zero-value provider.
func (provider *BraveProvider) CloseIdleConnections() {
	if provider != nil && provider.client != nil {
		provider.client.CloseIdleConnections()
	}
}

func (provider *BraveProvider) Search(ctx context.Context, query ProviderQuery) ([]ProviderResult, error) {
	if provider == nil || provider.client == nil || provider.apiKey == "" {
		return nil, ErrInvalidBraveConfig
	}
	if err := validateBraveQuery(ctx, query); err != nil {
		return nil, err
	}

	parameters := make(url.Values, 5)
	parameters.Set("q", query.Text)
	parameters.Set("count", strconv.Itoa(query.Limit))
	parameters.Set("result_filter", "web")
	parameters.Set("text_decorations", "false")
	if freshness := braveFreshness(query.Freshness); freshness != "" {
		parameters.Set("freshness", freshness)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, braveSearchEndpoint+"?"+parameters.Encode(), nil)
	if err != nil {
		return nil, ErrInvalidBraveQuery
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Subscription-Token", provider.apiKey)
	// Do not set Accept-Encoding manually. The standard Transport requests and
	// transparently decompresses gzip, so the reader below charges decoded bytes.

	response, err := provider.client.Do(request)
	if err != nil {
		return nil, classifyBraveTransportError(ctx, err)
	}
	if response == nil || response.Body == nil {
		return nil, NewProviderError(ProviderErrorInvalidResponse, 0, nil)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, classifyBraveStatus(response.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, braveMaximumResponseBytes+1))
	if err != nil {
		return nil, classifyBraveTransportError(ctx, err)
	}
	if err := currentBraveContextError(ctx); err != nil {
		return nil, err
	}
	if len(body) > braveMaximumResponseBytes {
		return nil, NewProviderError(ProviderErrorInvalidResponse, response.StatusCode, nil)
	}

	if !utf8.Valid(body) {
		return nil, NewProviderError(ProviderErrorInvalidResponse, response.StatusCode, nil)
	}

	var envelope braveSearchResponse
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Type != "search" {
		return nil, NewProviderError(ProviderErrorInvalidResponse, response.StatusCode, nil)
	}
	if err := currentBraveContextError(ctx); err != nil {
		return nil, err
	}
	if envelope.Web == nil || len(envelope.Web.Results) == 0 {
		return []ProviderResult{}, nil
	}

	limit := min(query.Limit, len(envelope.Web.Results))
	results := make([]ProviderResult, 0, limit)
	for index := range limit {
		candidate := envelope.Web.Results[index]
		if len(candidate.Title) > MaxProviderTitleBytes || len(candidate.URL) > models.MaxSearchURLBytes ||
			len(candidate.Description) > MaxProviderSnippetBytes {
			return nil, NewProviderError(ProviderErrorInvalidResponse, response.StatusCode, nil)
		}
		results = append(results, ProviderResult{
			Rank:        index + 1,
			Score:       nil,
			Title:       candidate.Title,
			URL:         candidate.URL,
			Snippet:     candidate.Description,
			PublishedAt: bravePublishedAt(candidate.PageAge, candidate.Age),
		})
	}
	if err := currentBraveContextError(ctx); err != nil {
		return nil, err
	}
	return results, nil
}

func currentBraveContextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return NewProviderError(ProviderErrorTimeout, 0, err)
	}
	return nil
}

func validateBraveQuery(ctx context.Context, query ProviderQuery) error {
	if ctx == nil {
		return ErrInvalidBraveQuery
	}
	if err := ctx.Err(); err != nil {
		return NewProviderError(ProviderErrorTimeout, 0, err)
	}
	if !utf8.ValidString(query.Text) || strings.TrimSpace(query.Text) == "" ||
		utf8.RuneCountInString(query.Text) > models.MaxSearchQueryRunes ||
		len(strings.Fields(query.Text)) > models.MaxSearchQueryWords ||
		query.Limit < 1 || query.Limit > MaxProviderResults {
		return ErrInvalidBraveQuery
	}
	for _, character := range query.Text {
		if unicode.IsControl(character) {
			return ErrInvalidBraveQuery
		}
	}
	switch query.Freshness {
	case FreshnessAny, FreshnessDay, FreshnessWeek, FreshnessMonth, FreshnessYear:
		return nil
	default:
		return ErrInvalidBraveQuery
	}
}

func braveFreshness(freshness Freshness) string {
	switch freshness {
	case FreshnessDay:
		return "pd"
	case FreshnessWeek:
		return "pw"
	case FreshnessMonth:
		return "pm"
	case FreshnessYear:
		return "py"
	default:
		return ""
	}
}

func classifyBraveTransportError(ctx context.Context, err error) error {
	if ctx != nil {
		if contextError := ctx.Err(); contextError != nil {
			return NewProviderError(ProviderErrorTimeout, 0, contextError)
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return NewProviderError(ProviderErrorTimeout, 0, err)
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return NewProviderError(ProviderErrorTimeout, 0, err)
	}
	return NewProviderError(ProviderErrorUpstream, 0, err)
}

func classifyBraveStatus(statusCode int) error {
	switch statusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return NewProviderError(ProviderErrorAuthentication, statusCode, nil)
	case http.StatusTooManyRequests:
		return NewProviderError(ProviderErrorRateLimited, statusCode, nil)
	default:
		return NewProviderError(ProviderErrorUpstream, statusCode, nil)
	}
}

func bravePublishedAt(pageAge, age string) *time.Time {
	for _, value := range []string{pageAge, age} {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		for _, layout := range []string{
			time.RFC3339Nano,
			"2006-01-02T15:04:05.999999999",
			"2006-01-02T15:04:05",
			"2006-01-02",
		} {
			if parsed, err := time.Parse(layout, value); err == nil {
				parsed = parsed.UTC()
				return &parsed
			}
		}
	}
	return nil
}

type braveSearchResponse struct {
	Type string `json:"type"`
	Web  *struct {
		Results braveSearchResults `json:"results"`
	} `json:"web"`
}

type braveSearchResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
	PageAge     string `json:"page_age"`
	Age         string `json:"age"`
}

type braveSearchResults []braveSearchResult

// UnmarshalJSON prevents a small JSON array containing many empty objects from
// expanding into an unbounded in-memory result slice. Unknown result fields
// remain forward-compatible through the ordinary struct decoder.
func (results *braveSearchResults) UnmarshalJSON(encoded []byte) error {
	if bytes.Equal(bytes.TrimSpace(encoded), []byte("null")) {
		*results = nil
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok || delimiter != '[' {
		return errors.New("search: Brave results must be an array")
	}
	bounded := make(braveSearchResults, 0, min(MaxProviderResults, 8))
	for decoder.More() {
		if len(bounded) >= MaxProviderResults {
			return errors.New("search: Brave results exceed maximum count")
		}
		var candidate braveSearchResult
		if err := decoder.Decode(&candidate); err != nil {
			return err
		}
		bounded = append(bounded, candidate)
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) || token != nil {
		if err != nil {
			return err
		}
		return errors.New("search: Brave results contain trailing data")
	}
	*results = bounded
	return nil
}
