package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/use-agent/purify/api/handler"
	"github.com/use-agent/purify/cache"
	compilerdomain "github.com/use-agent/purify/compiler"
	"github.com/use-agent/purify/config"
	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/receipts"
	"github.com/use-agent/purify/scraper"
	watchdomain "github.com/use-agent/purify/watch"
)

var _ func(
	*scraper.Scraper,
	handler.ExtractService,
	*receipts.Signer,
	*config.Config,
	*cache.Cache,
	time.Time,
	handler.ScrapeRunner,
	handler.BatchService,
	handler.CrawlService,
	handler.MapService,
	handler.VerifyService,
) *gin.Engine = NewRouter

type routerVerifyService struct {
	calls int
}

type routerExtractorHealService struct {
	calls int
}

type routerSearchService struct {
	calls int
}

type routerAnswerService struct {
	calls int
}

type routerWatchService struct {
	calls     int
	factCalls int
}

func routerWatchValue(id string, spec models.FactSpec, state watchdomain.State) watchdomain.Watch {
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	next := now.Add(time.Hour)
	value := watchdomain.Watch{
		ID: id, Spec: spec, State: state, NextCheckAt: &next,
		EWMAInterval: time.Hour, CreatedAt: now, UpdatedAt: now,
	}
	if state == watchdomain.StatePending {
		value.NextCheckAt = &now
	}
	if state == watchdomain.StatePaused {
		value.NextCheckAt = nil
		value.PausedAt = &now
	}
	return value
}

func routerWatchSpec() models.FactSpec {
	return models.FactSpec{
		Subject: "example price", Predicate: "price", Freshness: "day",
		MinIndependentSources: 2, OnConflict: models.FactConflictExpose,
	}
}

func (service *routerWatchService) Create(_ context.Context, spec models.FactSpec) (watchdomain.Watch, bool, error) {
	service.calls++
	return routerWatchValue("00000000-0000-4000-8000-000000000001", spec, watchdomain.StatePending), true, nil
}

func (service *routerWatchService) Get(_ context.Context, id string) (watchdomain.Watch, error) {
	service.calls++
	return routerWatchValue(id, routerWatchSpec(), watchdomain.StateActive), nil
}

func (service *routerWatchService) List(_ context.Context, _ watchdomain.WatchListOptions) (watchdomain.WatchPage, error) {
	service.calls++
	return watchdomain.WatchPage{Items: []watchdomain.Watch{}}, nil
}

func (service *routerWatchService) Pause(_ context.Context, id string) (watchdomain.Watch, error) {
	service.calls++
	return routerWatchValue(id, routerWatchSpec(), watchdomain.StatePaused), nil
}

func (service *routerWatchService) Resume(_ context.Context, id string) (watchdomain.Watch, error) {
	service.calls++
	return routerWatchValue(id, routerWatchSpec(), watchdomain.StateActive), nil
}

func (service *routerWatchService) Delete(_ context.Context, _ string) error {
	service.calls++
	return nil
}

func (service *routerWatchService) FactAt(_ context.Context, _, _ string, _ time.Time) (watchdomain.Fact, bool, error) {
	service.factCalls++
	return watchdomain.Fact{}, false, nil
}

func (service *routerAnswerService) Answer(_ context.Context, _ *models.AnswerRequest) (*models.AnswerResponse, error) {
	service.calls++
	return &models.AnswerResponse{
		Status: models.AnswerStatusUnknown,
		Belief: nil,
		Reason: models.AnswerUnknownNoSearchResults,
		Needs:  &models.AnswerNeeds{MoreIndependentSources: 2},
	}, nil
}

func (service *routerSearchService) Search(_ context.Context, request *models.SearchRequest) (*models.SearchResponse, error) {
	service.calls++
	return &models.SearchResponse{
		Success: true,
		Query:   request.Query,
		Results: []models.SearchResult{},
	}, nil
}

func (service *routerExtractorHealService) ScheduleExtractor(
	_ context.Context,
	extractorID string,
) (compilerdomain.HealSchedule, error) {
	service.calls++
	return compilerdomain.HealSchedule{
		ExtractorID: extractorID,
		HealRunID:   "00000000-0000-4000-8000-000000000002",
	}, nil
}

