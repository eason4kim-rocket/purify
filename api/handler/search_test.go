package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/verify/eav"
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
		{name: "duplicate root field", body: []byte(`{"query":"q","query":"` + secret + `"}`)},
		{name: "unicode duplicate root field", body: []byte(`{"query":"q","\u0071uery":"` + secret + `"}`)},
		{name: "case smuggled root field", body: []byte(`{"query":"q","Ranking":"provider"}`)},
		{name: "null ranking", body: []byte(`{"query":"q","ranking":null}`)},
		{name: "non-string ranking", body: []byte(`{"query":"q","ranking":1}`)},
		{name: "explicit zero limit", body: []byte(`{"query":"q","limit":0}`)},
		{name: "null limit", body: []byte(`{"query":"q","limit":null}`)},
		{name: "explicit zero timeout", body: []byte(`{"query":"q","timeout":0}`)},
		{name: "null timeout", body: []byte(`{"query":"q","timeout":null}`)},
		{name: "null query", body: []byte(`{"query":null}`)},
		{name: "empty ranking", body: []byte(`{"query":"q","ranking":""}`)},
		{name: "unknown ranking", body: []byte(`{"query":"q","ranking":"future"}`)},
		{name: "trust missing expected subject", body: []byte(`{"query":"q","ranking":"trust"}`)},
		{name: "trust limit above five", body: []byte(`{"query":"q","ranking":"trust","expected_subject":{"name":"q"},"limit":6}`)},
		{name: "provider forbids expected subject", body: []byte(`{"query":"q","ranking":"provider","expected_subject":{"name":"q"}}`)},
		{name: "relevance forbids expected subject", body: []byte(`{"query":"q","ranking":"relevance","expected_subject":{"name":"q"}}`)},
		{name: "duplicate schema field", body: []byte(`{"query":"q","schema":{"type":"object","type":"` + secret + `"}}`)},
		{name: "unicode duplicate schema field", body: []byte(`{"query":"q","schema":{"type":"object","\u0074ype":"` + secret + `"}}`)},
		{name: "null expected subject", body: []byte(`{"query":"q","ranking":"trust","expected_subject":null}`)},
		{name: "array expected subject", body: []byte(`{"query":"q","ranking":"trust","expected_subject":[]}`)},
		{name: "null expected subject name", body: []byte(`{"query":"q","ranking":"trust","expected_subject":{"name":null}}`)},
		{name: "null expected subject hint", body: []byte(`{"query":"q","ranking":"trust","expected_subject":{"name":"q","hint":null}}`)},
		{name: "unknown expected subject field", body: []byte(`{"query":"q","ranking":"trust","expected_subject":{"name":"q","unknown":"` + secret + `"}}`)},
		{name: "case smuggled expected subject field", body: []byte(`{"query":"q","ranking":"trust","expected_subject":{"Name":"q"}}`)},
		{name: "duplicate expected subject field", body: []byte(`{"query":"q","ranking":"trust","expected_subject":{"name":"q","name":"` + secret + `"}}`)},
		{name: "unicode duplicate expected subject field", body: []byte(`{"query":"q","ranking":"trust","expected_subject":{"name":"q","\u006eame":"` + secret + `"}}`)},
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

