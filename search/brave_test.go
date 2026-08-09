package search

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/publicnet"
)

func TestBraveProviderConfigurationAndDedicatedTransport(t *testing.T) {
	if _, err := NewBraveProvider("valid-key", nil); !errors.Is(err, ErrInvalidBraveConfig) {
		t.Fatalf("NewBraveProvider(nil policy) error = %v", err)
	}
	if client, err := newBraveHTTPClient(nil, time.Second); client != nil || !errors.Is(err, ErrInvalidBraveConfig) {
		t.Fatalf("newBraveHTTPClient(nil) = %#v, %v", client, err)
	}
	if client, err := newBraveHTTPClient(publicnet.NewPolicy(publicnet.Options{}), 0); client != nil ||
		!errors.Is(err, ErrInvalidBraveConfig) {
		t.Fatalf("newBraveHTTPClient(zero timeout) = %#v, %v", client, err)
	}

	provider, err := NewBraveProvider("valid-key", publicnet.NewPolicy(publicnet.Options{}))
	if err != nil {
		t.Fatalf("NewBraveProvider() error = %v", err)
	}
	if provider.Name() != braveProviderName || provider.client.Timeout != braveDefaultHTTPTimeout ||
		provider.client.CheckRedirect == nil {
		t.Fatalf("provider identity/client = %q/%#v", provider.Name(), provider.client)
	}
	transport, ok := provider.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", provider.client.Transport)
	}
	if transport.Proxy != nil || transport.DialContext == nil || !transport.ForceAttemptHTTP2 ||
		transport.MaxIdleConns != 32 || transport.MaxIdleConnsPerHost != 4 || transport.MaxConnsPerHost != 8 ||
		transport.IdleConnTimeout != 30*time.Second || transport.TLSHandshakeTimeout != braveDefaultHTTPTimeout ||
		transport.ResponseHeaderTimeout != braveDefaultHTTPTimeout || transport.ExpectContinueTimeout != time.Second ||
		transport.TLSClientConfig == nil || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("unsafe or unbounded Brave transport = %#v", transport)
	}
	if err := provider.client.CheckRedirect(&http.Request{}, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("CheckRedirect() error = %v", err)
	}

	provider.CloseIdleConnections()
	(*BraveProvider)(nil).CloseIdleConnections()
}

func TestValidateBraveAPIKeyBoundariesAndRedaction(t *testing.T) {
	atLimit := strings.Repeat("k", braveMaximumAPIKeyBytes)
	if err := ValidateBraveAPIKey(atLimit); err != nil {
		t.Fatalf("ValidateBraveAPIKey(N) error = %v", err)
	}
	invalidUTF8 := string([]byte{'k', 0xff})
	tests := []struct {
		name string
		key  string
	}{
		{name: "empty"},
		{name: "N plus one", key: atLimit + "k"},
		{name: "invalid UTF-8", key: invalidUTF8},
		{name: "ASCII space", key: "key value"},
		{name: "unicode space", key: "key\u2003value"},
		{name: "newline", key: "key\nvalue"},
		{name: "NUL", key: "key\x00value"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateBraveAPIKey(test.key)
			if !errors.Is(err, ErrInvalidBraveConfig) {
				t.Fatalf("ValidateBraveAPIKey() error = %v", err)
			}
			if test.key != "" && strings.Contains(err.Error(), test.key) {
				t.Fatalf("configuration error leaked key: %v", err)
			}
		})
	}
	if provider, err := newBraveProviderWithClient(atLimit, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return braveHTTPResponse(http.StatusOK, []byte(`{"type":"search","web":{"results":[]}}`)), nil
	})}); provider == nil || err != nil {
		t.Fatalf("newBraveProviderWithClient(N) = %#v, %v", provider, err)
	}
}

