package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/use-agent/purify/discovery"
	"github.com/use-agent/purify/models"
)

type mapServiceFixture struct {
	discover func(context.Context, string) (*discovery.Result, error)
}

func (fixture mapServiceFixture) Discover(ctx context.Context, rawURL string) (*discovery.Result, error) {
	return fixture.discover(ctx, rawURL)
}

func TestPostMapReturnsStablePartialResult(t *testing.T) {
	t.Parallel()

	type contextKey string
	const requestKey contextKey = "request"
	wantURLs := []string{"https://example.com/", "https://example.com/docs"}
	wantResult := &discovery.Result{
		URLs: wantURLs,
		Warnings: []discovery.Warning{{
			Source:  "sitemap",
			URL:     "https://example.com/sitemap.xml",
			Code:    "http_status",
			Message: "HTTP status 404",
		}},
		Sources: discovery.SourceStats{
			SitemapFiles:   1,
			RobotsSitemaps: 1,
			HomeLinks:      2,
			Truncated:      true,
		},
	}
	service := mapServiceFixture{discover: func(ctx context.Context, rawURL string) (*discovery.Result, error) {
		if rawURL != "https://example.com" {
			t.Fatalf("Discover() URL = %q", rawURL)
		}
		if got := ctx.Value(requestKey); got != "sentinel" {
			t.Fatalf("Discover() context value = %v", got)
		}
		return wantResult, nil
	}}

	requestContext := context.WithValue(context.Background(), requestKey, "sentinel")
	response := performMapRequest(t, PostMap(service), `{"url":"https://example.com"}`, requestContext)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}

	var got models.MapResponse
	decodeMapResponse(t, response, &got)
	want := models.MapResponse{
		Success: true,
		URLs:    wantURLs,
		Total:   2,
		Warnings: []models.MapWarning{{
			Source:  "sitemap",
			URL:     "https://example.com/sitemap.xml",
			Code:    "http_status",
			Message: "HTTP status 404",
		}},
		Sources: &models.MapSourceStats{
			SitemapFiles:   1,
			RobotsSitemaps: 1,
			HomeLinks:      2,
			Truncated:      true,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("response = %#v, want %#v", got, want)
	}

}

