package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"reflect"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/publicnet"
	"golang.org/x/net/publicsuffix"
)

// AnswerService is the transport-neutral Search -> multi-source consensus
// boundary used by POST /api/v1/answer.
type AnswerService interface {
	Answer(context.Context, *models.AnswerRequest) (*models.AnswerResponse, error)
}

// AnswerResponseEncoder lets the production Answer service keep final JSON
// encoding within its own shared resource budget. Other implementations use
// the bounded four-slot fallback below.
type AnswerResponseEncoder interface {
	EncodeResponse(context.Context, *models.AnswerResponse) ([]byte, error)
}

// AnswerRateLimiter consumes one weighted charge from the router's shared
// per-identity limiter.
type AnswerRateLimiter interface {
	Allow(*gin.Context, int) bool
}

// MaxAnswerRequestCost is the fixed admission cost of a structurally valid
// Answer request. Invalid input consumes one token instead.
const (
	MaxAnswerRequestCost      = 17
	maximumAnswerValueBytes   = 64 << 10
	maximumAnswerQuoteBytes   = 8 << 10
	maximumAnswerSelector     = 4 << 10
	maximumAnswerReceiptBytes = 2 << 20
	maximumAnswerRootBytes    = 253
)

var fallbackAnswerEncodingSlots = make(chan struct{}, 4)

// Answer returns the HTTP adapter without an implicit limiter. The API router
// supplies its shared limiter through AnswerWithRateLimiter.
func Answer(service AnswerService) gin.HandlerFunc {
	return AnswerWithRateLimiter(service, nil)
}

// AnswerWithRateLimiter returns the bounded HTTP adapter for POST
// /api/v1/answer.
func AnswerWithRateLimiter(service AnswerService, limiter AnswerRateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if isNilAnswerService(service) {
			respondAnswerError(c, http.StatusServiceUnavailable, models.ErrCodeAnswerUnavailable, "answer is unavailable")
			return
		}

		request, err := decodeAnswerRequest(c)
		if err != nil {
			if limiter != nil && !limiter.Allow(c, 1) {
				respondAnswerError(c, http.StatusTooManyRequests, models.ErrCodeRateLimited, "answer rate limited")
				return
			}
			var maximumBytesError *http.MaxBytesError
			if errors.As(err, &maximumBytesError) {
				respondAnswerError(c, http.StatusRequestEntityTooLarge, models.ErrCodeInvalidInput, "answer request is too large")
				return
			}
			respondAnswerError(c, http.StatusBadRequest, models.ErrCodeInvalidInput, "invalid answer request")
			return
		}

		if limiter != nil && !limiter.Allow(c, MaxAnswerRequestCost) {
			respondAnswerError(c, http.StatusTooManyRequests, models.ErrCodeRateLimited, "answer rate limited")
			return
		}

		minimum := effectiveAnswerMinimum(request)
		serviceRequest := cloneAnswerRequest(request)
		timeoutSeconds := serviceRequest.Timeout
		if timeoutSeconds == 0 {
			timeoutSeconds = models.DefaultAnswerTimeoutSeconds
		}
		taskContext, cancel := context.WithTimeout(c.Request.Context(), time.Duration(timeoutSeconds)*time.Second)
		defer cancel()
		if err := taskContext.Err(); err != nil {
			respondAnswerError(c, http.StatusGatewayTimeout, models.ErrCodeTimeout, "answer timed out")
			return
		}

		response, err := service.Answer(taskContext, serviceRequest)
		if contextErr := taskContext.Err(); contextErr != nil {
			respondAnswerError(c, http.StatusGatewayTimeout, models.ErrCodeTimeout, "answer timed out")
			return
		}
		if err != nil {
			status, code, message := mapAnswerError(err)
			respondAnswerError(c, status, code, message)
			return
		}
		if response == nil || validateAnswerResponse(response, minimum) != nil {
			respondAnswerError(c, http.StatusInternalServerError, models.ErrCodeInternal, "internal answer failure")
			return
		}
		if err := taskContext.Err(); err != nil {
			respondAnswerError(c, http.StatusGatewayTimeout, models.ErrCodeTimeout, "answer timed out")
			return
		}

		encoded, err := encodeAnswerResponse(taskContext, service, response)
		if err != nil {
			status, code, message := mapAnswerError(err)
			respondAnswerError(c, status, code, message)
			return
		}
		if err := taskContext.Err(); err != nil {
			respondAnswerError(c, http.StatusGatewayTimeout, models.ErrCodeTimeout, "answer timed out")
			return
		}
		c.Data(http.StatusOK, "application/json; charset=utf-8", encoded)
	}
}