func TestBraveProviderRequestMapping(t *testing.T) {
	const apiKey = "process-brave-key"
	tests := []struct {
		name       string
		freshness  Freshness
		wantWire   string
		wantParams int
	}{
		{name: "any", freshness: FreshnessAny, wantParams: 4},
		{name: "day", freshness: FreshnessDay, wantWire: "pd", wantParams: 5},
		{name: "week", freshness: FreshnessWeek, wantWire: "pw", wantParams: 5},
		{name: "month", freshness: FreshnessMonth, wantWire: "pm", wantParams: 5},
		{name: "year", freshness: FreshnessYear, wantWire: "py", wantParams: 5},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			provider := mustBraveProviderWithClient(t, apiKey, &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls.Add(1)
				if request.Method != http.MethodGet || request.URL.Scheme != "https" ||
					request.URL.Host != "api.search.brave.com" || request.URL.Path != "/res/v1/web/search" {
					t.Errorf("request target = %s %s", request.Method, request.URL)
				}
				if request.Header.Get("Accept") != "application/json" ||
					request.Header.Get("X-Subscription-Token") != apiKey ||
					request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" {
					t.Errorf("request headers = %#v", request.Header)
				}
				if request.Body != nil && request.Body != http.NoBody {
					body, readErr := io.ReadAll(request.Body)
					if readErr != nil || len(body) != 0 {
						t.Errorf("request body = %q, %v", body, readErr)
					}
				}
				parameters := request.URL.Query()
				if len(parameters) != test.wantParams || parameters.Get("q") != "Go 搜索 & proof" ||
					parameters.Get("count") != "7" || parameters.Get("result_filter") != "web" ||
					parameters.Get("text_decorations") != "false" || parameters.Get("freshness") != test.wantWire {
					t.Errorf("query parameters = %#v", parameters)
				}
				return braveHTTPResponse(http.StatusOK, []byte(`{"type":"search","web":{"results":[]}}`)), nil
			})})

			results, err := provider.Search(context.Background(), ProviderQuery{
				Text: "Go 搜索 & proof", Limit: 7, Freshness: test.freshness,
			})
			if err != nil || results == nil || len(results) != 0 || calls.Load() != 1 {
				t.Fatalf("Search() = %#v, %v; calls=%d", results, err, calls.Load())
			}
		})
	}
}

func TestBraveProviderResponseMappingAndForwardCompatibility(t *testing.T) {
	body := []byte(`{
		"type":"search",
		"future_top_level":{"added":true},
		"web":{"future_web":17,"results":[
			{"title":"one","url":"https://one.example/","description":"first","page_age":"2026-08-10T01:02:03.123456789+02:00","future":"ok"},
			{"title":"two","url":"https://two.example/","description":"second","page_age":"2026-08-09T04:05:06"},
			{"title":"three","url":"https://three.example/","description":"third","age":"2026-08-08T07:08:09Z"},
			{"title":"four","url":"https://four.example/","description":"fourth","page_age":"not-a-date","age":"2 days ago"}
		]}
	}`)
	provider := mustBraveStaticProvider(t, body)
	results, err := provider.Search(context.Background(), ProviderQuery{Text: "mapping", Limit: 4})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("results = %#v", results)
	}
	for index, result := range results {
		if result.Rank != index+1 || result.Score != nil {
			t.Errorf("result %d rank/score = %d/%v", index, result.Rank, result.Score)
		}
	}
	if results[0].Title != "one" || results[0].URL != "https://one.example/" || results[0].Snippet != "first" ||
		results[0].PublishedAt == nil || !results[0].PublishedAt.Equal(time.Date(2026, 8, 9, 23, 2, 3, 123456789, time.UTC)) {
		t.Fatalf("first result = %#v", results[0])
	}
	if results[1].PublishedAt == nil || !results[1].PublishedAt.Equal(time.Date(2026, 8, 9, 4, 5, 6, 0, time.UTC)) {
		t.Fatalf("second published_at = %v", results[1].PublishedAt)
	}
	if results[2].PublishedAt == nil || !results[2].PublishedAt.Equal(time.Date(2026, 8, 8, 7, 8, 9, 0, time.UTC)) {
		t.Fatalf("third published_at = %v", results[2].PublishedAt)
	}
	if results[3].PublishedAt != nil {
		t.Fatalf("relative/invalid age was inferred: %v", results[3].PublishedAt)
	}

	truncated, err := provider.Search(context.Background(), ProviderQuery{Text: "mapping", Limit: 2})
	if err != nil || len(truncated) != 2 || truncated[1].Title != "two" {
		t.Fatalf("truncated Search() = %#v, %v", truncated, err)
	}
}

