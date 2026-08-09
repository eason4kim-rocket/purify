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
	batchdomain "github.com/use-agent/purify/batch"
	"github.com/use-agent/purify/jobs"
	"github.com/use-agent/purify/models"
)

type batchServiceFixture struct {
	submit func(models.BatchRequest) (*models.BatchResponse, error)
	get    func(string) (*models.BatchStatusResponse, bool)
}

func (fixture batchServiceFixture) Submit(request models.BatchRequest) (*models.BatchResponse, error) {
	return fixture.submit(request)
}

func (fixture batchServiceFixture) Get(id string) (*models.BatchStatusResponse, bool) {
	return fixture.get(id)
}

var _ BatchService = (*batchdomain.Service)(nil)

func TestPostBatchDelegatesToServiceAndPreservesResponse(t *testing.T) {
	waitForNetworkIdle := false
	onlyMainContent := false
	wantRequest := models.BatchRequest{
		URLs: []string{"https://one.example/page", "https://two.example/page"},
		Options: models.BatchOptions{
			OutputFormat:       "html",
			ExtractMode:        "pruning",
			WaitForNetworkIdle: &waitForNetworkIdle,
			Timeout:            42,
			Stealth:            true,
			ProxyURL:           "https://proxy.example:8443",
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
		WebhookURL:    "https://hook.example/completed",
		WebhookSecret: "secret",
	}
	wantResponse := &models.BatchResponse{ID: "batch-123", Status: "processing", Total: 2}
	var calls int
	service := batchServiceFixture{
		submit: func(request models.BatchRequest) (*models.BatchResponse, error) {
			calls++
			if !reflect.DeepEqual(request, wantRequest) {
				t.Fatalf("Submit() request = %#v, want %#v", request, wantRequest)
			}
			return wantResponse, nil
		},
	}

	body, err := json.Marshal(wantRequest)
	if err != nil {
		t.Fatal(err)
	}
	response := performBatchPost(t, PostBatch(service), body)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	var got models.BatchResponse
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got != *wantResponse {
		t.Fatalf("response = %#v, want %#v", got, *wantResponse)
	}
	if calls != 1 {
		t.Fatalf("Submit() calls = %d, want 1", calls)
	}
}

func TestPostBatchBindingFailuresKeepLegacyResponse(t *testing.T) {
	tooManyURLs := make([]string, 101)
	for index := range tooManyURLs {
		tooManyURLs[index] = "https://example.test"
	}
	tooManyBody, err := json.Marshal(models.BatchRequest{URLs: tooManyURLs})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		body []byte
	}{
		{name: "invalid json", body: []byte(`{`)},
		{name: "empty urls", body: []byte(`{"urls":[]}`)},
		{name: "too many urls", body: tooManyBody},
		{name: "invalid option", body: []byte(`{"urls":["https://example.test"],"options":{"output_format":"pdf"}}`)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			service := batchServiceFixture{submit: func(models.BatchRequest) (*models.BatchResponse, error) {
				calls++
				return nil, errors.New("must not be called")
			}}
			response := performBatchPost(t, PostBatch(service), test.body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", response.Code, response.Body)
			}
			if got := strings.TrimSpace(response.Body.String()); got != `{"id":"","status":"failed","total":0}` {
				t.Fatalf("legacy bind JSON = %s", got)
			}
			var got models.BatchResponse
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Status != "failed" || got.ID != "" || got.Total != 0 {
				t.Fatalf("legacy bind response = %#v", got)
			}
			if calls != 0 {
				t.Fatalf("Submit() calls = %d, want 0", calls)
			}
		})
	}
}

func TestPostBatchMapsServiceErrors(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{
			name:        "invalid size",
			err:         errors.Join(errors.New("submit rejected"), batchdomain.ErrInvalidBatchSize),
			wantStatus:  http.StatusBadRequest,
			wantCode:    models.ErrCodeInvalidInput,
			wantMessage: "batch must contain between 1 and 100 URLs",
		},
		{
			name:        "manager full",
			err:         fmtWrapped(jobs.ErrManagerFull),
			wantStatus:  http.StatusTooManyRequests,
			wantCode:    models.ErrCodeRateLimited,
			wantMessage: "batch job capacity is full",
		},
		{
			name:        "manager closed",
			err:         fmtWrapped(jobs.ErrManagerClosed),
			wantStatus:  http.StatusServiceUnavailable,
			wantCode:    models.ErrCodeInternal,
			wantMessage: "batch service is unavailable",
		},
		{
			name:        "internal",
			err:         errors.New("id generator failed"),
			wantStatus:  http.StatusInternalServerError,
			wantCode:    models.ErrCodeInternal,
			wantMessage: "failed to create batch job",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := batchServiceFixture{submit: func(models.BatchRequest) (*models.BatchResponse, error) {
				return nil, test.err
			}}
			response := performBatchPost(t, PostBatch(service), validBatchBody(t))
			assertBatchError(t, response, test.wantStatus, test.wantCode, test.wantMessage)
		})
	}
}