func (service *routerVerifyService) Verify(_ context.Context, _ models.VerifyRequest) (*models.VerifyResponse, error) {
	service.calls++
	return &models.VerifyResponse{
		VerificationID: "verification-router-test",
		URL:            "https://example.com/",
		FinalURL:       "https://example.com/",
		StatusCode:     http.StatusOK,
		Results:        []models.ClaimResult{},
		SnapshotID:     "sha256:current",
		VerifiedAt:     time.Date(2026, time.August, 9, 8, 0, 0, 0, time.UTC),
	}, nil
}

func TestReceiptRoutesRemainPublicWhenAPIAuthIsEnabled(t *testing.T) {
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x71}, ed25519.SeedSize))
	signer, err := receipts.NewSigner(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Sign(receipts.Payload{
		URL:   "https://example.com",
		Path:  "name",
		Value: json.RawMessage(`"Ada"`),
		Anchor: evidence.Anchor{
			Method:     evidence.MethodUnlocated,
			SnapshotID: "sha256:abc",
			FetchedAt:  time.Date(2026, time.August, 9, 7, 59, 0, 0, time.UTC),
		},
		IssuedAt: time.Date(2026, time.August, 9, 8, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Server:    config.ServerConfig{Mode: "test"},
		Auth:      config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}},
		RateLimit: config.RateLimitConfig{RequestsPerSecond: 100, Burst: 100},
	}
	verifyService := &routerVerifyService{}
	router := NewRouter(nil, nil, signer, cfg, cache.New(1), time.Now(), nil, nil, nil, nil, verifyService)

	requestBody, _ := json.Marshal(map[string]string{"receipt": token})
	verifyRequest := httptest.NewRequest(http.MethodPost, "/api/v1/receipts/verify", bytes.NewReader(requestBody))
	verifyRequest.Header.Set("Content-Type", "application/json")
	verifyResponse := httptest.NewRecorder()
	router.ServeHTTP(verifyResponse, verifyRequest)
	if verifyResponse.Code != http.StatusOK {
		t.Fatalf("public verify status = %d, body = %s", verifyResponse.Code, verifyResponse.Body)
	}

	pubkeyResponse := httptest.NewRecorder()
	router.ServeHTTP(pubkeyResponse, httptest.NewRequest(http.MethodGet, "/api/v1/receipts/pubkey", nil))
	if pubkeyResponse.Code != http.StatusOK {
		t.Fatalf("public pubkey status = %d, body = %s", pubkeyResponse.Code, pubkeyResponse.Body)
	}

	protectedRequest := httptest.NewRequest(http.MethodPost, "/api/v1/extract", bytes.NewReader([]byte(`{}`)))
	protectedRequest.Header.Set("Content-Type", "application/json")
	protectedResponse := httptest.NewRecorder()
	router.ServeHTTP(protectedResponse, protectedRequest)
	if protectedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("protected extract status = %d, want 401; body = %s", protectedResponse.Code, protectedResponse.Body)
	}

	verifyBody := []byte(`{"url":"https://example.com/","claims":[{"path":"name","value":"Ada","anchor":{"quote":"Ada","text_range":[0,3],"selector":"#name","method":"exact","snapshot_id":"sha256:old","fetched_at":"2026-08-09T07:00:00Z"}}]}`)
	unauthorizedVerify := httptest.NewRequest(http.MethodPost, "/api/v1/verify", bytes.NewReader(verifyBody))
	unauthorizedVerify.Header.Set("Content-Type", "application/json")
	unauthorizedVerifyResponse := httptest.NewRecorder()
	router.ServeHTTP(unauthorizedVerifyResponse, unauthorizedVerify)
	if unauthorizedVerifyResponse.Code != http.StatusUnauthorized {
		t.Fatalf("protected verify status = %d, want 401; body = %s", unauthorizedVerifyResponse.Code, unauthorizedVerifyResponse.Body)
	}
	if verifyService.calls != 0 {
		t.Fatalf("unauthorized verify reached service %d times", verifyService.calls)
	}

	authorizedVerify := httptest.NewRequest(http.MethodPost, "/api/v1/verify", bytes.NewReader(verifyBody))
	authorizedVerify.Header.Set("Content-Type", "application/json")
	authorizedVerify.Header.Set("X-API-Key", "required-secret")
	authorizedVerifyResponse := httptest.NewRecorder()
	router.ServeHTTP(authorizedVerifyResponse, authorizedVerify)
	if authorizedVerifyResponse.Code != http.StatusOK {
		t.Fatalf("authorized verify status = %d, want 200; body = %s", authorizedVerifyResponse.Code, authorizedVerifyResponse.Body)
	}
	if verifyService.calls != 1 {
		t.Fatalf("authorized verify reached service %d times, want 1", verifyService.calls)
	}
}