func TestBraveProviderAcceptsEmptyWebResultShapes(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"type":"search"}`),
		[]byte(`{"type":"search","web":null}`),
		[]byte(`{"type":"search","web":{"results":null}}`),
	} {
		provider := mustBraveStaticProvider(t, body)
		results, err := provider.Search(context.Background(), ProviderQuery{Text: "none", Limit: 10})
		if err != nil || results == nil || len(results) != 0 {
			t.Fatalf("Search(%s) = %#v, %v", body, results, err)
		}
	}
}

func TestBraveProviderEnforcesProviderResultBounds(t *testing.T) {
	urlPrefix := "https://example.com/"
	tests := []struct {
		name      string
		candidate string
		wantError bool
	}{
		{
			name:      "title N",
			candidate: `{"title":"` + strings.Repeat("t", MaxProviderTitleBytes) + `","url":"https://example.com/"}`,
		},
		{
			name:      "title N plus one",
			candidate: `{"title":"` + strings.Repeat("t", MaxProviderTitleBytes+1) + `","url":"https://example.com/"}`,
			wantError: true,
		},
		{
			name:      "snippet N",
			candidate: `{"url":"https://example.com/","description":"` + strings.Repeat("s", MaxProviderSnippetBytes) + `"}`,
		},
		{
			name:      "snippet N plus one",
			candidate: `{"url":"https://example.com/","description":"` + strings.Repeat("s", MaxProviderSnippetBytes+1) + `"}`,
			wantError: true,
		},
		{
			name:      "URL N",
			candidate: `{"url":"` + urlPrefix + strings.Repeat("u", models.MaxSearchURLBytes-len(urlPrefix)) + `"}`,
		},
		{
			name:      "URL N plus one",
			candidate: `{"url":"` + urlPrefix + strings.Repeat("u", models.MaxSearchURLBytes-len(urlPrefix)+1) + `"}`,
			wantError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`{"type":"search","web":{"results":[` + test.candidate + `]}}`)
			provider := mustBraveStaticProvider(t, body)
			results, err := provider.Search(context.Background(), ProviderQuery{Text: "bounds", Limit: 1})
			if test.wantError {
				if results != nil || providerErrorKind(err) != ProviderErrorInvalidResponse {
					t.Fatalf("Search() = %#v, %#v", results, err)
				}
				return
			}
			if err != nil || len(results) != 1 {
				t.Fatalf("Search() = %#v, %v", results, err)
			}
		})
	}

	for _, count := range []int{MaxProviderResults, MaxProviderResults + 1} {
		t.Run(boundaryName(count, MaxProviderResults)+" results", func(t *testing.T) {
			body := braveResultCountEnvelope(count)
			provider := mustBraveStaticProvider(t, body)
			results, err := provider.Search(context.Background(), ProviderQuery{Text: "count", Limit: MaxProviderResults})
			if count == MaxProviderResults {
				if err != nil || len(results) != MaxProviderResults {
					t.Fatalf("Search(N results) = %d, %v", len(results), err)
				}
				return
			}
			if results != nil || providerErrorKind(err) != ProviderErrorInvalidResponse {
				t.Fatalf("Search(N+1 results) = %#v, %#v", results, err)
			}
		})
	}
}

func TestBraveProviderRejectsInvalidQueriesWithoutHTTP(t *testing.T) {
	var calls atomic.Int32
	provider := mustBraveProviderWithClient(t, "valid-key", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return braveHTTPResponse(http.StatusOK, []byte(`{"type":"search","web":{"results":[]}}`)), nil
	})})
	invalidUTF8 := string([]byte{'q', 0xff})
	tests := []struct {
		name  string
		ctx   context.Context
		query ProviderQuery
	}{
		{name: "nil context", query: ProviderQuery{Text: "query", Limit: 1}},
		{name: "empty", ctx: context.Background(), query: ProviderQuery{Limit: 1}},
		{name: "blank", ctx: context.Background(), query: ProviderQuery{Text: " \t ", Limit: 1}},
		{name: "invalid UTF-8", ctx: context.Background(), query: ProviderQuery{Text: invalidUTF8, Limit: 1}},
		{name: "control", ctx: context.Background(), query: ProviderQuery{Text: "line\nbreak", Limit: 1}},
		{name: "runes N plus one", ctx: context.Background(), query: ProviderQuery{Text: strings.Repeat("界", models.MaxSearchQueryRunes+1), Limit: 1}},
		{name: "words N plus one", ctx: context.Background(), query: ProviderQuery{Text: strings.TrimSpace(strings.Repeat("word ", models.MaxSearchQueryWords+1)), Limit: 1}},
		{name: "zero limit", ctx: context.Background(), query: ProviderQuery{Text: "query"}},
		{name: "limit N plus one", ctx: context.Background(), query: ProviderQuery{Text: "query", Limit: models.MaxSearchLimit + 1}},
		{name: "unknown freshness", ctx: context.Background(), query: ProviderQuery{Text: "query", Limit: 1, Freshness: Freshness(255)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			results, err := provider.Search(test.ctx, test.query)
			if results != nil || !errors.Is(err, ErrInvalidBraveQuery) {
				t.Fatalf("Search() = %#v, %v", results, err)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("HTTP calls = %d, want zero", calls.Load())
	}

	for _, query := range []ProviderQuery{
		{Text: strings.Repeat("界", models.MaxSearchQueryRunes), Limit: 1},
		{Text: strings.TrimSpace(strings.Repeat("word ", models.MaxSearchQueryWords)), Limit: models.MaxSearchLimit},
	} {
		if _, err := provider.Search(context.Background(), query); err != nil {
			t.Fatalf("Search(at boundary) error = %v", err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("boundary HTTP calls = %d, want 2", calls.Load())
	}
}

func TestBraveProviderReturnsTypedRedactedStatusErrors(t *testing.T) {
	const apiKey = "process-brave-secret"
	tests := []struct {
		status int
		kind   ProviderErrorKind
	}{
		{status: http.StatusUnauthorized, kind: ProviderErrorAuthentication},
		{status: http.StatusForbidden, kind: ProviderErrorAuthentication},
		{status: http.StatusTooManyRequests, kind: ProviderErrorRateLimited},
		{status: http.StatusInternalServerError, kind: ProviderErrorUpstream},
		{status: http.StatusBadGateway, kind: ProviderErrorUpstream},
		{status: http.StatusPermanentRedirect, kind: ProviderErrorUpstream},
		{status: http.StatusUnprocessableEntity, kind: ProviderErrorUpstream},
	}
	for _, test := range tests {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			provider := mustBraveProviderWithClient(t, apiKey, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return braveHTTPResponse(test.status, []byte(`{"error":"echo `+apiKey+` private detail"}`)), nil
			})})
			results, err := provider.Search(context.Background(), ProviderQuery{Text: "status", Limit: 1})
			var providerError *ProviderError
			if results != nil || !errors.As(err, &providerError) || providerError.Kind != test.kind ||
				providerError.StatusCode != test.status {
				t.Fatalf("Search() = %#v, %#v", results, err)
			}
			for _, forbidden := range []string{apiKey, "private detail", "echo"} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("public error leaked %q: %v", forbidden, err)
				}
			}
		})
	}
}

func TestBraveProviderStatusClassificationDoesNotDependOnErrorBody(t *testing.T) {
	const apiKey = "status-body-secret"
	for _, test := range []struct {
		status int
		kind   ProviderErrorKind
	}{
		{status: http.StatusUnauthorized, kind: ProviderErrorAuthentication},
		{status: http.StatusForbidden, kind: ProviderErrorAuthentication},
		{status: http.StatusTooManyRequests, kind: ProviderErrorRateLimited},
		{status: http.StatusBadGateway, kind: ProviderErrorUpstream},
	} {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			body := &unreadableStatusBody{secret: apiKey}
			provider := mustBraveProviderWithClient(t, apiKey, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: test.status, Body: body, Header: make(http.Header)}, nil
			})})
			results, err := provider.Search(context.Background(), ProviderQuery{Text: "status body", Limit: 1})
			if results != nil || providerErrorKind(err) != test.kind || body.reads != 0 || body.closes != 1 ||
				strings.Contains(err.Error(), apiKey) {
				t.Fatalf("status results/error/reads/closes = %#v/%#v/%d/%d", results, err, body.reads, body.closes)
			}
		})
	}

	t.Run("rate limited oversized body", func(t *testing.T) {
		provider := mustBraveProviderWithClient(t, apiKey, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return braveHTTPResponse(http.StatusTooManyRequests, bytes.Repeat([]byte{'x'}, braveMaximumResponseBytes+1)), nil
		})})
		results, err := provider.Search(context.Background(), ProviderQuery{Text: "oversized status body", Limit: 1})
		if results != nil || providerErrorKind(err) != ProviderErrorRateLimited {
			t.Fatalf("oversized status results/error = %#v/%#v", results, err)
		}
	})
}

func TestBraveProviderResponseEnvelopeNAndNPlusOne(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		mode := "plain"
		if compressed {
			mode = "decompressed gzip stream"
		}
		for _, size := range []int{braveMaximumResponseBytes, braveMaximumResponseBytes + 1} {
			size := size
			t.Run(mode+"/"+boundaryName(size, braveMaximumResponseBytes), func(t *testing.T) {
				raw := exactBraveEnvelope(t, size)
				provider := mustBraveProviderWithClient(t, "valid-key", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					body := io.ReadCloser(io.NopCloser(bytes.NewReader(raw)))
					if compressed {
						body = decompressedGZIPBody(t, raw)
					}
					return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header)}, nil
				})})
				results, err := provider.Search(context.Background(), ProviderQuery{Text: "boundary", Limit: 1})
				if size == braveMaximumResponseBytes {
					if err != nil || results == nil || len(results) != 0 {
						t.Fatalf("Search(N) = %#v, %v", results, err)
					}
					return
				}
				var providerError *ProviderError
				if results != nil || !errors.As(err, &providerError) || providerError.Kind != ProviderErrorInvalidResponse {
					t.Fatalf("Search(N+1) = %#v, %#v", results, err)
				}
			})
		}
	}
}

func TestBraveProviderRejectsInvalidResponses(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{name: "empty"},
		{name: "invalid UTF-8", body: []byte{'{', '"', 't', 'y', 'p', 'e', '"', ':', '"', 0xff, '"', '}'}},
		{name: "malformed JSON", body: []byte(`{"type":"search"`)},
		{name: "trailing value", body: []byte(`{"type":"search"} {}`)},
		{name: "missing type", body: []byte(`{"web":{"results":[]}}`)},
		{name: "wrong type", body: []byte(`{"type":"answer","web":{"results":[]}}`)},
		{name: "wrong web shape", body: []byte(`{"type":"search","web":"bad"}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := mustBraveStaticProvider(t, test.body)
			results, err := provider.Search(context.Background(), ProviderQuery{Text: "invalid", Limit: 1})
			var providerError *ProviderError
			if results != nil || !errors.As(err, &providerError) || providerError.Kind != ProviderErrorInvalidResponse {
				t.Fatalf("Search() = %#v, %#v", results, err)
			}
		})
	}

	provider := mustBraveProviderWithClient(t, "valid-key", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: nil, Header: make(http.Header)}, nil
	})})
	if _, err := provider.Search(context.Background(), ProviderQuery{Text: "nil body", Limit: 1}); providerErrorKind(err) != ProviderErrorInvalidResponse {
		t.Fatalf("nil body error = %#v", err)
	}
}

