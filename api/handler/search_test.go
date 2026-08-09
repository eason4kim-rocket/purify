package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/use-agent/purify/models"
)

type recordingSearchService struct {
	calls    int
	request  *models.SearchRequest
	response *models.SearchResponse
	err      error
	search   func(context.Context, *models.SearchRequest) (*models.SearchResponse, error)
}

func (service *recordingSearchService) Search(ctx context.Context, request *models.SearchRequest) (*models.SearchResponse, error) {
	service.calls++
	service.request = request
	if service.search != nil {
		return service.search(ctx, request)
	}
	return service.response, service.err
}

type encodingSearchService struct {
	recordingSearchService
	encode func(context.Context, *models.SearchResponse) ([]byte, error)
}

func (service *encodingSearchService) EncodeSearchResponse(ctx context.Context, response *models.SearchResponse) ([]byte, error) {
	if service.encode == nil {
		return nil, errors.New("encoder is not configured")
	}
	return service.encode(ctx, response)
}

type recordingSearchLimiter struct {
	allowed bool
	calls   int
	costs   []int
}

func (limiter *recordingSearchLimiter) Allow(_ *gin.Context, cost int) bool {
	limiter.calls++
	limiter.costs = append(limiter.costs, cost)
	return limiter.allowed
}

func newSearchTestRouter(handler gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/search", handler)
	return router
}

func performSearchRequest(router http.Handler, body []byte) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/search", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	return recorder
}

func decodeSearchTestResponse(t *testing.T, recorder *httptest.ResponseRecorder) models.SearchResponse {
	t.Helper()
	var response models.SearchResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, recorder.Body)
	}
	return response
}

func TestSearchReturnsStableSuccessAndNormalizesNilResults(t *testing.T) {
	service := &recordingSearchService{response: &models.SearchResponse{
		Success: true,
		Query:   "purify search",
	}}
	router := newSearchTestRouter(Search(service))
	recorder := performSearchRequest(router, []byte(`{"query":"purify search"}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body)
	}
	response := decodeSearchTestResponse(t, recorder)
	if !response.Success || response.Query != "purify search" || response.Results == nil || len(response.Results) != 0 {
		t.Fatalf("response = %#v", response)
	}
	if service.calls != 1 || service.request == nil || service.request.Query != "purify search" {
		t.Fatalf("service calls/request = %d %#v", service.calls, service.request)
	}
	if service.response.Results != nil {
		t.Fatal("handler mutated service-owned response")
	}
}

func TestSearchStrictDecoderRejectsUntrustedJSONWithoutLeakingIt(t *testing.T) {
	service := &recordingSearchService{response: &models.SearchResponse{Success: true, Results: []models.SearchResult{}}}
	router := newSearchTestRouter(Search(service))
	const secret = "must-not-appear-in-search-response"
	tests := []struct {
		name string
		body []byte
	}{
		{name: "unknown field", body: []byte(`{"query":"q","unknown":"` + secret + `"}`)},
		{name: "trailing value", body: []byte(`{"query":"q"} {"secret":"` + secret + `"}`)},
		{name: "null", body: []byte(`null`)},
		{name: "array", body: []byte(`[]`)},
		{name: "string", body: []byte(`"` + secret + `"`)},
		{name: "number", body: []byte(`3`)},
		{name: "malformed", body: []byte(`{"query":"` + secret)},
		{name: "empty", body: nil},
		{name: "invalid UTF-8", body: append([]byte(`{"query":"q`), []byte{0xff, '"', '}'}...)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := performSearchRequest(router, test.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body)
			}
			response := decodeSearchTestResponse(t, recorder)
			if response.Success || response.Results == nil || response.Error == nil ||
				response.Error.Code != models.ErrCodeInvalidInput || response.Error.Message != "invalid search request" {
				t.Fatalf("response = %#v", response)
			}
			if strings.Contains(recorder.Body.String(), secret) || strings.Contains(recorder.Body.String(), "unknown") {
				t.Fatalf("response leaked request detail: %s", recorder.Body)
			}
		})
	}
	if service.calls != 0 {
		t.Fatalf("service calls = %d, want zero", service.calls)
	}
}

