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
	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/models"
)

type recordingAnswerService struct {
	calls    int
	request  *models.AnswerRequest
	response *models.AnswerResponse
	err      error
	answer   func(context.Context, *models.AnswerRequest) (*models.AnswerResponse, error)
}

func (service *recordingAnswerService) Answer(ctx context.Context, request *models.AnswerRequest) (*models.AnswerResponse, error) {
	service.calls++
	service.request = request
	if service.answer != nil {
		return service.answer(ctx, request)
	}
	return service.response, service.err
}

type encodingAnswerService struct {
	recordingAnswerService
	encode func(context.Context, *models.AnswerResponse) ([]byte, error)
}

func (service *encodingAnswerService) EncodeResponse(ctx context.Context, response *models.AnswerResponse) ([]byte, error) {
	if service.encode == nil {
		return nil, errors.New("encoder is not configured")
	}
	return service.encode(ctx, response)
}

type recordingAnswerLimiter struct {
	allowed bool
	calls   int
	costs   []int
}

func (limiter *recordingAnswerLimiter) Allow(_ *gin.Context, cost int) bool {
	limiter.calls++
	limiter.costs = append(limiter.costs, cost)
	return limiter.allowed
}

func newAnswerTestRouter(handler gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/answer", handler)
	return router
}

func performAnswerRequest(router http.Handler, body []byte) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/answer", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	return recorder
}

func validAnswerRequestJSON() []byte {
	return []byte(`{"spec":{"subject":"anthropic claude","predicate":"price_per_mtok_input"}}`)
}

func validKnownAnswerResponse() *models.AnswerResponse {
	asOf := time.Date(2026, time.August, 10, 8, 0, 0, 0, time.UTC)
	return &models.AnswerResponse{
		Status: models.AnswerStatusKnown,
		Belief: &models.AnswerBelief{
			Value:      json.RawMessage(`"$3"`),
			Confidence: models.AnswerConfidenceMedium,
			Agreement:  models.MultiExtractAgreement{Pages: 2, IndependentRoots: 2},
			AsOf:       asOf,
			Evidence: []models.AnswerEvidence{
				{
					URL:        "https://example.com/pricing",
					Root:       "example.com",
					Quote:      "$3",
					TextRange:  [2]int{0, 2},
					Selector:   "#price",
					Method:     evidence.MethodExact,
					SnapshotID: "sha256:" + strings.Repeat("a", 64),
					FetchedAt:  asOf.Add(-time.Minute),
				},
				{
					URL:        "https://example.org/pricing",
					Root:       "example.org",
					Quote:      "$3",
					TextRange:  [2]int{0, 2},
					Selector:   "#price",
					Method:     evidence.MethodExact,
					SnapshotID: "sha256:" + strings.Repeat("b", 64),
					FetchedAt:  asOf.Add(-2 * time.Minute),
				},
			},
			Receipts: map[string]string{
				"https://example.com/pricing": "header.payload.signature",
				"https://example.org/pricing": "header.payload.signature",
			},
		},
		Lease: &models.AnswerLease{
			ExpiresAt:          asOf.Add(24 * time.Hour),
			RenewURL:           models.DefaultAnswerRenewURL,
			ConfidenceHalflife: models.DefaultAnswerConfidenceHalflifeSeconds,
		},
	}
}

func validUnknownAnswerResponse() *models.AnswerResponse {
	return &models.AnswerResponse{
		Status: models.AnswerStatusUnknown,
		Belief: nil,
		Reason: models.AnswerUnknownNoSearchResults,
		Needs:  &models.AnswerNeeds{MoreIndependentSources: 2},
	}
}

func validConflictingAnswerResponse() *models.AnswerResponse {
	return &models.AnswerResponse{
		Status: models.AnswerStatusUnknown,
		Belief: nil,
		Reason: models.AnswerUnknownConflict,
		Needs:  &models.AnswerNeeds{MoreIndependentSources: 1},
		Closest: &models.AnswerClosest{
			Value:            json.RawMessage(`"a"`),
			IndependentRoots: 2,
			Note:             "independent-root tie",
		},
		Conflicts: []models.AnswerCandidate{
			{Value: json.RawMessage(`"b"`), Agreement: models.MultiExtractAgreement{Pages: 2, IndependentRoots: 2}},
		},
	}
}