func TestBraveProviderTransportErrorsAndCancellationAreTypedAndRedacted(t *testing.T) {
	const apiKey = "transport-secret"
	t.Run("transport error", func(t *testing.T) {
		provider := mustBraveProviderWithClient(t, apiKey, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("transport echoed " + apiKey)
		})})
		_, err := provider.Search(context.Background(), ProviderQuery{Text: "network", Limit: 1})
		if providerErrorKind(err) != ProviderErrorUpstream || strings.Contains(err.Error(), apiKey) {
			t.Fatalf("transport error = %#v", err)
		}
	})

	t.Run("pre-canceled", func(t *testing.T) {
		var calls atomic.Int32
		provider := mustBraveProviderWithClient(t, apiKey, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return nil, errors.New("unexpected")
		})})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := provider.Search(ctx, ProviderQuery{Text: "cancel", Limit: 1})
		if providerErrorKind(err) != ProviderErrorTimeout || !errors.Is(err, context.Canceled) || calls.Load() != 0 {
			t.Fatalf("pre-canceled error/calls = %#v/%d", err, calls.Load())
		}
	})

	t.Run("mid-body cancellation", func(t *testing.T) {
		started := make(chan struct{})
		provider := mustBraveProviderWithClient(t, apiKey, &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       &blockingContextBody{ctx: request.Context(), started: started},
				Header:     make(http.Header),
			}, nil
		})})
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			_, err := provider.Search(ctx, ProviderQuery{Text: "cancel body", Limit: 1})
			result <- err
		}()
		awaitSignal(t, started)
		cancel()
		err := awaitError(t, result)
		if providerErrorKind(err) != ProviderErrorTimeout || !errors.Is(err, context.Canceled) {
			t.Fatalf("mid-body error = %#v", err)
		}
	})

	t.Run("final body read cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		body := []byte(`{"type":"search","web":{"results":[{"url":"https://example.com/"}]}}`)
		provider := mustBraveProviderWithClient(t, apiKey, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       &cancelOnFinalReadBody{body: body, cancel: cancel},
				Header:     make(http.Header),
			}, nil
		})})
		results, err := provider.Search(ctx, ProviderQuery{Text: "cancel final read", Limit: 1})
		if results != nil || providerErrorKind(err) != ProviderErrorTimeout || !errors.Is(err, context.Canceled) {
			t.Fatalf("final-read results/error = %#v/%#v", results, err)
		}
	})

	t.Run("client timeout", func(t *testing.T) {
		client := &http.Client{
			Timeout: 20 * time.Millisecond,
			Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				<-request.Context().Done()
				return nil, request.Context().Err()
			}),
		}
		provider := mustBraveProviderWithClient(t, apiKey, client)
		_, err := provider.Search(context.Background(), ProviderQuery{Text: "timeout", Limit: 1})
		if providerErrorKind(err) != ProviderErrorTimeout || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("client timeout error = %#v", err)
		}
	})
}

