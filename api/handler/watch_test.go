package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/use-agent/purify/models"
	watchdomain "github.com/use-agent/purify/watch"
)

type watchServiceStub struct {
	create func(context.Context, models.FactSpec) (watchdomain.Watch, bool, error)
	get    func(context.Context, string) (watchdomain.Watch, error)
	list   func(context.Context, watchdomain.WatchListOptions) (watchdomain.WatchPage, error)
	pause  func(context.Context, string) (watchdomain.Watch, error)
	resume func(context.Context, string) (watchdomain.Watch, error)
	delete func(context.Context, string) error
	factAt func(context.Context, string, string, time.Time) (watchdomain.Fact, bool, error)
}

func (stub *watchServiceStub) Create(ctx context.Context, spec models.FactSpec) (watchdomain.Watch, bool, error) {
	if stub.create == nil {
		return watchdomain.Watch{}, false, errors.New("unexpected Create")
	}
	return stub.create(ctx, spec)
}

func (stub *watchServiceStub) Get(ctx context.Context, id string) (watchdomain.Watch, error) {
	if stub.get == nil {
		return watchdomain.Watch{}, errors.New("unexpected Get")
	}
	return stub.get(ctx, id)
}

func (stub *watchServiceStub) List(ctx context.Context, options watchdomain.WatchListOptions) (watchdomain.WatchPage, error) {
	if stub.list == nil {
		return watchdomain.WatchPage{}, errors.New("unexpected List")
	}
	return stub.list(ctx, options)
}

func (stub *watchServiceStub) Pause(ctx context.Context, id string) (watchdomain.Watch, error) {
	if stub.pause == nil {
		return watchdomain.Watch{}, errors.New("unexpected Pause")
	}
	return stub.pause(ctx, id)
}

func (stub *watchServiceStub) Resume(ctx context.Context, id string) (watchdomain.Watch, error) {
	if stub.resume == nil {
		return watchdomain.Watch{}, errors.New("unexpected Resume")
	}
	return stub.resume(ctx, id)
}

func (stub *watchServiceStub) Delete(ctx context.Context, id string) error {
	if stub.delete == nil {
		return errors.New("unexpected Delete")
	}
	return stub.delete(ctx, id)
}

func (stub *watchServiceStub) FactAt(ctx context.Context, subject, predicate string, asOf time.Time) (watchdomain.Fact, bool, error) {
	if stub.factAt == nil {
		return watchdomain.Fact{}, false, errors.New("unexpected FactAt")
	}
	return stub.factAt(ctx, subject, predicate, asOf)
}

type watchLimiterStub struct {
	allow bool
	calls int
	costs []int
}

func (stub *watchLimiterStub) Allow(_ *gin.Context, cost int) bool {
	stub.calls++
	stub.costs = append(stub.costs, cost)
	return stub.allow
}

var watchHTTPTestTime = time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)

func validHandlerWatch(id string, created time.Time) watchdomain.Watch {
	next := created.Add(time.Hour)
	return watchdomain.Watch{
		ID: id,
		Spec: models.FactSpec{
			Subject: "example price", Predicate: "price", Freshness: "day",
			MinIndependentSources: 2, OnConflict: models.FactConflictExpose,
		},
		State: watchdomain.StateActive, NextCheckAt: &next, EWMAInterval: time.Hour,
		CreatedAt: created, UpdatedAt: created,
	}
}

