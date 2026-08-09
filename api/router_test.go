package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