func TestExtractorHealRouteIsProtectedAndRouterOptionPreservesOldConstruction(t *testing.T) {
	cfg := &config.Config{
		Server:    config.ServerConfig{Mode: "test"},
		Auth:      config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}},
		RateLimit: config.RateLimitConfig{RequestsPerSecond: 100, Burst: 100},
	}
	const extractorID = "00000000-0000-4000-8000-000000000001"
	path := "/api/v1/extractors/" + extractorID + "/heal"

	// The old fixed call surface still builds the route, but without its option
	// the capability fails closed after authentication.
	withoutOption := NewRouter(nil, nil, nil, cfg, cache.New(1), time.Now(), nil, nil, nil, nil, nil)
	unavailable := httptest.NewRequest(http.MethodPost, path, nil)
	unavailable.Header.Set("X-API-Key", "required-secret")
	unavailableResponse := httptest.NewRecorder()
	withoutOption.ServeHTTP(unavailableResponse, unavailable)
	if unavailableResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured heal status = %d, body = %s", unavailableResponse.Code, unavailableResponse.Body)
	}

	service := &routerExtractorHealService{}
	withOption := NewRouterWithOptions(nil, nil, nil, cfg, cache.New(1), time.Now(), nil, nil, nil, nil, nil,
		WithExtractorHealService(service))
	unauthorized := httptest.NewRequest(http.MethodPost, path, nil)
	unauthorizedResponse := httptest.NewRecorder()
	withOption.ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusUnauthorized || service.calls != 0 {
		t.Fatalf("unauthorized heal = %d calls=%d body=%s", unauthorizedResponse.Code, service.calls, unauthorizedResponse.Body)
	}

	authorized := httptest.NewRequest(http.MethodPost, path, nil)
	authorized.Header.Set("X-API-Key", "required-secret")
	authorizedResponse := httptest.NewRecorder()
	withOption.ServeHTTP(authorizedResponse, authorized)
	if authorizedResponse.Code != http.StatusAccepted || service.calls != 1 {
		t.Fatalf("authorized heal = %d calls=%d body=%s", authorizedResponse.Code, service.calls, authorizedResponse.Body)
	}
}

func TestSearchRouteIsAlwaysProtectedAndFailsClosedWithoutOption(t *testing.T) {
	cfg := &config.Config{
		Server:    config.ServerConfig{Mode: "test"},
		Auth:      config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}},
		RateLimit: config.RateLimitConfig{RequestsPerSecond: 100, Burst: 100},
	}
	router := NewRouter(nil, nil, nil, cfg, cache.New(1), time.Now(), nil, nil, nil, nil, nil)

	unauthorized := httptest.NewRequest(http.MethodPost, "/api/v1/search", bytes.NewBufferString(`{"query":"purify"}`))
	unauthorized.Header.Set("Content-Type", "application/json")
	unauthorizedResponse := httptest.NewRecorder()
	router.ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized search = %d, body=%s", unauthorizedResponse.Code, unauthorizedResponse.Body)
	}
	var unauthorizedEnvelope models.SearchResponse
	if err := json.Unmarshal(unauthorizedResponse.Body.Bytes(), &unauthorizedEnvelope); err != nil {
		t.Fatal(err)
	}
	if unauthorizedEnvelope.Results == nil || unauthorizedEnvelope.Error == nil || unauthorizedEnvelope.Error.Code != models.ErrCodeUnauthorized {
		t.Fatalf("unauthorized Search envelope = %#v", unauthorizedEnvelope)
	}

	authorized := httptest.NewRequest(http.MethodPost, "/api/v1/search", bytes.NewBufferString(`{"query":"purify"}`))
	authorized.Header.Set("Content-Type", "application/json")
	authorized.Header.Set("X-API-Key", "required-secret")
	authorizedResponse := httptest.NewRecorder()
	router.ServeHTTP(authorizedResponse, authorized)
	if authorizedResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured search = %d, body=%s", authorizedResponse.Code, authorizedResponse.Body)
	}
	var response models.SearchResponse
	if err := json.Unmarshal(authorizedResponse.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Results == nil || response.Error == nil || response.Error.Code != models.ErrCodeSearchUnavailable {
		t.Fatalf("unconfigured response = %#v", response)
	}
}