func TestPostBatchHandlesMissingServiceAndEmptyServiceResponse(t *testing.T) {
	t.Run("missing service", func(t *testing.T) {
		response := performBatchPost(t, PostBatch(nil), validBatchBody(t))
		assertBatchError(t, response, http.StatusInternalServerError, models.ErrCodeInternal, "batch service is not configured")
	})

	t.Run("empty response", func(t *testing.T) {
		service := batchServiceFixture{submit: func(models.BatchRequest) (*models.BatchResponse, error) {
			return nil, nil
		}}
		response := performBatchPost(t, PostBatch(service), validBatchBody(t))
		assertBatchError(t, response, http.StatusInternalServerError, models.ErrCodeInternal, "batch service returned an empty response")
	})
}

func TestGetBatchDelegatesAndPreservesStatusResponse(t *testing.T) {
	want := &models.BatchStatusResponse{
		ID:        "batch-123",
		Status:    "partial",
		Completed: 2,
		Total:     2,
		Results: []*models.ScrapeResponse{
			{Success: true, Content: "first"},
			{Success: false, Error: &models.ErrorDetail{Code: models.ErrCodeNavigation, Message: "failed"}},
		},
	}
	var gotID string
	service := batchServiceFixture{get: func(id string) (*models.BatchStatusResponse, bool) {
		gotID = id
		return want, true
	}}
	response := performBatchGet(t, GetBatch(service), want.ID)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if gotID != want.ID {
		t.Fatalf("Get() id = %q, want %q", gotID, want.ID)
	}
	var got models.BatchStatusResponse
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, *want) {
		t.Fatalf("response = %#v, want %#v", got, *want)
	}
}

func TestGetBatchMapsMissingAndInvalidServiceStates(t *testing.T) {
	tests := []struct {
		name        string
		service     BatchService
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{
			name:        "not found",
			service:     batchServiceFixture{get: func(string) (*models.BatchStatusResponse, bool) { return nil, false }},
			wantStatus:  http.StatusNotFound,
			wantCode:    models.ErrCodeInvalidInput,
			wantMessage: "batch job not found",
		},
		{
			name:        "missing service",
			wantStatus:  http.StatusInternalServerError,
			wantCode:    models.ErrCodeInternal,
			wantMessage: "batch service is not configured",
		},
		{
			name:        "empty service response",
			service:     batchServiceFixture{get: func(string) (*models.BatchStatusResponse, bool) { return nil, true }},
			wantStatus:  http.StatusInternalServerError,
			wantCode:    models.ErrCodeInternal,
			wantMessage: "batch service returned an empty response",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := performBatchGet(t, GetBatch(test.service), "batch-missing")
			assertBatchError(t, response, test.wantStatus, test.wantCode, test.wantMessage)
		})
	}
}

func performBatchPost(t *testing.T, handler gin.HandlerFunc, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/batch/scrape", handler)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/batch/scrape", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func performBatchGet(t *testing.T, handler gin.HandlerFunc, id string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/batch/:id", handler)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/batch/"+id, nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func validBatchBody(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(models.BatchRequest{URLs: []string{"https://example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func assertBatchError(t *testing.T, response *httptest.ResponseRecorder, wantStatus int, wantCode, wantMessage string) {
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

func fmtWrapped(err error) error {
	return errors.Join(errors.New("wrapped"), err)
}

func TestBatchErrorResponsesDoNotLeakInternalErrors(t *testing.T) {
	service := batchServiceFixture{submit: func(models.BatchRequest) (*models.BatchResponse, error) {
		return nil, errors.New("database password should never be exposed")
	}}
	response := performBatchPost(t, PostBatch(service), validBatchBody(t))
	if strings.Contains(response.Body.String(), "database password") {
		t.Fatalf("internal error leaked in response: %s", response.Body)
	}
}