func watchHandlerRecorder(method, path string, body []byte, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Handle(method, "/watches/:id", handler)
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func watchErrorCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var response models.WatchErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error == nil {
		t.Fatalf("missing error envelope: %s", recorder.Body)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if len(document) != 1 || document["error"] == nil {
		t.Fatalf("non-canonical error envelope: %s", recorder.Body)
	}
	return response.Error.Code
}

func TestCreateWatchNormalizesSpecAndDistinguishesRetry(t *testing.T) {
	for _, test := range []struct {
		name       string
		created    bool
		wantStatus int
	}{
		{name: "new", created: true, wantStatus: http.StatusCreated},
		{name: "exact retry", created: false, wantStatus: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			limiter := &watchLimiterStub{allow: true}
			calls := 0
			service := &watchServiceStub{create: func(_ context.Context, spec models.FactSpec) (watchdomain.Watch, bool, error) {
				calls++
				want := models.FactSpec{Subject: "example price", Predicate: "price", Freshness: "week",
					MinIndependentSources: 2, OnConflict: models.FactConflictExpose}
				if spec != want {
					t.Fatalf("service spec = %#v, want %#v", spec, want)
				}
				value := validHandlerWatch("00000000-0000-4000-8000-000000000001", watchHTTPTestTime)
				value.Spec = want
				if test.created {
					value.State = watchdomain.StatePending
					value.NextCheckAt = &value.CreatedAt
				}
				return value, test.created, nil
			}}
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.POST("/watches", CreateWatchWithRateLimiter(service, limiter))
			body := []byte(`{"spec":{"subject":"  example   price ","predicate":"price","freshness":"7d","min_independent_sources":2,"on_conflict":"expose"}}`)
			request := httptest.NewRequest(http.MethodPost, "/watches", bytes.NewReader(body))
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus || calls != 1 || limiter.calls != 1 || limiter.costs[0] != 1 {
				t.Fatalf("status/calls/limit = %d/%d/%#v body=%s", recorder.Code, calls, limiter.costs, recorder.Body)
			}
			var response models.CreateWatchResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Created != test.created || response.Watch.Spec.Freshness != "week" {
				t.Fatalf("response = %#v", response)
			}
			for _, forbidden := range []string{"lease_id", "lease_until", "last_verification_id"} {
				if strings.Contains(recorder.Body.String(), forbidden) {
					t.Fatalf("response leaked %q: %s", forbidden, recorder.Body)
				}
			}
		})
	}
}