func decodeAnswerRequest(c *gin.Context) (*models.AnswerRequest, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, models.MaxAnswerRequestBytes)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(body) {
		return nil, errors.New("answer request is not valid UTF-8")
	}
	if err := rejectDuplicateAnswerObjectKeys(body); err != nil {
		return nil, err
	}
	if err := validateCanonicalAnswerRequestKeys(body); err != nil {
		return nil, err
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var request *models.AnswerRequest
	if err := decoder.Decode(&request); err != nil {
		return nil, err
	}
	if request == nil {
		return nil, errors.New("answer request must be a JSON object")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("answer request contains multiple JSON values")
		}
		return nil, err
	}
	if request.Timeout < 0 || request.Timeout > models.MaxAnswerTimeoutSeconds {
		return nil, errors.New("answer timeout is outside its public range")
	}
	if err := binding.Validator.ValidateStruct(request); err != nil {
		return nil, err
	}
	return request, nil
}

func validateCanonicalAnswerRequestKeys(body []byte) error {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(body, &document); err != nil {
		return err
	}
	for name := range document {
		switch name {
		case "spec", "timeout":
		default:
			return errors.New("answer request contains a non-canonical field name")
		}
	}
	rawSpec, exists := document["spec"]
	if !exists {
		return nil
	}
	var spec map[string]json.RawMessage
	if err := json.Unmarshal(rawSpec, &spec); err != nil {
		return err
	}
	for name := range spec {
		switch name {
		case "subject", "predicate", "freshness", "min_independent_sources", "on_conflict":
		default:
			return errors.New("answer fact specification contains a non-canonical field name")
		}
	}
	return nil
}

func effectiveAnswerMinimum(request *models.AnswerRequest) int {
	if request == nil || request.Spec.MinIndependentSources == 0 {
		return models.DefaultAnswerMinIndependentSources
	}
	return request.Spec.MinIndependentSources
}

func cloneAnswerRequest(source *models.AnswerRequest) *models.AnswerRequest {
	if source == nil {
		return nil
	}
	cloned := *source
	cloned.Spec = source.Spec
	cloned.Spec.Subject = strings.Clone(source.Spec.Subject)
	cloned.Spec.Predicate = strings.Clone(source.Spec.Predicate)
	cloned.Spec.Freshness = strings.Clone(source.Spec.Freshness)
	cloned.Spec.OnConflict = models.FactConflictPolicy(strings.Clone(string(source.Spec.OnConflict)))
	return &cloned
}

// rejectDuplicateAnswerObjectKeys walks the complete JSON value without
// materializing it. Duplicate names are rejected at every nesting level so a
// caller cannot smuggle a second fact specification past intermediary parsers.
func rejectDuplicateAnswerObjectKeys(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := scanAnswerJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("answer request contains multiple JSON values")
		}
		return err
	}
	return nil
}

func scanAnswerJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := nameToken.(string)
			if !ok {
				return errors.New("answer request contains an invalid object key")
			}
			if _, duplicate := seen[name]; duplicate {
				return errors.New("answer request contains a duplicate object key")
			}
			seen[name] = struct{}{}
			if err := scanAnswerJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			if err != nil {
				return err
			}
			return errors.New("answer request contains an invalid object")
		}
	case '[':
		for decoder.More() {
			if err := scanAnswerJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			if err != nil {
				return err
			}
			return errors.New("answer request contains an invalid array")
		}
	default:
		return errors.New("answer request contains an unexpected delimiter")
	}
	return nil
}