func TestSearchRequestBodyLimitExactBoundary(t *testing.T) {
	service := &recordingSearchService{response: &models.SearchResponse{Success: true, Results: []models.SearchResult{}}}
	router := newSearchTestRouter(Search(service))
	base := `{"query":"q"}`
	if len(base) >= models.MaxSearchRequestBytes {
		t.Fatalf("base fixture is unexpectedly large: %d", len(base))
	}
	atLimit := base + strings.Repeat(" ", models.MaxSearchRequestBytes-len(base))
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{name: "N", body: atLimit, wantStatus: http.StatusOK},
		{name: "N plus one", body: atLimit + " ", wantStatus: http.StatusRequestEntityTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := performSearchRequest(router, []byte(test.body))
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, test.wantStatus, recorder.Body)
			}
			if test.wantStatus == http.StatusRequestEntityTooLarge {
				response := decodeSearchTestResponse(t, recorder)
				if response.Results == nil || response.Error == nil || response.Error.Code != models.ErrCodeInvalidInput ||
					response.Error.Message != "search request is too large" {
					t.Fatalf("response = %#v", response)
				}
			}
		})
	}
	if service.calls != 1 {
		t.Fatalf("service calls = %d, want one", service.calls)
	}
}

func TestSearchMapsAndSanitizesStableErrors(t *testing.T) {
	const privateDetail = "credential=secret at /private/provider/path"
	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{name: "invalid", err: models.NewScrapeError(models.ErrCodeInvalidInput, privateDetail, nil), wantStatus: 400, wantCode: models.ErrCodeInvalidInput, wantMessage: "invalid search request"},
		{name: "limited", err: models.NewScrapeError(models.ErrCodeRateLimited, privateDetail, nil), wantStatus: 429, wantCode: models.ErrCodeRateLimited, wantMessage: "search rate limited"},
		{name: "failed", err: models.NewScrapeError(models.ErrCodeSearchFailed, privateDetail, nil), wantStatus: 502, wantCode: models.ErrCodeSearchFailed, wantMessage: "search failed"},
		{name: "unavailable", err: models.NewScrapeError(models.ErrCodeSearchUnavailable, privateDetail, nil), wantStatus: 503, wantCode: models.ErrCodeSearchUnavailable, wantMessage: "search is unavailable"},
		{name: "timeout", err: models.NewScrapeError(models.ErrCodeTimeout, privateDetail, nil), wantStatus: 504, wantCode: models.ErrCodeTimeout, wantMessage: "search timed out"},
		{name: "internal", err: models.NewScrapeError(models.ErrCodeInternal, privateDetail, nil), wantStatus: 500, wantCode: models.ErrCodeInternal, wantMessage: "internal search failure"},
		{name: "wrapped context", err: models.NewScrapeError(models.ErrCodeSearchFailed, privateDetail, context.DeadlineExceeded), wantStatus: 504, wantCode: models.ErrCodeTimeout, wantMessage: "search timed out"},
		{name: "unknown code", err: models.NewScrapeError("PRIVATE_PROVIDER_CODE", privateDetail, nil), wantStatus: 500, wantCode: models.ErrCodeInternal, wantMessage: "internal search failure"},
		{name: "plain", err: errors.New(privateDetail), wantStatus: 500, wantCode: models.ErrCodeInternal, wantMessage: "internal search failure"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &recordingSearchService{err: test.err}
			recorder := performSearchRequest(newSearchTestRouter(Search(service)), []byte(`{"query":"q"}`))
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, test.wantStatus, recorder.Body)
			}
			response := decodeSearchTestResponse(t, recorder)
			if response.Results == nil || response.Error == nil || response.Error.Code != test.wantCode || response.Error.Message != test.wantMessage {
				t.Fatalf("response = %#v", response)
			}
			if strings.Contains(recorder.Body.String(), "credential") || strings.Contains(recorder.Body.String(), "/private") ||
				strings.Contains(recorder.Body.String(), "PRIVATE_PROVIDER_CODE") {
				t.Fatalf("response leaked internal detail: %s", recorder.Body)
			}
		})
	}
}

func TestSearchFailsClosedForNilTypedNilAndEmptyService(t *testing.T) {
	var typedNil *recordingSearchService
	tests := []struct {
		name        string
		service     SearchService
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{name: "nil", service: nil, wantStatus: 503, wantCode: models.ErrCodeSearchUnavailable, wantMessage: "search is unavailable"},
		{name: "typed nil", service: typedNil, wantStatus: 503, wantCode: models.ErrCodeSearchUnavailable, wantMessage: "search is unavailable"},
		{name: "empty response", service: &recordingSearchService{}, wantStatus: 500, wantCode: models.ErrCodeInternal, wantMessage: "internal search failure"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := performSearchRequest(newSearchTestRouter(Search(test.service)), []byte(`{"query":"q"}`))
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body)
			}
			response := decodeSearchTestResponse(t, recorder)
			if response.Results == nil || response.Error == nil || response.Error.Code != test.wantCode || response.Error.Message != test.wantMessage {
				t.Fatalf("response = %#v", response)
			}
		})
	}
}