func TestSearchRejectsInvalidExpectedSubjectBeforeWeightedCharge(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "name N plus one", body: `{"query":"q","ranking":"trust","expected_subject":{"name":"` + strings.Repeat("s", eav.MaxSubjectBytes+1) + `"}}`},
		{name: "hint N plus one", body: `{"query":"q","ranking":"trust","expected_subject":{"name":"subject","hint":"` + strings.Repeat("h", eav.MaxHintBytes+1) + `"}}`},
		{name: "name control", body: `{"query":"q","ranking":"trust","expected_subject":{"name":"bad\u0000subject"}}`},
		{name: "hint control", body: `{"query":"q","ranking":"trust","expected_subject":{"name":"subject","hint":"bad\u0000hint"}}`},
		{name: "trim empty", body: `{"query":"q","ranking":"trust","expected_subject":{"name":" \t "}}`},
		{name: "normalization empty", body: `{"query":"q","ranking":"trust","expected_subject":{"name":"...\"''"}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &recordingSearchService{response: handlerProviderSearchResponse()}
			limiter := &recordingSearchLimiter{allowed: true}
			recorder := performSearchRequest(newSearchTestRouter(SearchWithRateLimiter(service, limiter)), []byte(test.body))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body)
			}
			if service.calls != 0 || limiter.calls != 1 || len(limiter.costs) != 1 || limiter.costs[0] != MinSearchRequestCost {
				t.Fatalf("service/limiter/costs = %d/%d/%v", service.calls, limiter.calls, limiter.costs)
			}
		})
	}
}

func TestSearchExpectedSubjectByteBoundariesReachCapabilityGate(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "name N", body: `{"query":"q","ranking":"trust","expected_subject":{"name":"` + strings.Repeat("s", eav.MaxSubjectBytes) + `"}}`},
		{name: "hint N", body: `{"query":"q","ranking":"trust","expected_subject":{"name":"subject","hint":"` + strings.Repeat("h", eav.MaxHintBytes) + `"}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &recordingSearchService{err: models.NewScrapeError(models.ErrCodeSearchUnavailable, "private", nil)}
			limiter := &recordingSearchLimiter{allowed: true}
			recorder := performSearchRequest(newSearchTestRouter(SearchWithRateLimiter(service, limiter)), []byte(test.body))
			if recorder.Code != http.StatusServiceUnavailable || service.calls != 1 || limiter.calls != 1 ||
				len(limiter.costs) != 1 || limiter.costs[0] != 44 {
				t.Fatalf("status/service/limiter/costs = %d/%d/%d/%v; body=%s", recorder.Code, service.calls, limiter.calls, limiter.costs, recorder.Body)
			}
		})
	}
}

func TestSearchUsesModeAwareEffectiveTimeoutWithoutMutatingRawRequest(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantTimeout time.Duration
		wantStatus  int
	}{
		{name: "omitted provider", body: `{"query":"q"}`, wantTimeout: 30 * time.Second, wantStatus: http.StatusOK},
		{name: "explicit provider", body: `{"query":"q","ranking":"provider"}`, wantTimeout: 30 * time.Second, wantStatus: http.StatusOK},
		{name: "relevance", body: `{"query":"q","ranking":"relevance"}`, wantTimeout: 30 * time.Second, wantStatus: http.StatusOK},
		{name: "trust", body: `{"query":"q","ranking":"trust","expected_subject":{"name":"q"}}`, wantTimeout: 60 * time.Second, wantStatus: http.StatusServiceUnavailable},
		{name: "explicit", body: `{"query":"q","ranking":"trust","expected_subject":{"name":"q"},"timeout":41}`, wantTimeout: 41 * time.Second, wantStatus: http.StatusServiceUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &recordingSearchService{}
			service.search = func(ctx context.Context, request *models.SearchRequest) (*models.SearchResponse, error) {
				deadline, ok := ctx.Deadline()
				if !ok {
					t.Fatal("Search context is missing a deadline")
				}
				remaining := time.Until(deadline)
				if remaining < test.wantTimeout-time.Second || remaining > test.wantTimeout+time.Second {
					t.Fatalf("effective timeout = %s, want %s", remaining, test.wantTimeout)
				}
				if !strings.Contains(test.body, `"timeout"`) && request.Timeout != 0 {
					t.Fatalf("handler materialized omitted timeout into request: %#v", request)
				}
				resolved := models.ResolveSearchDefaults(request.Ranking, request.Limit, request.Timeout)
				if resolved.Ranking == models.SearchRankingTrust {
					return nil, models.NewScrapeError(models.ErrCodeSearchUnavailable, "search is unavailable", nil)
				}
				if resolved.Ranking == models.SearchRankingRelevance {
					zero := int64(0)
					return &models.SearchResponse{Success: true, Query: request.Query, Results: []models.SearchResult{},
						Timing:  models.SearchTimingInfo{RerankMs: &zero},
						Ranking: &models.SearchResponseRanking{Mode: models.SearchRankingRelevance, Status: models.SearchRankingApplied}}, nil
				}
				return &models.SearchResponse{Success: true, Query: request.Query, Results: []models.SearchResult{}}, nil
			}
			recorder := performSearchRequest(newSearchTestRouter(Search(service)), []byte(test.body))
			if recorder.Code != test.wantStatus || service.calls != 1 {
				t.Fatalf("status/calls = %d/%d; body=%s", recorder.Code, service.calls, recorder.Body)
			}
		})
	}
}