func TestBravePublicTransportRejectsPrivateAndMixedDNSBeforeDial(t *testing.T) {
	for _, test := range []struct {
		name      string
		addresses []netip.Addr
	}{
		{name: "private", addresses: []netip.Addr{netip.MustParseAddr("127.0.0.1")}},
		{name: "mixed", addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("10.0.0.1")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var dials atomic.Int32
			policy := publicnet.NewPolicy(publicnet.Options{
				Resolver: braveResolver{"api.search.brave.com": test.addresses},
				DialContext: func(context.Context, string, string) (net.Conn, error) {
					dials.Add(1)
					return nil, errors.New("unexpected dial")
				},
			})
			provider, err := NewBraveProvider("dns-secret", policy)
			if err != nil {
				t.Fatal(err)
			}
			_, err = provider.Search(context.Background(), ProviderQuery{Text: "dns", Limit: 1})
			if providerErrorKind(err) != ProviderErrorUpstream || !errors.Is(err, publicnet.ErrNotPublic) || dials.Load() != 0 ||
				strings.Contains(err.Error(), "dns-secret") {
				t.Fatalf("Search() error/dials = %#v/%d", err, dials.Load())
			}
		})
	}
}

func TestBravePublicTransportPinsValidatedLiteralIP(t *testing.T) {
	var dialed string
	sentinel := errors.New("stop after pinned dial")
	policy := publicnet.NewPolicy(publicnet.Options{
		Resolver: braveResolver{"api.search.brave.com": {netip.MustParseAddr("93.184.216.34")}},
		DialContext: func(_ context.Context, network, address string) (net.Conn, error) {
			if network != "tcp" {
				t.Errorf("network = %q", network)
			}
			dialed = address
			return nil, sentinel
		},
	})
	provider, err := NewBraveProvider("pin-secret", policy)
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Search(context.Background(), ProviderQuery{Text: "pin", Limit: 1})
	if providerErrorKind(err) != ProviderErrorUpstream || !errors.Is(err, sentinel) || dialed != "93.184.216.34:443" ||
		strings.Contains(dialed, "api.search.brave.com") {
		t.Fatalf("Search() error/dial = %#v/%q", err, dialed)
	}
}