func decodeAnswerError(t *testing.T, recorder *httptest.ResponseRecorder) models.AnswerErrorResponse {
	t.Helper()
	var response models.AnswerErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode Answer error: %v; body=%s", err, recorder.Body)
	}
	return response
}

func TestAnswerReturnsKnownAndUnknownSuccessShapes(t *testing.T) {
	for _, test := range []struct {
		name     string
		response *models.AnswerResponse
	}{
		{name: "known", response: validKnownAnswerResponse()},
		{name: "unknown", response: validUnknownAnswerResponse()},
		{name: "separated core conflict", response: validConflictingAnswerResponse()},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &recordingAnswerService{response: test.response}
			recorder := performAnswerRequest(newAnswerTestRouter(Answer(service)), validAnswerRequestJSON())
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body)
			}
			var response models.AnswerResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Status != test.response.Status || service.calls != 1 || service.request == nil ||
				service.request.Spec.Predicate != "price_per_mtok_input" {
				t.Fatalf("response/calls/request = %#v/%d/%#v", response, service.calls, service.request)
			}
			if bytes.Contains(recorder.Body.Bytes(), []byte("calibrat")) {
				t.Fatalf("response used forbidden calibration claim: %s", recorder.Body)
			}
		})
	}
}

func TestAnswerStrictDecoderRejectsUntrustedJSONAndDuplicates(t *testing.T) {
	service := &recordingAnswerService{response: validUnknownAnswerResponse()}
	limiter := &recordingAnswerLimiter{allowed: true}
	router := newAnswerTestRouter(AnswerWithRateLimiter(service, limiter))
	const secret = "must-not-appear-in-answer-response"
	tests := []struct {
		name string
		body []byte
	}{
		{name: "unknown field", body: []byte(`{"spec":{"subject":"s","predicate":"p"},"unknown":"` + secret + `"}`)},
		{name: "nested unknown field", body: []byte(`{"spec":{"subject":"s","predicate":"p","unknown":"` + secret + `"}}`)},
		{name: "duplicate top field", body: []byte(`{"spec":{"subject":"s","predicate":"p"},"spec":{"subject":"` + secret + `","predicate":"p"}}`)},
		{name: "duplicate nested field", body: []byte(`{"spec":{"subject":"s","subject":"` + secret + `","predicate":"p"}}`)},
		{name: "case-folded top field", body: []byte(`{"Spec":{"subject":"s","predicate":"p"}}`)},
		{name: "case-folded top collision", body: []byte(`{"spec":{"subject":"s","predicate":"p"},"Spec":{"subject":"` + secret + `","predicate":"p"}}`)},
		{name: "case-folded nested field", body: []byte(`{"spec":{"Subject":"s","predicate":"p"}}`)},
		{name: "case-folded nested collision", body: []byte(`{"spec":{"subject":"s","Subject":"` + secret + `","predicate":"p"}}`)},
		{name: "escaped exact duplicate", body: []byte(`{"spec":{"subject":"s","predicate":"p"},"\u0073pec":{"subject":"` + secret + `","predicate":"p"}}`)},
		{name: "trailing value", body: []byte(`{"spec":{"subject":"s","predicate":"p"}} {"secret":"` + secret + `"}`)},
		{name: "null", body: []byte(`null`)},
		{name: "array", body: []byte(`[]`)},
		{name: "string", body: []byte(`"` + secret + `"`)},
		{name: "malformed", body: []byte(`{"spec":{"subject":"` + secret)},
		{name: "empty", body: nil},
		{name: "invalid UTF-8", body: append([]byte(`{"spec":{"subject":"s","predicate":"p`), 0xff, '"', '}', '}')},
		{name: "invalid timeout", body: []byte(`{"spec":{"subject":"s","predicate":"p"},"timeout":121}`)},
		{name: "missing subject", body: []byte(`{"spec":{"predicate":"p"}}`)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := performAnswerRequest(router, test.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body)
			}
			response := decodeAnswerError(t, recorder)
			if response.Error == nil || response.Error.Code != models.ErrCodeInvalidInput ||
				response.Error.Message != "invalid answer request" {
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
	if limiter.calls != len(tests) {
		t.Fatalf("limiter calls = %d, want %d", limiter.calls, len(tests))
	}
	for _, cost := range limiter.costs {
		if cost != 1 {
			t.Fatalf("invalid request cost = %d, want 1", cost)
		}
	}
}

func TestAnswerRequestBodyLimitExactBoundary(t *testing.T) {
	service := &recordingAnswerService{response: validUnknownAnswerResponse()}
	base := string(validAnswerRequestJSON())
	if len(base) >= models.MaxAnswerRequestBytes {
		t.Fatalf("base fixture is unexpectedly large: %d", len(base))
	}
	atLimit := base + strings.Repeat(" ", models.MaxAnswerRequestBytes-len(base))
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
			recorder := performAnswerRequest(newAnswerTestRouter(Answer(service)), []byte(test.body))
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, test.wantStatus, recorder.Body)
			}
		})
	}
	if service.calls != 1 {
		t.Fatalf("service calls = %d, want one", service.calls)
	}
}

