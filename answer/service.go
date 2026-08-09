// Package answer composes fresh Search with multi-source extraction and
// consensus. It never derives or recomputes independence: the extraction
// service remains the sole authority for IndependentRoots.
package answer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"reflect"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/publicnet"
	"github.com/use-agent/purify/search"
	"golang.org/x/net/publicsuffix"
)

const (
	answerSearchLimit       = models.MaxExtractSources
	answerEncodingSlots     = 4
	maximumAnswerValueBytes = 64 << 10
	maximumAnswerQuoteBytes = 8 << 10
	maximumAnswerSelector   = 4 << 10
	maximumAnswerSnapshotID = 512
	maximumAnswerReceipt    = 2 << 20
	maximumAnswerOutput     = models.MaxAnswerResponseBytes
)

// Searcher is the trusted internal Search boundary. The RunOptions parameter
// is part of the interface so Answer cannot accidentally use a cached baseline.
type Searcher interface {
	SearchWithOptions(context.Context, *models.SearchRequest, search.RunOptions) (*models.SearchResponse, error)
}

// MultiExtractor is the existing evidence-required multi-source extraction
// boundary. Its consensus projection already contains independence accounting.
type MultiExtractor interface {
	ExtractMulti(context.Context, *models.ExtractRequest) (*models.MultiExtractResponse, error)
}

// Service owns the pure Search -> ExtractMulti -> belief orchestration.
type Service struct {
	searcher      Searcher
	extractor     MultiExtractor
	now           func() time.Time
	encodingSlots chan struct{}
}

// NewService constructs an Answer service without changing either dependency's
// lifecycle or configuration.
func NewService(searcher Searcher, extractor MultiExtractor) (*Service, error) {
	if isNilDependency(searcher) || isNilDependency(extractor) {
		return nil, errors.New("answer: search and multi-source extraction are required")
	}
	return &Service{
		searcher:      searcher,
		extractor:     extractor,
		now:           time.Now,
		encodingSlots: make(chan struct{}, answerEncodingSlots),
	}, nil
}

// Answer obtains a fresh provider baseline, extracts one evidence-backed fact
// from up to eight pages, and returns either a belief or an honest unknown.
// Caller-owned input and dependency-owned output are never retained.
func (service *Service) Answer(ctx context.Context, request *models.AnswerRequest) (*models.AnswerResponse, error) {
	if ctx == nil {
		return nil, invalidAnswer("answer context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, answerTimeout()
	}
	if service == nil || isNilDependency(service.searcher) || isNilDependency(service.extractor) || service.now == nil || service.encodingSlots == nil {
		return nil, answerUnavailable()
	}

	prepared, err := prepareRequest(request)
	if err != nil {
		return nil, err
	}
	answerCtx, cancel := context.WithTimeout(ctx, time.Duration(prepared.timeoutSeconds)*time.Second)
	defer cancel()
	if err := answerCtx.Err(); err != nil {
		return nil, answerTimeout()
	}
	deduplicate := true
	searchRequest := &models.SearchRequest{
		Query:       prepared.subject + " " + prepared.predicate,
		Limit:       answerSearchLimit,
		Freshness:   prepared.freshness,
		Deduplicate: &deduplicate,
		Timeout:     prepared.timeoutSeconds,
	}
	searchResponse, searchErr := service.searcher.SearchWithOptions(answerCtx, searchRequest, search.RunOptions{
		BypassProviderCache: true,
	})
	if err := answerCtx.Err(); err != nil {
		return nil, answerTimeout()
	}
	if searchErr != nil {
		return nil, sanitizeSearchError(searchErr)
	}
	if searchResponse == nil || !searchResponse.Success || searchResponse.Error != nil || searchResponse.Query != searchRequest.Query ||
		len(searchResponse.Results) > searchRequest.Limit {
		return nil, answerFailed("answer search returned an invalid response")
	}

	sources, err := answerSources(searchResponse.Results)
	if err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		response := unknownResponse(models.AnswerUnknownNoSearchResults, prepared.minimum, nil, nil)
		if err := answerCtx.Err(); err != nil {
			return nil, answerTimeout()
		}
		return response, nil
	}
	if err := answerCtx.Err(); err != nil {
		return nil, answerTimeout()
	}

	extractRequest := &models.ExtractRequest{
		Sources:  sources,
		Schema:   append(json.RawMessage(nil), prepared.schema...),
		Engine:   "auto",
		Evidence: true,
		Timeout:  prepared.timeoutSeconds,
	}
	extractResponse, extractErr := service.extractor.ExtractMulti(answerCtx, extractRequest)
	if err := answerCtx.Err(); err != nil {
		return nil, answerTimeout()
	}
	if extractResponse != nil && !extractResponse.Success && extractResponse.Error != nil &&
		extractResponse.Error.Code == models.ErrCodeNoValidSource {
		if extractErr == nil || scrapeErrorCode(extractErr) != models.ErrCodeNoValidSource {
			return nil, answerFailed("answer extraction returned inconsistent no-source results")
		}
		response := unknownResponse(models.AnswerUnknownNoValidSources, prepared.minimum, nil, nil)
		if err := answerCtx.Err(); err != nil {
			return nil, answerTimeout()
		}
		return response, nil
	}
	if extractErr != nil {
		return nil, sanitizeExtractError(extractErr)
	}
	if extractResponse == nil || !extractResponse.Success || extractResponse.Error != nil || extractResponse.Consensus == nil {
		return nil, answerFailed("answer extraction returned an invalid response")
	}
	allowedSupports, err := allowedConsensusSupports(sources, extractResponse.Sources)
	if err != nil {
		return nil, err
	}
	if err := validateAggregateShape(answerCtx, extractResponse, prepared.predicate, prepared.consensusPath); err != nil {
		return nil, err
	}

	field, exists := extractResponse.Consensus.Fields[prepared.consensusPath]
	if !exists {
		response := unknownResponse(models.AnswerUnknownMissingValue, prepared.minimum, nil, nil)
		if err := answerCtx.Err(); err != nil {
			return nil, answerTimeout()
		}
		return response, nil
	}
	decision, err := decide(answerCtx, field, prepared.minimum, allowedSupports)
	if err != nil {
		return nil, err
	}
	if decision.reason != "" {
		response := unknownResponse(decision.reason, prepared.minimum, decision.closest, decision.conflicts)
		if err := answerCtx.Err(); err != nil {
			return nil, answerTimeout()
		}
		return response, nil
	}
	if err := answerCtx.Err(); err != nil {
		return nil, answerTimeout()
	}

	belief, err := service.buildBelief(answerCtx, decision.value, decision.agreement, decision.supports, allowedSupports)
	if err != nil {
		return nil, err
	}
	expiresAt, err := normalizePublicTime(belief.AsOf.Add(models.DefaultAnswerLeaseSeconds * time.Second))
	if err != nil {
		return nil, answerFailed("answer lease time is invalid")
	}
	response := &models.AnswerResponse{
		Status:    models.AnswerStatusKnown,
		Belief:    belief,
		Conflicts: cloneCandidates(decision.conflicts),
		Lease: &models.AnswerLease{
			ExpiresAt:          expiresAt,
			RenewURL:           models.DefaultAnswerRenewURL,
			ConfidenceHalflife: models.DefaultAnswerConfidenceHalflifeSeconds,
		},
	}
	if err := answerCtx.Err(); err != nil {
		return nil, answerTimeout()
	}
	return response, nil
}

