package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/use-agent/purify/consensus"
	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/extract"
	"github.com/use-agent/purify/llm"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/receipts"
)

const (
	defaultSearchEnrichmentSlots   = 4
	defaultSearchEncodingSlots     = 4
	defaultSearchEnrichmentTimeout = 15 * time.Second
	maximumSearchArtifactBytes     = models.MaxSearchResultContentBytes
	maximumSearchSnippetClaimBytes = 8 << 10
	maximumSearchSnippetReceipt    = models.MaxSearchResultReceiptBytes
)

var errSearchDependencyPanic = errors.New("search: enrichment dependency panicked")

// ArtifactService is the complete enrichment boundary. FetchPublicArtifact
// accepts only a public URL, and ExtractArtifact must reuse that exact fetched
// artifact rather than performing a second network request.
type ArtifactService interface {
	FetchPublicArtifact(context.Context, string) (*extract.Artifact, error)
	ExtractArtifact(context.Context, *extract.Artifact, *models.ExtractRequest) (*models.ExtractResponse, error)
}

// ReceiptSigner signs a verified snippet claim with the process-owned receipt
// key. Caller credentials never cross this boundary.
type ReceiptSigner interface {
	Sign(receipts.Payload) (string, error)
}

type enrichmentOption struct {
	artifacts ArtifactService
	signer    ReceiptSigner
}

// WithEnrichment enables bounded content, snippet verification, and
// schema-shaped extraction. Both dependencies are required because a Search
// service must fail closed rather than silently weakening a heavy request.
func WithEnrichment(artifacts ArtifactService, signer ReceiptSigner) ServiceOption {
	return enrichmentOption{artifacts: artifacts, signer: signer}
}

func (option enrichmentOption) applySearchService(service *Service) error {
	if service == nil || isNilSearchDependency(option.artifacts) || isNilSearchDependency(option.signer) {
		return errors.New("search: enrichment requires an artifact service and receipt signer")
	}
	service.artifacts = option.artifacts
	service.signer = option.signer
	service.enrichmentSlots = make(chan struct{}, defaultSearchEnrichmentSlots)
	return nil
}

func (service *Service) enrichmentAvailable() bool {
	return service != nil && !isNilSearchDependency(service.artifacts) &&
		!isNilSearchDependency(service.signer) && service.enrichmentSlots != nil
}

type searchEnrichmentOutcome struct {
	result   models.SearchResult
	identity string
	stale    bool
	partial  bool
}

func (service *Service) enrichResults(
	ctx context.Context,
	results []models.SearchResult,
	request preparedSearchRequest,
) ([]models.SearchResult, int, int, bool) {
	heavyCount := min(len(results), models.MaxSearchHeavyResults)
	if heavyCount == 0 {
		return results, 0, 0, false
	}

	outcomes := make([]searchEnrichmentOutcome, heavyCount)
	localSlots := make(chan struct{}, defaultSearchEnrichmentSlots)
	var workers sync.WaitGroup
	workers.Add(heavyCount)
	for index := range heavyCount {
		index := index
		baseline := cloneSearchResult(results[index])
		go func() {
			defer workers.Done()
			outcomes[index] = service.runEnrichmentWorker(ctx, localSlots, baseline, request)
		}()
	}
	workers.Wait()

	kept := make([]models.SearchResult, 0, len(results))
	identities := make(map[string]struct{}, len(results))
	droppedStale := 0
	deduplicated := 0
	partial := false
	for index := range results {
		outcome := searchEnrichmentOutcome{
			result:   cloneSearchResult(results[index]),
			identity: results[index].URL,
		}
		if index < heavyCount {
			outcome = outcomes[index]
		}
		if outcome.stale {
			droppedStale++
			continue
		}
		identity := outcome.identity
		if identity == "" {
			identity = outcome.result.URL
		}
		if _, duplicate := identities[identity]; duplicate {
			deduplicated++
			continue
		}
		identities[identity] = struct{}{}
		outcome.result.Rank = len(kept) + 1
		kept = append(kept, outcome.result)
		partial = partial || outcome.partial
	}
	return kept, droppedStale, deduplicated, partial
}