func TestSearchRejectsUnsuccessfulOrErrorBearingServiceEnvelopes(t *testing.T) {
	const privateDetail = "provider credential and private response"
	tests := []struct {
		name     string
		response *models.SearchResponse
	}{
		{name: "unsuccessful", response: &models.SearchResponse{Success: false, Results: []models.SearchResult{}}},
		{name: "success with error", response: &models.SearchResponse{
			Success: true,
			Results: []models.SearchResult{},
			Error:   &models.ErrorDetail{Code: "PRIVATE_CODE", Message: privateDetail},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &recordingSearchService{response: test.response}
			recorder := performSearchRequest(newSearchTestRouter(Search(service)), []byte(`{"query":"q"}`))
			if recorder.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body)
			}
			response := decodeSearchTestResponse(t, recorder)
			if response.Results == nil || response.Error == nil || response.Error.Code != models.ErrCodeInternal ||
				response.Error.Message != "internal search failure" {
				t.Fatalf("response = %#v", response)
			}
			if strings.Contains(recorder.Body.String(), privateDetail) || strings.Contains(recorder.Body.String(), "PRIVATE_CODE") {
				t.Fatalf("response leaked service envelope: %s", recorder.Body)
			}
		})
	}
}

func TestSearchWeightedLimiterChargesOnceAndStopsBeforeService(t *testing.T) {
	service := &recordingSearchService{response: &models.SearchResponse{Success: true, Results: []models.SearchResult{}}}
	limiter := &recordingSearchLimiter{allowed: false}
	router := newSearchTestRouter(SearchWithRateLimiter(service, limiter))
	recorder := performSearchRequest(router, []byte(`{"query":"q","limit":20,"include_content":true,"verify":true,"schema":{"type":"object"}}`))
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body)
	}
	response := decodeSearchTestResponse(t, recorder)
	if response.Results == nil || response.Error == nil || response.Error.Code != models.ErrCodeRateLimited || response.Error.Message != "search rate limited" {
		t.Fatalf("response = %#v", response)
	}
	if limiter.calls != 1 || len(limiter.costs) != 1 || limiter.costs[0] != 22 {
		t.Fatalf("limiter calls/costs = %d %#v", limiter.calls, limiter.costs)
	}
	if service.calls != 0 {
		t.Fatalf("service calls = %d, want zero", service.calls)
	}
}

func TestSearchRequestCostFormulaAndBounds(t *testing.T) {
	if MaxSearchRequestCost != 22 {
		t.Fatalf("maximum Search cost = %d, want 22", MaxSearchRequestCost)
	}
	tests := []struct {
		name    string
		request *models.SearchRequest
		want    int
	}{
		{name: "nil", request: nil, want: 1},
		{name: "defaults", request: &models.SearchRequest{}, want: 1},
		{name: "limit eleven", request: &models.SearchRequest{Limit: 11}, want: 2},
		{name: "one all heavy", request: &models.SearchRequest{Limit: 1, IncludeContent: true, Verify: true, Schema: json.RawMessage(`{}`)}, want: 5},
		{name: "maximum", request: &models.SearchRequest{Limit: 20, IncludeContent: true, Verify: true, Schema: json.RawMessage(`{}`)}, want: 22},
		{name: "direct oversized clamps", request: &models.SearchRequest{Limit: 200, IncludeContent: true, Verify: true, Schema: json.RawMessage(`null`)}, want: 22},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := searchRequestCost(test.request); got != test.want {
				t.Fatalf("cost = %d, want %d", got, test.want)
			}
		})
	}
}

func TestSearchCustomEncoderMustReturnValidBoundedJSON(t *testing.T) {
	tests := []struct {
		name   string
		encode func(context.Context, *models.SearchResponse) ([]byte, error)
	}{
		{name: "invalid JSON", encode: func(context.Context, *models.SearchResponse) ([]byte, error) {
			return []byte(`{"success":`), nil
		}},
		{name: "empty", encode: func(context.Context, *models.SearchResponse) ([]byte, error) {
			return nil, nil
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &encodingSearchService{
				recordingSearchService: recordingSearchService{response: &models.SearchResponse{Success: true}},
				encode:                 test.encode,
			}
			recorder := performSearchRequest(newSearchTestRouter(Search(service)), []byte(`{"query":"q"}`))
			if recorder.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body)
			}
			response := decodeSearchTestResponse(t, recorder)
			if response.Results == nil || response.Error == nil || response.Error.Code != models.ErrCodeInternal ||
				response.Error.Message != "internal search failure" {
				t.Fatalf("response = %#v", response)
			}
		})
	}
}

