package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/use-agent/purify/models"
	verifydomain "github.com/use-agent/purify/verify"
)

func TestVerifyHTTPAdapterReturnsResponseAndForwardsRequestContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	verifiedAt := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	want := &models.VerifyResponse{
		VerificationID: "verification-http",
		URL:            "https://example.com/item",
		FinalURL:       "https://www.example.com/item",
		StatusCode:     http.StatusOK,
		Results: []models.ClaimResult{{
			Path:   "price",
			Status: models.VerifyStatusConfirmed,
		}},
		VerifiedAt: verifiedAt,
	}
	type contextKey struct{}
	service := verifyServiceFunc(func(ctx context.Context, request models.VerifyRequest) (*models.VerifyResponse, error) {
		if got := ctx.Value(contextKey{}); got != "request-context" {
			t.Fatalf("request context value = %#v", got)
		}
		if request.URL != "https://example.com/item" || request.WebhookURL != "https://hooks.example/facts" || request.WebhookSecret != "secret" {
			t.Fatalf("bound request = %#v", request)
		}
		return want, nil
	})
	router := gin.New()
	router.POST("/verify", Verify(service))
	body := `{"url":"https://example.com/item","claims":[],"webhook_url":"https://hooks.example/facts","webhook_secret":"secret"}`
	request := httptest.NewRequest(http.MethodPost, "/verify", strings.NewReader(body))
	request = request.WithContext(context.WithValue(request.Context(), contextKey{}, "request-context"))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	var got models.VerifyResponse
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got.VerificationID != want.VerificationID || got.FinalURL != want.FinalURL || !got.VerifiedAt.Equal(verifiedAt) || len(got.Results) != 1 {
		t.Fatalf("response = %#v, want %#v", got, want)
	}
}

func TestVerifyHTTPAdapterMapsErrorsWithoutLeakingInternals(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "invalid request", err: verifydomain.ErrInvalidRequest, wantStatus: http.StatusBadRequest, wantCode: models.ErrCodeInvalidInput},
		{name: "invalid claim", err: verifydomain.ErrInvalidClaim, wantStatus: http.StatusBadRequest, wantCode: models.ErrCodeInvalidInput},
		{name: "invalid receipt", err: verifydomain.ErrInvalidReceipt, wantStatus: http.StatusBadRequest, wantCode: models.ErrCodeInvalidReceipt},
		{name: "not configured", err: verifydomain.ErrNotConfigured, wantStatus: http.StatusServiceUnavailable, wantCode: models.ErrCodeEvidenceUnavailable},
		{name: "missing snapshot", err: fmt.Errorf("%w: disk-path-secret", verifydomain.ErrSnapshot), wantStatus: http.StatusServiceUnavailable, wantCode: models.ErrCodeEvidenceUnavailable},
		{name: "deadline", err: context.DeadlineExceeded, wantStatus: http.StatusGatewayTimeout, wantCode: models.ErrCodeTimeout},
		{name: "canceled", err: context.Canceled, wantStatus: http.StatusGatewayTimeout, wantCode: models.ErrCodeTimeout},
		{name: "wrapped revisit deadline", err: fmt.Errorf("%w: %w", verifydomain.ErrRevisit, context.DeadlineExceeded), wantStatus: http.StatusGatewayTimeout, wantCode: models.ErrCodeTimeout},
		{name: "source unauthorized", err: &verifydomain.HTTPStatusError{StatusCode: http.StatusUnauthorized}, wantStatus: http.StatusBadGateway, wantCode: models.ErrCodeNavigation},
		{name: "source rate limit", err: &verifydomain.HTTPStatusError{StatusCode: http.StatusTooManyRequests}, wantStatus: http.StatusBadGateway, wantCode: models.ErrCodeNavigation},
		{name: "source forbidden", err: &verifydomain.HTTPStatusError{StatusCode: http.StatusForbidden}, wantStatus: http.StatusBadGateway, wantCode: models.ErrCodeNavigation},
		{name: "source internal error", err: &verifydomain.HTTPStatusError{StatusCode: http.StatusInternalServerError}, wantStatus: http.StatusBadGateway, wantCode: models.ErrCodeNavigation},
		{name: "revisit", err: fmt.Errorf("%w: disk-path-secret", verifydomain.ErrRevisit), wantStatus: http.StatusBadGateway, wantCode: models.ErrCodeNavigation},
		{name: "record", err: fmt.Errorf("%w: disk-path-secret", verifydomain.ErrRecord), wantStatus: http.StatusServiceUnavailable, wantCode: models.ErrCodeInternal},
		{name: "receipt signing", err: fmt.Errorf("%w: disk-path-secret", verifydomain.ErrReceiptSigning), wantStatus: http.StatusInternalServerError, wantCode: models.ErrCodeInternal},
		{name: "unknown", err: errors.New("disk-path-secret"), wantStatus: http.StatusInternalServerError, wantCode: models.ErrCodeInternal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router := gin.New()
			router.POST("/verify", Verify(verifyServiceFunc(func(context.Context, models.VerifyRequest) (*models.VerifyResponse, error) {
				return nil, test.err
			})))
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/verify", strings.NewReader(`{"receipt":"token"}`))
			router.ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.wantStatus, response.Body)
			}
			var body models.VerifyErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			if body.Error == nil || body.Error.Code != test.wantCode || body.Error.Message == "" {
				t.Fatalf("error body = %#v, want code %q", body, test.wantCode)
			}
			if bytes.Contains(response.Body.Bytes(), []byte("disk-path-secret")) {
				t.Fatalf("response leaked internal error: %s", response.Body)
			}
		})
	}
}