func TestSearchInvalidModeAwareInputPaysOnlyMinimumCharge(t *testing.T) {
	service := &recordingSearchService{response: &models.SearchResponse{Success: true, Results: []models.SearchResult{}}}
	limiter := &recordingSearchLimiter{allowed: true}
	router := newSearchTestRouter(SearchWithRateLimiter(service, limiter))
	recorder := performSearchRequest(router, []byte(
		`{"query":"q","ranking":"trust","expected_subject":{"name":"q"},"limit":6}`,
	))
	if recorder.Code != http.StatusBadRequest || service.calls != 0 {
		t.Fatalf("status/service calls = %d/%d; body=%s", recorder.Code, service.calls, recorder.Body)
	}
	if limiter.calls != 1 || len(limiter.costs) != 1 || limiter.costs[0] != MinSearchRequestCost {
		t.Fatalf("limiter calls/costs = %d/%v", limiter.calls, limiter.costs)
	}
}

func TestSearchRequestBodyLimitExactBoundary(t *testing.T) {
	service := &recordingSearchService{response: &models.SearchResponse{Success: true, Query: "q", Results: []models.SearchResult{}}}
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

func TestSearchFailsClosedWhenResponseRankingDoesNotMatchRequest(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		response *models.SearchResponse
	}{
		{
			name:     "relevance rejects provider shape",
			body:     `{"query":"q","ranking":"relevance"}`,
			response: handlerProviderSearchResponse(),
		},
		{
			name: "provider rejects mismatched query",
			body: `{"query":"q"}`,
			response: func() *models.SearchResponse {
				response := handlerProviderSearchResponse()
				response.Query = "different"
				return response
			}(),
		},
		{
			name:     "trust rejects provider shape",
			body:     `{"query":"q","ranking":"trust","expected_subject":{"name":"q"}}`,
			response: handlerProviderSearchResponse(),
		},
		{
			name:     "omitted provider rejects relevance shape",
			body:     `{"query":"q"}`,
			response: handlerRelevanceSearchResponse(models.SearchRankingApplied),
		},
		{
			name: "explicit provider rejects ranking timing",
			body: `{"query":"q","ranking":"provider"}`,
			response: func() *models.SearchResponse {
				response := handlerProviderSearchResponse()
				zero := int64(0)
				response.Timing.RerankMs = &zero
				return response
			}(),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &recordingSearchService{response: test.response}
			recorder := performSearchRequest(newSearchTestRouter(Search(service)), []byte(test.body))
			assertInternalSearchResponse(t, recorder)
		})
	}
}

func TestSearchRejectsImpossibleHeavyCapabilityStageFlow(t *testing.T) {
	verifyUnavailable := func(code, message string) *models.SearchResponse {
		response := handlerProviderSearchResponse()
		response.Results = response.Results[:1]
		response.Results[0].FinalURL = "https://first.example/final"
		verified := false
		response.Results[0].Verified = &verified
		response.Results[0].VerificationStatus = models.SearchVerificationUnavailable
		response.Results[0].Errors = []models.SearchResultError{{
			Stage: models.SearchResultStageVerify, Code: code, Message: message,
		}}
		response.Partial = true
		return response
	}
	request := &models.SearchRequest{
		Query: "q", Limit: models.MaxSearchHeavyResults, Verify: true, IncludeContent: true, Schema: json.RawMessage(`{}`),
	}
	tests := []struct {
		name     string
		request  *models.SearchRequest
		response *models.SearchResponse
		wantErr  bool
	}{
		{
			name:    "first retained result silently omits default-limit extraction",
			request: &models.SearchRequest{Query: "q", Schema: json.RawMessage(`{}`)},
			response: func() *models.SearchResponse {
				response := handlerProviderSearchResponse()
				response.Results = response.Results[:1]
				return response
			}(),
			wantErr: true,
		},
		{
			name:    "final URL alone",
			request: &models.SearchRequest{Query: "q", Schema: json.RawMessage(`{}`)},
			response: func() *models.SearchResponse {
				response := handlerProviderSearchResponse()
				response.Results = response.Results[:1]
				response.Results[0].FinalURL = "https://first.example/final"
				return response
			}(),
			wantErr: true,
		},
		{
			name: "nonterminal verification error omits later work", request: request,
			response: verifyUnavailable(models.ErrCodeEvidenceUnavailable, "result verification is unavailable"), wantErr: true,
		},
		{
			name: "verification timeout terminates later work", request: request,
			response: verifyUnavailable(models.ErrCodeTimeout, "result verification timed out"),
		},
		{
			name: "verification timeout cannot publish later content", request: request,
			response: func() *models.SearchResponse {
				response := verifyUnavailable(models.ErrCodeTimeout, "result verification timed out")
				response.Results[0].Content = "impossible late content"
				return response
			}(),
			wantErr: true,
		},
		{
			name: "fetch error cannot publish extraction", request: request,
			response: func() *models.SearchResponse {
				response := handlerProviderSearchResponse()
				response.Results = response.Results[:1]
				response.Results[0].FinalURL = "https://first.example/final"
				response.Results[0].Data = json.RawMessage(`null`)
				response.Results[0].Errors = []models.SearchResultError{{
					Stage: models.SearchResultStageFetch, Code: models.ErrCodeNavigation, Message: "result fetch failed",
				}}
				response.Partial = true
				return response
			}(),
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateSearchResponseForRequest(test.request, test.response)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateSearchResponseForRequest() error = %v, want error %t", err, test.wantErr)
			}
		})
	}
}

