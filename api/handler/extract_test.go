package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/use-agent/purify/models"
)

func TestExtractDelegatesToServiceAndPreservesResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	wantResponse := &models.ExtractResponse{
		Success:    true,
		Data:       json.RawMessage(`{"count":3}`),
		SnapshotID: "sha256:abc",
		Timing:     models.ExtractTimingInfo{TotalMs: 12, NavigationMs: 5, CleaningMs: 3, ExtractionMs: 4},
	}
	service := &recordingExtractService{response: wantResponse}
	router := gin.New()
	router.POST("/extract", Extract(service))
	body := `{
		"url":"https://example.test/page",
		"schema":{"type":"object"},
		"llm_api_key":"secret",
		"extract_mode":"pruning",
		"evidence":true
	}`
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/extract", bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body)
	}
	if service.calls != 1 || service.request == nil || service.request.URL != "https://example.test/page" || service.request.ExtractMode != "pruning" || !service.request.Evidence {
		t.Fatalf("service call = %d, request = %#v", service.calls, service.request)
	}
	var got models.ExtractResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !reflect.DeepEqual(got, *wantResponse) {
		t.Fatalf("response = %#v, want %#v", got, *wantResponse)
	}
}

func TestExtractRejectsBindingErrorsWithoutCallingService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &recordingExtractService{}
	router := gin.New()
	router.POST("/extract", Extract(service))

	for _, body := range []string{
		`{`,
		`{"url":"not-a-url","schema":{},"llm_api_key":"secret"}`,
		`{"url":"https://example.test","schema":{},"engine":"invalid"}`,
		`{"url":"https://example.test","schema":{},"llm_api_key":"secret","timeout":121}`,
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/extract", bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("body %s status = %d, response = %s", body, recorder.Code, recorder.Body)
		}
	}
	if service.calls != 0 {
		t.Fatalf("service calls = %d, want 0", service.calls)
	}
}

func TestExtractStrictDecoderRejectsUntrustedJSONWithoutLeakingIt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &recordingExtractService{}
	router := gin.New()
	router.POST("/extract", Extract(service))

	const secret = "must-not-appear-in-response"
	tests := []struct {
		name string
		body []byte
	}{
		{name: "unknown field", body: []byte(`{"url":"https://example.test","schema":{},"unknown":"` + secret + `"}`)},
		{name: "trailing value", body: []byte(`{"url":"https://example.test","schema":{}} {"secret":"` + secret + `"}`)},
		{name: "null", body: []byte(`null`)},
		{name: "array", body: []byte(`[]`)},
		{name: "string", body: []byte(`"` + secret + `"`)},
		{name: "number", body: []byte(`3`)},
		{name: "malformed", body: []byte(`{"url":"` + secret)},
		{name: "invalid UTF-8", body: append([]byte(`{"url":"https://example.test/`), []byte{0xff, '"', ',', '"', 's', 'c', 'h', 'e', 'm', 'a', '"', ':', '{', '}', '}'}...)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/extract", bytes.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body)
			}
			var response models.ExtractResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if response.Error == nil || response.Error.Code != models.ErrCodeInvalidInput ||
				response.Error.Message != "invalid extract request" || strings.Contains(recorder.Body.String(), secret) ||
				strings.Contains(recorder.Body.String(), "unknown") {
				t.Fatalf("unsanitized response = %s", recorder.Body)
			}
		})
	}
	if service.calls != 0 {
		t.Fatalf("service calls = %d, want zero", service.calls)
	}
}

func TestExtractRequestBodyLimitExactBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &recordingExtractService{response: &models.ExtractResponse{Success: true}}
	router := gin.New()
	router.POST("/extract", Extract(service))

	base := `{"url":"https://example.test","schema":{},"engine":"compiled"}`
	if len(base) >= maximumExtractRequestBytes {
		t.Fatalf("base fixture is unexpectedly large: %d", len(base))
	}
	atLimit := base + strings.Repeat(" ", maximumExtractRequestBytes-len(base))
	for _, test := range []struct {
		name       string
		body       string
		wantStatus int
	}{
		{name: "N", body: atLimit, wantStatus: http.StatusOK},
		{name: "N plus one", body: atLimit + " ", wantStatus: http.StatusRequestEntityTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/extract", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, test.wantStatus, recorder.Body)
			}
			if test.wantStatus == http.StatusRequestEntityTooLarge {
				var response models.ExtractResponse
				if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if response.Error == nil || response.Error.Code != models.ErrCodeInvalidInput || response.Error.Message != "extract request is too large" {
					t.Fatalf("response = %#v", response)
				}
			}
		})
	}
	if service.calls != 1 {
		t.Fatalf("service calls = %d, want one exact-limit call", service.calls)
	}
}

func TestExtractAllowsMissingLLMKeyForEngineDispatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &recordingExtractService{response: &models.ExtractResponse{Success: true}}
	router := gin.New()
	router.POST("/extract", Extract(service))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/extract", bytes.NewBufferString(
		`{"url":"https://example.test","schema":{"type":"object"},"engine":"compiled"}`,
	))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || service.calls != 1 || service.request == nil || service.request.LLMAPIKey != "" || service.request.Engine != "compiled" {
		t.Fatalf("status/service = %d, %d, %#v; body=%s", recorder.Code, service.calls, service.request, recorder.Body)
	}
}