// EncodeResponse performs bounded JSON encoding for HTTP and other transport
// adapters. It accepts only the two valid successful Answer shapes and never
// returns bytes completed after the caller's deadline.
func (service *Service) EncodeResponse(ctx context.Context, response *models.AnswerResponse) ([]byte, error) {
	if ctx == nil {
		return nil, invalidAnswer("answer encoding context is required")
	}
	if service == nil || service.encodingSlots == nil {
		return nil, answerUnavailable()
	}
	if err := ctx.Err(); err != nil {
		return nil, answerTimeout()
	}
	select {
	case service.encodingSlots <- struct{}{}:
		defer func() { <-service.encodingSlots }()
	case <-ctx.Done():
		return nil, answerTimeout()
	}
	if err := validateAnswerProjection(ctx, response); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(response)
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, answerTimeout()
	}
	if err != nil || len(encoded) > models.MaxAnswerResponseBytes {
		return nil, answerFailed("answer response exceeds its output budget")
	}
	return encoded, nil
}

func validateAnswerProjection(ctx context.Context, response *models.AnswerResponse) error {
	if response == nil || len(response.Conflicts) > models.MaxExtractSources {
		return answerFailed("answer response is invalid")
	}
	canonicalConflicts, conflictPages, err := validateCandidateProjections(ctx, response.Conflicts)
	if err != nil {
		return err
	}
	switch response.Status {
	case models.AnswerStatusKnown:
		if response.Belief == nil || response.Lease == nil || response.Reason != "" || response.Needs != nil || response.Closest != nil {
			return answerFailed("known answer response is invalid")
		}
		belief := response.Belief
		value, isNull, err := cloneScalar(ctx, belief.Value)
		if err != nil || isNull || len(value) == 0 || validateAgreement(belief.Agreement) != nil ||
			belief.Confidence != confidenceFor(belief.Agreement.IndependentRoots) ||
			len(belief.Evidence) != belief.Agreement.Pages || len(belief.Evidence) > models.MaxExtractSources ||
			len(belief.Receipts) != len(belief.Evidence) {
			return answerFailed("known answer belief is invalid")
		}
		if belief.Agreement.Pages > models.MaxExtractSources-conflictPages || len(response.Conflicts)+1 > models.MaxExtractSources {
			return answerFailed("known answer candidate totals are invalid")
		}
		for index, conflict := range response.Conflicts {
			if conflict.Agreement.IndependentRoots >= belief.Agreement.IndependentRoots {
				return answerFailed("known answer exposes a non-losing conflict")
			}
			if bytes.Equal(canonicalConflicts[index], value) {
				return answerFailed("known answer repeats its winner as a conflict")
			}
		}
		asOf, err := normalizePublicTime(belief.AsOf)
		if err != nil || belief.AsOf.Location() != time.UTC || !asOf.Equal(belief.AsOf) {
			return answerFailed("answer belief time is invalid")
		}
		seen := make(map[string]struct{}, len(belief.Evidence))
		distinctRoots := make(map[string]struct{}, len(belief.Evidence))
		for _, item := range belief.Evidence {
			if err := ctx.Err(); err != nil {
				return answerTimeout()
			}
			if len(item.URL) == 0 || len(item.URL) > models.MaxExtractSourceURLBytes ||
				len(item.Root) == 0 || len(item.Root) > models.MaxSearchDomainBytes ||
				item.Quote == "" || len(item.Quote) > maximumAnswerQuoteBytes || len(item.Selector) > maximumAnswerSelector ||
				!validSnapshotID(item.SnapshotID) || !validAnchorMethod(item.Method) ||
				!validText(item.Quote) || !validText(item.Selector) {
				return answerFailed("answer evidence projection is invalid")
			}
			canonical, parsed, err := publicnet.NormalizeHTTPURL(item.URL, nil, false)
			hostname := ""
			if parsed != nil {
				hostname = parsed.Hostname()
			}
			root, rootErr := supportRoot(hostname)
			fetchedAt, timeErr := normalizePublicTime(item.FetchedAt)
			start, end := item.TextRange[0], item.TextRange[1]
			if err != nil || canonical != item.URL || rootErr != nil || root != item.Root || timeErr != nil ||
				item.FetchedAt.Location() != time.UTC || !fetchedAt.Equal(item.FetchedAt) || fetchedAt.After(asOf) ||
				start < 0 || end <= start || end-start != len(item.Quote) {
				return answerFailed("answer evidence projection is invalid")
			}
			if _, duplicate := seen[canonical]; duplicate {
				return answerFailed("answer evidence projection is duplicated")
			}
			seen[canonical] = struct{}{}
			distinctRoots[item.Root] = struct{}{}
			receipt, exists := belief.Receipts[canonical]
			if !exists || len(receipt) == 0 || len(receipt) > maximumAnswerReceipt || !validReceipt(receipt) {
				return answerFailed("answer receipt projection is invalid")
			}
		}
		if belief.Agreement.IndependentRoots > len(distinctRoots) {
			return answerFailed("answer belief overstates independent evidence roots")
		}
		expiresAt, err := normalizePublicTime(response.Lease.ExpiresAt)
		if err != nil || response.Lease.ExpiresAt.Location() != time.UTC || !expiresAt.Equal(response.Lease.ExpiresAt) ||
			!expiresAt.Equal(asOf.Add(models.DefaultAnswerLeaseSeconds*time.Second)) || response.Lease.RenewURL != models.DefaultAnswerRenewURL ||
			response.Lease.ConfidenceHalflife != models.DefaultAnswerConfidenceHalflifeSeconds {
			return answerFailed("answer lease projection is invalid")
		}
	case models.AnswerStatusUnknown:
		if response.Belief != nil || response.Lease != nil || response.Needs == nil ||
			response.Needs.MoreIndependentSources < 1 || response.Needs.MoreIndependentSources > models.MaxExtractSources ||
			!validUnknownReason(response.Reason) {
			return answerFailed("unknown answer response is invalid")
		}
		var closestValue json.RawMessage
		if response.Closest != nil {
			value, isNull, err := cloneScalar(ctx, response.Closest.Value)
			if err != nil || isNull || len(value) == 0 || response.Closest.IndependentRoots < 1 ||
				response.Closest.IndependentRoots > models.MaxExtractSources || response.Closest.Note == "" || len(response.Closest.Note) > 128 ||
				!validText(response.Closest.Note) {
				return answerFailed("answer closest projection is invalid")
			}
			closestValue = value
		}
		switch response.Reason {
		case models.AnswerUnknownNoSearchResults, models.AnswerUnknownNoValidSources, models.AnswerUnknownMissingValue:
			if response.Closest != nil || len(response.Conflicts) != 0 {
				return answerFailed("answer unknown reason has stray candidate data")
			}
		case models.AnswerUnknownInsufficient:
			if response.Closest == nil || len(response.Conflicts)+1 > models.MaxExtractSources ||
				response.Closest.IndependentRoots > models.MaxExtractSources-conflictPages {
				return answerFailed("insufficient answer response lacks a closest candidate")
			}
			for index, conflict := range response.Conflicts {
				if conflict.Agreement.IndependentRoots >= response.Closest.IndependentRoots {
					return answerFailed("insufficient answer response exposes a non-losing conflict")
				}
				if bytes.Equal(canonicalConflicts[index], closestValue) {
					return answerFailed("insufficient answer repeats its closest value as a conflict")
				}
			}
		case models.AnswerUnknownConflict:
			if response.Closest == nil || len(response.Conflicts) < 1 || len(response.Conflicts)+1 > models.MaxExtractSources ||
				response.Closest.IndependentRoots > models.MaxExtractSources-conflictPages {
				return answerFailed("conflicting answer response lacks a resolvable tie")
			}
			if err := validateProjectedConflictTie(response.Closest, closestValue, response.Conflicts, canonicalConflicts); err != nil {
				return err
			}
		}
	default:
		return answerFailed("answer response status is invalid")
	}
	return nil
}