func TestSearchValidatesRelevanceResponsePresenceAndOrder(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		response  func() *models.SearchResponse
		wantValid bool
	}{
		{name: "applied", response: func() *models.SearchResponse {
			return handlerRelevanceSearchResponse(models.SearchRankingApplied)
		}, wantValid: true},
		{name: "empty applied", response: func() *models.SearchResponse {
			response := handlerRelevanceSearchResponse(models.SearchRankingApplied)
			response.Results = []models.SearchResult{}
			response.Ranking.CandidateCount = 0
			*response.Timing.RerankMs = 0
			return response
		}, wantValid: true},
		{name: "empty applied requires zero timing", response: func() *models.SearchResponse {
			response := handlerRelevanceSearchResponse(models.SearchRankingApplied)
			response.Results = []models.SearchResult{}
			response.Ranking.CandidateCount = 0
			return response
		}},
		{name: "degraded requires a candidate", response: func() *models.SearchResponse {
			response := handlerRelevanceSearchResponse(models.SearchRankingDegraded)
			response.Results = []models.SearchResult{}
			response.Ranking.CandidateCount = 0
			return response
		}},
		{name: "degraded", response: func() *models.SearchResponse {
			return handlerRelevanceSearchResponse(models.SearchRankingDegraded)
		}, wantValid: true},
		{name: "partial matches returned result errors", response: func() *models.SearchResponse {
			response := handlerRelevanceSearchResponse(models.SearchRankingApplied)
			response.Results[0].FinalURL = "https://first.example/final"
			response.Results[0].Errors = []models.SearchResultError{{
				Stage: models.SearchResultStageFetch, Code: models.ErrCodeInternal, Message: "fetch unavailable",
			}}
			response.Results[1].FinalURL = "https://second.example/final"
			response.Results[1].Content = "content"
			response.Partial = true
			return response
		}, body: `{"query":"q","ranking":"relevance","include_content":true}`, wantValid: true},
		{name: "partial true requires returned result error", response: func() *models.SearchResponse {
			response := handlerRelevanceSearchResponse(models.SearchRankingApplied)
			response.Partial = true
			return response
		}},
		{name: "returned result error requires partial true", response: func() *models.SearchResponse {
			response := handlerRelevanceSearchResponse(models.SearchRankingApplied)
			response.Results[0].Errors = []models.SearchResultError{{
				Stage: models.SearchResultStageFetch, Code: models.ErrCodeInternal, Message: "fetch unavailable",
			}}
			return response
		}},
		{name: "rejects future trust error stage", response: func() *models.SearchResponse {
			response := handlerRelevanceSearchResponse(models.SearchRankingApplied)
			response.Results[0].Errors = []models.SearchResultError{{
				Stage: models.SearchResultErrorStage("trust"), Code: models.ErrCodeInternal, Message: "trust unavailable",
			}}
			response.Partial = true
			return response
		}, body: `{"query":"q","ranking":"relevance","include_content":true}`},
		{name: "wrong summary mode", response: func() *models.SearchResponse {
			response := handlerRelevanceSearchResponse(models.SearchRankingApplied)
			response.Ranking.Mode = models.SearchRankingTrust
			return response
		}},
		{name: "partial forbidden", response: func() *models.SearchResponse {
			response := handlerRelevanceSearchResponse(models.SearchRankingApplied)
			response.Ranking.Status = models.SearchRankingStatus("partial")
			return response
		}},
		{name: "applied forbids degraded reason", response: func() *models.SearchResponse {
			response := handlerRelevanceSearchResponse(models.SearchRankingApplied)
			response.Ranking.DegradedReason = models.SearchRankingReasonRerankerFailed
			return response
		}},
		{name: "applied requires result ranking", response: func() *models.SearchResponse {
			response := handlerRelevanceSearchResponse(models.SearchRankingApplied)
			response.Results[0].Ranking = nil
			return response
		}},
		{name: "applied requires relevance score", response: func() *models.SearchResponse {
			response := handlerRelevanceSearchResponse(models.SearchRankingApplied)
			response.Results[0].Ranking.RelevanceScore = nil
			return response
		}},
		{name: "applied rejects non finite score", response: func() *models.SearchResponse {
			response := handlerRelevanceSearchResponse(models.SearchRankingApplied)
			value := math.NaN()
			response.Results[0].Ranking.RelevanceScore = &value
			return response
		}},
		{name: "applied rejects score above one", response: func() *models.SearchResponse {
			response := handlerRelevanceSearchResponse(models.SearchRankingApplied)
			value := 1.01
			response.Results[0].Ranking.RelevanceScore = &value
			return response
		}},
		{name: "applied rejects invalid provider rank", response: func() *models.SearchResponse {
			response := handlerRelevanceSearchResponse(models.SearchRankingApplied)
			response.Results[0].Ranking.ProviderRank = 0
			return response
		}},
		{name: "applied rejects duplicate provider rank", response: func() *models.SearchResponse {
			response := handlerRelevanceSearchResponse(models.SearchRankingApplied)
			response.Results[1].Ranking.ProviderRank = response.Results[0].Ranking.ProviderRank
			return response
		}},
		{name: "applied rejects relevance order", response: func() *models.SearchResponse {
			response := handlerRelevanceSearchResponse(models.SearchRankingApplied)
			*response.Results[0].Ranking.RelevanceScore = 0.2
			return response
		}},
		{name: "applied rejects provider tie break", response: func() *models.SearchResponse {
			response := handlerRelevanceSearchResponse(models.SearchRankingApplied)
			*response.Results[1].Ranking.RelevanceScore = *response.Results[0].Ranking.RelevanceScore
			response.Results[0].Ranking.ProviderRank = 9
			response.Results[1].Ranking.ProviderRank = 2
			return response
		}},
		{name: "rejects non contiguous public rank", response: func() *models.SearchResponse {
			response := handlerRelevanceSearchResponse(models.SearchRankingApplied)
			response.Results[1].Rank = 3
			return response
		}},
		{name: "candidate count cannot trail results", response: func() *models.SearchResponse {
			response := handlerRelevanceSearchResponse(models.SearchRankingApplied)
			response.Ranking.CandidateCount = 1
			return response
		}},
		{name: "candidate count is bounded", response: func() *models.SearchResponse {
			response := handlerRelevanceSearchResponse(models.SearchRankingApplied)
			response.Ranking.CandidateCount = models.MaxSearchLimit + 1
			return response
		}},
		{name: "relevance requires rerank timing", response: func() *models.SearchResponse {
			response := handlerRelevanceSearchResponse(models.SearchRankingApplied)
			response.Timing.RerankMs = nil
			return response
		}},
		{name: "degraded requires reranker reason", response: func() *models.SearchResponse {
			response := handlerRelevanceSearchResponse(models.SearchRankingDegraded)
			response.Ranking.DegradedReason = ""
			return response
		}},
		{name: "degraded forbids result ranking", response: func() *models.SearchResponse {
			response := handlerRelevanceSearchResponse(models.SearchRankingDegraded)
			score := 0.5
			response.Results[0].Ranking = &models.SearchResultRanking{ProviderRank: 1, RelevanceScore: &score}
			return response
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &recordingSearchService{response: test.response()}
			body := test.body
			if body == "" {
				body = `{"query":"q","ranking":"relevance"}`
			}
			recorder := performSearchRequest(newSearchTestRouter(Search(service)), []byte(body))
			if test.wantValid {
				if recorder.Code != http.StatusOK {
					t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body)
				}
				return
			}
			assertInternalSearchResponse(t, recorder)
		})
	}
}