func validateAnswerResponse(response *models.AnswerResponse, minimum int) error {
	if response == nil {
		return errors.New("empty answer response")
	}
	if len(response.Conflicts) > models.MaxExtractSources {
		return errors.New("too many answer conflicts")
	}
	conflictValues := make(map[string]struct{}, len(response.Conflicts))
	for _, conflict := range response.Conflicts {
		if !validAnswerCandidate(conflict) {
			return errors.New("invalid answer conflict")
		}
		canonical, ok := canonicalAnswerValue(conflict.Value, true)
		if !ok {
			return errors.New("invalid answer conflict value")
		}
		if _, duplicate := conflictValues[canonical]; duplicate {
			return errors.New("duplicate answer conflict value")
		}
		conflictValues[canonical] = struct{}{}
	}
	if !orderedAnswerCandidates(response.Conflicts) {
		return errors.New("answer conflicts are out of order")
	}

	switch response.Status {
	case models.AnswerStatusKnown:
		if response.Belief == nil || response.Reason != "" || response.Needs != nil || response.Closest != nil || response.Lease == nil {
			return errors.New("invalid known answer shape")
		}
		if !validKnownAnswerBelief(response.Belief) || !validAnswerLease(response.Lease, response.Belief.AsOf) {
			return errors.New("invalid known answer")
		}
		if response.Belief.Agreement.IndependentRoots < minimum {
			return errors.New("known answer has insufficient independent support")
		}
		if len(response.Conflicts)+1 > models.MaxExtractSources {
			return errors.New("known answer contains too many value groups")
		}
		if !answerPageBudgetFits(response.Belief.Agreement.Pages, response.Conflicts) {
			return errors.New("known answer exceeds the mutually exclusive page budget")
		}
		winnerValue, _ := canonicalAnswerValue(response.Belief.Value, false)
		for _, conflict := range response.Conflicts {
			conflictValue, _ := canonicalAnswerValue(conflict.Value, true)
			if conflictValue == winnerValue || conflict.Agreement.IndependentRoots >= response.Belief.Agreement.IndependentRoots {
				return errors.New("invalid known answer conflicts")
			}
		}
	case models.AnswerStatusUnknown:
		if response.Belief != nil || response.Lease != nil || !validUnknownAnswerReason(response.Reason) ||
			response.Needs == nil || response.Needs.MoreIndependentSources < 1 ||
			response.Needs.MoreIndependentSources > models.MaxAnswerMinIndependentSources {
			return errors.New("invalid unknown answer shape")
		}
		if response.Closest != nil && !validAnswerClosest(response.Closest) {
			return errors.New("invalid closest answer")
		}
		roots := 0
		if response.Closest != nil {
			roots = response.Closest.IndependentRoots
		}
		expectedNeed := minimum - roots
		if expectedNeed < 1 {
			expectedNeed = 1
		}
		if response.Needs.MoreIndependentSources != expectedNeed {
			return errors.New("unknown answer need is inconsistent")
		}
		switch response.Reason {
		case models.AnswerUnknownNoSearchResults, models.AnswerUnknownNoValidSources, models.AnswerUnknownMissingValue:
			if response.Closest != nil || len(response.Conflicts) != 0 {
				return errors.New("empty-source answer contains stray candidates")
			}
		case models.AnswerUnknownInsufficient:
			if response.Closest == nil || response.Closest.Note != "insufficient independent roots" ||
				response.Closest.IndependentRoots >= minimum || len(response.Conflicts)+1 > models.MaxExtractSources ||
				!answerPageBudgetFits(response.Closest.IndependentRoots, response.Conflicts) {
				return errors.New("invalid insufficient answer")
			}
			closestValue, _ := canonicalAnswerValue(response.Closest.Value, false)
			for _, conflict := range response.Conflicts {
				conflictValue, _ := canonicalAnswerValue(conflict.Value, true)
				if conflictValue == closestValue || conflict.Agreement.IndependentRoots >= response.Closest.IndependentRoots {
					return errors.New("invalid insufficient answer conflicts")
				}
			}
		case models.AnswerUnknownConflict:
			if len(response.Conflicts) < 1 || len(response.Conflicts)+1 > models.MaxExtractSources ||
				response.Closest == nil || response.Closest.Note != "independent-root tie" ||
				!answerPageBudgetFits(response.Closest.IndependentRoots, response.Conflicts) ||
				response.Conflicts[0].Agreement.IndependentRoots != response.Closest.IndependentRoots {
				return errors.New("invalid conflicting answer")
			}
			closestValue, _ := canonicalAnswerValue(response.Closest.Value, false)
			for _, conflict := range response.Conflicts {
				conflictValue, _ := canonicalAnswerValue(conflict.Value, true)
				if conflictValue == closestValue {
					return errors.New("conflicting answer repeats its closest value")
				}
			}
		}
	default:
		return errors.New("invalid answer status")
	}
	return nil
}