func validateCandidateProjections(
	ctx context.Context,
	candidates []models.AnswerCandidate,
) ([]json.RawMessage, int, error) {
	canonical := make([]json.RawMessage, 0, len(candidates))
	seenValues := make(map[string]struct{}, len(candidates))
	totalPages := 0
	for index, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, 0, answerTimeout()
		}
		// JSON null is allowed only as an explicitly exposed competing
		// candidate. Winner and closest validation both reject it.
		value, _, err := cloneScalar(ctx, candidate.Value)
		if err != nil || len(value) == 0 || validateAgreement(candidate.Agreement) != nil {
			return nil, 0, answerFailed("answer conflict projection is invalid")
		}
		if index > 0 {
			previous := candidates[index-1].Agreement
			current := candidate.Agreement
			if previous.IndependentRoots < current.IndependentRoots ||
				previous.IndependentRoots == current.IndependentRoots && previous.Pages < current.Pages {
				return nil, 0, answerFailed("answer conflict projection is out of order")
			}
		}
		if _, duplicate := seenValues[string(value)]; duplicate {
			return nil, 0, answerFailed("answer conflict projection repeats a canonical value")
		}
		seenValues[string(value)] = struct{}{}
		if candidate.Agreement.Pages > models.MaxExtractSources-totalPages {
			return nil, 0, answerFailed("answer conflict page totals exceed their resource limit")
		}
		totalPages += candidate.Agreement.Pages
		canonical = append(canonical, value)
	}
	return canonical, totalPages, nil
}

func validateProjectedConflictTie(
	closest *models.AnswerClosest,
	closestValue json.RawMessage,
	candidates []models.AnswerCandidate,
	canonicalCandidates []json.RawMessage,
) error {
	if closest == nil || len(candidates) < 1 || len(candidates) != len(canonicalCandidates) ||
		closest.IndependentRoots != candidates[0].Agreement.IndependentRoots {
		return answerFailed("answer conflict projection lacks a highest independent-root tie")
	}
	for index := range candidates {
		if bytes.Equal(canonicalCandidates[index], closestValue) {
			return answerFailed("answer closest value is repeated as a conflict")
		}
	}
	return nil
}

func validUnknownReason(reason models.AnswerUnknownReason) bool {
	switch reason {
	case models.AnswerUnknownNoSearchResults, models.AnswerUnknownNoValidSources, models.AnswerUnknownMissingValue,
		models.AnswerUnknownInsufficient, models.AnswerUnknownConflict:
		return true
	default:
		return false
	}
}

type preparedRequest struct {
	subject        string
	predicate      string
	freshness      string
	minimum        int
	timeoutSeconds int
	consensusPath  string
	schema         json.RawMessage
}