func TestAnswerWeightedLimiterChargesOneOrSeventeenExactlyOnce(t *testing.T) {
	if MaxAnswerRequestCost != 17 {
		t.Fatalf("maximum Answer cost = %d, want 17", MaxAnswerRequestCost)
	}
	tests := []struct {
		name     string
		body     []byte
		wantCost int
	}{
		{name: "invalid", body: []byte(`{}`), wantCost: 1},
		{name: "valid", body: validAnswerRequestJSON(), wantCost: MaxAnswerRequestCost},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &recordingAnswerService{response: validUnknownAnswerResponse()}
			limiter := &recordingAnswerLimiter{allowed: false}
			recorder := performAnswerRequest(newAnswerTestRouter(AnswerWithRateLimiter(service, limiter)), test.body)
			if recorder.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body)
			}
			response := decodeAnswerError(t, recorder)
			if response.Error == nil || response.Error.Code != models.ErrCodeRateLimited || response.Error.Message != "answer rate limited" {
				t.Fatalf("response = %#v", response)
			}
			if limiter.calls != 1 || len(limiter.costs) != 1 || limiter.costs[0] != test.wantCost || service.calls != 0 {
				t.Fatalf("limiter/service = %d %#v/%d", limiter.calls, limiter.costs, service.calls)
			}
		})
	}
}

func TestAnswerRequestTimeoutOwnsServiceAndEncodingDeadline(t *testing.T) {
	service := &recordingAnswerService{answer: func(ctx context.Context, request *models.AnswerRequest) (*models.AnswerResponse, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("Answer service context has no deadline")
		}
		remaining := time.Until(deadline)
		if request.Timeout != 1 || remaining <= 0 || remaining > 1100*time.Millisecond {
			t.Fatalf("request timeout/remaining = %d/%s", request.Timeout, remaining)
		}
		return validUnknownAnswerResponse(), nil
	}}
	body := []byte(`{"spec":{"subject":"anthropic claude","predicate":"price"},"timeout":1}`)
	recorder := performAnswerRequest(newAnswerTestRouter(Answer(service)), body)
	if recorder.Code != http.StatusOK || service.calls != 1 {
		t.Fatalf("status/calls = %d/%d; body=%s", recorder.Code, service.calls, recorder.Body)
	}
}

