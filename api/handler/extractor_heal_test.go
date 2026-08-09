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

	"github.com/gin-gonic/gin"
	compilerdomain "github.com/use-agent/purify/compiler"
	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/models"
)

const (
	healHandlerExtractorID = "00000000-0000-4000-8000-000000000001"
	healHandlerRunID       = "00000000-0000-4000-8000-000000000002"
)

func TestPostExtractorHealReturnsExactAcceptedReceipt(t *testing.T) {
	service := &extractorHealServiceStub{
		schedule: compilerdomain.HealSchedule{
			ExtractorID: healHandlerExtractorID,
			HealRunID:   healHandlerRunID,
		},
	}
	response := performExtractorHeal(t, PostExtractorHeal(service), healHandlerExtractorID, nil, context.Background())
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if got := strings.TrimSpace(response.Body.String()); got !=
		`{"extractor_id":"00000000-0000-4000-8000-000000000001","heal_run_id":"00000000-0000-4000-8000-000000000002","status":"accepted"}` {
		t.Fatalf("response = %s", got)
	}
	if service.calls != 1 || service.extractorID != healHandlerExtractorID {
		t.Fatalf("service calls/id = %d/%q", service.calls, service.extractorID)
	}
}

func TestPostExtractorHealRequiresAByteEmptyBody(t *testing.T) {
	for _, body := range [][]byte{[]byte(`{}`), []byte("\n"), []byte("x")} {
		service := &extractorHealServiceStub{}
		response := performExtractorHeal(t, PostExtractorHeal(service), healHandlerExtractorID, body, context.Background())
		assertExtractorHealError(t, response, http.StatusBadRequest, models.ErrCodeInvalidInput,
			"extractor heal request body must be empty")
		if service.calls != 0 {
			t.Fatalf("body %q reached service %d times", body, service.calls)
		}
	}
}

func TestPostExtractorHealMapsErrorsWithoutLeakingDetails(t *testing.T) {
	providerDetail := "sqlite failure containing sensitive-value"
	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{"invalid", fmt.Errorf("wrapped: %w", compilerdomain.ErrInvalidHealSchedule), http.StatusBadRequest,
			models.ErrCodeInvalidInput, "extractor ID is invalid"},
		{"extractor missing", fmt.Errorf("wrapped: %w", compilerdomain.ErrHealExtractorNotFound), http.StatusNotFound,
			models.ErrCodeExtractorUnavailable, "extractor was not found"},
		{"not ready", fmt.Errorf("wrapped: %w", compilerdomain.ErrHealScheduleNotReady), http.StatusConflict,
			models.ErrCodeExtractorUnavailable, "extractor has no ready heal candidate"},
		{"ambiguous", fmt.Errorf("wrapped: %w", compilerdomain.ErrHealScheduleAmbiguous), http.StatusConflict,
			models.ErrCodeExtractorUnavailable, "extractor heal candidate is ambiguous"},
		{"closed", fmt.Errorf("wrapped: %w", compilerdomain.ErrHealWorkerClosed), http.StatusServiceUnavailable,
			models.ErrCodeExtractorUnavailable, "extractor healing is unavailable"},
		{"store closed", fmt.Errorf("wrapped: %w", ledger.ErrClosed), http.StatusServiceUnavailable,
			models.ErrCodeExtractorUnavailable, "extractor healing is unavailable"},
		{"timeout", fmt.Errorf("wrapped: %w", context.DeadlineExceeded), http.StatusGatewayTimeout,
			models.ErrCodeTimeout, "extractor healing timed out"},
		{"internal", errors.New(providerDetail), http.StatusInternalServerError,
			models.ErrCodeInternal, "extractor healing could not be scheduled"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &extractorHealServiceStub{err: test.err}
			response := performExtractorHeal(t, PostExtractorHeal(service), healHandlerExtractorID, nil, context.Background())
			assertExtractorHealError(t, response, test.wantStatus, test.wantCode, test.wantMessage)
			if strings.Contains(response.Body.String(), providerDetail) {
				t.Fatalf("response leaked detail: %s", response.Body)
			}
		})
	}
}

func TestPostExtractorHealFailsClosedForUnavailableOrMalformedService(t *testing.T) {
	response := performExtractorHeal(t, PostExtractorHeal(nil), healHandlerExtractorID, nil, context.Background())
	assertExtractorHealError(t, response, http.StatusServiceUnavailable, models.ErrCodeExtractorUnavailable,
		"extractor healing is unavailable")

	for _, schedule := range []compilerdomain.HealSchedule{
		{},
		{ExtractorID: "00000000-0000-4000-8000-000000000099", HealRunID: healHandlerRunID},
		{ExtractorID: healHandlerExtractorID, HealRunID: "bad-run"},
	} {
		service := &extractorHealServiceStub{schedule: schedule}
		response := performExtractorHeal(t, PostExtractorHeal(service), healHandlerExtractorID, nil, context.Background())
		assertExtractorHealError(t, response, http.StatusInternalServerError, models.ErrCodeInternal,
			"extractor healing could not be scheduled")
	}
}

func TestPostExtractorHealPropagatesCanceledRequestAsStableTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service := &extractorHealServiceStub{err: context.Canceled}
	response := performExtractorHeal(t, PostExtractorHeal(service), healHandlerExtractorID, nil, ctx)
	assertExtractorHealError(t, response, http.StatusGatewayTimeout, models.ErrCodeTimeout,
		"extractor healing timed out")
}

type extractorHealServiceStub struct {
	schedule    compilerdomain.HealSchedule
	err         error
	calls       int
	extractorID string
}

func (stub *extractorHealServiceStub) ScheduleExtractor(
	_ context.Context,
	extractorID string,
) (compilerdomain.HealSchedule, error) {
	stub.calls++
	stub.extractorID = extractorID
	return stub.schedule, stub.err
}

func performExtractorHeal(
	t *testing.T,
	handler gin.HandlerFunc,
	extractorID string,
	body []byte,
	ctx context.Context,
) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/extractors/:id/heal", handler)
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader(body)
	}
	request := httptest.NewRequest(http.MethodPost,
		"/api/v1/extractors/"+extractorID+"/heal", reader).WithContext(ctx)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func assertExtractorHealError(
	t *testing.T,
	response *httptest.ResponseRecorder,
	wantStatus int,
	wantCode string,
	wantMessage string,
) {
	t.Helper()
	if response.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, wantStatus, response.Body)
	}
	var envelope models.ExtractorHealErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error == nil || envelope.Error.Code != wantCode || envelope.Error.Message != wantMessage {
		t.Fatalf("error = %#v, want %q/%q", envelope.Error, wantCode, wantMessage)
	}
}