func prepareRequest(source *models.AnswerRequest) (preparedRequest, error) {
	if source == nil {
		return preparedRequest{}, invalidAnswer("answer request is required")
	}
	request := *source
	request.Defaults()
	spec := request.Spec

	if len(spec.Subject) > models.MaxAnswerSubjectBytes || !utf8.ValidString(spec.Subject) ||
		utf8.RuneCountInString(spec.Subject) > models.MaxAnswerSubjectRunes || containsControl(spec.Subject) {
		return preparedRequest{}, invalidAnswer("answer subject is invalid or exceeds its resource limit")
	}
	subject := strings.Join(strings.Fields(strings.TrimSpace(spec.Subject)), " ")
	if subject == "" || utf8.RuneCountInString(subject) > models.MaxAnswerSubjectRunes ||
		len(strings.Fields(subject)) > models.MaxAnswerSubjectWords {
		return preparedRequest{}, invalidAnswer("answer subject is invalid or exceeds its resource limit")
	}
	if len(spec.Predicate) > models.MaxAnswerPredicateBytes || !utf8.ValidString(spec.Predicate) || containsControl(spec.Predicate) || containsSpace(spec.Predicate) {
		return preparedRequest{}, invalidAnswer("answer predicate is invalid or exceeds its resource limit")
	}
	predicate := spec.Predicate
	if predicate == "" {
		return preparedRequest{}, invalidAnswer("answer predicate is invalid or exceeds its resource limit")
	}
	if spec.MinIndependentSources < 1 || spec.MinIndependentSources > models.MaxAnswerMinIndependentSources {
		return preparedRequest{}, invalidAnswer("min_independent_sources must be between 1 and 8")
	}
	if spec.OnConflict != models.FactConflictExpose {
		return preparedRequest{}, invalidAnswer("on_conflict must be expose")
	}
	if len(spec.Freshness) > models.MaxAnswerFreshnessBytes || strings.TrimSpace(spec.Freshness) == "" {
		return preparedRequest{}, invalidAnswer("answer freshness must be day, 1d, week, 7d, month, or year")
	}
	if _, err := search.ParseFreshness(spec.Freshness); err != nil {
		return preparedRequest{}, invalidAnswer("answer freshness must be day, 1d, week, 7d, month, or year")
	}
	freshness := strings.ToLower(strings.TrimSpace(spec.Freshness))
	queryRunes := utf8.RuneCountInString(subject) + 1 + utf8.RuneCountInString(predicate)
	queryWords := len(strings.Fields(subject + " " + predicate))
	if queryRunes > models.MaxSearchQueryRunes || queryWords > models.MaxSearchQueryWords {
		return preparedRequest{}, invalidAnswer("answer search query exceeds its resource limit")
	}
	if request.Timeout < 1 || request.Timeout > models.MaxAnswerTimeoutSeconds {
		return preparedRequest{}, invalidAnswer("answer timeout must be between 1 and 120 seconds")
	}

	schema, err := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			predicate: map[string]any{"type": "string"},
		},
		"required":             []string{predicate},
		"additionalProperties": false,
	})
	if err != nil {
		return preparedRequest{}, answerFailed("answer schema could not be prepared")
	}
	return preparedRequest{
		subject:        strings.Clone(subject),
		predicate:      strings.Clone(predicate),
		freshness:      strings.Clone(freshness),
		minimum:        spec.MinIndependentSources,
		timeoutSeconds: request.Timeout,
		consensusPath:  strings.ReplaceAll(predicate, ".", `\.`),
		schema:         append(json.RawMessage(nil), schema...),
	}, nil
}

func answerSources(results []models.SearchResult) ([]string, error) {
	sources := make([]string, 0, min(len(results), answerSearchLimit))
	seen := make(map[string]struct{}, min(len(results), answerSearchLimit))
	used := 0
	for index := 0; index < len(results) && len(sources) < answerSearchLimit; index++ {
		if results[index].Rank != index+1 {
			return nil, answerFailed("answer search returned an invalid result rank")
		}
		raw := results[index].URL
		if len(raw) == 0 || len(raw) > models.MaxExtractSourceURLBytes || !utf8.ValidString(raw) || containsControl(raw) {
			return nil, answerFailed("answer search returned an invalid result URL")
		}
		canonical, parsed, err := publicnet.NormalizeHTTPURL(raw, nil, false)
		if err != nil || parsed == nil || parsed.User != nil || len(canonical) > models.MaxExtractSourceURLBytes ||
			len(canonical) > models.MaxExtractSourcesURLBytes-used {
			return nil, answerFailed("answer search returned an invalid result URL")
		}
		if _, duplicate := seen[canonical]; duplicate {
			return nil, answerFailed("answer search returned a duplicate result URL")
		}
		seen[canonical] = struct{}{}
		used += len(canonical)
		sources = append(sources, strings.Clone(canonical))
	}
	return sources, nil
}

func allowedConsensusSupports(searched []string, summaries []models.MultiExtractSource) (map[string]string, error) {
	if len(searched) == 0 || len(summaries) != len(searched) || len(summaries) > models.MaxExtractSources {
		return nil, answerFailed("answer extraction returned invalid source summaries")
	}
	wanted := make(map[string]struct{}, len(searched))
	for _, sourceURL := range searched {
		wanted[sourceURL] = struct{}{}
	}
	seen := make(map[string]struct{}, len(summaries))
	allowed := make(map[string]string, len(summaries))
	for _, summary := range summaries {
		canonical, parsed, err := publicnet.NormalizeHTTPURL(summary.URL, nil, false)
		if err != nil || parsed == nil || parsed.User != nil || canonical != summary.URL {
			return nil, answerFailed("answer extraction returned an invalid source summary")
		}
		if _, exists := wanted[canonical]; !exists {
			return nil, answerFailed("answer extraction returned an unexpected source summary")
		}
		if _, duplicate := seen[canonical]; duplicate {
			return nil, answerFailed("answer extraction returned a duplicate source summary")
		}
		seen[canonical] = struct{}{}
		if summary.Success != (summary.Status == models.MultiExtractSourceStatusValid) || summary.Success && summary.Error != nil {
			return nil, answerFailed("answer extraction returned an inconsistent source summary")
		}
		if !summary.Success || summary.Status != models.MultiExtractSourceStatusValid {
			continue
		}
		if !validSnapshotID(summary.SnapshotID) {
			return nil, answerFailed("answer extraction returned an invalid source snapshot")
		}
		finalURL := canonical
		if summary.FinalURL != "" {
			finalURL, parsed, err = publicnet.NormalizeHTTPURL(summary.FinalURL, nil, false)
			if err != nil || parsed == nil || parsed.User != nil || finalURL != summary.FinalURL {
				return nil, answerFailed("answer extraction returned an invalid final source URL")
			}
		}
		if _, duplicateFinal := allowed[finalURL]; duplicateFinal {
			return nil, answerFailed("answer extraction returned duplicate valid final URLs")
		}
		allowed[finalURL] = strings.Clone(summary.SnapshotID)
	}
	if len(seen) != len(wanted) || len(allowed) == 0 {
		return nil, answerFailed("answer extraction returned incomplete source summaries")
	}
	return allowed, nil
}