func (service *Service) runEnrichmentWorker(
	ctx context.Context,
	localSlots chan struct{},
	result models.SearchResult,
	request preparedSearchRequest,
) (outcome searchEnrichmentOutcome) {
	outcome = searchEnrichmentOutcome{result: result, identity: result.URL}
	stage := models.SearchResultStageFetch
	defer func() {
		if recover() == nil {
			return
		}
		switch stage {
		case models.SearchResultStageVerify:
			outcome.addError(verifyResultError(ctx, errSearchDependencyPanic))
		case models.SearchResultStageExtract:
			outcome.addError(extractResultError(ctx, errSearchDependencyPanic))
		default:
			outcome.addError(fetchResultError(ctx, errSearchDependencyPanic))
		}
	}()
	if err := acquireSearchSlot(ctx, localSlots); err != nil {
		outcome.addError(fetchResultError(ctx, err))
		return outcome
	}
	defer releaseSearchSlot(localSlots)
	if err := acquireSearchSlot(ctx, service.enrichmentSlots); err != nil {
		outcome.addError(fetchResultError(ctx, err))
		return outcome
	}
	defer releaseSearchSlot(service.enrichmentSlots)

	timeout := service.enrichmentTimeout
	if timeout <= 0 {
		timeout = defaultSearchEnrichmentTimeout
	}
	workerContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := workerContext.Err(); err != nil {
		outcome.addError(fetchResultError(workerContext, err))
		return outcome
	}
	artifact, err := safeFetchPublicArtifact(service.artifacts, workerContext, result.URL)
	if err != nil {
		outcome.addError(fetchResultError(workerContext, err))
		return outcome
	}
	if err := workerContext.Err(); err != nil {
		outcome.addError(fetchResultError(workerContext, err))
		return outcome
	}
	finalURL, statusCode, fetchedAt, err := validateSearchArtifact(artifact)
	if err != nil {
		outcome.addError(fetchResultError(workerContext, err))
		return outcome
	}
	if err := workerContext.Err(); err != nil {
		outcome.addError(fetchResultError(workerContext, err))
		return outcome
	}
	outcome.identity = finalURL
	outcome.result.FinalURL = strings.Clone(finalURL)
	if statusCode == 404 || statusCode == 410 {
		outcome.stale = true
		return outcome
	}
	if statusCode < 200 || statusCode >= 300 {
		outcome.addError(fetchResultError(workerContext, models.NewScrapeError(models.ErrCodeNavigation, "non-success page status", nil)))
		return outcome
	}
	if err := validateSearchArtifactContent(artifact.Public.Content); err != nil {
		outcome.addError(fetchResultError(workerContext, err))
		return outcome
	}

	if request.verify {
		stage = models.SearchResultStageVerify
		if err := workerContext.Err(); err != nil {
			outcome.result.Verified = boolPointer(false)
			outcome.result.VerificationStatus = models.SearchVerificationUnavailable
			outcome.addError(verifyResultError(workerContext, err))
			return outcome
		}
		verified, status, anchor, token, verificationError := service.verifySearchSnippet(
			workerContext,
			artifact,
			finalURL,
			fetchedAt,
			result.Snippet,
		)
		if err := workerContext.Err(); err != nil {
			outcome.result.Verified = boolPointer(false)
			outcome.result.VerificationStatus = models.SearchVerificationUnavailable
			outcome.addError(verifyResultError(workerContext, err))
			return outcome
		}
		outcome.result.Verified = boolPointer(verified)
		outcome.result.VerificationStatus = status
		outcome.result.Evidence = anchor
		outcome.result.Receipt = token
		if verificationError != nil {
			outcome.addError(*verificationError)
			if verificationError.Code == models.ErrCodeTimeout {
				return outcome
			}
		}
	}

	if request.includeContent {
		stage = models.SearchResultStageFetch
		if err := workerContext.Err(); err != nil {
			outcome.addError(fetchResultError(workerContext, err))
			return outcome
		} else {
			outcome.result.Content = strings.Clone(artifact.Public.Content)
			if err := workerContext.Err(); err != nil {
				outcome.result.Content = ""
				outcome.addError(fetchResultError(workerContext, err))
				return outcome
			}
		}
	}

	if len(request.schema) > 0 {
		stage = models.SearchResultStageExtract
		if err := workerContext.Err(); err != nil {
			outcome.addError(extractResultError(workerContext, err))
			return outcome
		}
		response, extractErr := safeExtractArtifact(service.artifacts, workerContext, artifact, &models.ExtractRequest{
			URL:        finalURL,
			Schema:     bytes.Clone(request.schema),
			Engine:     strings.Clone(request.engine),
			LLMAPIKey:  strings.Clone(request.llmAPIKey),
			LLMModel:   strings.Clone(request.llmModel),
			LLMBaseURL: strings.Clone(request.llmBaseURL),
			Evidence:   true,
		})
		if err := workerContext.Err(); err != nil {
			outcome.addError(extractResultError(workerContext, err))
			return outcome
		} else if extractErr != nil {
			outcome.addError(extractResultError(workerContext, extractErr))
			return outcome
		}

		candidate := cloneSearchResult(outcome.result)
		partial, copyErr := copySearchExtraction(
			workerContext,
			&candidate,
			artifact,
			finalURL,
			fetchedAt,
			request.schema,
			response,
		)
		if err := workerContext.Err(); err != nil {
			outcome.addError(extractResultError(workerContext, err))
			return outcome
		}
		if copyErr != nil {
			outcome.addError(extractResultError(workerContext, copyErr))
			return outcome
		}
		outcome.result = candidate
		if partial {
			outcome.addError(models.SearchResultError{
				Stage:   models.SearchResultStageExtract,
				Code:    models.ErrCodeLLMFailure,
				Message: "result extraction was partial",
			})
		}
	}
	return outcome
}

