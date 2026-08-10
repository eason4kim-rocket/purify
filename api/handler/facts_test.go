package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/models"
	watchdomain "github.com/use-agent/purify/watch"
)

func validHandlerFact() watchdomain.Fact {
	observed := watchHTTPTestTime
	return watchdomain.Fact{
		ID: strings.Repeat("a", 64), WatchID: "00000000-0000-4000-8000-000000000001",
		Subject: "example price", Predicate: "price", Path: "price",
		Value: json.RawMessage(`"19"`), Root: "example.com",
		SourceURL: "https://example.com/pricing", Receipt: "receipt_A.B-c",
		SnapshotID:            "sha256:" + strings.Repeat("b", 64),
		CreatedVerificationID: "verification-created", CreatedClaimIndex: 0,
		LatestVerificationID: "verification-latest", LatestClaimIndex: 1,
		ObservedAt: observed, ValidFrom: observed, LastVerifiedAt: observed.Add(time.Minute),
	}
}

func factQueryPath(subject, predicate string, asOf time.Time) string {
	values := url.Values{}
	values.Set("subject", subject)
	values.Set("predicate", predicate)
	values.Set("as_of", asOf.Format(time.RFC3339Nano))
	return "/facts?" + values.Encode()
}

func factHandlerResponse(service WatchService, limiter WatchRateLimiter, path string, body []byte) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/facts", GetFactAtWithRateLimiter(service, limiter))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, bytes.NewReader(body)))
	return recorder
}

func TestFactLookupReturnsPublicProjectionAndTemporalGap(t *testing.T) {
	asOf := watchHTTPTestTime.Add(30 * time.Minute)
	for _, found := range []bool{true, false} {
		t.Run(map[bool]string{true: "found", false: "gap"}[found], func(t *testing.T) {
			calls := 0
			service := &watchServiceStub{factAt: func(_ context.Context, subject, predicate string, gotAsOf time.Time) (watchdomain.Fact, bool, error) {
				calls++
				if subject != "example price" || predicate != "price" || !gotAsOf.Equal(asOf) || gotAsOf.Location() != time.UTC {
					t.Fatalf("FactAt arguments = %q/%q/%v", subject, predicate, gotAsOf)
				}
				if found {
					return validHandlerFact(), true, nil
				}
				return watchdomain.Fact{}, false, nil
			}}
			limiter := &watchLimiterStub{allow: true}
			recorder := factHandlerResponse(service, limiter, factQueryPath("example price", "price", asOf), nil)
			if recorder.Code != http.StatusOK || calls != 1 || limiter.calls != 1 {
				t.Fatalf("status/calls = %d/%d/%d body=%s", recorder.Code, calls, limiter.calls, recorder.Body)
			}
			var response models.FactLookupResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if !response.AsOf.Equal(asOf) || (response.Fact != nil) != found {
				t.Fatalf("fact response = %#v", response)
			}
			if found {
				if string(response.Fact.Value) != `"19"` || response.Fact.Root != "example.com" || response.Fact.ValidTo != nil {
					t.Fatalf("fact projection = %#v", response.Fact)
				}
				for _, forbidden := range []string{
					"created_verification", "latest_verification", "closed_verification", "claim_index",
					"lease_id", "lease_until", "webhook", "secret", "credential",
				} {
					if strings.Contains(recorder.Body.String(), forbidden) {
						t.Fatalf("fact projection leaked %q: %s", forbidden, recorder.Body)
					}
				}
			} else if !strings.Contains(recorder.Body.String(), `"fact":null`) {
				t.Fatalf("gap response = %s", recorder.Body)
			}
		})
	}
}