func TestBravePublicHTTPClientDoesNotFollowCredentialedRedirects(t *testing.T) {
	var targetHits atomic.Int32
	var targetCredential atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		targetHits.Add(1)
		targetCredential.Store(request.Header.Get("X-Subscription-Token"))
	}))
	t.Cleanup(target.Close)
	targetURL := testHostURL(t, target.URL, "target.test")

	for _, status := range []int{
		http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			redirector := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Location", targetURL+"/secret")
				writer.WriteHeader(status)
			}))
			t.Cleanup(redirector.Close)
			redirectURL := testHostURL(t, redirector.URL, "redirect.test")
			policy := loopbackTestPolicy()
			client, err := newBraveHTTPClient(policy, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			request, err := http.NewRequest(http.MethodGet, redirectURL+"/start", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("X-Subscription-Token", "redirect-secret")
			response, err := client.Do(request)
			if err != nil {
				t.Fatalf("Do() error = %v", err)
			}
			if response.StatusCode != status {
				response.Body.Close()
				t.Fatalf("status = %d, want %d", response.StatusCode, status)
			}
			response.Body.Close()
		})
	}
	if targetHits.Load() != 0 {
		t.Fatalf("redirect target hits = %d, credential=%v", targetHits.Load(), targetCredential.Load())
	}
}