func TestSearchRejectsEveryTrustSuccessUntilPublicExecutionCard(t *testing.T) {
	responses := []struct {
		name     string
		response *models.SearchResponse
	}{
		{name: "provider shape", response: handlerProviderSearchResponse()},
		{name: "relevance shape", response: handlerRelevanceSearchResponse(models.SearchRankingApplied)},
	}
	for _, test := range responses {
		t.Run(test.name, func(t *testing.T) {
			service := &recordingSearchService{response: test.response}
			recorder := performSearchRequest(newSearchTestRouter(Search(service)), []byte(
				`{"query":"q","ranking":"trust","expected_subject":{"name":"q"}}`,
			))
			assertInternalSearchResponse(t, recorder)
		})
	}
}

func TestSearchRejectsCustomEncoderThatChangesValidatedRelevanceShape(t *testing.T) {
	tests := []struct {
		name   string
		encode func(context.Context, *models.SearchResponse) ([]byte, error)
	}{
		{name: "substitutes bytes", encode: func(context.Context, *models.SearchResponse) ([]byte, error) {
			return json.Marshal(handlerProviderSearchResponse())
		}},
		{name: "mutates validated response", encode: func(_ context.Context, response *models.SearchResponse) ([]byte, error) {
			response.Query = "changed-after-validation"
			return json.Marshal(response)
		}},
		{name: "mutates to another valid score", encode: func(_ context.Context, response *models.SearchResponse) ([]byte, error) {
			value := 0.85
			response.Results[0].Ranking.RelevanceScore = &value
			return json.Marshal(response)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &encodingSearchService{
				recordingSearchService: recordingSearchService{response: handlerRelevanceSearchResponse(models.SearchRankingApplied)},
				encode:                 test.encode,
			}
			recorder := performSearchRequest(newSearchTestRouter(Search(service)), []byte(`{"query":"q","ranking":"relevance"}`))
			assertInternalSearchResponse(t, recorder)
		})
	}
}