func TestAnswerFailsClosedForNilTypedNilAndInvalidServiceResponses(t *testing.T) {
	var typedNil *recordingAnswerService
	invalidKnown := validKnownAnswerResponse()
	invalidKnown.Lease = nil
	invalidUnknown := validUnknownAnswerResponse()
	invalidUnknown.Belief = validKnownAnswerResponse().Belief
	tests := []struct {
		name       string
		service    AnswerService
		wantStatus int
		wantCode   string
	}{
		{name: "nil", service: nil, wantStatus: 503, wantCode: models.ErrCodeAnswerUnavailable},
		{name: "typed nil", service: typedNil, wantStatus: 503, wantCode: models.ErrCodeAnswerUnavailable},
		{name: "empty response", service: &recordingAnswerService{}, wantStatus: 500, wantCode: models.ErrCodeInternal},
		{name: "known without lease", service: &recordingAnswerService{response: invalidKnown}, wantStatus: 500, wantCode: models.ErrCodeInternal},
		{name: "unknown with belief", service: &recordingAnswerService{response: invalidUnknown}, wantStatus: 500, wantCode: models.ErrCodeInternal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := performAnswerRequest(newAnswerTestRouter(Answer(test.service)), validAnswerRequestJSON())
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body)
			}
			response := decodeAnswerError(t, recorder)
			if response.Error == nil || response.Error.Code != test.wantCode {
				t.Fatalf("response = %#v", response)
			}
		})
	}
}

func TestAnswerRejectsInvalidPublicEvidenceShape(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*models.AnswerResponse)
	}{
		{name: "snapshot format", mutate: func(response *models.AnswerResponse) { response.Belief.Evidence[0].SnapshotID = "sha256:short" }},
		{name: "future evidence", mutate: func(response *models.AnswerResponse) {
			response.Belief.Evidence[0].FetchedAt = response.Belief.AsOf.Add(time.Second)
		}},
		{name: "non UTC observation", mutate: func(response *models.AnswerResponse) {
			response.Belief.AsOf = response.Belief.AsOf.In(time.FixedZone("offset", 60*60))
		}},
		{name: "credentialed URL", mutate: func(response *models.AnswerResponse) {
			response.Belief.Evidence[0].URL = "https://user:pass@example.com/"
		}},
		{name: "private URL", mutate: func(response *models.AnswerResponse) {
			response.Belief.Evidence[0].URL = "http://127.0.0.1/"
		}},
		{name: "noncanonical URL", mutate: func(response *models.AnswerResponse) {
			response.Belief.Evidence[0].URL = "https://EXAMPLE.com/pricing"
		}},
		{name: "mismatched root", mutate: func(response *models.AnswerResponse) {
			response.Belief.Evidence[0].Root = "other.example"
		}},
		{name: "independent roots exceed distinct authoritative roots", mutate: func(response *models.AnswerResponse) {
			oldURL := response.Belief.Evidence[1].URL
			response.Belief.Evidence[1].URL = "https://www.example.com/other-pricing"
			response.Belief.Evidence[1].Root = "example.com"
			delete(response.Belief.Receipts, oldURL)
			response.Belief.Receipts[response.Belief.Evidence[1].URL] = "header.payload.signature"
		}},
		{name: "oversized quote", mutate: func(response *models.AnswerResponse) {
			response.Belief.Evidence[0].Quote = strings.Repeat("a", maximumAnswerQuoteBytes+1)
			response.Belief.Evidence[0].TextRange = [2]int{0, maximumAnswerQuoteBytes + 1}
		}},
		{name: "oversized selector", mutate: func(response *models.AnswerResponse) {
			response.Belief.Evidence[0].Selector = strings.Repeat("a", maximumAnswerSelector+1)
		}},
		{name: "oversized receipt", mutate: func(response *models.AnswerResponse) {
			response.Belief.Receipts[response.Belief.Evidence[0].URL] = strings.Repeat("a", maximumAnswerReceiptBytes+1)
		}},
		{name: "confidence mismatch", mutate: func(response *models.AnswerResponse) {
			response.Belief.Confidence = models.AnswerConfidenceHigh
		}},
		{name: "lease halflife mismatch", mutate: func(response *models.AnswerResponse) {
			response.Lease.ConfidenceHalflife++
		}},
		{name: "lease is not exact phase five duration", mutate: func(response *models.AnswerResponse) {
			response.Lease.ExpiresAt = response.Belief.AsOf.Add(24*time.Hour + time.Second)
		}},
		{name: "non string value", mutate: func(response *models.AnswerResponse) { response.Belief.Value = json.RawMessage(`3`) }},
		{name: "oversized value", mutate: func(response *models.AnswerResponse) {
			response.Belief.Value = json.RawMessage(`"` + strings.Repeat("a", maximumAnswerValueBytes) + `"`)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := validKnownAnswerResponse()
			test.mutate(response)
			recorder := performAnswerRequest(newAnswerTestRouter(Answer(&recordingAnswerService{response: response})), validAnswerRequestJSON())
			if recorder.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body)
			}
		})
	}
}