func TestFactQueryRejectsNonCanonicalOrAmbiguousInput(t *testing.T) {
	serviceCalls := 0
	service := &watchServiceStub{factAt: func(context.Context, string, string, time.Time) (watchdomain.Fact, bool, error) {
		serviceCalls++
		return watchdomain.Fact{}, false, nil
	}}
	queries := []string{
		"/facts",
		"/facts?subject=example+price&predicate=price",
		"/facts?subject=example+price&predicate=price&as_of=2026-08-10T12%3A00%3A00Z&extra=1",
		"/facts?s%75bject=example+price&predicate=price&as_of=2026-08-10T12%3A00%3A00Z",
		"/facts?subject=example+price&predicate=price&as_of=2026-08-10T12%3A00%3A00Z&",
		"/facts?subject=example+price&subject=other&predicate=price&as_of=2026-08-10T12%3A00%3A00Z",
		"/facts?Subject=example+price&predicate=price&as_of=2026-08-10T12%3A00%3A00Z",
		"/facts?subject=example++price&predicate=price&as_of=2026-08-10T12%3A00%3A00Z",
		"/facts?subject=example+price&predicate=unit+price&as_of=2026-08-10T12%3A00%3A00Z",
		"/facts?subject=example+price&predicate=price&as_of=2026-08-10T20%3A00%3A00%2B08%3A00",
		"/facts?subject=example+price&predicate=price&as_of=2026-08-10T12%3A00%3A00.000Z",
		"/facts?subject=example+price&predicate=price&as_of=not-a-time",
	}
	for _, path := range queries {
		recorder := factHandlerResponse(service, &watchLimiterStub{allow: true}, path, nil)
		if recorder.Code != http.StatusBadRequest || watchErrorCode(t, recorder) != models.ErrCodeInvalidInput {
			t.Fatalf("path %q = %d %s", path, recorder.Code, recorder.Body)
		}
	}
	if serviceCalls != 0 {
		t.Fatalf("invalid queries reached service %d times", serviceCalls)
	}

	recorder := factHandlerResponse(service, &watchLimiterStub{allow: true},
		factQueryPath("example price", "price", watchHTTPTestTime), []byte(" "))
	if recorder.Code != http.StatusBadRequest || serviceCalls != 0 {
		t.Fatalf("body-bearing query = %d/%d %s", recorder.Code, serviceCalls, recorder.Body)
	}
}

func TestFactLookupFailsClosedRateLimitsAndSanitizesErrors(t *testing.T) {
	var typedNil *watchServiceStub
	for _, service := range []WatchService{nil, typedNil} {
		limiter := &watchLimiterStub{allow: true}
		recorder := factHandlerResponse(service, limiter,
			factQueryPath("example price", "price", watchHTTPTestTime), nil)
		if recorder.Code != http.StatusServiceUnavailable || watchErrorCode(t, recorder) != models.ErrCodeFactUnavailable || limiter.calls != 0 {
			t.Fatalf("unavailable fact = %d/%d %s", recorder.Code, limiter.calls, recorder.Body)
		}
	}

	service := &watchServiceStub{factAt: func(context.Context, string, string, time.Time) (watchdomain.Fact, bool, error) {
		return watchdomain.Fact{}, false, nil
	}}
	limiter := &watchLimiterStub{allow: false}
	recorder := factHandlerResponse(service, limiter,
		factQueryPath("example price", "price", watchHTTPTestTime), nil)
	if recorder.Code != http.StatusTooManyRequests || watchErrorCode(t, recorder) != models.ErrCodeRateLimited || limiter.calls != 1 {
		t.Fatalf("limited fact = %d/%d %s", recorder.Code, limiter.calls, recorder.Body)
	}

	for _, test := range []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "timeout", err: context.Canceled, wantStatus: 504, wantCode: models.ErrCodeTimeout},
		{name: "invalid", err: watchdomain.ErrInvalidWatchSpec, wantStatus: 400, wantCode: models.ErrCodeInvalidInput},
		{name: "unavailable", err: watchdomain.ErrInvalidStore, wantStatus: 503, wantCode: models.ErrCodeFactUnavailable},
		{name: "internal", err: errors.New("sensitive sqlite detail"), wantStatus: 500, wantCode: models.ErrCodeInternal},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &watchServiceStub{factAt: func(context.Context, string, string, time.Time) (watchdomain.Fact, bool, error) {
				return watchdomain.Fact{}, false, test.err
			}}
			recorder := factHandlerResponse(service, &watchLimiterStub{allow: true},
				factQueryPath("example price", "price", watchHTTPTestTime), nil)
			if recorder.Code != test.wantStatus || watchErrorCode(t, recorder) != test.wantCode ||
				strings.Contains(recorder.Body.String(), "sensitive") {
				t.Fatalf("service error = %d %s", recorder.Code, recorder.Body)
			}
		})
	}
}