func TestSearchCapabilityRequiresAuthUsableKeyAndMaximumCostBurst(t *testing.T) {
	tests := []struct {
		name       string
		auth       config.AuthConfig
		burst      int
		header     string
		wantStatus int
		wantCalls  int
	}{
		{
			name:       "auth disabled",
			auth:       config.AuthConfig{Enabled: false, APIKeys: []string{"required-secret"}},
			burst:      handler.MaxSearchRequestCost,
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "no keys",
			auth:       config.AuthConfig{Enabled: true},
			burst:      handler.MaxSearchRequestCost,
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "blank keys",
			auth:       config.AuthConfig{Enabled: true, APIKeys: []string{"", " \t"}},
			burst:      handler.MaxSearchRequestCost,
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "burst N minus one",
			auth:       config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}},
			burst:      handler.MaxSearchRequestCost - 1,
			header:     "required-secret",
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "safe exact boundary",
			auth:       config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}},
			burst:      handler.MaxSearchRequestCost,
			header:     "required-secret",
			wantStatus: http.StatusOK,
			wantCalls:  1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{
				Server:    config.ServerConfig{Mode: "test"},
				Auth:      test.auth,
				RateLimit: config.RateLimitConfig{RequestsPerSecond: 0, Burst: test.burst},
			}
			service := &routerSearchService{}
			router := NewRouterWithOptions(nil, nil, nil, cfg, cache.New(1), time.Now(), nil, nil, nil, nil, nil,
				WithSearchService(service))
			request := httptest.NewRequest(http.MethodPost, "/api/v1/search", bytes.NewBufferString(
				`{"query":"purify","limit":20,"include_content":true,"verify":true,"schema":{"type":"object"}}`,
			))
			request.Header.Set("Content-Type", "application/json")
			if test.header != "" {
				request.Header.Set("X-API-Key", test.header)
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus || service.calls != test.wantCalls {
				t.Fatalf("status/calls = %d/%d, want %d/%d; body=%s", recorder.Code, service.calls, test.wantStatus, test.wantCalls, recorder.Body)
			}
			var response models.SearchResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Results == nil {
				t.Fatalf("nullable results: %#v", response)
			}
			if test.wantStatus == http.StatusServiceUnavailable &&
				(response.Error == nil || response.Error.Code != models.ErrCodeSearchUnavailable) {
				t.Fatalf("fail-closed response = %#v", response)
			}
		})
	}
}

func TestSearchRouterOptionUsesSharedBucketWithoutDoubleCharge(t *testing.T) {
	cfg := &config.Config{
		Server:    config.ServerConfig{Mode: "test"},
		Auth:      config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}},
		RateLimit: config.RateLimitConfig{RequestsPerSecond: 0, Burst: handler.MaxSearchRequestCost},
	}
	service := &routerSearchService{}
	router := NewRouterWithOptions(nil, nil, nil, cfg, cache.New(1), time.Now(), nil, nil, nil, nil, nil,
		WithSearchService(service))

	// The maximum Search request costs exactly 22. It succeeds with a burst of
	// 22; any preceding fixed Search charge would make this request fail.
	searchRequest := httptest.NewRequest(http.MethodPost, "/api/v1/search", bytes.NewBufferString(
		`{"query":"purify","limit":20,"include_content":true,"verify":true,"schema":{"type":"object"}}`,
	))
	searchRequest.Header.Set("Content-Type", "application/json")
	searchRequest.Header.Set("X-API-Key", "required-secret")
	searchResponse := httptest.NewRecorder()
	router.ServeHTTP(searchResponse, searchRequest)
	if searchResponse.Code != http.StatusOK || service.calls != 1 {
		t.Fatalf("search status/calls = %d/%d, body=%s", searchResponse.Code, service.calls, searchResponse.Body)
	}

	// A legacy protected route shares the same identity bucket and is denied
	// after Search consumes the burst, proving the two groups do not fork state.
	exhausted := httptest.NewRequest(http.MethodPost, "/api/v1/extract", bytes.NewBufferString(`{}`))
	exhausted.Header.Set("Content-Type", "application/json")
	exhausted.Header.Set("X-API-Key", "required-secret")
	exhaustedResponse := httptest.NewRecorder()
	router.ServeHTTP(exhaustedResponse, exhausted)
	if exhaustedResponse.Code != http.StatusTooManyRequests || service.calls != 1 {
		t.Fatalf("exhausted status/calls = %d/%d, body=%s", exhaustedResponse.Code, service.calls, exhaustedResponse.Body)
	}
	var response models.ScrapeResponse
	if err := json.Unmarshal(exhaustedResponse.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error == nil || response.Error.Code != models.ErrCodeRateLimited {
		t.Fatalf("exhausted response = %#v", response)
	}
}