func TestSearchWeightedLimiterChargesOnceAndStopsBeforeService(t *testing.T) {
	service := &recordingSearchService{response: &models.SearchResponse{Success: true, Results: []models.SearchResult{}}}
	limiter := &recordingSearchLimiter{allowed: false}
	router := newSearchTestRouter(SearchWithRateLimiter(service, limiter))
	recorder := performSearchRequest(router, []byte(`{"query":"q","ranking":"trust","expected_subject":{"name":"q"},"include_content":true,"verify":true,"schema":{"type":"object"}}`))
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body)
	}
	response := decodeSearchTestResponse(t, recorder)
	if response.Results == nil || response.Error == nil || response.Error.Code != models.ErrCodeRateLimited || response.Error.Message != "search rate limited" {
		t.Fatalf("response = %#v", response)
	}
	if limiter.calls != 1 || len(limiter.costs) != 1 || limiter.costs[0] != MaxSearchRequestCost {
		t.Fatalf("limiter calls/costs = %d %#v", limiter.calls, limiter.costs)
	}
	if service.calls != 0 {
		t.Fatalf("service calls = %d, want zero", service.calls)
	}
}

func TestSearchRequestCostFormulaAndBounds(t *testing.T) {
	if MinSearchRequestCost != 1 || MaxSearchRequestCost != 59 {
		t.Fatalf("Search cost bounds = %d/%d, want 1/59", MinSearchRequestCost, MaxSearchRequestCost)
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
		{name: "provider maximum", request: &models.SearchRequest{Limit: 20, IncludeContent: true, Verify: true, Schema: json.RawMessage(`{}`)}, want: 22},
		{name: "relevance defaults", request: &models.SearchRequest{Ranking: models.SearchRankingRelevance}, want: 4},
		{name: "relevance maximum", request: &models.SearchRequest{Ranking: models.SearchRankingRelevance, Limit: 20, IncludeContent: true, Verify: true, Schema: json.RawMessage(`{}`)}, want: 24},
		{name: "trust defaults", request: &models.SearchRequest{Ranking: models.SearchRankingTrust}, want: 44},
		{name: "trust content reuses fetch", request: &models.SearchRequest{Ranking: models.SearchRankingTrust, IncludeContent: true}, want: 44},
		{name: "trust maximum", request: &models.SearchRequest{Ranking: models.SearchRankingTrust, IncludeContent: true, Verify: true, Schema: json.RawMessage(`{}`)}, want: 59},
		{name: "direct oversized clamps", request: &models.SearchRequest{Ranking: models.SearchRankingTrust, Limit: 200, IncludeContent: true, Verify: true, Schema: json.RawMessage(`null`)}, want: 59},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := searchRequestCost(test.request); got != test.want {
				t.Fatalf("cost = %d, want %d", got, test.want)
			}
		})
	}

	modes := []models.SearchRankingMode{models.SearchRankingProvider, models.SearchRankingRelevance, models.SearchRankingTrust}
	limits := []int{1, 5, 10, 11, 20}
	for _, mode := range modes {
		for _, limit := range limits {
			for mask := 0; mask < 8; mask++ {
				includeContent := mask&1 != 0
				verify := mask&2 != 0
				hasSchema := mask&4 != 0
				request := &models.SearchRequest{
					Ranking: mode, Limit: limit, IncludeContent: includeContent, Verify: verify,
				}
				if hasSchema {
					request.Schema = json.RawMessage(`{}`)
				}
				candidateLimit := limit
				if mode == models.SearchRankingRelevance || mode == models.SearchRankingTrust {
					candidateLimit = models.MaxSearchLimit
				}
				heavyResults := min(limit, models.MaxSearchHeavyResults)
				want := (candidateLimit + 9) / 10
				if mode == models.SearchRankingRelevance || mode == models.SearchRankingTrust {
					want += 2
				}
				if mode == models.SearchRankingTrust {
					want += 40
				} else if includeContent {
					want += heavyResults
				}
				if verify {
					want += heavyResults
				}
				if hasSchema {
					want += 2 * heavyResults
				}
				if got := searchRequestCost(request); got != want {
					t.Fatalf("cost mode=%s limit=%d mask=%03b = %d, want %d", mode, limit, mask, got, want)
				}
				if repeated := searchRequestCost(request); repeated != want {
					t.Fatalf("repeated cost mode=%s limit=%d mask=%03b = %d, want %d", mode, limit, mask, repeated, want)
				}
			}
		}
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
		recordingSearchService: recordingSearchService{response: &models.SearchResponse{Success: true, Query: "q"}},
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

func TestSearchOuterDeadlineCoversCanonicalEncodingValidation(t *testing.T) {
	for index := 0; index < cap(fallbackSearchEncodingSlots); index++ {
		fallbackSearchEncodingSlots <- struct{}{}
	}
	defer func() {
		for index := 0; index < cap(fallbackSearchEncodingSlots); index++ {
			<-fallbackSearchEncodingSlots
		}
	}()
	service := &encodingSearchService{
		recordingSearchService: recordingSearchService{response: &models.SearchResponse{Success: true, Query: "q"}},
		encode: func(_ context.Context, response *models.SearchResponse) ([]byte, error) {
			return json.Marshal(response)
		},
	}
	requestContext, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/search", strings.NewReader(`{"query":"q","timeout":120}`)).WithContext(requestContext)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	newSearchTestRouter(Search(service)).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body)
	}
	response := decodeSearchTestResponse(t, recorder)
	if response.Results == nil || response.Error == nil || response.Error.Code != models.ErrCodeTimeout ||
		response.Error.Message != "search timed out" {
		t.Fatalf("response = %#v", response)
	}
}