func TestBravePublicHTTPClientIgnoresEnvironmentProxy(t *testing.T) {
	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		proxyHits.Add(1)
		http.Error(writer, "unexpected proxy", http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)
	for _, variable := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(variable, proxy.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	var directHits atomic.Int32
	direct := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		directHits.Add(1)
		writer.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(direct.Close)
	client, err := newBraveHTTPClient(loopbackTestPolicy(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(testHostURL(t, direct.URL, "direct.test"))
	if err != nil {
		t.Fatalf("GET direct error = %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent || directHits.Load() != 1 || proxyHits.Load() != 0 {
		t.Fatalf("status/direct/proxy = %d/%d/%d", response.StatusCode, directHits.Load(), proxyHits.Load())
	}
}

func TestBraveProviderCancellationInterruptsPinnedDial(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	policy := publicnet.NewPolicy(publicnet.Options{
		Resolver: braveResolver{"api.search.brave.com": {netip.MustParseAddr("93.184.216.34")}},
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			once.Do(func() { close(started) })
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	provider, err := NewBraveProvider("cancel-secret", policy)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := provider.Search(ctx, ProviderQuery{Text: "cancel dial", Limit: 1})
		result <- err
	}()
	awaitSignal(t, started)
	cancel()
	err = awaitError(t, result)
	if providerErrorKind(err) != ProviderErrorTimeout || !errors.Is(err, context.Canceled) {
		t.Fatalf("Search() error = %#v", err)
	}
}

func TestBraveProviderConcurrentSearchIsIndependent(t *testing.T) {
	var calls atomic.Int32
	provider := mustBraveProviderWithClient(t, "concurrent-key", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return braveHTTPResponse(http.StatusOK, []byte(`{"type":"search","web":{"results":[{"title":"title","url":"https://example.com","description":"snippet"}]}}`)), nil
	})})

	const workers = 32
	errorsFound := make(chan error, workers)
	var wait sync.WaitGroup
	wait.Add(workers)
	for range workers {
		go func() {
			defer wait.Done()
			results, err := provider.Search(context.Background(), ProviderQuery{Text: "concurrent", Limit: 1})
			if err != nil {
				errorsFound <- err
				return
			}
			if len(results) != 1 || results[0].Title != "title" || results[0].Rank != 1 || results[0].Score != nil {
				errorsFound <- errors.New("unexpected concurrent result")
			}
		}()
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
	if calls.Load() != workers {
		t.Fatalf("calls = %d, want %d", calls.Load(), workers)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

// newBraveProviderWithClient exists only in the same-package test binary. The
// production API exposes no client or endpoint override.
func newBraveProviderWithClient(apiKey string, client *http.Client) (*BraveProvider, error) {
	if err := ValidateBraveAPIKey(apiKey); err != nil || client == nil {
		return nil, ErrInvalidBraveConfig
	}
	return &BraveProvider{apiKey: apiKey, client: client}, nil
}

type braveResolver map[string][]netip.Addr

func (resolver braveResolver) LookupNetIP(_ context.Context, _, hostname string) ([]netip.Addr, error) {
	addresses, ok := resolver[strings.ToLower(hostname)]
	if !ok {
		return nil, errors.New("missing DNS fixture")
	}
	return append([]netip.Addr(nil), addresses...), nil
}

type blockingContextBody struct {
	ctx     context.Context
	started chan struct{}
	once    sync.Once
}

func (body *blockingContextBody) Read([]byte) (int, error) {
	body.once.Do(func() { close(body.started) })
	<-body.ctx.Done()
	return 0, body.ctx.Err()
}

func (*blockingContextBody) Close() error { return nil }

type unreadableStatusBody struct {
	secret string
	reads  int
	closes int
}

func (body *unreadableStatusBody) Read([]byte) (int, error) {
	body.reads++
	return 0, errors.New("unread status body: " + body.secret)
}

func (body *unreadableStatusBody) Close() error {
	body.closes++
	return nil
}

type cancelOnFinalReadBody struct {
	body   []byte
	cancel context.CancelFunc
	read   bool
}

func (body *cancelOnFinalReadBody) Read(target []byte) (int, error) {
	if body.read {
		return 0, io.EOF
	}
	body.read = true
	written := copy(target, body.body)
	if written != len(body.body) {
		return written, errors.New("test body did not fit in one read")
	}
	body.cancel()
	return written, io.EOF
}

func (*cancelOnFinalReadBody) Close() error { return nil }

func braveHTTPResponse(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     make(http.Header),
	}
}

func mustBraveStaticProvider(t *testing.T, body []byte) *BraveProvider {
	t.Helper()
	return mustBraveProviderWithClient(t, "valid-key", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return braveHTTPResponse(http.StatusOK, body), nil
	})})
}

func mustBraveProviderWithClient(t *testing.T, key string, client *http.Client) *BraveProvider {
	t.Helper()
	provider, err := newBraveProviderWithClient(key, client)
	if err != nil {
		t.Fatalf("newBraveProviderWithClient() error = %v", err)
	}
	return provider
}

func providerErrorKind(err error) ProviderErrorKind {
	var providerError *ProviderError
	if errors.As(err, &providerError) {
		return providerError.Kind
	}
	return 0
}

func exactBraveEnvelope(t *testing.T, size int) []byte {
	t.Helper()
	prefix := []byte(`{"type":"search","web":{"results":[]},"padding":"`)
	suffix := []byte(`"}`)
	if size < len(prefix)+len(suffix) {
		t.Fatalf("envelope size %d is too small", size)
	}
	body := make([]byte, 0, size)
	body = append(body, prefix...)
	body = append(body, bytes.Repeat([]byte{'x'}, size-len(prefix)-len(suffix))...)
	body = append(body, suffix...)
	if len(body) != size {
		t.Fatalf("envelope length = %d, want %d", len(body), size)
	}
	return body
}

func braveResultCountEnvelope(count int) []byte {
	var body strings.Builder
	body.WriteString(`{"type":"search","web":{"results":[`)
	for index := 0; index < count; index++ {
		if index > 0 {
			body.WriteByte(',')
		}
		body.WriteString(`{"url":"https://example.com/"}`)
	}
	body.WriteString(`]}}`)
	return []byte(body.String())
}

func decompressedGZIPBody(t *testing.T, raw []byte) io.ReadCloser {
	t.Helper()
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(raw); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed.Bytes()))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	return reader
}

func boundaryName(size, maximum int) string {
	if size == maximum {
		return "N"
	}
	return "N-plus-one"
}

func loopbackTestPolicy() *publicnet.Policy {
	return publicnet.NewPolicy(publicnet.Options{
		AllowPrivateNetworks: true,
		Resolver: braveResolver{
			"redirect.test": {netip.MustParseAddr("127.0.0.1")},
			"target.test":   {netip.MustParseAddr("127.0.0.1")},
			"direct.test":   {netip.MustParseAddr("127.0.0.1")},
		},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, address)
		},
	})
}

func testHostURL(t *testing.T, serverURL, hostname string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(serverURL, "http://"))
	if err != nil {
		t.Fatalf("split test server URL: %v", err)
	}
	return "http://" + net.JoinHostPort(hostname, port)
}

func awaitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for test signal")
	}
}

func awaitError(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Search result")
		return nil
	}
}