func TestExtractMapsWrappedDomainErrorsAndTiming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	timing := models.ExtractTimingInfo{TotalMs: 44, NavigationMs: 20, CleaningMs: 10, ExtractionMs: 14}
	service := &recordingExtractService{err: &timedExtractError{
		cause:  models.NewScrapeError(models.ErrCodeLLMRateLimited, "slow down", nil),
		timing: timing,
	}}
	router := gin.New()
	router.POST("/extract", Extract(service))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/extract", bytes.NewBufferString(
		`{"url":"https://example.test","schema":{},"llm_api_key":"secret"}`,
	))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body)
	}
	var response models.ExtractResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Success || response.Error == nil || response.Error.Code != models.ErrCodeLLMRateLimited || response.Timing != timing {
		t.Fatalf("response = %#v", response)
	}
}

func TestExtractSanitizesUnknownServiceErrorsAndPreservesAttachedTiming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const privateCause = "provider secret at /private/provider/path"
	tests := []struct {
		name       string
		err        error
		wantTiming models.ExtractTimingInfo
	}{
		{name: "direct", err: errors.New(privateCause)},
		{
			name: "wrapped with timing",
			err: &timedExtractError{
				cause:  errors.New(privateCause),
				timing: models.ExtractTimingInfo{TotalMs: 17, NavigationMs: 5, CleaningMs: 4, ExtractionMs: 8},
			},
			wantTiming: models.ExtractTimingInfo{TotalMs: 17, NavigationMs: 5, CleaningMs: 4, ExtractionMs: 8},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &recordingExtractService{err: test.err}
			router := gin.New()
			router.POST("/extract", Extract(service))
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/extract", bytes.NewBufferString(
				`{"url":"https://example.test","schema":{},"engine":"compiled"}`,
			))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body)
			}
			var response models.ExtractResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if response.Error == nil || response.Error.Code != models.ErrCodeInternal ||
				response.Error.Message != "internal extraction failure" || response.Timing != test.wantTiming {
				t.Fatalf("response = %#v", response)
			}
			if strings.Contains(recorder.Body.String(), "provider secret") || strings.Contains(recorder.Body.String(), "/private/provider/path") {
				t.Fatalf("response leaked private cause: %s", recorder.Body)
			}
		})
	}
}

func TestExtractMapsCompiledAvailabilityAndInternalFailures(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name       string
		code       string
		message    string
		wantStatus int
	}{
		{name: "semantic unavailable", code: models.ErrCodeExtractorUnavailable, message: "no active compiled extractor matches this page", wantStatus: http.StatusConflict},
		{name: "compiled subsystem failure", code: models.ErrCodeInternal, message: "compiled extractor subsystem failed", wantStatus: http.StatusInternalServerError},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &recordingExtractService{err: models.NewScrapeError(test.code, test.message, errors.New("private repository detail"))}
			router := gin.New()
			router.POST("/extract", Extract(service))
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/extract", bytes.NewBufferString(
				`{"url":"https://example.test","schema":{},"engine":"compiled"}`,
			))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(recorder, request)

			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, test.wantStatus, recorder.Body)
			}
			var response models.ExtractResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if response.Error == nil || response.Error.Code != test.code || response.Error.Message != test.message || bytes.Contains(recorder.Body.Bytes(), []byte("private repository detail")) {
				t.Fatalf("response leaked or changed error = %#v; body=%s", response.Error, recorder.Body)
			}
		})
	}
}

func TestExtractFailsClosedForUnavailableOrEmptyService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name    string
		handler gin.HandlerFunc
	}{
		{name: "nil service", handler: Extract(nil)},
		{name: "nil response", handler: Extract(&recordingExtractService{})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router := gin.New()
			router.POST("/extract", test.handler)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/extract", bytes.NewBufferString(
				`{"url":"https://example.test","schema":{},"llm_api_key":"secret"}`,
			))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body)
			}
		})
	}
}

func TestMapExtractErrorToStatus(t *testing.T) {
	tests := map[string]int{
		models.ErrCodeTimeout:              http.StatusGatewayTimeout,
		models.ErrCodeNavigation:           http.StatusBadGateway,
		models.ErrCodeInvalidInput:         http.StatusBadRequest,
		models.ErrCodeRateLimited:          http.StatusTooManyRequests,
		models.ErrCodeLLMRateLimited:       http.StatusTooManyRequests,
		models.ErrCodeUnauthorized:         http.StatusUnauthorized,
		models.ErrCodeLLMAuthFailure:       http.StatusUnauthorized,
		models.ErrCodeLLMFailure:           http.StatusBadGateway,
		models.ErrCodeEvidenceUnavailable:  http.StatusServiceUnavailable,
		models.ErrCodeExtractorUnavailable: http.StatusConflict,
		models.ErrCodeInternal:             http.StatusInternalServerError,
	}
	for code, want := range tests {
		if got := mapExtractErrorToStatus(models.NewScrapeError(code, "test", nil)); got != want {
			t.Fatalf("code %q status = %d, want %d", code, got, want)
		}
	}
	if got := mapExtractErrorToStatus(nil); got != http.StatusInternalServerError {
		t.Fatalf("nil error status = %d", got)
	}
}

type recordingExtractService struct {
	response *models.ExtractResponse
	err      error
	calls    int
	request  *models.ExtractRequest
}

func (service *recordingExtractService) Extract(_ context.Context, request *models.ExtractRequest) (*models.ExtractResponse, error) {
	service.calls++
	if request != nil {
		copy := *request
		copy.Schema = append(json.RawMessage(nil), request.Schema...)
		service.request = &copy
	}
	return service.response, service.err
}

type timedExtractError struct {
	cause  error
	timing models.ExtractTimingInfo
}

func (e *timedExtractError) Error() string { return e.cause.Error() }
func (e *timedExtractError) Unwrap() error { return e.cause }
func (e *timedExtractError) ExtractTiming() models.ExtractTimingInfo {
	return e.timing
}

var _ error = (*timedExtractError)(nil)
var _ interface {
	ExtractTiming() models.ExtractTimingInfo
} = (*timedExtractError)(nil)