func validKnownAnswerBelief(belief *models.AnswerBelief) bool {
	if belief == nil || !validAnswerStringValue(belief.Value, false) || !validAnswerAgreement(belief.Agreement) ||
		!validAnswerTime(belief.AsOf) || len(belief.Evidence) != belief.Agreement.Pages ||
		len(belief.Receipts) != len(belief.Evidence) {
		return false
	}
	switch belief.Confidence {
	case models.AnswerConfidenceLow, models.AnswerConfidenceMedium, models.AnswerConfidenceHigh:
	default:
		return false
	}
	if belief.Confidence != answerConfidenceForRoots(belief.Agreement.IndependentRoots) {
		return false
	}
	seen := make(map[string]struct{}, len(belief.Evidence))
	roots := make(map[string]struct{}, len(belief.Evidence))
	for _, item := range belief.Evidence {
		if !validAnswerEvidence(item, belief.AsOf) {
			return false
		}
		if _, duplicate := seen[item.URL]; duplicate {
			return false
		}
		seen[item.URL] = struct{}{}
		roots[item.Root] = struct{}{}
		receipt, exists := belief.Receipts[item.URL]
		if !exists || !validAnswerReceipt(receipt) {
			return false
		}
	}
	return belief.Agreement.IndependentRoots <= len(roots)
}

func validAnswerEvidence(item models.AnswerEvidence, asOf time.Time) bool {
	if len(item.URL) == 0 || len(item.URL) > models.MaxExtractSourceURLBytes {
		return false
	}
	canonical, parsed, err := publicnet.NormalizeHTTPURL(item.URL, nil, false)
	if err != nil || parsed == nil || parsed.User != nil || canonical != item.URL {
		return false
	}
	root, err := authoritativeAnswerRoot(parsed.Hostname())
	if err != nil || item.Root != root || len(item.Root) > maximumAnswerRootBytes ||
		item.Quote == "" || len(item.Quote) > maximumAnswerQuoteBytes ||
		!utf8.ValidString(item.Quote) || containsAnswerControl(item.Quote) ||
		len(item.Selector) > maximumAnswerSelector || !utf8.ValidString(item.Selector) || containsAnswerControl(item.Selector) ||
		!validAnswerSnapshotID(item.SnapshotID) || !validAnswerTime(item.FetchedAt) || item.FetchedAt.After(asOf) {
		return false
	}
	start, end := item.TextRange[0], item.TextRange[1]
	if start < 0 || end <= start || end-start != len(item.Quote) {
		return false
	}
	switch item.Method {
	case evidence.MethodExact, evidence.MethodNormalized, evidence.MethodFuzzy, evidence.MethodCompiled:
		return true
	default:
		return false
	}
}

func authoritativeAnswerRoot(hostname string) (string, error) {
	hostname = strings.ToLower(hostname)
	if address, err := netip.ParseAddr(hostname); err == nil {
		return address.Unmap().String(), nil
	}
	root, err := publicsuffix.EffectiveTLDPlusOne(hostname)
	if err != nil {
		return "", err
	}
	return strings.ToLower(root), nil
}

