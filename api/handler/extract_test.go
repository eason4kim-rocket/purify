package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

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

func TestExtractEnforcesStrictURLSourcesXORAndSourceBudgets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &recordingMultiExtractService{
		multiResponse: &models.MultiExtractResponse{
			Success:       true,
			Status:        models.MultiExtractStatusComplete,
			Sources:       []models.MultiExtractSource{},
			UsageComplete: true,
		},
	}
	router := gin.New()
	router.POST("/extract", Extract(service))
	tooMany, err := json.Marshal(map[string]any{
		"sources": []string{
			"https://a.example.com", "https://b.example.com", "https://c.example.com",
			"https://d.example.com", "https://e.example.com", "https://f.example.com",
			"https://g.example.com", "https://h.example.com", "https://i.example.com",
		},
		"schema": map[string]any{"type": "object"},
	})
	if err != nil {
		t.Fatalf("marshal too-many fixture: %v", err)
	}
	tooLongURL := "https://a.example.com/" + strings.Repeat("x", models.MaxExtractSourceURLBytes)
	tooLong, err := json.Marshal(map[string]any{
		"sources": []string{tooLongURL},
		"schema":  map[string]any{"type": "object"},
	})
	if err != nil {
		t.Fatalf("marshal too-long fixture: %v", err)
	}
	tests := []string{
		`{"schema":{"type":"object"}}`,
		`{"url":"https://a.example.com","sources":["https://b.example.com"],"schema":{"type":"object"}}`,
		`{"sources":[],"schema":{"type":"object"}}`,
		string(tooMany),
		string(tooLong),
	}
	for _, body := range tests {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/extract", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("body prefix %.80q status = %d; response=%s", body, recorder.Code, recorder.Body)
		}
	}
	if service.calls != 0 || service.multiCalls != 0 {
		t.Fatalf("single/multi calls = %d/%d, want zero", service.calls, service.multiCalls)
	}

	exactSources := make([]string, models.MaxExtractSources)
	for index := range exactSources {
		prefix := fmt.Sprintf("https://s%d.example.com/", index)
		exactSources[index] = prefix + strings.Repeat("x", models.MaxExtractSourceURLBytes-len(prefix))
	}
	exactBody, err := json.Marshal(map[string]any{
		"sources": exactSources,
		"schema":  map[string]any{"type": "object"},
	})
	if err != nil {
		t.Fatalf("marshal exact-budget fixture: %v", err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/extract", bytes.NewReader(exactBody))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || service.multiCalls != 1 || service.multiRequest == nil || len(service.multiRequest.Sources) != models.MaxExtractSources {
		t.Fatalf("exact budget status/calls/request = %d/%d/%#v; body=%s", recorder.Code, service.multiCalls, service.multiRequest, recorder.Body)
	}
}

func TestExtractMultiDelegatesWithoutCallingLegacySingleService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	want := &models.MultiExtractResponse{
		Success: true,
		Status:  models.MultiExtractStatusComplete,
		Data:    json.RawMessage(`{"name":"Ada"}`),
		Consensus: &models.MultiExtractConsensus{Fields: map[string]models.MultiExtractFieldConsensus{
			"name": {Value: json.RawMessage(`"Ada"`), Agreement: models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1}},
		}},
		Sources: []models.MultiExtractSource{
			{URL: "https://a.example.com/", FinalURL: "https://a.example.com/", Success: true, Status: models.MultiExtractSourceStatusValid},
		},
		UsageComplete: true,
	}
	service := &recordingMultiExtractService{multiResponse: want}
	router := gin.New()
	router.POST("/extract", Extract(service))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/extract", strings.NewReader(
		`{"sources":["https://a.example.com"],"schema":{"type":"object"},"engine":"compiled"}`,
	))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || service.multiCalls != 1 || service.calls != 0 || service.multiRequest == nil ||
		!reflect.DeepEqual(service.multiRequest.Sources, []string{"https://a.example.com"}) {
		t.Fatalf("status/single/multi/request = %d/%d/%d/%#v; body=%s", recorder.Code, service.calls, service.multiCalls, service.multiRequest, recorder.Body)
	}
	var response models.MultiExtractResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !reflect.DeepEqual(response, *want) {
		t.Fatalf("response = %#v, want %#v", response, *want)
	}
}