func validateAggregateShape(
	ctx context.Context,
	response *models.MultiExtractResponse,
	predicate string,
	consensusPath string,
) error {
	if response == nil || response.Consensus == nil || len(response.Consensus.Fields) > 1 {
		return answerFailed("answer extraction returned an invalid aggregate")
	}
	field, hasField := response.Consensus.Fields[consensusPath]
	if !hasField && len(response.Consensus.Fields) != 0 {
		return answerFailed("answer extraction returned an unexpected consensus field")
	}
	switch response.Status {
	case models.MultiExtractStatusComplete:
		if !hasField || field.Ambiguous || len(response.Violations) != 0 {
			return answerFailed("answer extraction returned an inconsistent complete aggregate")
		}
		dataValue, err := decodeAggregateValue(ctx, response.Data, predicate)
		if err != nil {
			return err
		}
		winner, isNull, err := cloneScalar(ctx, field.Value)
		if err != nil || isNull || len(winner) == 0 || !bytes.Equal(dataValue, winner) {
			return answerFailed("answer aggregate data disagrees with consensus")
		}
	case models.MultiExtractStatusAmbiguous:
		if len(response.Data) != 0 || len(response.Violations) != 0 || !hasField || !field.Ambiguous {
			return answerFailed("answer extraction returned an inconsistent ambiguous aggregate")
		}
	case models.MultiExtractStatusSchemaInvalid:
		if len(response.Data) != 0 || len(response.Violations) == 0 || len(response.Violations) > 16 {
			return answerFailed("answer extraction returned an inconsistent schema-invalid aggregate")
		}
		for _, violation := range response.Violations {
			if len(violation.Path) > 4<<10 || len(violation.Message) > 8<<10 ||
				!validText(violation.Path) || !validText(violation.Message) {
				return answerFailed("answer extraction returned an invalid schema violation")
			}
		}
		if hasField {
			if field.Ambiguous || field.Agreement != (models.MultiExtractAgreement{}) || len(field.Supports) != 0 || len(field.Conflicts) != 0 {
				return answerFailed("answer schema-invalid aggregate contains consensus metadata")
			}
			_, isNull, err := cloneScalar(ctx, field.Value)
			if err != nil || len(field.Value) != 0 && !isNull {
				return answerFailed("answer schema-invalid aggregate contains an answerable value")
			}
		}
	default:
		return answerFailed("answer extraction returned an invalid aggregate status")
	}
	if err := ctx.Err(); err != nil {
		return answerTimeout()
	}
	return nil
}