func TestAnswerRejectsImpossibleUnknownShapes(t *testing.T) {
	strayClosest := &models.AnswerClosest{
		Value:            json.RawMessage(`"v"`),
		IndependentRoots: 1,
		Note:             "insufficient independent roots",
	}
	candidate := models.AnswerCandidate{
		Value:     json.RawMessage(`"v"`),
		Agreement: models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1},
	}
	tests := []struct {
		name     string
		response *models.AnswerResponse
	}{
		{name: "no results with closest", response: &models.AnswerResponse{
			Status: models.AnswerStatusUnknown, Belief: nil, Reason: models.AnswerUnknownNoSearchResults,
			Needs: &models.AnswerNeeds{MoreIndependentSources: 1}, Closest: strayClosest,
		}},
		{name: "missing value with conflicts", response: &models.AnswerResponse{
			Status: models.AnswerStatusUnknown, Belief: nil, Reason: models.AnswerUnknownMissingValue,
			Needs: &models.AnswerNeeds{MoreIndependentSources: 1}, Conflicts: []models.AnswerCandidate{candidate},
		}},
		{name: "insufficient without closest", response: &models.AnswerResponse{
			Status: models.AnswerStatusUnknown, Belief: nil, Reason: models.AnswerUnknownInsufficient,
			Needs: &models.AnswerNeeds{MoreIndependentSources: 1},
		}},
		{name: "conflict without tie", response: &models.AnswerResponse{
			Status: models.AnswerStatusUnknown, Belief: nil, Reason: models.AnswerUnknownConflict,
			Needs: &models.AnswerNeeds{MoreIndependentSources: 1}, Conflicts: []models.AnswerCandidate{
				{Value: json.RawMessage(`"a"`), Agreement: models.MultiExtractAgreement{Pages: 2, IndependentRoots: 2}},
				{Value: json.RawMessage(`"b"`), Agreement: models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1}},
			},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := performAnswerRequest(newAnswerTestRouter(Answer(&recordingAnswerService{response: test.response})), validAnswerRequestJSON())
			if recorder.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body)
			}
			response := decodeAnswerError(t, recorder)
			if response.Error == nil || response.Error.Code != models.ErrCodeInternal || response.Error.Message != "internal answer failure" {
				t.Fatalf("response = %#v", response)
			}
		})
	}
}