func TestExtractMultiMapsCapabilityNoValidAndTimeoutErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	requestBody := `{"sources":["https://a.example.com"],"schema":{"type":"object"},"engine":"compiled"}`
	tests := []struct {
		name        string
		service     ExtractService
		wantStatus  int
		wantCode    string
		wantSources int
	}{
		{
			name:        "service lacks multi capability",
			service:     &recordingExtractService{},
			wantStatus:  http.StatusServiceUnavailable,
			wantCode:    models.ErrCodeMultiSourceUnavailable,
			wantSources: 0,
		},
		{
			name: "all timeout",
			service: &recordingMultiExtractService{
				multiResponse: aggregateFailureResponse(models.MultiExtractSourceStatusTimeout),
				multiErr:      models.NewScrapeError(models.ErrCodeTimeout, "private timeout detail", nil),
			},
			wantStatus:  http.StatusGatewayTimeout,
			wantCode:    models.ErrCodeTimeout,
			wantSources: 1,
		},
		{
			name: "non-timeout no valid source",
			service: &recordingMultiExtractService{
				multiResponse: aggregateFailureResponse(models.MultiExtractSourceStatusFetchFailed),
				multiErr:      models.NewScrapeError(models.ErrCodeNoValidSource, "private upstream credential", nil),
			},
			wantStatus:  http.StatusBadGateway,
			wantCode:    models.ErrCodeNoValidSource,
			wantSources: 1,
		},
		{
			name: "snapshot capability unavailable",
			service: &recordingMultiExtractService{
				multiResponse: aggregateFailureResponse(models.MultiExtractSourceStatusEvidenceUnavailable),
				multiErr:      models.NewScrapeError(models.ErrCodeMultiSourceUnavailable, "private signer path", nil),
			},
			wantStatus:  http.StatusServiceUnavailable,
			wantCode:    models.ErrCodeMultiSourceUnavailable,
			wantSources: 1,
		},
		{
			name: "LLM authentication failure",
			service: &recordingMultiExtractService{
				multiResponse: aggregateFailureResponse(models.MultiExtractSourceStatusExtractionFailed),
				multiErr:      models.NewScrapeError(models.ErrCodeLLMAuthFailure, "private provider credential", nil),
			},
			wantStatus:  http.StatusUnauthorized,
			wantCode:    models.ErrCodeLLMAuthFailure,
			wantSources: 1,
		},
		{
			name: "LLM rate limit",
			service: &recordingMultiExtractService{
				multiResponse: aggregateFailureResponse(models.MultiExtractSourceStatusExtractionFailed),
				multiErr:      models.NewScrapeError(models.ErrCodeLLMRateLimited, "private provider response", nil),
			},
			wantStatus:  http.StatusTooManyRequests,
			wantCode:    models.ErrCodeLLMRateLimited,
			wantSources: 1,
		},
		{
			name: "compiled extractor unavailable",
			service: &recordingMultiExtractService{
				multiResponse: aggregateFailureResponse(models.MultiExtractSourceStatusExtractionFailed),
				multiErr:      models.NewScrapeError(models.ErrCodeExtractorUnavailable, "private repository path", nil),
			},
			wantStatus:  http.StatusConflict,
			wantCode:    models.ErrCodeExtractorUnavailable,
			wantSources: 1,
		},
		{
			name: "internal failure",
			service: &recordingMultiExtractService{
				multiResponse: aggregateFailureResponse(models.MultiExtractSourceStatusExtractionFailed),
				multiErr:      models.NewScrapeError(models.ErrCodeInternal, "private filesystem path", nil),
			},
			wantStatus:  http.StatusInternalServerError,
			wantCode:    models.ErrCodeInternal,
			wantSources: 1,
		},
		{
			name: "deadline after valid source",
			service: &recordingMultiExtractService{
				multiResponse: &models.MultiExtractResponse{
					Sources: []models.MultiExtractSource{
						{URL: "https://a.example.com/", Success: true, Status: models.MultiExtractSourceStatusValid},
					},
					Tokens: models.TokenInfo{OriginalEstimate: 10, CleanedEstimate: 5, SavingsPercent: 50},
				},
				multiErr: models.NewScrapeError(models.ErrCodeTimeout, "private merge detail", nil),
			},
			wantStatus:  http.StatusGatewayTimeout,
			wantCode:    models.ErrCodeTimeout,
			wantSources: 1,
		},
		{
			name: "no valid without response",
			service: &recordingMultiExtractService{
				multiErr: models.NewScrapeError(models.ErrCodeNoValidSource, "private detail", nil),
			},
			wantStatus:  http.StatusBadGateway,
			wantCode:    models.ErrCodeNoValidSource,
			wantSources: 0,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router := gin.New()
			router.POST("/extract", Extract(test.service))
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/extract", strings.NewReader(requestBody))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, test.wantStatus, recorder.Body)
			}
			var response models.MultiExtractResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if response.Success || response.Error == nil || response.Error.Code != test.wantCode || len(response.Sources) != test.wantSources {
				t.Fatalf("response = %#v", response)
			}
			for _, private := range []string{"private", "credential", "signer path", "merge detail"} {
				if strings.Contains(recorder.Body.String(), private) {
					t.Fatalf("response leaked %q: %s", private, recorder.Body)
				}
			}
		})
	}
}