func TestAnswerRouteIsAlwaysProtectedAndFailsClosedWithoutOption(t *testing.T) {
	cfg := &config.Config{
		Server:    config.ServerConfig{Mode: "test"},
		Auth:      config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}},
		RateLimit: config.RateLimitConfig{RequestsPerSecond: 100, Burst: 100},
	}
	router := NewRouter(nil, nil, nil, cfg, cache.New(1), time.Now(), nil, nil, nil, nil, nil)
	body := bytes.NewBufferString(`{"spec":{"subject":"anthropic claude","predicate":"price"}}`)
	unauthorized := httptest.NewRequest(http.MethodPost, "/api/v1/answer", body)
	unauthorized.Header.Set("Content-Type", "application/json")
	unauthorizedResponse := httptest.NewRecorder()
	router.ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized Answer = %d, body=%s", unauthorizedResponse.Code, unauthorizedResponse.Body)
	}
	var unauthorizedEnvelope models.AnswerErrorResponse
	if err := json.Unmarshal(unauthorizedResponse.Body.Bytes(), &unauthorizedEnvelope); err != nil {
		t.Fatal(err)
	}
	if unauthorizedEnvelope.Error == nil || unauthorizedEnvelope.Error.Code != models.ErrCodeUnauthorized {
		t.Fatalf("unauthorized Answer envelope = %#v", unauthorizedEnvelope)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(unauthorizedResponse.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if len(document) != 1 || document["error"] == nil {
		t.Fatalf("unauthorized Answer fields = %s", unauthorizedResponse.Body)
	}

	authorized := httptest.NewRequest(http.MethodPost, "/api/v1/answer", bytes.NewBufferString(
		`{"spec":{"subject":"anthropic claude","predicate":"price"}}`,
	))
	authorized.Header.Set("Content-Type", "application/json")
	authorized.Header.Set("X-API-Key", "required-secret")
	authorizedResponse := httptest.NewRecorder()
	router.ServeHTTP(authorizedResponse, authorized)
	if authorizedResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured Answer = %d, body=%s", authorizedResponse.Code, authorizedResponse.Body)
	}
	response := decodeRouterAnswerError(t, authorizedResponse)
	if response.Error == nil || response.Error.Code != models.ErrCodeAnswerUnavailable {
		t.Fatalf("unconfigured Answer response = %#v", response)
	}
}