func TestFactProjectionRejectsCorruptAndOutOfIntervalResults(t *testing.T) {
	asOf := watchHTTPTestTime.Add(30 * time.Minute)
	mutations := []struct {
		name   string
		mutate func(*watchdomain.Fact)
	}{
		{name: "null", mutate: func(value *watchdomain.Fact) { value.Value = json.RawMessage(`null`) }},
		{name: "object", mutate: func(value *watchdomain.Fact) { value.Value = json.RawMessage(`{}`) }},
		{name: "private source", mutate: func(value *watchdomain.Fact) { value.SourceURL = "http://127.0.0.1/"; value.Root = "127.0.0.1" }},
		{name: "mismatched root", mutate: func(value *watchdomain.Fact) { value.Root = "other.example" }},
		{name: "bad snapshot", mutate: func(value *watchdomain.Fact) { value.SnapshotID = "sha256:short" }},
		{name: "half latest provenance", mutate: func(value *watchdomain.Fact) { value.LatestVerificationID = "" }},
		{name: "future interval", mutate: func(value *watchdomain.Fact) {
			value.ValidFrom = asOf.Add(time.Second)
			value.ObservedAt = value.ValidFrom
			value.LastVerifiedAt = value.ValidFrom
		}},
		{name: "closed at boundary", mutate: func(value *watchdomain.Fact) {
			closed := asOf
			index := 2
			value.ValidTo = &closed
			value.ClosedVerificationID = "verification-closed"
			value.ClosedClaimIndex = &index
			value.ClosedOutcome = ledger.OutcomeGone
			value.GoneScope = ledger.GoneScopeField
		}},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			service := &watchServiceStub{factAt: func(context.Context, string, string, time.Time) (watchdomain.Fact, bool, error) {
				value := validHandlerFact()
				test.mutate(&value)
				return value, true, nil
			}}
			recorder := factHandlerResponse(service, &watchLimiterStub{allow: true},
				factQueryPath("example price", "price", asOf), nil)
			if recorder.Code != http.StatusInternalServerError || watchErrorCode(t, recorder) != models.ErrCodeInternal {
				t.Fatalf("corrupt projection = %d %s", recorder.Code, recorder.Body)
			}
		})
	}

	service := &watchServiceStub{factAt: func(context.Context, string, string, time.Time) (watchdomain.Fact, bool, error) {
		return validHandlerFact(), false, nil
	}}
	recorder := factHandlerResponse(service, &watchLimiterStub{allow: true},
		factQueryPath("example price", "price", asOf), nil)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("nonzero gap result = %d %s", recorder.Code, recorder.Body)
	}
}

func TestFactLookupPrioritizesCanceledRequestContext(t *testing.T) {
	service := &watchServiceStub{factAt: func(context.Context, string, string, time.Time) (watchdomain.Fact, bool, error) {
		return validHandlerFact(), true, nil
	}}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/facts", GetFactAtWithRateLimiter(service, &watchLimiterStub{allow: true}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodGet,
		factQueryPath("example price", "price", watchHTTPTestTime.Add(time.Minute)), nil).WithContext(ctx)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusGatewayTimeout || watchErrorCode(t, recorder) != models.ErrCodeTimeout {
		t.Fatalf("canceled lookup = %d %s", recorder.Code, recorder.Body)
	}
}