func TestSearchFallbackEncodingHasFourSlotsAndExactHardBoundary(t *testing.T) {
	if got := cap(fallbackSearchEncodingSlots); got != 4 {
		t.Fatalf("fallback slot capacity = %d, want 4", got)
	}

	response := exactSizedHandlerRelevanceSearchResponse(t, models.MaxSearchResponseBytes)
	if err := validateSearchResponseForRequest(&models.SearchRequest{
		Query: "q", Limit: models.MaxSearchHeavyResults, Ranking: models.SearchRankingRelevance,
		IncludeContent: true, Schema: json.RawMessage(`{}`),
	}, response); err != nil {
		t.Fatalf("bounded fixture is not a valid relevance response: %v", err)
	}
	encoded, err := encodeSearchResponseFallback(context.Background(), response)
	if err != nil {
		t.Fatalf("exact-limit encode: %v", err)
	}
	if len(encoded) != models.MaxSearchResponseBytes {
		t.Fatalf("exact-limit bytes = %d, want %d", len(encoded), models.MaxSearchResponseBytes)
	}
	encoded = nil
	response.Results[0].Content += "x"
	if ok, preflightErr := preflightSearchResponseSize(context.Background(), response); preflightErr != nil || ok {
		t.Fatalf("N+1 ranking preflight = %t, %v; want false, nil", ok, preflightErr)
	}
	if _, err := encodeSearchResponseFallback(context.Background(), response); err == nil {
		t.Fatal("N+1 response was accepted")
	}
}