func decodeAggregateValue(ctx context.Context, data json.RawMessage, predicate string) (json.RawMessage, error) {
	if len(data) == 0 || len(data) > maximumAnswerValueBytes+models.MaxAnswerPredicateBytes+128 || !utf8.Valid(data) {
		return nil, answerFailed("answer aggregate data is invalid or exceeds its resource limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') || !decoder.More() {
		return nil, answerFailed("answer aggregate data must contain exactly one property")
	}
	nameToken, err := decoder.Token()
	name, ok := nameToken.(string)
	if err != nil || !ok || name != predicate {
		return nil, answerFailed("answer aggregate data contains an unexpected property")
	}
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil || decoder.More() {
		return nil, answerFailed("answer aggregate data must contain exactly one property")
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return nil, answerFailed("answer aggregate data is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, answerFailed("answer aggregate data contains trailing JSON")
	}
	value, isNull, err := cloneScalar(ctx, raw)
	if err != nil || isNull || len(value) == 0 {
		return nil, answerFailed("answer aggregate data is not a string")
	}
	return value, nil
}

func validateSupportIdentities(
	ctx context.Context,
	supports []models.MultiExtractSupport,
	pages int,
	independentRoots int,
	allowed map[string]string,
	claimed map[string]struct{},
) error {
	if len(supports) != pages || len(supports) > models.MaxExtractSources {
		return answerFailed("answer consensus returned invalid supports")
	}
	if claimed == nil {
		return answerFailed("answer consensus support accounting is unavailable")
	}
	distinctRoots := make(map[string]struct{}, len(supports))
	for _, support := range supports {
		if err := ctx.Err(); err != nil {
			return answerTimeout()
		}
		canonical, err := validateSupportIdentity(support, allowed)
		if err != nil {
			return err
		}
		if _, duplicate := claimed[canonical]; duplicate {
			return answerFailed("answer consensus support is duplicated across candidates")
		}
		claimed[canonical] = struct{}{}
		distinctRoots[support.Root] = struct{}{}
	}
	if independentRoots > len(distinctRoots) {
		return answerFailed("answer consensus overstates independent support roots")
	}
	return nil
}

func validateSupportIdentity(support models.MultiExtractSupport, allowed map[string]string) (string, error) {
	canonical, parsed, err := publicnet.NormalizeHTTPURL(support.URL, nil, false)
	if err != nil || parsed == nil || parsed.User != nil || canonical != support.URL {
		return "", answerFailed("answer consensus support is invalid")
	}
	expectedSnapshotID, exists := allowed[canonical]
	if !exists {
		return "", answerFailed("answer consensus support was not searched")
	}
	if support.Evidence == nil || support.Evidence.SnapshotID != expectedSnapshotID {
		return "", answerFailed("answer consensus support snapshot is invalid")
	}
	root, err := supportRoot(parsed.Hostname())
	if err != nil || support.Root != root {
		return "", answerFailed("answer consensus support root is invalid")
	}
	return canonical, nil
}

func supportRoot(hostname string) (string, error) {
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

type answerDecision struct {
	value     json.RawMessage
	agreement models.MultiExtractAgreement
	supports  []models.MultiExtractSupport
	conflicts []models.AnswerCandidate
	reason    models.AnswerUnknownReason
	closest   *models.AnswerClosest
}

func decide(
	ctx context.Context,
	field models.MultiExtractFieldConsensus,
	minimum int,
	allowedSupports map[string]string,
) (answerDecision, error) {
	if err := ctx.Err(); err != nil {
		return answerDecision{}, answerTimeout()
	}
	if field.Ambiguous {
		if len(field.Value) != 0 || field.Agreement != (models.MultiExtractAgreement{}) || len(field.Supports) != 0 || len(field.Conflicts) < 2 {
			return answerDecision{}, answerFailed("answer consensus returned an invalid field")
		}
		claimed := make(map[string]struct{}, len(allowedSupports))
		candidates, err := projectConflicts(ctx, field.Conflicts, allowedSupports, claimed)
		if err != nil {
			return answerDecision{}, err
		}
		if _, err := boundedCandidatePages(candidates); err != nil {
			return answerDecision{}, err
		}
		if len(candidates) < 2 || candidates[0].Agreement.IndependentRoots != candidates[1].Agreement.IndependentRoots {
			return answerDecision{}, answerFailed("answer ambiguous consensus lacks a highest independent-root tie")
		}
		for index := 1; index < len(candidates); index++ {
			previous := candidates[index-1].Agreement
			current := candidates[index].Agreement
			if previous.IndependentRoots < current.IndependentRoots ||
				previous.IndependentRoots == current.IndependentRoots && previous.Pages < current.Pages {
				return answerDecision{}, answerFailed("answer ambiguous consensus candidates are out of order")
			}
		}
		closest := closestNonNullAtRoots(candidates, candidates[0].Agreement.IndependentRoots, "independent-root tie")
		if closest == nil {
			return answerDecision{}, answerFailed("answer ambiguous consensus has no non-null closest candidate")
		}
		alternatives, err := alternativesWithoutClosest(candidates, closest)
		if err != nil {
			return answerDecision{}, err
		}
		return answerDecision{reason: models.AnswerUnknownConflict, closest: closest, conflicts: alternatives}, nil
	}
	if len(field.Conflicts)+1 > models.MaxExtractSources {
		return answerDecision{}, answerFailed("answer consensus returned too many candidates")
	}

	value, isNull, err := cloneScalar(ctx, field.Value)
	if err != nil {
		return answerDecision{}, err
	}
	if len(value) == 0 || isNull {
		return answerDecision{reason: models.AnswerUnknownMissingValue}, nil
	}
	if err := validateAgreement(field.Agreement); err != nil {
		return answerDecision{}, err
	}
	claimed := make(map[string]struct{}, len(allowedSupports))
	if err := validateSupportIdentities(ctx, field.Supports, field.Agreement.Pages, field.Agreement.IndependentRoots, allowedSupports, claimed); err != nil {
		return answerDecision{}, err
	}
	conflicts, err := projectConflicts(ctx, field.Conflicts, allowedSupports, claimed)
	if err != nil {
		return answerDecision{}, err
	}
	conflictPages, err := boundedCandidatePages(conflicts)
	if err != nil {
		return answerDecision{}, err
	}
	if field.Agreement.Pages > models.MaxExtractSources-conflictPages {
		return answerDecision{}, answerFailed("answer consensus candidate pages exceed their resource limit")
	}
	winner := models.AnswerCandidate{Value: append(json.RawMessage(nil), value...), Agreement: field.Agreement}
	seenValues := map[string]struct{}{string(winner.Value): {}}
	previous := field.Agreement
	hasIndependentTie := false
	for _, conflict := range conflicts {
		current := conflict.Agreement
		if previous.IndependentRoots < current.IndependentRoots ||
			previous.IndependentRoots == current.IndependentRoots && previous.Pages < current.Pages {
			return answerDecision{}, answerFailed("answer consensus returned an invalid ordering")
		}
		if _, duplicate := seenValues[string(conflict.Value)]; duplicate {
			return answerDecision{}, answerFailed("answer consensus returned a duplicate candidate value")
		}
		seenValues[string(conflict.Value)] = struct{}{}
		if current.IndependentRoots == field.Agreement.IndependentRoots {
			hasIndependentTie = true
		}
		previous = current
	}
	if hasIndependentTie {
		return answerDecision{
			reason:    models.AnswerUnknownConflict,
			closest:   closestFromCandidate(winner, "independent-root tie"),
			conflicts: conflicts,
		}, nil
	}
	if field.Agreement.IndependentRoots < minimum {
		return answerDecision{
			reason:    models.AnswerUnknownInsufficient,
			closest:   closestFromCandidate(winner, "insufficient independent roots"),
			conflicts: conflicts,
		}, nil
	}
	if len(field.Supports) == 0 || len(field.Supports) > models.MaxExtractSources || len(field.Supports) != field.Agreement.Pages {
		return answerDecision{}, answerFailed("answer consensus returned invalid supports")
	}
	return answerDecision{
		value:     value,
		agreement: field.Agreement,
		supports:  append([]models.MultiExtractSupport(nil), field.Supports...),
		conflicts: conflicts,
	}, nil
}

func projectConflicts(
	ctx context.Context,
	input []models.MultiExtractConflict,
	allowedSupports map[string]string,
	claimed map[string]struct{},
) ([]models.AnswerCandidate, error) {
	if len(input) > models.MaxExtractSources {
		return nil, answerFailed("answer consensus returned too many conflicts")
	}
	output := make([]models.AnswerCandidate, 0, len(input))
	seenValues := make(map[string]struct{}, len(input))
	used := 0
	for _, conflict := range input {
		if err := ctx.Err(); err != nil {
			return nil, answerTimeout()
		}
		value, _, err := cloneScalar(ctx, conflict.Value)
		if err != nil {
			return nil, err
		}
		if len(value) == 0 {
			return nil, answerFailed("answer consensus returned an empty conflict")
		}
		if err := validateAgreement(conflict.Agreement); err != nil {
			return nil, err
		}
		if err := validateSupportIdentities(ctx, conflict.Supports, conflict.Agreement.Pages, conflict.Agreement.IndependentRoots, allowedSupports, claimed); err != nil {
			return nil, err
		}
		if len(value) > maximumAnswerOutput-used {
			return nil, answerFailed("answer conflicts exceed their output budget")
		}
		if _, duplicate := seenValues[string(value)]; duplicate {
			return nil, answerFailed("answer consensus returned a duplicate conflict value")
		}
		seenValues[string(value)] = struct{}{}
		used += len(value)
		output = append(output, models.AnswerCandidate{Value: value, Agreement: conflict.Agreement})
	}
	if err := ctx.Err(); err != nil {
		return nil, answerTimeout()
	}
	return output, nil
}

func boundedCandidatePages(candidates []models.AnswerCandidate) (int, error) {
	total := 0
	for _, candidate := range candidates {
		if candidate.Agreement.Pages < 1 || candidate.Agreement.Pages > models.MaxExtractSources-total {
			return 0, answerFailed("answer consensus candidate pages exceed their resource limit")
		}
		total += candidate.Agreement.Pages
	}
	return total, nil
}

func (service *Service) buildBelief(
	ctx context.Context,
	value json.RawMessage,
	agreement models.MultiExtractAgreement,
	supports []models.MultiExtractSupport,
	allowedSupports map[string]string,
) (*models.AnswerBelief, error) {
	asOf, err := normalizePublicTime(service.now())
	if err != nil {
		return nil, answerFailed("answer clock is invalid")
	}
	evidenceItems := make([]models.AnswerEvidence, 0, len(supports))
	receipts := make(map[string]string, len(supports))
	budget := len(value)
	for _, support := range supports {
		if err := ctx.Err(); err != nil {
			return nil, answerTimeout()
		}
		if support.Evidence == nil || support.Receipt == "" || len(support.Receipt) > maximumAnswerReceipt || !validReceipt(support.Receipt) {
			return nil, answerFailed("answer consensus evidence is incomplete")
		}
		canonical, err := validateSupportIdentity(support, allowedSupports)
		if err != nil {
			return nil, err
		}
		anchor := support.Evidence
		start, end := anchor.TextRange[0], anchor.TextRange[1]
		if anchor.Quote == "" || len(anchor.Quote) > maximumAnswerQuoteBytes || len(anchor.Selector) > maximumAnswerSelector ||
			!validSnapshotID(anchor.SnapshotID) || len(anchor.SnapshotID) > maximumAnswerSnapshotID || anchor.FetchedAt.IsZero() ||
			start < 0 || end <= start || end-start != len(anchor.Quote) || !validAnchorMethod(anchor.Method) ||
			!validText(anchor.Quote) || !validText(anchor.Selector) || !validText(anchor.SnapshotID) {
			return nil, answerFailed("answer consensus evidence is invalid")
		}
		if _, duplicate := receipts[canonical]; duplicate {
			return nil, answerFailed("answer consensus support is duplicated")
		}
		added := len(canonical) + len(support.Root) + len(anchor.Quote) + len(anchor.Selector) + len(anchor.SnapshotID) + len(support.Receipt)
		if added > maximumAnswerOutput-budget {
			return nil, answerFailed("answer response exceeds its output budget")
		}
		budget += added
		fetchedAt, err := normalizePublicTime(anchor.FetchedAt)
		if err != nil {
			return nil, answerFailed("answer evidence time is invalid")
		}
		if fetchedAt.After(asOf) {
			return nil, answerFailed("answer evidence time is after belief observation")
		}
		evidenceItems = append(evidenceItems, models.AnswerEvidence{
			URL:        strings.Clone(canonical),
			Root:       strings.Clone(support.Root),
			Quote:      strings.Clone(anchor.Quote),
			TextRange:  anchor.TextRange,
			Selector:   strings.Clone(anchor.Selector),
			Method:     evidence.Method(strings.Clone(string(anchor.Method))),
			SnapshotID: strings.Clone(anchor.SnapshotID),
			FetchedAt:  fetchedAt,
		})
		receipts[strings.Clone(canonical)] = strings.Clone(support.Receipt)
	}
	if err := ctx.Err(); err != nil {
		return nil, answerTimeout()
	}
	return &models.AnswerBelief{
		Value:      append(json.RawMessage(nil), value...),
		Confidence: confidenceFor(agreement.IndependentRoots),
		Agreement:  agreement,
		AsOf:       asOf,
		Evidence:   evidenceItems,
		Receipts:   receipts,
	}, nil
}

func unknownResponse(
	reason models.AnswerUnknownReason,
	minimum int,
	closest *models.AnswerClosest,
	conflicts []models.AnswerCandidate,
) *models.AnswerResponse {
	roots := 0
	if closest != nil {
		roots = closest.IndependentRoots
	}
	needed := minimum - roots
	if needed < 1 {
		needed = 1
	}
	return &models.AnswerResponse{
		Status:    models.AnswerStatusUnknown,
		Belief:    nil,
		Reason:    reason,
		Needs:     &models.AnswerNeeds{MoreIndependentSources: needed},
		Closest:   cloneClosest(closest),
		Conflicts: cloneCandidates(conflicts),
	}
}

func closestFromCandidate(candidate models.AnswerCandidate, note string) *models.AnswerClosest {
	return &models.AnswerClosest{
		Value:            append(json.RawMessage(nil), candidate.Value...),
		IndependentRoots: candidate.Agreement.IndependentRoots,
		Note:             note,
	}
}

func closestNonNullAtRoots(candidates []models.AnswerCandidate, roots int, note string) *models.AnswerClosest {
	for _, candidate := range candidates {
		if candidate.Agreement.IndependentRoots != roots {
			continue
		}
		if !bytes.Equal(bytes.TrimSpace(candidate.Value), []byte("null")) {
			return closestFromCandidate(candidate, note)
		}
	}
	return nil
}

func alternativesWithoutClosest(
	candidates []models.AnswerCandidate,
	closest *models.AnswerClosest,
) ([]models.AnswerCandidate, error) {
	if closest == nil {
		return nil, answerFailed("answer closest candidate is required")
	}
	output := make([]models.AnswerCandidate, 0, max(0, len(candidates)-1))
	removed := false
	for _, candidate := range candidates {
		if !removed && bytes.Equal(candidate.Value, closest.Value) &&
			candidate.Agreement.IndependentRoots == closest.IndependentRoots {
			removed = true
			continue
		}
		output = append(output, candidate)
	}
	if !removed || len(output) == 0 {
		return nil, answerFailed("answer closest candidate has no competing alternative")
	}
	return output, nil
}

func cloneClosest(input *models.AnswerClosest) *models.AnswerClosest {
	if input == nil {
		return nil
	}
	output := *input
	output.Value = append(json.RawMessage(nil), input.Value...)
	output.Note = strings.Clone(input.Note)
	return &output
}

func cloneCandidates(input []models.AnswerCandidate) []models.AnswerCandidate {
	if len(input) == 0 {
		return nil
	}
	output := make([]models.AnswerCandidate, len(input))
	for index := range input {
		output[index] = input[index]
		output[index].Value = append(json.RawMessage(nil), input[index].Value...)
	}
	return output
}

func cloneScalar(ctx context.Context, raw json.RawMessage) (json.RawMessage, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, answerTimeout()
	}
	if len(raw) == 0 {
		return nil, false, nil
	}
	if len(raw) > maximumAnswerValueBytes || !utf8.Valid(raw) {
		return nil, false, answerFailed("answer consensus value exceeds its resource limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false, answerFailed("answer consensus value is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, false, answerFailed("answer consensus value is invalid")
	}
	if err := ctx.Err(); err != nil {
		return nil, false, answerTimeout()
	}
	switch value.(type) {
	case nil:
		return json.RawMessage("null"), true, nil
	case string:
		encoded, err := json.Marshal(value)
		if err != nil || len(encoded) > maximumAnswerValueBytes {
			return nil, false, answerFailed("answer consensus value is invalid")
		}
		return append(json.RawMessage(nil), encoded...), false, nil
	default:
		return nil, false, answerFailed("answer consensus value is not a string")
	}
}

func validateAgreement(agreement models.MultiExtractAgreement) error {
	if agreement.Pages < 1 || agreement.Pages > models.MaxExtractSources || agreement.IndependentRoots < 1 ||
		agreement.IndependentRoots > agreement.Pages {
		return answerFailed("answer consensus agreement is invalid")
	}
	return nil
}

func confidenceFor(independentRoots int) models.AnswerConfidence {
	switch {
	case independentRoots >= 3:
		return models.AnswerConfidenceHigh
	case independentRoots == 2:
		return models.AnswerConfidenceMedium
	default:
		return models.AnswerConfidenceLow
	}
}

func validAnchorMethod(method evidence.Method) bool {
	switch method {
	case evidence.MethodExact, evidence.MethodNormalized, evidence.MethodFuzzy, evidence.MethodCompiled:
		return true
	default:
		return false
	}
}

func validText(value string) bool {
	return utf8.ValidString(value) && !containsControl(value)
}

func validReceipt(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if character == '.' || character == '-' || character == '_' ||
			character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return true
}

func validSnapshotID(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for index := len("sha256:"); index < len(value); index++ {
		character := value[index]
		if character >= '0' && character <= '9' || character >= 'a' && character <= 'f' {
			continue
		}
		return false
	}
	return true
}

func normalizePublicTime(value time.Time) (time.Time, error) {
	if value.IsZero() {
		return time.Time{}, errors.New("zero time")
	}
	value = value.UTC()
	if _, err := value.MarshalJSON(); err != nil {
		return time.Time{}, err
	}
	return value, nil
}

func containsControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}

func containsSpace(value string) bool {
	for _, character := range value {
		if unicode.IsSpace(character) {
			return true
		}
	}
	return false
}

func isNilDependency(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func invalidAnswer(message string) error {
	return models.NewScrapeError(models.ErrCodeInvalidInput, message, nil)
}

func answerUnavailable() error {
	return models.NewScrapeError(models.ErrCodeAnswerUnavailable, "answer is unavailable", nil)
}

func answerTimeout() error {
	return models.NewScrapeError(models.ErrCodeTimeout, "answer request timed out", nil)
}

func answerFailed(message string) error {
	return models.NewScrapeError(models.ErrCodeAnswerFailed, message, nil)
}

func scrapeErrorCode(err error) string {
	var failure *models.ScrapeError
	if !errors.As(err, &failure) || failure == nil {
		return ""
	}
	return failure.Code
}

func sanitizeSearchError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return answerTimeout()
	}
	var failure *models.ScrapeError
	if errors.As(err, &failure) {
		switch failure.Code {
		case models.ErrCodeTimeout:
			return answerTimeout()
		case models.ErrCodeInvalidInput:
			return answerFailed("answer search request was rejected")
		case models.ErrCodeRateLimited:
			return models.NewScrapeError(models.ErrCodeRateLimited, "answer search is rate limited", nil)
		case models.ErrCodeSearchUnavailable:
			return answerUnavailable()
		}
	}
	return answerFailed("answer search failed")
}

func sanitizeExtractError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return answerTimeout()
	}
	var failure *models.ScrapeError
	if errors.As(err, &failure) {
		switch failure.Code {
		case models.ErrCodeTimeout:
			return answerTimeout()
		case models.ErrCodeMultiSourceUnavailable, models.ErrCodeExtractorUnavailable, models.ErrCodeLLMAuthFailure:
			return answerUnavailable()
		case models.ErrCodeLLMRateLimited, models.ErrCodeRateLimited:
			return models.NewScrapeError(models.ErrCodeRateLimited, "answer extraction is rate limited", nil)
		}
	}
	return answerFailed("answer extraction failed")
}