func TestCreateWatchRejectsStructuralSmugglingBeforeService(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{name: "empty"},
		{name: "null", body: []byte(`null`)},
		{name: "array", body: []byte(`[]`)},
		{name: "multiple", body: []byte(`{"spec":{}} {}`)},
		{name: "top case variant", body: []byte(`{"Spec":{"subject":"x","predicate":"p"}}`)},
		{name: "nested case variant", body: []byte(`{"spec":{"Subject":"x","predicate":"p"}}`)},
		{name: "unknown", body: []byte(`{"spec":{"subject":"x","predicate":"p"},"timeout":1}`)},
		{name: "retired flat draft", body: []byte(`{"subject":"x","predicate":"p","url":"https://example.com","schema":{"type":"object"}}`)},
		{name: "duplicate", body: []byte(`{"spec":{"subject":"x","\u0073ubject":"y","predicate":"p"}}`)},
		{name: "invalid UTF8", body: []byte{'{', 0xff, '}'}},
		{name: "invalid scalar spec", body: []byte(`{"spec":1}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			service := &watchServiceStub{create: func(context.Context, models.FactSpec) (watchdomain.Watch, bool, error) {
				calls++
				return watchdomain.Watch{}, false, nil
			}}
			limiter := &watchLimiterStub{allow: true}
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.POST("/watches", CreateWatchWithRateLimiter(service, limiter))
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/watches", bytes.NewReader(test.body)))
			if recorder.Code != http.StatusBadRequest || watchErrorCode(t, recorder) != models.ErrCodeInvalidInput ||
				calls != 0 || limiter.calls != 1 {
				t.Fatalf("status/service/limiter = %d/%d/%d body=%s", recorder.Code, calls, limiter.calls, recorder.Body)
			}
		})
	}
}

func TestCreateWatchRequestHasExactOneMiBBoundary(t *testing.T) {
	base := []byte(`{"spec":{"subject":"example price","predicate":"price","freshness":"day","min_independent_sources":2,"on_conflict":"expose"}}`)
	makeBody := func(size int) []byte {
		body := append([]byte(nil), base...)
		return append(body, bytes.Repeat([]byte{' '}, size-len(body))...)
	}
	for _, test := range []struct {
		size       int
		wantStatus int
		wantCalls  int
	}{
		{size: models.MaxWatchRequestBytes, wantStatus: http.StatusCreated, wantCalls: 1},
		{size: models.MaxWatchRequestBytes + 1, wantStatus: http.StatusRequestEntityTooLarge},
	} {
		calls := 0
		service := &watchServiceStub{create: func(_ context.Context, spec models.FactSpec) (watchdomain.Watch, bool, error) {
			calls++
			value := validHandlerWatch("00000000-0000-4000-8000-000000000001", watchHTTPTestTime)
			value.Spec = spec
			value.State = watchdomain.StatePending
			value.NextCheckAt = &value.CreatedAt
			return value, true, nil
		}}
		gin.SetMode(gin.TestMode)
		router := gin.New()
		router.POST("/watches", CreateWatchWithRateLimiter(service, &watchLimiterStub{allow: true}))
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/watches", bytes.NewReader(makeBody(test.size))))
		if recorder.Code != test.wantStatus || calls != test.wantCalls {
			t.Fatalf("size %d status/calls = %d/%d body=%s", test.size, recorder.Code, calls, recorder.Body)
		}
	}
}

func TestWatchHandlersFailClosedRateLimitAndMapErrors(t *testing.T) {
	var typedNil *watchServiceStub
	for _, service := range []WatchService{nil, typedNil} {
		limiter := &watchLimiterStub{allow: true}
		recorder := watchHandlerRecorder(http.MethodGet, "/watches/00000000-0000-4000-8000-000000000001", nil,
			GetWatchWithRateLimiter(service, limiter))
		if recorder.Code != http.StatusServiceUnavailable || watchErrorCode(t, recorder) != models.ErrCodeWatchUnavailable || limiter.calls != 0 {
			t.Fatalf("typed nil status/limit = %d/%d body=%s", recorder.Code, limiter.calls, recorder.Body)
		}
	}

	service := &watchServiceStub{get: func(context.Context, string) (watchdomain.Watch, error) {
		return watchdomain.Watch{}, nil
	}}
	limiter := &watchLimiterStub{allow: false}
	recorder := watchHandlerRecorder(http.MethodGet, "/watches/00000000-0000-4000-8000-000000000001", nil,
		GetWatchWithRateLimiter(service, limiter))
	if recorder.Code != http.StatusTooManyRequests || watchErrorCode(t, recorder) != models.ErrCodeRateLimited || limiter.calls != 1 {
		t.Fatalf("rate response = %d/%d %s", recorder.Code, limiter.calls, recorder.Body)
	}

	for _, test := range []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "invalid", err: watchdomain.ErrInvalidWatchID, wantStatus: 400, wantCode: models.ErrCodeInvalidInput},
		{name: "missing", err: watchdomain.ErrWatchNotFound, wantStatus: 404, wantCode: models.ErrCodeWatchNotFound},
		{name: "unavailable", err: watchdomain.ErrInvalidStore, wantStatus: 503, wantCode: models.ErrCodeWatchUnavailable},
		{name: "timeout", err: context.DeadlineExceeded, wantStatus: 504, wantCode: models.ErrCodeTimeout},
		{name: "internal", err: errors.New("sensitive database failure"), wantStatus: 500, wantCode: models.ErrCodeInternal},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &watchServiceStub{get: func(context.Context, string) (watchdomain.Watch, error) {
				return watchdomain.Watch{}, test.err
			}}
			recorder := watchHandlerRecorder(http.MethodGet, "/watches/00000000-0000-4000-8000-000000000001", nil,
				GetWatchWithRateLimiter(service, &watchLimiterStub{allow: true}))
			if recorder.Code != test.wantStatus || watchErrorCode(t, recorder) != test.wantCode ||
				strings.Contains(recorder.Body.String(), "sensitive") {
				t.Fatalf("error response = %d %s", recorder.Code, recorder.Body)
			}
		})
	}
}

func TestWatchActionsRequireCanonicalIDAndZeroByteBody(t *testing.T) {
	calls := 0
	service := &watchServiceStub{
		pause: func(_ context.Context, id string) (watchdomain.Watch, error) {
			calls++
			value := validHandlerWatch(id, watchHTTPTestTime)
			value.State = watchdomain.StatePaused
			value.NextCheckAt = nil
			value.PausedAt = &value.UpdatedAt
			return value, nil
		},
		delete: func(context.Context, string) error { calls++; return nil },
	}
	for _, test := range []struct {
		name       string
		method     string
		path       string
		body       []byte
		handler    gin.HandlerFunc
		wantStatus int
		wantCalls  int
	}{
		{name: "pause", method: http.MethodPost, path: "/watches/00000000-0000-4000-8000-000000000001", handler: PauseWatchWithRateLimiter(service, &watchLimiterStub{allow: true}), wantStatus: 200, wantCalls: 1},
		{name: "whitespace body rejected", method: http.MethodPost, path: "/watches/00000000-0000-4000-8000-000000000001", body: []byte(" "), handler: PauseWatchWithRateLimiter(service, &watchLimiterStub{allow: true}), wantStatus: 400},
		{name: "uppercase UUID rejected", method: http.MethodPost, path: "/watches/00000000-0000-4000-8000-00000000000A", handler: PauseWatchWithRateLimiter(service, &watchLimiterStub{allow: true}), wantStatus: 400},
		{name: "delete", method: http.MethodDelete, path: "/watches/00000000-0000-4000-8000-000000000001", handler: DeleteWatchWithRateLimiter(service, &watchLimiterStub{allow: true}), wantStatus: 204, wantCalls: 1},
	} {
		before := calls
		recorder := watchHandlerRecorder(test.method, test.path, test.body, test.handler)
		if recorder.Code != test.wantStatus || calls-before != test.wantCalls {
			t.Fatalf("%s status/calls = %d/%d body=%s", test.name, recorder.Code, calls-before, recorder.Body)
		}
		if test.wantStatus == http.StatusNoContent && recorder.Body.Len() != 0 {
			t.Fatalf("delete body = %q", recorder.Body.String())
		}
	}
}

func TestWatchReadAndMutationsBindReturnedIdentityAndState(t *testing.T) {
	const requested = "00000000-0000-4000-8000-000000000001"
	const different = "00000000-0000-4000-8000-000000000002"
	tests := []struct {
		name    string
		method  string
		handler func(*watchServiceStub) gin.HandlerFunc
		stub    *watchServiceStub
	}{
		{
			name: "get different ID", method: http.MethodGet,
			handler: func(service *watchServiceStub) gin.HandlerFunc {
				return GetWatchWithRateLimiter(service, &watchLimiterStub{allow: true})
			},
			stub: &watchServiceStub{get: func(context.Context, string) (watchdomain.Watch, error) {
				return validHandlerWatch(different, watchHTTPTestTime), nil
			}},
		},
		{
			name: "pause remains active", method: http.MethodPost,
			handler: func(service *watchServiceStub) gin.HandlerFunc {
				return PauseWatchWithRateLimiter(service, &watchLimiterStub{allow: true})
			},
			stub: &watchServiceStub{pause: func(context.Context, string) (watchdomain.Watch, error) {
				return validHandlerWatch(requested, watchHTTPTestTime), nil
			}},
		},
		{
			name: "resume remains paused", method: http.MethodPost,
			handler: func(service *watchServiceStub) gin.HandlerFunc {
				return ResumeWatchWithRateLimiter(service, &watchLimiterStub{allow: true})
			},
			stub: &watchServiceStub{resume: func(context.Context, string) (watchdomain.Watch, error) {
				value := validHandlerWatch(requested, watchHTTPTestTime)
				value.State = watchdomain.StatePaused
				value.NextCheckAt = nil
				value.PausedAt = &value.UpdatedAt
				return value, nil
			}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := watchHandlerRecorder(test.method, "/watches/"+requested, nil, test.handler(test.stub))
			if recorder.Code != http.StatusInternalServerError || watchErrorCode(t, recorder) != models.ErrCodeInternal {
				t.Fatalf("bound response = %d %s", recorder.Code, recorder.Body)
			}
		})
	}
}

func TestWatchListCursorIsStrictOpaqueAndBoundToLastItem(t *testing.T) {
	first := validHandlerWatch("00000000-0000-4000-8000-000000000001", watchHTTPTestTime)
	second := validHandlerWatch("00000000-0000-4000-8000-000000000002", watchHTTPTestTime.Add(time.Second))
	second.Spec.Subject = "second price"
	service := &watchServiceStub{list: func(_ context.Context, options watchdomain.WatchListOptions) (watchdomain.WatchPage, error) {
		if options.Limit != 2 || options.Cursor != nil {
			t.Fatalf("options = %#v", options)
		}
		return watchdomain.WatchPage{
			Items: []watchdomain.Watch{first, second},
			Next:  &watchdomain.WatchCursor{CreatedAt: second.CreatedAt, ID: second.ID},
		}, nil
	}}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/watches", ListWatchesWithRateLimiter(service, &watchLimiterStub{allow: true}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/watches?limit=2", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", recorder.Code, recorder.Body)
	}
	var response models.WatchListResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	cursor, err := decodeWatchCursor(response.NextCursor)
	if err != nil || cursor.ID != second.ID || !cursor.CreatedAt.Equal(second.CreatedAt) || len(response.Watches) != 2 {
		t.Fatalf("page/cursor = %#v/%#v err=%v", response, cursor, err)
	}

	for _, query := range []string{
		"?unknown=1", "?l%69mit=1", "?limit", "?limit=1&", "?limit=1&&cursor=x",
		"?limit=01", "?limit=0", "?limit=101", "?limit=1&limit=2",
		"?cursor=", "?cursor=YWJj=", "?cursor=not_base64!",
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/watches"+query, nil))
		if recorder.Code != http.StatusBadRequest || watchErrorCode(t, recorder) != models.ErrCodeInvalidInput {
			t.Fatalf("query %q = %d %s", query, recorder.Code, recorder.Body)
		}
	}
}

func TestWatchListNormalizesNilAndRejectsImpossiblePages(t *testing.T) {
	for _, test := range []struct {
		name string
		page watchdomain.WatchPage
		want int
	}{
		{name: "nil items", page: watchdomain.WatchPage{}, want: 200},
		{name: "continuation without items", page: watchdomain.WatchPage{Next: &watchdomain.WatchCursor{
			CreatedAt: watchHTTPTestTime, ID: "00000000-0000-4000-8000-000000000001",
		}}, want: 500},
		{name: "deleted projection", page: watchdomain.WatchPage{Items: []watchdomain.Watch{func() watchdomain.Watch {
			value := validHandlerWatch("00000000-0000-4000-8000-000000000001", watchHTTPTestTime)
			value.State = watchdomain.StateDeleted
			return value
		}()}}, want: 500},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &watchServiceStub{list: func(context.Context, watchdomain.WatchListOptions) (watchdomain.WatchPage, error) {
				return test.page, nil
			}}
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.GET("/watches", ListWatchesWithRateLimiter(service, &watchLimiterStub{allow: true}))
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/watches", nil))
			if recorder.Code != test.want {
				t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body)
			}
			if test.want == 200 && !strings.Contains(recorder.Body.String(), `"watches":[]`) {
				t.Fatalf("nil list response = %s", recorder.Body)
			}
		})
	}
}

type blockingWatchJSON struct {
	entered chan<- struct{}
	release <-chan struct{}
}

func (value blockingWatchJSON) MarshalJSON() ([]byte, error) {
	value.entered <- struct{}{}
	<-value.release
	return []byte(`{}`), nil
}

func TestWatchEncodingHasExactBudgetAndFourGlobalSlots(t *testing.T) {
	exact := append([]byte{'"'}, bytes.Repeat([]byte{'a'}, models.MaxWatchResponseBytes-2)...)
	exact = append(exact, '"')
	encoded, err := encodeWatchPayload(context.Background(), json.RawMessage(exact))
	if err != nil || len(encoded) != models.MaxWatchResponseBytes {
		t.Fatalf("exact budget len/err = %d/%v", len(encoded), err)
	}
	over := append(exact[:len(exact)-1], 'a', '"')
	if encoded, err := encodeWatchPayload(context.Background(), json.RawMessage(over)); err == nil || encoded != nil {
		t.Fatalf("N+1 encoding = len %d err %v", len(encoded), err)
	}

	entered := make(chan struct{}, 5)
	release := make(chan struct{}, 5)
	results := make(chan error, 5)
	var group sync.WaitGroup
	for range 5 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := encodeWatchPayload(context.Background(), blockingWatchJSON{entered: entered, release: release})
			results <- err
		}()
	}
	for range 4 {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("four encoding slots did not start")
		}
	}
	select {
	case <-entered:
		t.Fatal("a fifth encoder entered the four-slot critical section")
	case <-time.After(50 * time.Millisecond):
	}
	for range 4 {
		release <- struct{}{}
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("fifth encoder did not enter after a slot was released")
	}
	release <- struct{}{}
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if encoded, err := encodeWatchPayload(canceled, map[string]bool{"ok": true}); !errors.Is(err, context.Canceled) || encoded != nil {
		t.Fatalf("canceled encode = %q, %v", encoded, err)
	}
}