func TestMapResponseCopiesDomainSlices(t *testing.T) {
	t.Parallel()

	result := &discovery.Result{
		URLs:     []string{"https://example.com/"},
		Warnings: []discovery.Warning{{Source: "homepage", Code: "partial", Message: "partial result"}},
	}
	response := mapResponse(result)
	response.URLs[0] = "https://changed.example/"
	response.Warnings[0].Code = "changed"
	if result.URLs[0] != "https://example.com/" || result.Warnings[0].Code != "partial" {
		t.Fatalf("mapResponse() aliases domain result: %#v", result)
	}

	emptyResponse := mapResponse(&discovery.Result{})
	encoded, err := json.Marshal(emptyResponse)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"urls":[]`) {
		t.Fatalf("empty result must preserve legacy array shape: %s", encoded)
	}
}

func TestPostMapRejectsInvalidRequestsWithoutCallingService(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{name: "invalid JSON", body: `{"url":`},
		{name: "missing URL", body: `{}`},
		{name: "invalid URL", body: `{"url":"not a URL"}`},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			service := mapServiceFixture{discover: func(context.Context, string) (*discovery.Result, error) {
				calls++
				return nil, nil
			}}

			response := performMapRequest(t, PostMap(service), test.body, context.Background())
			assertMapError(t, response, http.StatusBadRequest, models.ErrCodeInvalidInput, "map URL is invalid")
			if calls != 0 {
				t.Fatalf("Discover() calls = %d, want 0", calls)
			}
		})
	}
}

func TestPostMapMapsServiceErrorsWithoutLeakingInternals(t *testing.T) {
	t.Parallel()

	diagnostic := &discovery.Result{
		URLs: []string{"https://example.com/"},
		Warnings: []discovery.Warning{{
			Source:  "homepage",
			Code:    "fetch_failed",
			Message: "connection refused",
		}},
		Sources: discovery.SourceStats{SitemapFiles: 1},
	}
	tests := []struct {
		name        string
		result      *discovery.Result
		err         error
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{
			name:        "invalid root URL",
			err:         errors.Join(errors.New("secret parser detail"), discovery.ErrInvalidRootURL),
			wantStatus:  http.StatusBadRequest,
			wantCode:    models.ErrCodeInvalidInput,
			wantMessage: "map URL is invalid",
		},
		{
			name:        "deadline",
			result:      diagnostic,
			err:         errors.Join(errors.New("secret upstream address"), context.DeadlineExceeded),
			wantStatus:  http.StatusGatewayTimeout,
			wantCode:    models.ErrCodeTimeout,
			wantMessage: "map discovery timed out",
		},
		{
			name:        "canceled",
			err:         errors.Join(errors.New("secret shutdown reason"), context.Canceled),
			wantStatus:  http.StatusGatewayTimeout,
			wantCode:    models.ErrCodeTimeout,
			wantMessage: "map discovery timed out",
		},
		{
			name:        "all sources failed",
			result:      diagnostic,
			err:         errors.Join(errors.New("secret network detail"), discovery.ErrAllSourcesFailed),
			wantStatus:  http.StatusBadGateway,
			wantCode:    models.ErrCodeNavigation,
			wantMessage: "map discovery sources are unavailable",
		},
		{
			name:        "unexpected error",
			err:         errors.New("secret implementation detail"),
			wantStatus:  http.StatusInternalServerError,
			wantCode:    models.ErrCodeInternal,
			wantMessage: "map discovery failed",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			service := mapServiceFixture{discover: func(context.Context, string) (*discovery.Result, error) {
				return test.result, test.err
			}}
			response := performMapRequest(t, PostMap(service), `{"url":"https://example.com"}`, context.Background())
			got := assertMapError(t, response, test.wantStatus, test.wantCode, test.wantMessage)
			if strings.Contains(response.Body.String(), "secret") {
				t.Fatalf("response leaked internal error: %s", response.Body)
			}
			if test.result != nil {
				if got.Total != len(test.result.URLs) || !reflect.DeepEqual(got.URLs, test.result.URLs) {
					t.Fatalf("diagnostic URLs = %#v, total %d", got.URLs, got.Total)
				}
				if len(got.Warnings) != len(test.result.Warnings) || got.Sources == nil {
					t.Fatalf("diagnostics missing from response: %#v", got)
				}
			}
		})
	}
}

func TestPostMapHandlesMissingOrEmptyService(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		service     MapService
		wantMessage string
	}{
		{
			name:        "missing service",
			wantMessage: "map service is not configured",
		},
		{
			name: "empty response",
			service: mapServiceFixture{discover: func(context.Context, string) (*discovery.Result, error) {
				return nil, nil
			}},
			wantMessage: "map service returned an empty response",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			response := performMapRequest(t, PostMap(test.service), `{"url":"https://example.com"}`, context.Background())
			assertMapError(t, response, http.StatusInternalServerError, models.ErrCodeInternal, test.wantMessage)
		})
	}
}

func performMapRequest(t *testing.T, handler gin.HandlerFunc, body string, ctx context.Context) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/map", strings.NewReader(body)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	engine := gin.New()
	engine.POST("/api/v1/map", handler)
	engine.ServeHTTP(response, request)
	return response
}

func assertMapError(t *testing.T, response *httptest.ResponseRecorder, wantStatus int, wantCode, wantMessage string) models.MapResponse {
	t.Helper()
	if response.Code != wantStatus {
		t.Fatalf("status = %d, want %d, body = %s", response.Code, wantStatus, response.Body)
	}
	var got models.MapResponse
	decodeMapResponse(t, response, &got)
	if got.Success || got.Error == nil || got.Error.Code != wantCode || got.Error.Message != wantMessage {
		t.Fatalf("response = %#v, want error %s/%q", got, wantCode, wantMessage)
	}
	return got
}

func decodeMapResponse(t *testing.T, response *httptest.ResponseRecorder, target *models.MapResponse) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatalf("decode response: %v; body = %s", err, response.Body)
	}
}