func validAnswerSnapshotID(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, character := range value[len("sha256:"):] {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}

func validAnswerLease(lease *models.AnswerLease, asOf time.Time) bool {
	return lease != nil && validAnswerTime(lease.ExpiresAt) &&
		lease.ExpiresAt.Equal(asOf.Add(time.Duration(models.DefaultAnswerLeaseSeconds)*time.Second)) &&
		lease.RenewURL == models.DefaultAnswerRenewURL &&
		lease.ConfidenceHalflife == models.DefaultAnswerConfidenceHalflifeSeconds
}

func validAnswerClosest(closest *models.AnswerClosest) bool {
	return closest != nil && validAnswerStringValue(closest.Value, false) &&
		closest.IndependentRoots >= 1 && closest.IndependentRoots <= models.MaxExtractSources &&
		closest.Note != "" && utf8.ValidString(closest.Note) && !containsAnswerControl(closest.Note)
}

func validAnswerCandidate(candidate models.AnswerCandidate) bool {
	return validAnswerStringValue(candidate.Value, true) && validAnswerAgreement(candidate.Agreement)
}

func orderedAnswerCandidates(candidates []models.AnswerCandidate) bool {
	previousRoots := models.MaxExtractSources + 1
	previousPages := models.MaxExtractSources + 1
	for _, candidate := range candidates {
		if candidate.Agreement.IndependentRoots > previousRoots ||
			candidate.Agreement.IndependentRoots == previousRoots && candidate.Agreement.Pages > previousPages {
			return false
		}
		previousRoots = candidate.Agreement.IndependentRoots
		previousPages = candidate.Agreement.Pages
	}
	return true
}

func answerPageBudgetFits(initial int, candidates []models.AnswerCandidate) bool {
	if initial < 0 || initial > models.MaxExtractSources {
		return false
	}
	used := initial
	for _, candidate := range candidates {
		if candidate.Agreement.Pages < 0 || candidate.Agreement.Pages > models.MaxExtractSources-used {
			return false
		}
		used += candidate.Agreement.Pages
	}
	return true
}

func validAnswerAgreement(agreement models.MultiExtractAgreement) bool {
	return agreement.Pages >= 1 && agreement.Pages <= models.MaxExtractSources &&
		agreement.IndependentRoots >= 1 && agreement.IndependentRoots <= agreement.Pages &&
		(agreement.Pages != agreement.IndependentRoots || agreement.FoldReason == "") &&
		validAnswerFoldReason(agreement.FoldReason)
}

func validAnswerFoldReason(reason models.MultiExtractFoldReason) bool {
	switch reason {
	case "", models.MultiExtractFoldReasonSameRoot, models.MultiExtractFoldReasonNearDuplicate,
		models.MultiExtractFoldReasonQuoteLineage:
		return true
	default:
		return false
	}
}

func validAnswerStringValue(raw json.RawMessage, allowNull bool) bool {
	_, ok := canonicalAnswerValue(raw, allowNull)
	return ok
}

func canonicalAnswerValue(raw json.RawMessage, allowNull bool) (string, bool) {
	if len(raw) == 0 || len(raw) > maximumAnswerValueBytes || !utf8.Valid(raw) {
		return "", false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return "", false
	}
	switch value.(type) {
	case string:
		encoded, err := json.Marshal(value)
		if err != nil || len(encoded) > maximumAnswerValueBytes {
			return "", false
		}
		return string(encoded), true
	case nil:
		return "null", allowNull
	default:
		return "", false
	}
}

func answerConfidenceForRoots(independentRoots int) models.AnswerConfidence {
	switch {
	case independentRoots >= 3:
		return models.AnswerConfidenceHigh
	case independentRoots == 2:
		return models.AnswerConfidenceMedium
	default:
		return models.AnswerConfidenceLow
	}
}

func validAnswerReceipt(receipt string) bool {
	if receipt == "" || len(receipt) > maximumAnswerReceiptBytes || !utf8.ValidString(receipt) {
		return false
	}
	for index := range len(receipt) {
		character := receipt[index]
		if character == '.' || character == '-' || character == '_' ||
			character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return true
}

func validUnknownAnswerReason(reason models.AnswerUnknownReason) bool {
	switch reason {
	case models.AnswerUnknownNoSearchResults, models.AnswerUnknownNoValidSources,
		models.AnswerUnknownMissingValue, models.AnswerUnknownInsufficient,
		models.AnswerUnknownConflict:
		return true
	default:
		return false
	}
}

func validAnswerTime(value time.Time) bool {
	if value.IsZero() || value.Location() != time.UTC {
		return false
	}
	_, err := value.MarshalJSON()
	return err == nil
}

func containsAnswerControl(value string) bool {
	if !utf8.ValidString(value) {
		return true
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}

func encodeAnswerResponse(ctx context.Context, service AnswerService, response *models.AnswerResponse) ([]byte, error) {
	if encoder, ok := service.(AnswerResponseEncoder); ok {
		if err := ctx.Err(); err != nil {
			return nil, answerEncodingTimeout(err)
		}
		encoded, err := encoder.EncodeResponse(ctx, response)
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, answerEncodingTimeout(contextErr)
		}
		if err != nil {
			return nil, err
		}
		if len(encoded) == 0 || len(encoded) > models.MaxAnswerResponseBytes {
			return nil, errors.New("answer response exceeds its output budget")
		}
		if !json.Valid(encoded) {
			return nil, errors.New("answer response encoder returned invalid JSON")
		}
		if err := ctx.Err(); err != nil {
			return nil, answerEncodingTimeout(err)
		}
		return encoded, nil
	}
	return encodeAnswerResponseFallback(ctx, response)
}

func encodeAnswerResponseFallback(ctx context.Context, response *models.AnswerResponse) ([]byte, error) {
	select {
	case fallbackAnswerEncodingSlots <- struct{}{}:
		defer func() { <-fallbackAnswerEncodingSlots }()
	case <-ctx.Done():
		return nil, answerEncodingTimeout(ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return nil, answerEncodingTimeout(err)
	}
	preflightOK, err := preflightAnswerResponseSize(ctx, response)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, answerEncodingTimeout(contextErr)
		}
		return nil, err
	}
	if !preflightOK {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, answerEncodingTimeout(contextErr)
		}
		return nil, errors.New("answer response exceeds its output budget")
	}
	if err := ctx.Err(); err != nil {
		return nil, answerEncodingTimeout(err)
	}
	encoded, err := json.Marshal(response)
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, answerEncodingTimeout(contextErr)
	}
	if err != nil {
		return nil, err
	}
	if len(encoded) > models.MaxAnswerResponseBytes {
		return nil, errors.New("answer response exceeds its output budget")
	}
	return encoded, nil
}

