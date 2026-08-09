package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	crawldomain "github.com/use-agent/purify/crawl"
	"github.com/use-agent/purify/jobs"
	"github.com/use-agent/purify/models"
)

type crawlServiceFixture struct {
	submit func(models.CrawlRequest) (*models.CrawlResponse, error)
	get    func(string) (*models.CrawlStatusResponse, bool)
}

func (fixture crawlServiceFixture) Submit(request models.CrawlRequest) (*models.CrawlResponse, error) {
	return fixture.submit(request)
}

func (fixture crawlServiceFixture) Get(id string) (*models.CrawlStatusResponse, bool) {
	return fixture.get(id)
}

var _ CrawlService = (*crawldomain.Service)(nil)

func TestPostCrawlDelegatesCompleteRequestAndPreservesResponse(t *testing.T) {
	waitForNetworkIdle := false
	onlyMainContent := false
	wantRequest := models.CrawlRequest{
		URL:             "https://docs.example.test/start",
		MaxDepth:        4,
		MaxPages:        37,
		Scope:           "domain",
		ExcludePatterns: []string{"/private/*", "*.pdf"},
		Options: models.CrawlOptions{
			WaitForNetworkIdle: &waitForNetworkIdle,
			Timeout:            42,
			Stealth:            true,
			ProxyURL:           "https://proxy.example:8443",
			OutputFormat:       "html",
			ExtractMode:        "pruning",
			CSSSelector:        "main.content",
			Headers:            map[string]string{"X-First": "one", "X-Second": "two"},
			Cookies: []models.Cookie{
				{Name: "first", Value: "one", Domain: "example.test", Path: "/"},
				{Name: "second", Value: "two", Domain: "example.test", Path: "/docs"},
			},
			Actions: []models.Action{
				{Type: "click", Selector: "#first"},
				{Type: "execute_js", Code: "() => document.title"},
			},
			IncludeTags:     []string{"main", "article"},
			ExcludeTags:     []string{"nav", ".ad"},
			OnlyMainContent: &onlyMainContent,
			RemoveOverlays:  true,
			BlockAds:        true,
			CDPURL:          "wss://browser.example/devtools/browser/id",
			MaxAge:          12_345,
		},
		WebhookURL:    "https://hook.example/crawl",
		WebhookSecret: "secret",
	}
	wantResponse := &models.CrawlResponse{ID: "crawl-123", Status: "processing"}
	var calls int
	service := crawlServiceFixture{submit: func(request models.CrawlRequest) (*models.CrawlResponse, error) {
		calls++
		if !reflect.DeepEqual(request, wantRequest) {
			t.Fatalf("Submit() request = %#v, want %#v", request, wantRequest)
		}
		return wantResponse, nil
	}}

	body, err := json.Marshal(wantRequest)
	if err != nil {
		t.Fatal(err)
	}
	response := performCrawlPost(t, PostCrawl(service), body)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	var got models.CrawlResponse
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got != *wantResponse || calls != 1 {
		t.Fatalf("response/calls = (%#v, %d), want (%#v, 1)", got, calls, *wantResponse)
	}
}

func TestPostCrawlPassesUnsetDefaultsToDomain(t *testing.T) {
	service := crawlServiceFixture{submit: func(request models.CrawlRequest) (*models.CrawlResponse, error) {
		if request.MaxDepth != 0 || request.MaxPages != 0 || request.Scope != "" ||
			request.Options.OutputFormat != "" || request.Options.ExtractMode != "" {
			t.Fatalf("handler applied domain defaults: %#v", request)
		}
		return &models.CrawlResponse{ID: "crawl-defaults", Status: "processing"}, nil
	}}
	response := performCrawlPost(t, PostCrawl(service), []byte(`{"url":"https://example.test"}`))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
}

func TestPostCrawlBindingFailuresKeepLegacyResponse(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{name: "invalid json", body: []byte(`{`)},
		{name: "missing URL", body: []byte(`{}`)},
		{name: "invalid URL syntax", body: []byte(`{"url":"not a URL"}`)},
		{name: "max depth", body: []byte(`{"url":"https://example.test","max_depth":11}`)},
		{name: "max pages", body: []byte(`{"url":"https://example.test","max_pages":501}`)},
		{name: "scope", body: []byte(`{"url":"https://example.test","scope":"global"}`)},
		{name: "invalid option", body: []byte(`{"url":"https://example.test","options":{"output_format":"pdf"}}`)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			service := crawlServiceFixture{submit: func(models.CrawlRequest) (*models.CrawlResponse, error) {
				calls++
				return nil, errors.New("must not be called")
			}}
			response := performCrawlPost(t, PostCrawl(service), test.body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", response.Code, response.Body)
			}
			if got := strings.TrimSpace(response.Body.String()); got != `{"id":"","status":"failed"}` {
				t.Fatalf("legacy bind JSON = %s", got)
			}
			if calls != 0 {
				t.Fatalf("Submit() calls = %d, want 0", calls)
			}
		})
	}
}