func (outcome *searchEnrichmentOutcome) addError(resultError models.SearchResultError) {
	if outcome == nil {
		return
	}
	outcome.result.Errors = append(outcome.result.Errors, resultError)
	outcome.partial = true
}

func (service *Service) verifySearchSnippet(
	ctx context.Context,
	artifact *extract.Artifact,
	finalURL string,
	fetchedAt time.Time,
	snippet string,
) (bool, models.SearchVerificationStatus, *evidence.Anchor, string, *models.SearchResultError) {
	unavailable := func(err error) (bool, models.SearchVerificationStatus, *evidence.Anchor, string, *models.SearchResultError) {
		resultError := verifyResultError(ctx, err)
		return false, models.SearchVerificationUnavailable, nil, "", &resultError
	}
	if ctx == nil || artifact == nil || artifact.Public == nil || artifact.Source == nil || finalURL == "" ||
		snippet == "" || len(snippet) > maximumSearchSnippetClaimBytes || !utf8.ValidString(snippet) {
		return unavailable(errors.New("snippet claim is unavailable"))
	}
	if err := ctx.Err(); err != nil {
		return unavailable(err)
	}
	scalar, err := json.Marshal(snippet)
	if err != nil || len(scalar) > maximumSearchSnippetClaimBytes {
		return unavailable(errors.New("snippet claim is unavailable"))
	}
	snapshotID := string(artifact.Source.SnapshotID)
	if !validSearchSnapshotID(snapshotID) || fetchedAt.IsZero() {
		return unavailable(errors.New("snapshot metadata is unavailable"))
	}

	anchor, err := safeAlignValueContext(ctx, snippet, artifact.Public.Content, artifact.Source.RawHTML)
	if err != nil {
		return unavailable(err)
	}
	if err := ctx.Err(); err != nil {
		return unavailable(err)
	}
	if anchor.Method == evidence.MethodUnlocated {
		return false, models.SearchVerificationMismatch, nil, "", nil
	}
	if err := validateSearchSnippetAnchor(&anchor, artifact.Public.Content); err != nil {
		return unavailable(err)
	}
	anchor.SnapshotID = strings.Clone(snapshotID)
	anchor.FetchedAt = fetchedAt
	anchor.Method = evidence.Method(strings.Clone(string(anchor.Method)))
	if len(anchor.Selector) > consensus.MaxEvidenceSelectorBytes || !utf8.ValidString(anchor.Selector) {
		anchor.Selector = ""
	} else {
		anchor.Selector = strings.Clone(anchor.Selector)
	}
	anchor.Quote = strings.Clone(anchor.Quote)

	token, err := safeSignReceipt(service.signer, receipts.Payload{
		URL:      finalURL,
		Path:     "snippet",
		Value:    bytes.Clone(scalar),
		Anchor:   anchor,
		IssuedAt: service.now().UTC(),
	})
	if contextErr := ctx.Err(); contextErr != nil {
		return unavailable(contextErr)
	}
	if err != nil || token == "" || len(token) > maximumSearchSnippetReceipt ||
		!utf8.ValidString(token) || containsControl(token) {
		if err == nil {
			err = errors.New("snippet receipt is invalid")
		}
		return unavailable(err)
	}
	return true, models.SearchVerificationVerified, &anchor, strings.Clone(token), nil
}