func TestAnswerRejectsImpossibleConflictSets(t *testing.T) {
	makeCandidate := func(value string, pages, roots int) models.AnswerCandidate {
		return models.AnswerCandidate{
			Value:     json.RawMessage(value),
			Agreement: models.MultiExtractAgreement{Pages: pages, IndependentRoots: roots},
		}
	}
	tests := []struct {
		name   string
		mutate func(*models.AnswerResponse)
	}{
		{name: "semantic duplicate candidates", mutate: func(response *models.AnswerResponse) {
			response.Conflicts = []models.AnswerCandidate{
				makeCandidate(`"a"`, 1, 1),
				makeCandidate(`"\u0061"`, 1, 1),
			}
		}},
		{name: "pages out of order", mutate: func(response *models.AnswerResponse) {
			response.Conflicts = []models.AnswerCandidate{
				makeCandidate(`"a"`, 1, 1),
				makeCandidate(`"b"`, 2, 1),
			}
		}},
		{name: "winner repeated as conflict", mutate: func(response *models.AnswerResponse) {
			response.Conflicts = []models.AnswerCandidate{makeCandidate(`"$3"`, 1, 1)}
		}},
		{name: "winner and conflicts exceed page budget", mutate: func(response *models.AnswerResponse) {
			response.Conflicts = []models.AnswerCandidate{makeCandidate(`"other"`, 7, 1)}
		}},
		{name: "nine total value groups", mutate: func(response *models.AnswerResponse) {
			response.Conflicts = make([]models.AnswerCandidate, models.MaxExtractSources)
			for index := range response.Conflicts {
				response.Conflicts[index] = makeCandidate(`"conflict-`+string(rune('a'+index))+`"`, 1, 1)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := validKnownAnswerResponse()
			test.mutate(response)
			recorder := performAnswerRequest(newAnswerTestRouter(Answer(&recordingAnswerService{response: response})), validAnswerRequestJSON())
			if recorder.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body)
			}
		})
	}

	duplicateClosest := validConflictingAnswerResponse()
	duplicateClosest.Conflicts[0].Value = json.RawMessage(`"\u0061"`)
	recorder := performAnswerRequest(newAnswerTestRouter(Answer(&recordingAnswerService{response: duplicateClosest})), validAnswerRequestJSON())
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("duplicate closest status = %d, body=%s", recorder.Code, recorder.Body)
	}

	nonTie := validConflictingAnswerResponse()
	nonTie.Conflicts[0].Agreement = models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1}
	recorder = performAnswerRequest(newAnswerTestRouter(Answer(&recordingAnswerService{response: nonTie})), validAnswerRequestJSON())
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("non-tie status = %d, body=%s", recorder.Code, recorder.Body)
	}

	conflictPageOverflow := validConflictingAnswerResponse()
	conflictPageOverflow.Conflicts[0].Agreement = models.MultiExtractAgreement{Pages: 7, IndependentRoots: 2}
	recorder = performAnswerRequest(newAnswerTestRouter(Answer(&recordingAnswerService{response: conflictPageOverflow})), validAnswerRequestJSON())
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("conflict page overflow status = %d, body=%s", recorder.Code, recorder.Body)
	}

	noAlternative := validConflictingAnswerResponse()
	noAlternative.Conflicts = nil
	recorder = performAnswerRequest(newAnswerTestRouter(Answer(&recordingAnswerService{response: noAlternative})), validAnswerRequestJSON())
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("missing alternative status = %d, body=%s", recorder.Code, recorder.Body)
	}

	tooManyAlternatives := validConflictingAnswerResponse()
	tooManyAlternatives.Conflicts = make([]models.AnswerCandidate, models.MaxExtractSources)
	for index := range tooManyAlternatives.Conflicts {
		tooManyAlternatives.Conflicts[index] = makeCandidate(`"alternative-`+string(rune('a'+index))+`"`, 2, 2)
	}
	recorder = performAnswerRequest(newAnswerTestRouter(Answer(&recordingAnswerService{response: tooManyAlternatives})), validAnswerRequestJSON())
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("too many alternatives status = %d, body=%s", recorder.Code, recorder.Body)
	}

	insufficientPageOverflow := &models.AnswerResponse{
		Status: models.AnswerStatusUnknown,
		Belief: nil,
		Reason: models.AnswerUnknownInsufficient,
		Needs:  &models.AnswerNeeds{MoreIndependentSources: 1},
		Closest: &models.AnswerClosest{
			Value:            json.RawMessage(`"winner"`),
			IndependentRoots: 2,
			Note:             "insufficient independent roots",
		},
		Conflicts: []models.AnswerCandidate{makeCandidate(`"other"`, 7, 1)},
	}
	requestWithMinimumThree := []byte(`{"spec":{"subject":"anthropic claude","predicate":"price","min_independent_sources":3}}`)
	recorder = performAnswerRequest(newAnswerTestRouter(Answer(&recordingAnswerService{response: insufficientPageOverflow})), requestWithMinimumThree)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("insufficient page overflow status = %d, body=%s", recorder.Code, recorder.Body)
	}
}