func exactSizedHandlerRelevanceSearchResponse(t *testing.T, target int) *models.SearchResponse {
	t.Helper()
	zero := int64(0)
	response := &models.SearchResponse{
		Success: true,
		Query:   "q",
		Results: make([]models.SearchResult, 0, models.MaxSearchHeavyResults),
		Timing:  models.SearchTimingInfo{RerankMs: &zero},
		Ranking: &models.SearchResponseRanking{Mode: models.SearchRankingRelevance, Status: models.SearchRankingApplied},
	}
	fetchedAt := time.Date(2026, time.August, 10, 1, 2, 3, 0, time.UTC)
	for index := range models.MaxSearchHeavyResults {
		basis := models.EvidenceBasis{"blob": {
			Quote: "x", TextRange: [2]int{0, 1}, Method: evidence.MethodExact,
			SnapshotID: "sha256:" + strings.Repeat("a", 64), FetchedAt: fetchedAt,
		}}
		receipts := models.FieldReceipts{"blob": "field-receipt"}
		unlocatedRate := 0.0
		score := 0.9 - float64(index)/10
		response.Results = append(response.Results, models.SearchResult{
			Rank: index + 1, Title: "result", URL: fmt.Sprintf("https://result-%d.example/article", index),
			FinalURL: fmt.Sprintf("https://result-%d.example/final", index), Content: "x",
			VerificationStatus: models.SearchVerificationNotChecked,
			Data:               json.RawMessage(`{"blob":"x"}`), Basis: &basis, Receipts: &receipts, UnlocatedRate: &unlocatedRate,
			Ranking: &models.SearchResultRanking{ProviderRank: index + 1, RelevanceScore: &score},
		})
	}
	response.Ranking.CandidateCount = len(response.Results)
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	padding := target - len(encoded)
	if padding < 0 {
		t.Fatalf("target %d is below response overhead %d", target, len(encoded))
	}
	for index := range response.Results {
		added := min(padding, models.MaxSearchResultContentBytes-len(response.Results[index].Content))
		response.Results[index].Content += strings.Repeat("x", added)
		padding -= added
	}
	for index := range response.Results {
		added := min(padding, models.MaxSearchResultDataBytes-len(response.Results[index].Data))
		data := response.Results[index].Data
		response.Results[index].Data = json.RawMessage(string(data[:len(data)-2]) + strings.Repeat("d", added) + string(data[len(data)-2:]))
		padding -= added
	}
	if padding != 0 {
		t.Fatalf("target %d exceeds bounded fixture capacity by %d bytes", target, padding)
	}
	encoded, err = json.Marshal(response)
	if err != nil || len(encoded) != target {
		t.Fatalf("fixture bytes/error = %d/%v, want %d", len(encoded), err, target)
	}
	return response
}

func TestSearchEncodingSlotWaitHonorsContext(t *testing.T) {
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
	response := &models.SearchResponse{Success: true, Results: []models.SearchResult{}}
	service := &encodingSearchService{encode: func(_ context.Context, response *models.SearchResponse) ([]byte, error) {
		return json.Marshal(response)
	}}
	if _, err := encodeSearchResponse(ctx, service, response); !errors.Is(err, context.Canceled) {
		t.Fatalf("custom encoding slot cancellation = %v, want canceled", err)
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

func handlerProviderSearchResponse() *models.SearchResponse {
	return &models.SearchResponse{
		Success: true,
		Query:   "q",
		Results: []models.SearchResult{
			{Rank: 1, Title: "first", URL: "https://first.example/", VerificationStatus: models.SearchVerificationNotChecked},
			{Rank: 2, Title: "second", URL: "https://second.example/", VerificationStatus: models.SearchVerificationNotChecked},
		},
		Timing: models.SearchTimingInfo{TotalMs: 5, ProviderMs: 5},
	}
}

func handlerRelevanceSearchResponse(status models.SearchRankingStatus) *models.SearchResponse {
	response := handlerProviderSearchResponse()
	firstScore, secondScore := 0.9, 0.8
	response.Results[0].Ranking = &models.SearchResultRanking{ProviderRank: 2, RelevanceScore: &firstScore}
	response.Results[1].Ranking = &models.SearchResultRanking{ProviderRank: 5, RelevanceScore: &secondScore}
	rerankMilliseconds := int64(2)
	response.Timing.TotalMs += rerankMilliseconds
	response.Timing.RerankMs = &rerankMilliseconds
	response.Ranking = &models.SearchResponseRanking{
		Mode:           models.SearchRankingRelevance,
		Status:         status,
		CandidateCount: len(response.Results),
	}
	if status == models.SearchRankingDegraded {
		response.Ranking.DegradedReason = models.SearchRankingReasonRerankerFailed
		for index := range response.Results {
			response.Results[index].Ranking = nil
		}
	}
	return response
}

func assertInternalSearchResponse(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body)
	}
	response := decodeSearchTestResponse(t, recorder)
	if response.Success || response.Results == nil || response.Error == nil ||
		response.Error.Code != models.ErrCodeInternal || response.Error.Message != "internal search failure" {
		t.Fatalf("response = %#v", response)
	}
}