func TestExtractMultiResponseBudgetFailsClosedBeforeHandlerMarshal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	response := &models.MultiExtractResponse{
		Success:       true,
		Sources:       []models.MultiExtractSource{},
		UsageComplete: true,
		Error: &models.ErrorDetail{
			Code:    models.ErrCodeInternal,
			Message: strings.Repeat("x", models.MaxMultiExtractResponseBytes),
		},
	}
	if preflightMultiExtractResponseSize(response) {
		t.Fatal("oversized response passed preflight")
	}
	service := &recordingMultiExtractService{multiResponse: response}
	router := gin.New()
	router.POST("/extract", Extract(service))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/extract", strings.NewReader(
		`{"sources":["https://a.example.com"],"schema":{"type":"object"},"engine":"compiled"}`,
	))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusInternalServerError || strings.Contains(recorder.Body.String(), strings.Repeat("x", 64)) {
		t.Fatalf("status/body = %d/%s", recorder.Code, recorder.Body)
	}
}

func TestFallbackMultiResponseEncoderExactWholeResponseBudget(t *testing.T) {
	service := &recordingMultiExtractService{}

	t.Run("exact limit succeeds", func(t *testing.T) {
		response := exactSizedHandlerMultiResponse(t, models.MaxMultiExtractResponseBytes)
		encoded, err := encodeMultiExtractResponse(context.Background(), service, response)
		if err != nil || len(encoded) != models.MaxMultiExtractResponseBytes {
			t.Fatalf("encodeMultiExtractResponse() bytes/error = %d/%v", len(encoded), err)
		}
	})

	t.Run("one byte over fails without partial HTTP output", func(t *testing.T) {
		response := exactSizedHandlerMultiResponse(t, models.MaxMultiExtractResponseBytes+1)
		encoded, err := encodeMultiExtractResponse(context.Background(), service, response)
		if encoded != nil || err == nil || err.Error() != "multi-source response exceeds its output budget" {
			t.Fatalf("encodeMultiExtractResponse() bytes/error = %d/%v", len(encoded), err)
		}

		gin.SetMode(gin.TestMode)
		service := &recordingMultiExtractService{multiResponse: response}
		router := gin.New()
		router.POST("/extract", Extract(service))
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/extract", strings.NewReader(
			`{"sources":["https://a.example.com"],"schema":{"type":"object"},"engine":"compiled"}`,
		))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusInternalServerError || recorder.Body.Len() >= models.MaxMultiExtractResponseBytes ||
			strings.Contains(recorder.Body.String(), strings.Repeat("x", 64)) {
			t.Fatalf("status/body bytes = %d/%d; body=%s", recorder.Code, recorder.Body.Len(), recorder.Body)
		}
		var got models.MultiExtractResponse
		if decodeErr := json.Unmarshal(recorder.Body.Bytes(), &got); decodeErr != nil || got.Success ||
			got.Error == nil || got.Error.Code != models.ErrCodeInternal || len(got.Sources) != 0 {
			t.Fatalf("HTTP response/decode error = %#v/%v", got, decodeErr)
		}
	})
}