func TestAnswerFreezesEffectiveMinimumBeforeCallingService(t *testing.T) {
	service := &recordingAnswerService{answer: func(_ context.Context, request *models.AnswerRequest) (*models.AnswerResponse, error) {
		request.Spec.MinIndependentSources = 1
		request.Spec.Subject = "mutated by service"
		return validKnownAnswerResponse(), nil
	}}
	body := []byte(`{"spec":{"subject":"anthropic claude","predicate":"price","min_independent_sources":4}}`)
	recorder := performAnswerRequest(newAnswerTestRouter(Answer(service)), body)
	if recorder.Code != http.StatusInternalServerError || service.calls != 1 {
		t.Fatalf("status/calls = %d/%d, body=%s", recorder.Code, service.calls, recorder.Body)
	}
	response := decodeAnswerError(t, recorder)
	if response.Error == nil || response.Error.Code != models.ErrCodeInternal || response.Error.Message != "internal answer failure" {
		t.Fatalf("response = %#v", response)
	}
}

func TestAnswerMapsAndSanitizesStableErrors(t *testing.T) {
	const privateDetail = "credential=secret at /private/provider/path"
	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{name: "invalid", err: models.NewScrapeError(models.ErrCodeInvalidInput, privateDetail, nil), wantStatus: 400, wantCode: models.ErrCodeInvalidInput, wantMessage: "invalid answer request"},
		{name: "limited", err: models.NewScrapeError(models.ErrCodeRateLimited, privateDetail, nil), wantStatus: 429, wantCode: models.ErrCodeRateLimited, wantMessage: "answer rate limited"},
		{name: "failed", err: models.NewScrapeError(models.ErrCodeAnswerFailed, privateDetail, nil), wantStatus: 502, wantCode: models.ErrCodeAnswerFailed, wantMessage: "answer failed"},
		{name: "unavailable", err: models.NewScrapeError(models.ErrCodeAnswerUnavailable, privateDetail, nil), wantStatus: 503, wantCode: models.ErrCodeAnswerUnavailable, wantMessage: "answer is unavailable"},
		{name: "timeout", err: models.NewScrapeError(models.ErrCodeTimeout, privateDetail, nil), wantStatus: 504, wantCode: models.ErrCodeTimeout, wantMessage: "answer timed out"},
		{name: "internal", err: models.NewScrapeError(models.ErrCodeInternal, privateDetail, nil), wantStatus: 500, wantCode: models.ErrCodeInternal, wantMessage: "internal answer failure"},
		{name: "wrapped context", err: models.NewScrapeError(models.ErrCodeAnswerFailed, privateDetail, context.DeadlineExceeded), wantStatus: 504, wantCode: models.ErrCodeTimeout, wantMessage: "answer timed out"},
		{name: "unknown code", err: models.NewScrapeError("PRIVATE_PROVIDER_CODE", privateDetail, nil), wantStatus: 500, wantCode: models.ErrCodeInternal, wantMessage: "internal answer failure"},
		{name: "plain", err: errors.New(privateDetail), wantStatus: 500, wantCode: models.ErrCodeInternal, wantMessage: "internal answer failure"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &recordingAnswerService{err: test.err}
			recorder := performAnswerRequest(newAnswerTestRouter(Answer(service)), validAnswerRequestJSON())
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, test.wantStatus, recorder.Body)
			}
			response := decodeAnswerError(t, recorder)
			if response.Error == nil || response.Error.Code != test.wantCode || response.Error.Message != test.wantMessage {
				t.Fatalf("response = %#v", response)
			}
			if strings.Contains(recorder.Body.String(), "credential") || strings.Contains(recorder.Body.String(), "/private") ||
				strings.Contains(recorder.Body.String(), "PRIVATE_PROVIDER_CODE") {
				t.Fatalf("response leaked internal detail: %s", recorder.Body)
			}
		})
	}
}