func validateSearchArtifact(artifact *extract.Artifact) (string, int, time.Time, error) {
	if artifact == nil || artifact.Public == nil || artifact.Source == nil || !artifact.Public.Success {
		return "", 0, time.Time{}, errors.New("invalid public artifact")
	}
	if strings.TrimSpace(artifact.Source.RawHTML) == "" || len(artifact.Source.RawHTML) > maximumSearchArtifactBytes {
		return "", 0, time.Time{}, errors.New("invalid public artifact")
	}
	statusCode := artifact.Source.StatusCode
	if statusCode < 100 || statusCode > 599 ||
		(artifact.Public.StatusCode != 0 && artifact.Public.StatusCode != statusCode) {
		return "", 0, time.Time{}, errors.New("invalid public artifact")
	}
	finalURL, err := normalizeProviderURL(artifact.Source.FinalURL)
	if err != nil || finalURL == "" {
		return "", 0, time.Time{}, errors.New("invalid public artifact")
	}
	if artifact.Public.FinalURL != "" {
		publicFinalURL, normalizeErr := normalizeProviderURL(artifact.Public.FinalURL)
		if normalizeErr != nil || publicFinalURL != finalURL {
			return "", 0, time.Time{}, errors.New("invalid public artifact")
		}
	}
	fetchedAt, err := canonicalSearchTime(artifact.Source.FetchedAt)
	if err != nil {
		return "", 0, time.Time{}, errors.New("invalid public artifact")
	}
	return finalURL, statusCode, fetchedAt, nil
}

func validateSearchArtifactContent(content string) error {
	if strings.TrimSpace(content) == "" || len(content) > maximumSearchArtifactBytes || !utf8.ValidString(content) {
		return models.NewScrapeError(models.ErrCodeContentUnusable, "public artifact content is unusable", nil)
	}
	return nil
}

func validateSearchSnippetAnchor(anchor *evidence.Anchor, cleaned string) error {
	if anchor == nil {
		return errors.New("snippet evidence is unavailable")
	}
	switch anchor.Method {
	case evidence.MethodExact, evidence.MethodNormalized, evidence.MethodFuzzy:
	default:
		return errors.New("snippet evidence is unavailable")
	}
	start, end := anchor.TextRange[0], anchor.TextRange[1]
	if start < 0 || end <= start || end > len(cleaned) ||
		len(anchor.Quote) == 0 || len(anchor.Quote) > maximumSearchSnippetClaimBytes ||
		!utf8.ValidString(anchor.Quote) || anchor.Quote != cleaned[start:end] {
		return errors.New("snippet evidence is unavailable")
	}
	return nil
}