func TestExtractMultiUsesServiceEncoderAndFallbackEncodingSlots(t *testing.T) {
	t.Run("service encoder", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		response := &models.MultiExtractResponse{Success: true, Sources: []models.MultiExtractSource{}, UsageComplete: true}
		service := &recordingEncodedMultiExtractService{
			recordingMultiExtractService: &recordingMultiExtractService{multiResponse: response},
			encoded:                      []byte(`{"encoded_by_service":true}`),
		}
		router := gin.New()
		router.POST("/extract", Extract(service))
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/extract", strings.NewReader(
			`{"sources":["https://a.example.com"],"schema":{"type":"object"},"engine":"compiled"}`,
		))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK || recorder.Body.String() != string(service.encoded) ||
			service.encodeCalls != 1 || service.encodedResponse != response {
			t.Fatalf("status/body/encode calls/response = %d/%s/%d/%p", recorder.Code, recorder.Body, service.encodeCalls, service.encodedResponse)
		}
	})

	t.Run("fallback is globally capped and cancelable", func(t *testing.T) {
		for range cap(fallbackMultiExtractEncodingSlots) {
			fallbackMultiExtractEncodingSlots <- struct{}{}
		}
		defer func() {
			for range cap(fallbackMultiExtractEncodingSlots) {
				<-fallbackMultiExtractEncodingSlots
			}
		}()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := encodeMultiExtractResponse(ctx, &recordingMultiExtractService{}, &models.MultiExtractResponse{})
		var scrapeError *models.ScrapeError
		if !errors.As(err, &scrapeError) || scrapeError.Code != models.ErrCodeTimeout {
			t.Fatalf("encodeMultiExtractResponse() error = %v", err)
		}
	})
}

func TestExtractMultiOuterDeadlinePreservesSummariesWhenEncodingTimesOut(t *testing.T) {
	gin.SetMode(gin.TestMode)
	usage := &models.LLMUsage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5}
	response := &models.MultiExtractResponse{
		Success: true,
		Status:  models.MultiExtractStatusComplete,
		Data:    json.RawMessage(`{"name":"Ada"}`),
		Consensus: &models.MultiExtractConsensus{Fields: map[string]models.MultiExtractFieldConsensus{
			"name": {Value: json.RawMessage(`"Ada"`)},
		}},
		Sources: []models.MultiExtractSource{
			{
				URL:        "https://a.example.com/",
				FinalURL:   "https://a.example.com/",
				Success:    true,
				Status:     models.MultiExtractSourceStatusValid,
				SnapshotID: "sha256:test",
			},
		},
		Violations:    []models.SchemaViolation{{Path: "$.private", Message: "private schema detail"}},
		Tokens:        models.TokenInfo{OriginalEstimate: 10, CleanedEstimate: 5, SavingsPercent: 50},
		Timing:        models.ExtractTimingInfo{TotalMs: 11, NavigationMs: 4, CleaningMs: 2, ExtractionMs: 5},
		LLMUsage:      usage,
		UsageComplete: true,
	}
	service := &recordingEncodedMultiExtractService{
		recordingMultiExtractService: &recordingMultiExtractService{multiResponse: response},
		encode: func(ctx context.Context, _ *models.MultiExtractResponse) ([]byte, error) {
			<-ctx.Done()
			return nil, models.NewScrapeError(models.ErrCodeTimeout, "private encoder timeout", ctx.Err())
		},
	}
	router := gin.New()
	router.POST("/extract", Extract(service))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/extract", strings.NewReader(
		`{"sources":["https://a.example.com"],"schema":{"type":"object"},"engine":"compiled","timeout":1}`,
	))
	request.Header.Set("Content-Type", "application/json")
	startedAt := time.Now()
	router.ServeHTTP(recorder, request)
	if elapsed := time.Since(startedAt); elapsed < 750*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("outer deadline elapsed = %v", elapsed)
	}
	if recorder.Code != http.StatusGatewayTimeout || service.encodeCalls != 1 {
		t.Fatalf("status/encode calls = %d/%d; body=%s", recorder.Code, service.encodeCalls, recorder.Body)
	}
	var got models.MultiExtractResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Success || got.Status != "" || len(got.Data) != 0 || got.Consensus != nil || len(got.Violations) != 0 ||
		got.Error == nil || got.Error.Code != models.ErrCodeTimeout || len(got.Sources) != 1 ||
		got.Sources[0].Status != models.MultiExtractSourceStatusValid || !reflect.DeepEqual(got.Tokens, response.Tokens) ||
		!reflect.DeepEqual(got.Timing, response.Timing) || !reflect.DeepEqual(got.LLMUsage, response.LLMUsage) ||
		got.UsageComplete != response.UsageComplete || strings.Contains(recorder.Body.String(), "private") {
		t.Fatalf("timeout response = %#v", got)
	}
}