func TestAnswerOuterDeadlineCoversServiceAndResponseEncoder(t *testing.T) {
	tests := []struct {
		name    string
		service AnswerService
	}{
		{name: "service", service: &recordingAnswerService{answer: func(ctx context.Context, _ *models.AnswerRequest) (*models.AnswerResponse, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}},
		{name: "encoder", service: &encodingAnswerService{
			recordingAnswerService: recordingAnswerService{response: validUnknownAnswerResponse()},
			encode: func(ctx context.Context, _ *models.AnswerResponse) ([]byte, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router := newAnswerTestRouter(Answer(test.service))
			requestContext, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
			defer cancel()
			request := httptest.NewRequest(http.MethodPost, "/answer", bytes.NewReader(validAnswerRequestJSON())).WithContext(requestContext)
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
			response := decodeAnswerError(t, recorder)
			if response.Error == nil || response.Error.Code != models.ErrCodeTimeout || response.Error.Message != "answer timed out" {
				t.Fatalf("response = %#v", response)
			}
		})
	}
}

func TestAnswerCustomEncoderMustReturnValidBoundedJSON(t *testing.T) {
	tests := []struct {
		name   string
		encode func(context.Context, *models.AnswerResponse) ([]byte, error)
	}{
		{name: "invalid JSON", encode: func(context.Context, *models.AnswerResponse) ([]byte, error) { return []byte(`{"status":`), nil }},
		{name: "empty", encode: func(context.Context, *models.AnswerResponse) ([]byte, error) { return nil, nil }},
		{name: "oversized", encode: func(context.Context, *models.AnswerResponse) ([]byte, error) {
			return bytes.Repeat([]byte(" "), models.MaxAnswerResponseBytes+1), nil
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &encodingAnswerService{
				recordingAnswerService: recordingAnswerService{response: validUnknownAnswerResponse()},
				encode:                 test.encode,
			}
			recorder := performAnswerRequest(newAnswerTestRouter(Answer(service)), validAnswerRequestJSON())
			if recorder.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body)
			}
		})
	}
}

func TestAnswerFallbackEncodingHasFourSlotsAndExactHardBoundary(t *testing.T) {
	if got := cap(fallbackAnswerEncodingSlots); got != 4 {
		t.Fatalf("fallback slot capacity = %d, want 4", got)
	}
	probe := &models.AnswerResponse{
		Status: models.AnswerStatusUnknown,
		Belief: nil,
		Reason: models.AnswerUnknownInsufficient,
		Needs:  &models.AnswerNeeds{MoreIndependentSources: 1},
		Closest: &models.AnswerClosest{
			Value:            json.RawMessage(`"v"`),
			IndependentRoots: 1,
			Note:             "a",
		},
	}
	probeBytes, err := json.Marshal(probe)
	if err != nil {
		t.Fatal(err)
	}
	overhead := len(probeBytes) - 1
	payload := models.MaxAnswerResponseBytes - overhead
	if payload < 1 {
		t.Fatalf("invalid fixture payload = %d", payload)
	}
	probe.Closest.Note = strings.Repeat("a", payload)
	encoded, err := encodeAnswerResponseFallback(context.Background(), probe)
	if err != nil {
		t.Fatalf("exact-limit encode: %v", err)
	}
	if len(encoded) != models.MaxAnswerResponseBytes {
		t.Fatalf("exact-limit bytes = %d, want %d", len(encoded), models.MaxAnswerResponseBytes)
	}
	encoded = nil
	probe.Closest.Note += "a"
	if _, err := encodeAnswerResponseFallback(context.Background(), probe); err == nil {
		t.Fatal("N+1 response was accepted")
	}
}

func TestAnswerFallbackSlotWaitHonorsContext(t *testing.T) {
	for index := 0; index < cap(fallbackAnswerEncodingSlots); index++ {
		fallbackAnswerEncodingSlots <- struct{}{}
	}
	defer func() {
		for index := 0; index < cap(fallbackAnswerEncodingSlots); index++ {
			<-fallbackAnswerEncodingSlots
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := encodeAnswerResponseFallback(ctx, validUnknownAnswerResponse())
	status, code, message := mapAnswerError(err)
	if status != http.StatusGatewayTimeout || code != models.ErrCodeTimeout || message != "answer timed out" {
		t.Fatalf("slot cancellation = %d %q %q, err=%v", status, code, message, err)
	}
}