func preflightAnswerResponseSize(ctx context.Context, response *models.AnswerResponse) (bool, error) {
	if response == nil {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	remaining := models.MaxAnswerResponseBytes
	if !reserveAnswerResponseBytes(&remaining, 2) {
		return false, nil
	}
	fields := 0
	var preflightErr error
	addField := func(name string, value any) bool {
		if err := ctx.Err(); err != nil {
			preflightErr = err
			return false
		}
		encodedName, err := json.Marshal(name)
		if err != nil {
			preflightErr = err
			return false
		}
		encodedValue, err := json.Marshal(value)
		if err != nil {
			preflightErr = err
			return false
		}
		if err := ctx.Err(); err != nil {
			preflightErr = err
			return false
		}
		if fields > 0 && !reserveAnswerResponseBytes(&remaining, 1) {
			return false
		}
		if !reserveAnswerResponseBytes(&remaining, len(encodedName)) ||
			!reserveAnswerResponseBytes(&remaining, 1) ||
			!reserveAnswerResponseBytes(&remaining, len(encodedValue)) {
			return false
		}
		fields++
		return true
	}

	valid := addField("status", response.Status) && addField("belief", response.Belief)
	if response.Reason != "" {
		valid = valid && addField("reason", response.Reason)
	}
	if response.Needs != nil {
		valid = valid && addField("needs", response.Needs)
	}
	if response.Closest != nil {
		valid = valid && addField("closest", response.Closest)
	}
	if len(response.Conflicts) > 0 {
		valid = valid && addField("conflicts", response.Conflicts)
	}
	if response.Lease != nil {
		valid = valid && addField("lease", response.Lease)
	}
	if preflightErr != nil {
		return false, preflightErr
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return valid, nil
}

func reserveAnswerResponseBytes(remaining *int, requested int) bool {
	if remaining == nil || requested < 0 || requested > *remaining {
		return false
	}
	*remaining -= requested
	return true
}

func answerEncodingTimeout(cause error) error {
	return models.NewScrapeError(models.ErrCodeTimeout, "answer timed out", cause)
}

func respondAnswerError(c *gin.Context, status int, code, message string) {
	c.JSON(status, models.AnswerErrorResponse{
		Error: &models.ErrorDetail{Code: code, Message: message},
	})
}

func mapAnswerError(err error) (int, string, string) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, models.ErrCodeTimeout, "answer timed out"
	}
	var scrapeError *models.ScrapeError
	if errors.As(err, &scrapeError) {
		switch scrapeError.Code {
		case models.ErrCodeInvalidInput:
			return http.StatusBadRequest, models.ErrCodeInvalidInput, "invalid answer request"
		case models.ErrCodeRateLimited:
			return http.StatusTooManyRequests, models.ErrCodeRateLimited, "answer rate limited"
		case models.ErrCodeAnswerFailed:
			return http.StatusBadGateway, models.ErrCodeAnswerFailed, "answer failed"
		case models.ErrCodeAnswerUnavailable:
			return http.StatusServiceUnavailable, models.ErrCodeAnswerUnavailable, "answer is unavailable"
		case models.ErrCodeTimeout:
			return http.StatusGatewayTimeout, models.ErrCodeTimeout, "answer timed out"
		case models.ErrCodeInternal:
			return http.StatusInternalServerError, models.ErrCodeInternal, "internal answer failure"
		}
	}
	return http.StatusInternalServerError, models.ErrCodeInternal, "internal answer failure"
}

func isNilAnswerService(service AnswerService) bool {
	if service == nil {
		return true
	}
	value := reflect.ValueOf(service)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