func TestPostCrawlMapsDomainAndLifecycleErrors(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{name: "invalid URL", err: wrapped(crawldomain.ErrInvalidURL), wantStatus: 400, wantCode: models.ErrCodeInvalidInput, wantMessage: "crawl URL is invalid"},
		{name: "invalid max depth", err: wrapped(crawldomain.ErrInvalidMaxDepth), wantStatus: 400, wantCode: models.ErrCodeInvalidInput, wantMessage: "max_depth must be between 1 and 10"},
		{name: "invalid max pages", err: wrapped(crawldomain.ErrInvalidMaxPages), wantStatus: 400, wantCode: models.ErrCodeInvalidInput, wantMessage: "max_pages must be between 1 and 500"},
		{name: "invalid scope", err: wrapped(crawldomain.ErrInvalidScope), wantStatus: 400, wantCode: models.ErrCodeInvalidInput, wantMessage: "scope must be page, domain, or subdomain"},
		{name: "invalid exclude", err: wrapped(crawldomain.ErrInvalidExcludePattern), wantStatus: 400, wantCode: models.ErrCodeInvalidInput, wantMessage: "exclude_patterns contains an invalid pattern"},
		{name: "manager full", err: wrapped(jobs.ErrManagerFull), wantStatus: 429, wantCode: models.ErrCodeRateLimited, wantMessage: "crawl job capacity is full"},
		{name: "manager closed", err: wrapped(jobs.ErrManagerClosed), wantStatus: 503, wantCode: models.ErrCodeInternal, wantMessage: "crawl service is unavailable"},
		{name: "internal", err: errors.New("id generation failed"), wantStatus: 500, wantCode: models.ErrCodeInternal, wantMessage: "failed to create crawl job"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := crawlServiceFixture{submit: func(models.CrawlRequest) (*models.CrawlResponse, error) {
				return nil, test.err
			}}
			response := performCrawlPost(t, PostCrawl(service), validCrawlBody(t))
			assertCrawlError(t, response, test.wantStatus, test.wantCode, test.wantMessage)
		})
	}
}

func TestPostCrawlHandlesMissingServiceAndEmptyResponse(t *testing.T) {
	t.Run("missing service", func(t *testing.T) {
		response := performCrawlPost(t, PostCrawl(nil), validCrawlBody(t))
		assertCrawlError(t, response, 500, models.ErrCodeInternal, "crawl service is not configured")
	})

	t.Run("empty response", func(t *testing.T) {
		service := crawlServiceFixture{submit: func(models.CrawlRequest) (*models.CrawlResponse, error) { return nil, nil }}
		response := performCrawlPost(t, PostCrawl(service), validCrawlBody(t))
		assertCrawlError(t, response, 500, models.ErrCodeInternal, "crawl service returned an empty response")
	})
}

func TestGetCrawlDelegatesAndPreservesStatusResponse(t *testing.T) {
	want := &models.CrawlStatusResponse{
		ID:        "crawl-123",
		Status:    "partial",
		Completed: 2,
		Total:     2,
		Results: []*models.ScrapeResponse{
			{Success: true, Content: "first"},
			{Success: false, Error: &models.ErrorDetail{Code: models.ErrCodeNavigation, Message: "failed"}},
		},
	}
	var gotID string
	service := crawlServiceFixture{get: func(id string) (*models.CrawlStatusResponse, bool) {
		gotID = id
		return want, true
	}}
	response := performCrawlGet(t, GetCrawl(service), want.ID)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if gotID != want.ID {
		t.Fatalf("Get() id = %q, want %q", gotID, want.ID)
	}
	var got models.CrawlStatusResponse
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, *want) {
		t.Fatalf("response = %#v, want %#v", got, *want)
	}
}

func TestGetCrawlMapsMissingAndInvalidServiceStates(t *testing.T) {
	tests := []struct {
		name        string
		service     CrawlService
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{name: "not found", service: crawlServiceFixture{get: func(string) (*models.CrawlStatusResponse, bool) { return nil, false }}, wantStatus: 404, wantCode: models.ErrCodeInvalidInput, wantMessage: "crawl job not found"},
		{name: "missing service", wantStatus: 500, wantCode: models.ErrCodeInternal, wantMessage: "crawl service is not configured"},
		{name: "empty service response", service: crawlServiceFixture{get: func(string) (*models.CrawlStatusResponse, bool) { return nil, true }}, wantStatus: 500, wantCode: models.ErrCodeInternal, wantMessage: "crawl service returned an empty response"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := performCrawlGet(t, GetCrawl(test.service), "crawl-missing")
			assertCrawlError(t, response, test.wantStatus, test.wantCode, test.wantMessage)
		})
	}
}

func TestCrawlErrorResponsesDoNotLeakInternalErrors(t *testing.T) {
	service := crawlServiceFixture{submit: func(models.CrawlRequest) (*models.CrawlResponse, error) {
		return nil, errors.New("database password should never be exposed")
	}}
	response := performCrawlPost(t, PostCrawl(service), validCrawlBody(t))
	if strings.Contains(response.Body.String(), "database password") {
		t.Fatalf("internal error leaked in response: %s", response.Body)
	}
}

func performCrawlPost(t *testing.T, handler gin.HandlerFunc, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/crawl", handler)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/crawl", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func performCrawlGet(t *testing.T, handler gin.HandlerFunc, id string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/crawl/:id", handler)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/crawl/"+id, nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func validCrawlBody(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(models.CrawlRequest{URL: "https://example.test"})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func assertCrawlError(t *testing.T, response *httptest.ResponseRecorder, wantStatus int, wantCode, wantMessage string) {
	t.Helper()
	if response.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, wantStatus, response.Body)
	}
	var body struct {
		Error models.ErrorDetail `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != wantCode || body.Error.Message != wantMessage {
		t.Fatalf("error = %#v, want code=%q message=%q", body.Error, wantCode, wantMessage)
	}
}

func wrapped(err error) error {
	return errors.Join(errors.New("wrapped"), err)
}