func TestAnswerCapabilityRequiresAuthUsableKeyAndMaximumCostBurst(t *testing.T) {
	tests := []struct {
		name       string
		auth       config.AuthConfig
		burst      int
		header     string
		wantStatus int
		wantCalls  int
	}{
		{
			name:       "auth disabled",
			auth:       config.AuthConfig{Enabled: false, APIKeys: []string{"required-secret"}},
			burst:      handler.MaxAnswerRequestCost,
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "no keys",
			auth:       config.AuthConfig{Enabled: true},
			burst:      handler.MaxAnswerRequestCost,
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "blank keys",
			auth:       config.AuthConfig{Enabled: true, APIKeys: []string{"", " \t"}},
			burst:      handler.MaxAnswerRequestCost,
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "burst N minus one",
			auth:       config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}},
			burst:      handler.MaxAnswerRequestCost - 1,
			header:     "required-secret",
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "safe exact boundary",
			auth:       config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}},
			burst:      handler.MaxAnswerRequestCost,
			header:     "required-secret",
			wantStatus: http.StatusOK,
			wantCalls:  1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{
				Server:    config.ServerConfig{Mode: "test"},
				Auth:      test.auth,
				RateLimit: config.RateLimitConfig{RequestsPerSecond: 0, Burst: test.burst},
			}
			service := &routerAnswerService{}
			router := NewRouterWithOptions(nil, nil, nil, cfg, cache.New(1), time.Now(), nil, nil, nil, nil, nil,
				WithAnswerService(service))
			request := httptest.NewRequest(http.MethodPost, "/api/v1/answer", bytes.NewBufferString(
				`{"spec":{"subject":"anthropic claude","predicate":"price"}}`,
			))
			request.Header.Set("Content-Type", "application/json")
			if test.header != "" {
				request.Header.Set("X-API-Key", test.header)
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus || service.calls != test.wantCalls {
				t.Fatalf("status/calls = %d/%d, want %d/%d; body=%s", recorder.Code, service.calls, test.wantStatus, test.wantCalls, recorder.Body)
			}
			if test.wantStatus == http.StatusServiceUnavailable {
				response := decodeRouterAnswerError(t, recorder)
				if response.Error == nil || response.Error.Code != models.ErrCodeAnswerUnavailable {
					t.Fatalf("fail-closed response = %#v", response)
				}
			}
		})
	}
}

func TestAnswerRouterOptionUsesSharedBucketWithoutDoubleCharge(t *testing.T) {
	cfg := &config.Config{
		Server:    config.ServerConfig{Mode: "test"},
		Auth:      config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}},
		RateLimit: config.RateLimitConfig{RequestsPerSecond: 0, Burst: handler.MaxAnswerRequestCost},
	}
	service := &routerAnswerService{}
	router := NewRouterWithOptions(nil, nil, nil, cfg, cache.New(1), time.Now(), nil, nil, nil, nil, nil,
		WithAnswerService(service))

	answerRequest := httptest.NewRequest(http.MethodPost, "/api/v1/answer", bytes.NewBufferString(
		`{"spec":{"subject":"anthropic claude","predicate":"price"}}`,
	))
	answerRequest.Header.Set("Content-Type", "application/json")
	answerRequest.Header.Set("X-API-Key", "required-secret")
	answerResponse := httptest.NewRecorder()
	router.ServeHTTP(answerResponse, answerRequest)
	if answerResponse.Code != http.StatusOK || service.calls != 1 {
		t.Fatalf("Answer status/calls = %d/%d, body=%s", answerResponse.Code, service.calls, answerResponse.Body)
	}

	exhausted := httptest.NewRequest(http.MethodPost, "/api/v1/extract", bytes.NewBufferString(`{}`))
	exhausted.Header.Set("Content-Type", "application/json")
	exhausted.Header.Set("X-API-Key", "required-secret")
	exhaustedResponse := httptest.NewRecorder()
	router.ServeHTTP(exhaustedResponse, exhausted)
	if exhaustedResponse.Code != http.StatusTooManyRequests || service.calls != 1 {
		t.Fatalf("exhausted status/calls = %d/%d, body=%s", exhaustedResponse.Code, service.calls, exhaustedResponse.Body)
	}
}

func decodeRouterAnswerError(t *testing.T, recorder *httptest.ResponseRecorder) models.AnswerErrorResponse {
	t.Helper()
	var response models.AnswerErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode Answer error: %v; body=%s", err, recorder.Body)
	}
	return response
}