func TestVerifyHTTPAdapterRejectsInvalidBodiesAndUnavailableServices(t *testing.T) {
	gin.SetMode(gin.TestMode)
	called := false
	validService := verifyServiceFunc(func(context.Context, models.VerifyRequest) (*models.VerifyResponse, error) {
		called = true
		return &models.VerifyResponse{}, nil
	})
	tests := []struct {
		name       string
		service    VerifyService
		body       string
		wantStatus int
		wantCode   string
		wantCalled bool
	}{
		{
			name:       "bad JSON",
			service:    validService,
			body:       `{`,
			wantStatus: http.StatusBadRequest,
			wantCode:   models.ErrCodeInvalidInput,
		},
		{
			name:       "empty body",
			service:    validService,
			body:       ``,
			wantStatus: http.StatusBadRequest,
			wantCode:   models.ErrCodeInvalidInput,
		},
		{
			name:       "unknown field",
			service:    validService,
			body:       `{"receipt":"token","unexpected":true}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   models.ErrCodeInvalidInput,
		},
		{
			name:       "trailing JSON",
			service:    validService,
			body:       `{"receipt":"token"} {}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   models.ErrCodeInvalidInput,
		},
		{
			name:       "nil service",
			body:       `{}`,
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   models.ErrCodeEvidenceUnavailable,
		},
		{
			name: "nil response",
			service: verifyServiceFunc(func(context.Context, models.VerifyRequest) (*models.VerifyResponse, error) {
				return nil, nil
			}),
			body:       `{}`,
			wantStatus: http.StatusInternalServerError,
			wantCode:   models.ErrCodeInternal,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called = false
			router := gin.New()
			router.POST("/verify", Verify(test.service))
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/verify", strings.NewReader(test.body)))
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.wantStatus, response.Body)
			}
			var body models.VerifyErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			if body.Error == nil || body.Error.Code != test.wantCode {
				t.Fatalf("error body = %#v, want code %q", body, test.wantCode)
			}
			if called != test.wantCalled {
				t.Fatalf("service called = %v, want %v", called, test.wantCalled)
			}
		})
	}
}

func TestVerifyHTTPAdapterRejectsOversizedBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	called := false
	service := verifyServiceFunc(func(context.Context, models.VerifyRequest) (*models.VerifyResponse, error) {
		called = true
		return nil, nil
	})
	router := gin.New()
	router.POST("/verify", Verify(service))
	response := httptest.NewRecorder()
	body := `{"receipt":"` + strings.Repeat("x", maximumVerifyRequestBytes) + `"}`
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/verify", strings.NewReader(body)))

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusRequestEntityTooLarge, response.Body)
	}
	if called {
		t.Fatal("service was called for an oversized request")
	}
}

type verifyServiceFunc func(context.Context, models.VerifyRequest) (*models.VerifyResponse, error)

func (function verifyServiceFunc) Verify(ctx context.Context, request models.VerifyRequest) (*models.VerifyResponse, error) {
	return function(ctx, request)
}