func TestSearchCustomEncoderReceivesNormalizedEnvelope(t *testing.T) {
	service := &encodingSearchService{
		recordingSearchService: recordingSearchService{response: &models.SearchResponse{Success: true, Query: "q"}},
		encode: func(_ context.Context, response *models.SearchResponse) ([]byte, error) {
			if response.Results == nil {
				t.Fatal("custom encoder received nullable results")
			}
			return json.Marshal(response)
		},
	}
	recorder := performSearchRequest(newSearchTestRouter(Search(service)), []byte(`{"query":"q"}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body)
	}
	response := decodeSearchTestResponse(t, recorder)
	if !response.Success || response.Results == nil {
		t.Fatalf("response = %#v", response)
	}
}

func TestSearchOuterDeadlineCoversResponseEncoder(t *testing.T) {
	service := &encodingSearchService{
		recordingSearchService: recordingSearchService{response: &models.SearchResponse{Success: true}},
		encode: func(ctx context.Context, response *models.SearchResponse) ([]byte, error) {
			if response.Results == nil {
				t.Fatal("encoder received nullable results")
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	router := newSearchTestRouter(Search(service))
	requestContext, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/search", strings.NewReader(`{"query":"q","timeout":120}`)).WithContext(requestContext)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	startedAt := time.Now()
	router.ServeHTTP(recorder, request)
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("handler ignored outer context for %s", elapsed)
	}
	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body)
	}
	response := decodeSearchTestResponse(t, recorder)
	if response.Results == nil || response.Error == nil || response.Error.Code != models.ErrCodeTimeout || response.Error.Message != "search timed out" {
		t.Fatalf("response = %#v", response)
	}
}

func TestSearchFallbackEncodingHasFourSlotsAndExactHardBoundary(t *testing.T) {
	if got := cap(fallbackSearchEncodingSlots); got != 4 {
		t.Fatalf("fallback slot capacity = %d, want 4", got)
	}

	probe := &models.SearchResponse{Success: true, Query: "a", Results: []models.SearchResult{}}
	probeBytes, err := json.Marshal(probe)
	if err != nil {
		t.Fatal(err)
	}
	overhead := len(probeBytes) - 1
	payload := models.MaxSearchResponseBytes - overhead
	if payload < 1 {
		t.Fatalf("invalid fixture payload = %d", payload)
	}
	response := &models.SearchResponse{
		Success: true,
		Query:   strings.Repeat("a", payload),
		Results: []models.SearchResult{},
	}
	encoded, err := encodeSearchResponseFallback(context.Background(), response)
	if err != nil {
		t.Fatalf("exact-limit encode: %v", err)
	}
	if len(encoded) != models.MaxSearchResponseBytes {
		t.Fatalf("exact-limit bytes = %d, want %d", len(encoded), models.MaxSearchResponseBytes)
	}
	encoded = nil
	response.Query += "a"
	if _, err := encodeSearchResponseFallback(context.Background(), response); err == nil {
		t.Fatal("N+1 response was accepted")
	}
}

func TestSearchFallbackSlotWaitHonorsContext(t *testing.T) {
	for index := 0; index < cap(fallbackSearchEncodingSlots); index++ {
		fallbackSearchEncodingSlots <- struct{}{}
	}
	defer func() {
		for index := 0; index < cap(fallbackSearchEncodingSlots); index++ {
			<-fallbackSearchEncodingSlots
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := encodeSearchResponseFallback(ctx, &models.SearchResponse{Results: []models.SearchResult{}})
	status, code, message := mapSearchError(err)
	if status != http.StatusGatewayTimeout || code != models.ErrCodeTimeout || message != "search timed out" {
		t.Fatalf("slot cancellation = %d %q %q, err=%v", status, code, message, err)
	}
}

func TestSearchResponsePreflightReportsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ok, err := preflightSearchResponseSize(ctx, &models.SearchResponse{Success: true, Results: []models.SearchResult{}})
	if ok || !errors.Is(err, context.Canceled) {
		t.Fatalf("preflight = %t, %v; want canceled", ok, err)
	}
}