func TestWatchAndFactsRoutesArePermanentProtectedAndFailClosed(t *testing.T) {
	cfg := &config.Config{
		Server:    config.ServerConfig{Mode: "test"},
		Auth:      config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}},
		RateLimit: config.RateLimitConfig{RequestsPerSecond: 100, Burst: 100},
	}
	router := NewRouter(nil, nil, nil, cfg, cache.New(1), time.Now(), nil, nil, nil, nil, nil)
	const id = "00000000-0000-4000-8000-000000000001"
	routes := []struct {
		name   string
		method string
		path   string
		body   string
		code   string
	}{
		{name: "create", method: http.MethodPost, path: "/api/v1/watches", body: `{"spec":{"subject":"example price","predicate":"price"}}`, code: models.ErrCodeWatchUnavailable},
		{name: "list", method: http.MethodGet, path: "/api/v1/watches", code: models.ErrCodeWatchUnavailable},
		{name: "get", method: http.MethodGet, path: "/api/v1/watches/" + id, code: models.ErrCodeWatchUnavailable},
		{name: "pause", method: http.MethodPost, path: "/api/v1/watches/" + id + "/pause", code: models.ErrCodeWatchUnavailable},
		{name: "resume", method: http.MethodPost, path: "/api/v1/watches/" + id + "/resume", code: models.ErrCodeWatchUnavailable},
		{name: "delete", method: http.MethodDelete, path: "/api/v1/watches/" + id, code: models.ErrCodeWatchUnavailable},
		{name: "facts", method: http.MethodGet, path: "/api/v1/facts?subject=example+price&predicate=price&as_of=2026-08-10T12%3A00%3A00Z", code: models.ErrCodeFactUnavailable},
	}
	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			unauthorized := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
			unauthorizedResponse := httptest.NewRecorder()
			router.ServeHTTP(unauthorizedResponse, unauthorized)
			if unauthorizedResponse.Code != http.StatusUnauthorized ||
				decodeRouterWatchError(t, unauthorizedResponse).Error.Code != models.ErrCodeUnauthorized {
				t.Fatalf("unauthorized = %d %s", unauthorizedResponse.Code, unauthorizedResponse.Body)
			}

			authorized := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
			authorized.Header.Set("X-API-Key", "required-secret")
			authorizedResponse := httptest.NewRecorder()
			router.ServeHTTP(authorizedResponse, authorized)
			if authorizedResponse.Code != http.StatusServiceUnavailable ||
				decodeRouterWatchError(t, authorizedResponse).Error.Code != route.code {
				t.Fatalf("fail closed = %d %s", authorizedResponse.Code, authorizedResponse.Body)
			}
		})
	}
}

func TestWatchCapabilityRequiresAuthEffectiveKeyServiceAndOneTokenBurst(t *testing.T) {
	tests := []struct {
		name       string
		auth       config.AuthConfig
		burst      int
		header     string
		withOption bool
		typedNil   bool
		wantStatus int
		wantCalls  int
	}{
		{name: "auth disabled", auth: config.AuthConfig{APIKeys: []string{"required-secret"}}, burst: 1, withOption: true, wantStatus: 503},
		{name: "no keys", auth: config.AuthConfig{Enabled: true}, burst: 1, withOption: true, wantStatus: 503},
		{name: "blank keys", auth: config.AuthConfig{Enabled: true, APIKeys: []string{"", " \t"}}, burst: 1, withOption: true, wantStatus: 503},
		{name: "zero burst", auth: config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}}, burst: 0, header: "required-secret", withOption: true, wantStatus: 503},
		{name: "missing option", auth: config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}}, burst: 1, header: "required-secret", wantStatus: 503},
		{name: "typed nil", auth: config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}}, burst: 1, header: "required-secret", withOption: true, typedNil: true, wantStatus: 503},
		{name: "safe boundary", auth: config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}}, burst: 1, header: "required-secret", withOption: true, wantStatus: 200, wantCalls: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{
				Server: config.ServerConfig{Mode: "test"}, Auth: test.auth,
				RateLimit: config.RateLimitConfig{RequestsPerSecond: 0, Burst: test.burst},
			}
			service := &routerWatchService{}
			options := []RouterOption{}
			if test.withOption {
				if test.typedNil {
					var typedNil *routerWatchService
					options = append(options, WithWatchService(typedNil))
				} else {
					options = append(options, WithWatchService(service))
				}
			}
			router := NewRouterWithOptions(nil, nil, nil, cfg, cache.New(1), time.Now(), nil, nil, nil, nil, nil, options...)
			request := httptest.NewRequest(http.MethodGet, "/api/v1/watches", nil)
			if test.header != "" {
				request.Header.Set("X-API-Key", test.header)
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus || service.calls != test.wantCalls {
				t.Fatalf("status/calls = %d/%d want %d/%d body=%s", recorder.Code, service.calls, test.wantStatus, test.wantCalls, recorder.Body)
			}
			if test.wantStatus == http.StatusServiceUnavailable &&
				decodeRouterWatchError(t, recorder).Error.Code != models.ErrCodeWatchUnavailable {
				t.Fatalf("fail-closed envelope = %s", recorder.Body)
			}
		})
	}
}