func validSearchSnapshotID(snapshotID string) bool {
	if len(snapshotID) != len("sha256:")+64 || !strings.HasPrefix(snapshotID, "sha256:") {
		return false
	}
	for _, character := range snapshotID[len("sha256:"):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func copySearchExtraction(
	ctx context.Context,
	result *models.SearchResult,
	artifact *extract.Artifact,
	finalURL string,
	fetchedAt time.Time,
	schema json.RawMessage,
	response *models.ExtractResponse,
) (bool, error) {
	if ctx == nil {
		return false, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if result == nil || artifact == nil || artifact.Public == nil || artifact.Source == nil || finalURL == "" ||
		len(schema) == 0 || response == nil || !response.Success || response.Error != nil || len(response.Data) == 0 ||
		len(response.Data) > models.MaxSearchResultDataBytes {
		return false, errors.New("invalid extraction response")
	}
	if _, err := consensus.Merge([]consensus.SourceResult{{URL: finalURL, Data: response.Data}}); err != nil {
		return false, errors.New("invalid extraction data")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	leaves, err := evidence.LeafValues(response.Data)
	if err != nil || len(leaves) > consensus.MaxLeavesPerSource {
		return false, errors.New("invalid extraction data")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if response.Basis == nil || response.Receipts == nil || response.UnlocatedRate == nil ||
		math.IsNaN(*response.UnlocatedRate) || math.IsInf(*response.UnlocatedRate, 0) ||
		*response.UnlocatedRate < 0 || *response.UnlocatedRate > 1 ||
		response.SnapshotID != string(artifact.Source.SnapshotID) || !validSearchSnapshotID(response.SnapshotID) ||
		len(*response.Basis) != len(leaves) || len(*response.Receipts) != len(leaves) ||
		len(response.Violations) > consensus.MaxLeavesPerSource {
		return false, errors.New("invalid extraction evidence")
	}
	for path := range leaves {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if _, ok := (*response.Basis)[path]; !ok {
			return false, errors.New("extraction evidence path set is incomplete")
		}
		if _, ok := (*response.Receipts)[path]; !ok {
			return false, errors.New("extraction receipt path set is incomplete")
		}
	}

	if err := ctx.Err(); err != nil {
		return false, err
	}
	authoritativeViolations, err := llm.ValidateAgainstSchema(schema, response.Data)
	if err != nil || len(authoritativeViolations) > consensus.MaxLeavesPerSource {
		return false, errors.New("extraction schema validation failed")
	}
	partial := len(authoritativeViolations) > 0
	if response.Partial != partial || !equalSearchViolations(response.Violations, authoritativeViolations) {
		return false, errors.New("extraction partial status is inconsistent with its schema")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}

	metadataBytes := 0
	unlocated := 0
	basis := make(models.EvidenceBasis, len(*response.Basis))
	for path, sourceAnchor := range *response.Basis {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if !validSearchEvidencePath(path) {
			return false, errors.New("invalid extraction evidence path")
		}
		anchor, isUnlocated, err := cloneSearchExtractionAnchor(
			sourceAnchor,
			artifact.Public.Content,
			response.SnapshotID,
			fetchedAt,
		)
		if err != nil {
			return false, err
		}
		if isUnlocated {
			unlocated++
		}
		if !reserveSearchMetadata(&metadataBytes, len(path)+len(anchor.Quote)+len(anchor.Selector)+len(anchor.SnapshotID)+len(anchor.Method)+64) {
			return false, errors.New("extraction metadata exceeds its resource limit")
		}
		basis[strings.Clone(path)] = anchor
	}
	wantUnlocatedRate := 0.0
	if len(leaves) > 0 {
		wantUnlocatedRate = float64(unlocated) / float64(len(leaves))
	}
	if *response.UnlocatedRate != wantUnlocatedRate {
		return false, errors.New("extraction unlocated rate is inconsistent with its evidence")
	}

	receiptTokens := make(models.FieldReceipts, len(*response.Receipts))
	for path, token := range *response.Receipts {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if !validSearchEvidencePath(path) || token == "" || len(token) > consensus.MaxReceiptBytes ||
			!utf8.ValidString(token) || containsControl(token) {
			return false, errors.New("invalid extraction receipt")
		}
		if _, ok := basis[path]; !ok || !reserveSearchMetadata(&metadataBytes, len(path)+len(token)+16) {
			return false, errors.New("invalid extraction receipt")
		}
		receiptTokens[strings.Clone(path)] = strings.Clone(token)
	}

	violations := make([]models.SchemaViolation, len(authoritativeViolations))
	for index, violation := range authoritativeViolations {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if len(violation.Path) == 0 || len(violation.Path) > consensus.MaxPathBytes || !utf8.ValidString(violation.Path) ||
			len(violation.Message) > MaxProviderSnippetBytes || !utf8.ValidString(violation.Message) ||
			!reserveSearchMetadata(&metadataBytes, len(violation.Path)+len(violation.Message)+16) {
			return false, errors.New("invalid extraction violation")
		}
		violations[index] = models.SchemaViolation{
			Path:    strings.Clone(violation.Path),
			Message: strings.Clone(violation.Message),
		}
	}

	var extractorMetadata *models.ExtractorMetadata
	if response.Extractor != nil {
		if response.Extractor.ID == "" || response.Extractor.Mode == "" || response.Extractor.Version < 1 ||
			response.Extractor.Validation < 0 || response.Extractor.Validation > 1 ||
			len(response.Extractor.ID) > consensus.MaxPathBytes || len(response.Extractor.Mode) > MaxProviderNameBytes ||
			!utf8.ValidString(response.Extractor.ID) || !utf8.ValidString(response.Extractor.Mode) ||
			math.IsNaN(response.Extractor.Validation) || math.IsInf(response.Extractor.Validation, 0) {
			return false, errors.New("invalid extractor metadata")
		}
		compiledAt, err := canonicalSearchTime(response.Extractor.CompiledAt)
		if err != nil {
			return false, errors.New("invalid extractor metadata")
		}
		cloned := *response.Extractor
		cloned.ID = strings.Clone(response.Extractor.ID)
		cloned.Mode = strings.Clone(response.Extractor.Mode)
		cloned.CompiledAt = compiledAt
		extractorMetadata = &cloned
	}
	var llmUsage *models.LLMUsage
	if response.LLMUsage != nil {
		if !validSearchLLMUsage(*response.LLMUsage) {
			return false, errors.New("invalid extraction usage")
		}
		usage := *response.LLMUsage
		llmUsage = &usage
	}
	data := bytes.Clone(response.Data)
	if err := ctx.Err(); err != nil {
		return false, err
	}

	// Publication is intentionally last: a late deadline or any malformed
	// metadata leaves the caller's result completely free of extraction data.
	result.Data = data
	result.Basis = &basis
	result.Receipts = &receiptTokens
	unlocatedRate := wantUnlocatedRate
	result.UnlocatedRate = &unlocatedRate
	result.Violations = violations
	result.Extractor = extractorMetadata
	result.LLMUsage = llmUsage
	if err := ctx.Err(); err != nil {
		result.Data = nil
		result.Basis = nil
		result.Receipts = nil
		result.UnlocatedRate = nil
		result.Violations = nil
		result.Extractor = nil
		result.LLMUsage = nil
		return false, err
	}
	return partial, nil
}

func cloneSearchExtractionAnchor(
	source evidence.Anchor,
	cleaned string,
	snapshotID string,
	fetchedAt time.Time,
) (evidence.Anchor, bool, error) {
	if len(source.Quote) > consensus.MaxEvidenceQuoteBytes || len(source.Selector) > consensus.MaxEvidenceSelectorBytes ||
		len(source.SnapshotID) > consensus.MaxEvidenceSnapshotIDBytes || len(source.Method) > consensus.MaxEvidenceMethodBytes ||
		!utf8.ValidString(source.Quote) || !utf8.ValidString(source.Selector) || !utf8.ValidString(source.SnapshotID) ||
		source.SnapshotID != snapshotID {
		return evidence.Anchor{}, false, errors.New("invalid extraction evidence")
	}
	observedAt, err := canonicalSearchTime(source.FetchedAt)
	if err != nil || !observedAt.Equal(fetchedAt) {
		return evidence.Anchor{}, false, errors.New("invalid extraction evidence")
	}

	unlocated := false
	switch source.Method {
	case evidence.MethodExact, evidence.MethodNormalized, evidence.MethodFuzzy:
		if !searchAnchorRangeMatches(source, cleaned) {
			return evidence.Anchor{}, false, errors.New("invalid extraction evidence")
		}
	case evidence.MethodCompiled:
		if source.TextRange == [2]int{} {
			unlocated = true
		} else if !searchAnchorRangeMatches(source, cleaned) {
			return evidence.Anchor{}, false, errors.New("invalid extraction evidence")
		}
	case evidence.MethodUnlocated:
		if source.Quote != "" || source.Selector != "" || source.TextRange != [2]int{} {
			return evidence.Anchor{}, false, errors.New("invalid extraction evidence")
		}
		unlocated = true
	default:
		return evidence.Anchor{}, false, errors.New("invalid extraction evidence")
	}

	anchor := source
	anchor.Quote = strings.Clone(source.Quote)
	anchor.Selector = strings.Clone(source.Selector)
	anchor.Method = evidence.Method(strings.Clone(string(source.Method)))
	anchor.SnapshotID = strings.Clone(source.SnapshotID)
	anchor.FetchedAt = observedAt
	return anchor, unlocated, nil
}

func searchAnchorRangeMatches(anchor evidence.Anchor, cleaned string) bool {
	start, end := anchor.TextRange[0], anchor.TextRange[1]
	return start >= 0 && end > start && end <= len(cleaned) && anchor.Quote != "" && anchor.Quote == cleaned[start:end]
}

func equalSearchViolations(left, right []models.SchemaViolation) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func validSearchLLMUsage(usage models.LLMUsage) bool {
	if usage.PromptTokens < 0 || usage.CompletionTokens < 0 || usage.TotalTokens < 0 {
		return false
	}
	maximumInt := int(^uint(0) >> 1)
	return usage.PromptTokens <= maximumInt-usage.CompletionTokens &&
		usage.PromptTokens+usage.CompletionTokens == usage.TotalTokens
}

func validSearchEvidencePath(path string) bool {
	return path != "" && len(path) <= consensus.MaxPathBytes && utf8.ValidString(path)
}

func canonicalSearchTime(value time.Time) (time.Time, error) {
	if value.IsZero() {
		return time.Time{}, errors.New("search: observation time is missing")
	}
	canonical := value.Round(0).UTC()
	if _, err := canonical.MarshalJSON(); err != nil {
		return time.Time{}, errors.New("search: observation time is invalid")
	}
	return canonical, nil
}

func reserveSearchMetadata(used *int, requested int) bool {
	if used == nil || requested < 0 || *used < 0 || *used > consensus.MaxTotalMetadataBytes ||
		requested > consensus.MaxTotalMetadataBytes-*used {
		return false
	}
	*used += requested
	return true
}

func fetchResultError(ctx context.Context, err error) models.SearchResultError {
	code := models.ErrCodeNavigation
	message := "result fetch failed"
	if contextFailed(ctx, err) {
		code = models.ErrCodeTimeout
		message = "result fetch timed out"
	} else {
		var scrapeError *models.ScrapeError
		if errors.As(err, &scrapeError) {
			switch scrapeError.Code {
			case models.ErrCodeRateLimited:
				code = models.ErrCodeRateLimited
				message = "result fetch was rate limited"
			case models.ErrCodeUnauthorized:
				code = models.ErrCodeUnauthorized
				message = "result fetch was not authorized"
			case models.ErrCodeTimeout:
				code = models.ErrCodeTimeout
				message = "result fetch timed out"
			case models.ErrCodeNavigation, models.ErrCodeBrowserCrash, models.ErrCodeReadability,
				models.ErrCodeActionFailed, models.ErrCodeContentUnusable:
				code = scrapeError.Code
			}
		}
	}
	return models.SearchResultError{Stage: models.SearchResultStageFetch, Code: code, Message: message}
}

func verifyResultError(ctx context.Context, err error) models.SearchResultError {
	if contextFailed(ctx, err) {
		return models.SearchResultError{
			Stage: models.SearchResultStageVerify, Code: models.ErrCodeTimeout, Message: "result verification timed out",
		}
	}
	return models.SearchResultError{
		Stage: models.SearchResultStageVerify, Code: models.ErrCodeEvidenceUnavailable, Message: "result verification is unavailable",
	}
}

func extractResultError(ctx context.Context, err error) models.SearchResultError {
	resultError := models.SearchResultError{Stage: models.SearchResultStageExtract, Code: models.ErrCodeInternal, Message: "result extraction failed"}
	if contextFailed(ctx, err) {
		resultError.Code = models.ErrCodeTimeout
		resultError.Message = "result extraction timed out"
		return resultError
	}
	var scrapeError *models.ScrapeError
	if !errors.As(err, &scrapeError) {
		return resultError
	}
	switch scrapeError.Code {
	case models.ErrCodeExtractorUnavailable:
		resultError.Code = models.ErrCodeExtractorUnavailable
		resultError.Message = "result extractor is unavailable"
	case models.ErrCodeEvidenceUnavailable:
		resultError.Code = models.ErrCodeEvidenceUnavailable
		resultError.Message = "result extraction evidence is unavailable"
	case models.ErrCodeLLMAuthFailure:
		resultError.Code = models.ErrCodeLLMAuthFailure
		resultError.Message = "result extraction authentication failed"
	case models.ErrCodeLLMRateLimited:
		resultError.Code = models.ErrCodeLLMRateLimited
		resultError.Message = "result extraction rate limit exceeded"
	case models.ErrCodeLLMFailure:
		resultError.Code = models.ErrCodeLLMFailure
		resultError.Message = "result extraction failed"
	case models.ErrCodeTimeout:
		resultError.Code = models.ErrCodeTimeout
		resultError.Message = "result extraction timed out"
	}
	return resultError
}

func contextFailed(ctx context.Context, err error) bool {
	return ctx != nil && ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func acquireSearchSlot(ctx context.Context, slots chan struct{}) error {
	if ctx == nil {
		return context.Canceled
	}
	if slots == nil {
		return errors.New("search: work slots are unavailable")
	}
	select {
	case slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func releaseSearchSlot(slots chan struct{}) {
	if slots != nil {
		<-slots
	}
}

func safeFetchPublicArtifact(service ArtifactService, ctx context.Context, rawURL string) (artifact *extract.Artifact, err error) {
	defer func() {
		if recover() != nil {
			artifact = nil
			err = errSearchDependencyPanic
		}
	}()
	return service.FetchPublicArtifact(ctx, rawURL)
}

func safeExtractArtifact(service ArtifactService, ctx context.Context, artifact *extract.Artifact, request *models.ExtractRequest) (response *models.ExtractResponse, err error) {
	defer func() {
		if recover() != nil {
			response = nil
			err = errSearchDependencyPanic
		}
	}()
	return service.ExtractArtifact(ctx, artifact, request)
}

func safeAlignValueContext(ctx context.Context, value, cleaned, rawHTML string) (anchor evidence.Anchor, err error) {
	defer func() {
		if recover() != nil {
			anchor = evidence.Anchor{}
			err = errSearchDependencyPanic
		}
	}()
	return evidence.AlignValueContext(ctx, value, cleaned, rawHTML)
}

func safeSignReceipt(signer ReceiptSigner, payload receipts.Payload) (token string, err error) {
	defer func() {
		if recover() != nil {
			token = ""
			err = errSearchDependencyPanic
		}
	}()
	return signer.Sign(payload)
}

func boolPointer(value bool) *bool {
	return &value
}

func cloneSearchResult(source models.SearchResult) models.SearchResult {
	cloned := source
	cloned.Title = strings.Clone(source.Title)
	cloned.URL = strings.Clone(source.URL)
	cloned.FinalURL = strings.Clone(source.FinalURL)
	cloned.Snippet = strings.Clone(source.Snippet)
	cloned.Content = strings.Clone(source.Content)
	if source.Score != nil {
		value := *source.Score
		cloned.Score = &value
	}
	if source.PublishedAt != nil {
		value := *source.PublishedAt
		cloned.PublishedAt = &value
	}
	if source.Ranking != nil {
		ranking := *source.Ranking
		if source.Ranking.RelevanceScore != nil {
			value := *source.Ranking.RelevanceScore
			ranking.RelevanceScore = &value
		}
		cloned.Ranking = &ranking
	}
	return cloned
}

func isNilSearchDependency(dependency any) bool {
	if dependency == nil {
		return true
	}
	value := reflect.ValueOf(dependency)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