func aggregateFailureResponse(status models.MultiExtractSourceStatus) *models.MultiExtractResponse {
	return &models.MultiExtractResponse{
		Sources: []models.MultiExtractSource{
			{
				URL:    "https://a.example.com/",
				Status: status,
				Error:  &models.ErrorDetail{Code: models.ErrCodeNavigation, Message: "stable source failure"},
			},
		},
		UsageComplete: false,
	}
}

func exactSizedHandlerMultiResponse(t *testing.T, size int) *models.MultiExtractResponse {
	t.Helper()
	response := &models.MultiExtractResponse{
		Success:       false,
		Sources:       []models.MultiExtractSource{},
		UsageComplete: true,
		Error: &models.ErrorDetail{
			Code: models.ErrCodeInternal,
		},
	}
	baseline, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal multi response baseline: %v", err)
	}
	fillerBytes := size - len(baseline)
	if fillerBytes < 0 {
		t.Fatalf("response size %d is below baseline %d", size, len(baseline))
	}
	response.Error.Message = strings.Repeat("x", fillerBytes)
	return response
}

type recordingExtractService struct {
	response *models.ExtractResponse
	err      error
	calls    int
	request  *models.ExtractRequest
}

type recordingMultiExtractService struct {
	recordingExtractService
	multiResponse *models.MultiExtractResponse
	multiErr      error
	multiCalls    int
	multiRequest  *models.ExtractRequest
}

type recordingEncodedMultiExtractService struct {
	*recordingMultiExtractService
	encoded         []byte
	encodeErr       error
	encode          func(context.Context, *models.MultiExtractResponse) ([]byte, error)
	encodeCalls     int
	encodedResponse *models.MultiExtractResponse
}

func (service *recordingEncodedMultiExtractService) EncodeMultiResponse(
	ctx context.Context,
	response *models.MultiExtractResponse,
) ([]byte, error) {
	service.encodeCalls++
	service.encodedResponse = response
	if service.encode != nil {
		return service.encode(ctx, response)
	}
	return append([]byte(nil), service.encoded...), service.encodeErr
}

func (service *recordingMultiExtractService) ExtractMulti(_ context.Context, request *models.ExtractRequest) (*models.MultiExtractResponse, error) {
	service.multiCalls++
	if request != nil {
		copy := *request
		copy.Schema = append(json.RawMessage(nil), request.Schema...)
		copy.Sources = append([]string(nil), request.Sources...)
		service.multiRequest = &copy
	}
	return service.multiResponse, service.multiErr
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