func TestWatchOptionExposesAllSevenRoutes(t *testing.T) {
	cfg := &config.Config{
		Server:    config.ServerConfig{Mode: "test"},
		Auth:      config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}},
		RateLimit: config.RateLimitConfig{RequestsPerSecond: 1000, Burst: 100},
	}
	service := &routerWatchService{}
	router := NewRouterWithOptions(nil, nil, nil, cfg, cache.New(1), time.Now(), nil, nil, nil, nil, nil,
		WithWatchService(service))
	const id = "00000000-0000-4000-8000-000000000001"
	routes := []struct {
		method string
		path   string
		body   string
		want   int
	}{
		{method: http.MethodPost, path: "/api/v1/watches", body: `{"spec":{"subject":"example price","predicate":"price","freshness":"day","min_independent_sources":2,"on_conflict":"expose"}}`, want: 201},
		{method: http.MethodGet, path: "/api/v1/watches", want: 200},
		{method: http.MethodGet, path: "/api/v1/watches/" + id, want: 200},
		{method: http.MethodPost, path: "/api/v1/watches/" + id + "/pause", want: 200},
		{method: http.MethodPost, path: "/api/v1/watches/" + id + "/resume", want: 200},
		{method: http.MethodDelete, path: "/api/v1/watches/" + id, want: 204},
		{method: http.MethodGet, path: "/api/v1/facts?subject=example+price&predicate=price&as_of=2026-08-10T12%3A00%3A00Z", want: 200},
	}
	for _, route := range routes {
		request := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
		request.Header.Set("X-API-Key", "required-secret")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != route.want {
			t.Fatalf("%s %s = %d body=%s", route.method, route.path, recorder.Code, recorder.Body)
		}
	}
	if service.calls != 6 || service.factCalls != 1 {
		t.Fatalf("Watch/Fact calls = %d/%d, want 6/1", service.calls, service.factCalls)
	}
}

func TestWatchAndFactsShareOneIdentityBucketAtCostOne(t *testing.T) {
	cfg := &config.Config{
		Server:    config.ServerConfig{Mode: "test"},
		Auth:      config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}},
		RateLimit: config.RateLimitConfig{RequestsPerSecond: 0, Burst: 1},
	}
	service := &routerWatchService{}
	router := NewRouterWithOptions(nil, nil, nil, cfg, cache.New(1), time.Now(), nil, nil, nil, nil, nil,
		WithWatchService(service))

	first := httptest.NewRequest(http.MethodGet, "/api/v1/watches", nil)
	first.Header.Set("X-API-Key", "required-secret")
	firstResponse := httptest.NewRecorder()
	router.ServeHTTP(firstResponse, first)
	if firstResponse.Code != http.StatusOK || service.calls != 1 {
		t.Fatalf("first Watch request = %d/%d %s", firstResponse.Code, service.calls, firstResponse.Body)
	}

	second := httptest.NewRequest(http.MethodGet,
		"/api/v1/facts?subject=example+price&predicate=price&as_of=2026-08-10T12%3A00%3A00Z", nil)
	second.Header.Set("X-API-Key", "required-secret")
	secondResponse := httptest.NewRecorder()
	router.ServeHTTP(secondResponse, second)
	if secondResponse.Code != http.StatusTooManyRequests || service.factCalls != 0 ||
		decodeRouterWatchError(t, secondResponse).Error.Code != models.ErrCodeRateLimited {
		t.Fatalf("shared exhausted request = %d facts=%d %s", secondResponse.Code, service.factCalls, secondResponse.Body)
	}
}

func decodeRouterWatchError(t *testing.T, recorder *httptest.ResponseRecorder) models.WatchErrorResponse {
	t.Helper()
	var response models.WatchErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode Watch error: %v; body=%s", err, recorder.Body)
	}
	if response.Error == nil {
		t.Fatalf("missing Watch error: %s", recorder.Body)
	}
	return response
}
