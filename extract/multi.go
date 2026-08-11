package extract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/use-agent/purify/consensus"
	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/llm"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/publicnet"
	"github.com/use-agent/purify/simhash"
	"github.com/use-agent/purify/verify/eav"
	"golang.org/x/sync/errgroup"
)

const (
	defaultMultiSourceSlots = 4
	maximumMultiSourceSlots = defaultMultiSourceSlots
	maximumMultiTimeout     = 120
	// sourceJudgeTimeout bounds one entity-attribution judgment so a slow
	// judge can degrade one source to uncertain instead of stalling the
	// whole multi-source request.
	sourceJudgeTimeout = 20 * time.Second
)

type multiSourceOutcome struct {
	summary       models.MultiExtractSource
	consensus     *consensus.SourceResult
	fetchedAt     time.Time
	usageComplete bool
}

// ExtractMulti performs one bounded, evidence-required extraction per unique
// canonical source and materializes their field consensus. The caller-owned
// request is never mutated.
func (s *Service) ExtractMulti(ctx context.Context, request *models.ExtractRequest) (*models.MultiExtractResponse, error) {
	startedAt := s.now()
	prepared, sources, err := s.prepareMultiRequest(request)
	if err != nil {
		return nil, s.operationError(err, startedAt, models.ExtractTimingInfo{})
	}
	if s.safeProxyURL == "" || s.signer == nil || s.sourceSlots == nil {
		return nil, s.operationError(models.NewScrapeError(
			models.ErrCodeMultiSourceUnavailable,
			"multi-source extraction is unavailable",
			nil,
		), startedAt, models.ExtractTimingInfo{})
	}
	if _, err := s.prepareDispatch(prepared); err != nil {
		return nil, s.operationError(err, startedAt, models.ExtractTimingInfo{})
	}

	taskContext, cancel := context.WithTimeout(ctx, time.Duration(prepared.Timeout)*time.Second)
	defer cancel()
	outcomes := make([]multiSourceOutcome, len(sources))
	group, groupContext := errgroup.WithContext(taskContext)
	group.SetLimit(defaultMultiSourceSlots)
	for index, sourceURL := range sources {
		index, sourceURL := index, sourceURL
		group.Go(func() error {
			outcomes[index] = s.extractMultiSource(groupContext, prepared, sourceURL)
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, s.operationError(models.NewScrapeError(
			models.ErrCodeInternal,
			"multi-source coordination failed",
			nil,
		), startedAt, models.ExtractTimingInfo{})
	}

	sort.Slice(outcomes, func(first, second int) bool {
		if outcomes[first].summary.URL != outcomes[second].summary.URL {
			return outcomes[first].summary.URL < outcomes[second].summary.URL
		}
		return outcomes[first].summary.FinalURL < outcomes[second].summary.FinalURL
	})
	dedupeMultiFinalURLs(outcomes)
	summaries := make([]models.MultiExtractSource, len(outcomes))
	valid := make([]consensus.SourceResult, 0, len(outcomes))
	for index := range outcomes {
		summaries[index] = cloneMultiSourceSummary(outcomes[index].summary)
		if outcomes[index].consensus != nil {
			valid = append(valid, *outcomes[index].consensus)
		}
	}
	tokens, timing, usage, usageComplete, err := aggregateMultiMetrics(outcomes)
	if err != nil {
		return nil, s.operationError(models.NewScrapeError(
			models.ErrCodeInternal,
			"multi-source metrics are invalid",
			nil,
		), startedAt, models.ExtractTimingInfo{})
	}

	if s.beforeMultiMerge != nil {
		s.beforeMultiMerge()
	}
	if taskContext.Err() != nil {
		return s.multiTimeoutResponse(startedAt, summaries, tokens, timing, usage, usageComplete)
	}
	if err := s.acquireMultiSourceSlot(taskContext); err != nil {
		return s.multiTimeoutResponse(startedAt, summaries, tokens, timing, usage, usageComplete)
	}
	defer s.releaseMultiSourceSlot()
	if taskContext.Err() != nil {
		return s.multiTimeoutResponse(startedAt, summaries, tokens, timing, usage, usageComplete)
	}

	if len(valid) == 0 {
		code, message := aggregateMultiFailure(summaries)
		response := &models.MultiExtractResponse{
			Success:       false,
			Sources:       summaries,
			Tokens:        tokens,
			Timing:        timing,
			LLMUsage:      usage,
			UsageComplete: usageComplete,
			Error: &models.ErrorDetail{
				Code:    code,
				Message: message,
			},
		}
		response.Timing.TotalMs = elapsedMilliseconds(startedAt, s.now())
		sizeErr := validateMultiResponseSize(response)
		if taskContext.Err() != nil {
			return s.multiTimeoutResponse(startedAt, summaries, tokens, timing, usage, usageComplete)
		}
		if sizeErr != nil {
			return nil, s.operationError(sizeErr, startedAt, timing)
		}
		return response, models.NewScrapeError(code, message, nil)
	}

	mergeMulti := s.mergeMulti
	if mergeMulti == nil {
		mergeMulti = consensus.MergeWithMaterialization
	}
	merged, materialized, err := mergeMulti(valid)
	if taskContext.Err() != nil {
		return s.multiTimeoutResponse(startedAt, summaries, tokens, timing, usage, usageComplete)
	}
	if err != nil {
		return nil, s.operationError(models.NewScrapeError(
			models.ErrCodeInternal,
			"multi-source consensus failed",
			nil,
		), startedAt, timing)
	}
	projected := projectMultiConsensus(merged)
	if taskContext.Err() != nil {
		return s.multiTimeoutResponse(startedAt, summaries, tokens, timing, usage, usageComplete)
	}
	response := &models.MultiExtractResponse{
		Success:       true,
		Consensus:     &projected,
		Sources:       summaries,
		Tokens:        tokens,
		Timing:        timing,
		LLMUsage:      usage,
		UsageComplete: usageComplete,
	}
	switch materialized.Status {
	case consensus.MaterializationStatusAmbiguous:
		response.Status = models.MultiExtractStatusAmbiguous
	case consensus.MaterializationStatusComplete:
		violations, validationErr := llm.ValidateAgainstSchema(prepared.Schema, materialized.Data)
		if taskContext.Err() != nil {
			return s.multiTimeoutResponse(startedAt, summaries, tokens, timing, usage, usageComplete)
		}
		if validationErr != nil {
			return nil, s.operationError(models.NewScrapeError(
				models.ErrCodeInternal,
				"multi-source materialization validation failed",
				nil,
			), startedAt, timing)
		}
		if len(violations) > 0 {
			response.Status = models.MultiExtractStatusSchemaInvalid
			response.Violations = append([]models.SchemaViolation(nil), violations...)
		} else {
			response.Status = models.MultiExtractStatusComplete
			response.Data = append(json.RawMessage(nil), materialized.Data...)
		}
	default:
		return nil, s.operationError(models.NewScrapeError(
			models.ErrCodeInternal,
			"multi-source materialization returned an invalid status",
			nil,
		), startedAt, timing)
	}
	if taskContext.Err() != nil {
		return s.multiTimeoutResponse(startedAt, summaries, tokens, timing, usage, usageComplete)
	}
	response.Timing.TotalMs = elapsedMilliseconds(startedAt, s.now())
	sizeErr := validateMultiResponseSize(response)
	if taskContext.Err() != nil {
		return s.multiTimeoutResponse(startedAt, summaries, tokens, timing, usage, usageComplete)
	}
	if sizeErr != nil {
		return nil, s.operationError(sizeErr, startedAt, timing)
	}
	return response, nil
}

func (s *Service) multiTimeoutResponse(
	startedAt time.Time,
	sources []models.MultiExtractSource,
	tokens models.TokenInfo,
	timing models.ExtractTimingInfo,
	usage *models.LLMUsage,
	usageComplete bool,
) (*models.MultiExtractResponse, error) {
	timing.TotalMs = elapsedMilliseconds(startedAt, s.now())
	response := &models.MultiExtractResponse{
		Success:       false,
		Sources:       append([]models.MultiExtractSource(nil), sources...),
		Tokens:        tokens,
		Timing:        timing,
		LLMUsage:      cloneLLMUsage(usage),
		UsageComplete: usageComplete,
		Error: &models.ErrorDetail{
			Code:    models.ErrCodeTimeout,
			Message: "multi-source extraction timed out",
		},
	}
	if err := validateMultiResponseSize(response); err != nil {
		return response, models.NewScrapeError(models.ErrCodeTimeout, "multi-source extraction timed out", nil)
	}
	return response, models.NewScrapeError(models.ErrCodeTimeout, "multi-source extraction timed out", nil)
}

// EncodeMultiResponse keeps the final JSON materialization under the same
// process-wide slots as source work and consensus CPU. It never returns bytes
// produced after the caller's deadline.
func (s *Service) EncodeMultiResponse(ctx context.Context, response *models.MultiExtractResponse) ([]byte, error) {
	if s == nil || s.sourceSlots == nil {
		return nil, models.NewScrapeError(models.ErrCodeMultiSourceUnavailable, "multi-source extraction is unavailable", nil)
	}
	if err := ctx.Err(); err != nil {
		return nil, models.NewScrapeError(models.ErrCodeTimeout, "multi-source response encoding timed out", err)
	}
	if err := s.acquireMultiSourceSlot(ctx); err != nil {
		return nil, models.NewScrapeError(models.ErrCodeTimeout, "multi-source response encoding timed out", err)
	}
	defer s.releaseMultiSourceSlot()
	if err := ctx.Err(); err != nil {
		return nil, models.NewScrapeError(models.ErrCodeTimeout, "multi-source response encoding timed out", err)
	}
	sizeErr := validateMultiResponseSize(response)
	if err := ctx.Err(); err != nil {
		return nil, models.NewScrapeError(models.ErrCodeTimeout, "multi-source response encoding timed out", err)
	}
	if sizeErr != nil {
		return nil, sizeErr
	}
	if err := ctx.Err(); err != nil {
		return nil, models.NewScrapeError(models.ErrCodeTimeout, "multi-source response encoding timed out", err)
	}
	encodeMulti := s.encodeMulti
	if encodeMulti == nil {
		encodeMulti = json.Marshal
	}
	encoded, err := encodeMulti(response)
	if err := ctx.Err(); err != nil {
		return nil, models.NewScrapeError(models.ErrCodeTimeout, "multi-source response encoding timed out", err)
	}
	if err != nil || len(encoded) > models.MaxMultiExtractResponseBytes {
		return nil, models.NewScrapeError(models.ErrCodeInternal, "multi-source response exceeds its output budget", nil)
	}
	return encoded, nil
}

func (s *Service) prepareMultiRequest(request *models.ExtractRequest) (*models.ExtractRequest, []string, error) {
	if request == nil {
		return nil, nil, invalidExtractRequest("extract request is required")
	}
	if request.URL != "" || request.Sources == nil || len(request.Sources) < 1 ||
		len(request.Sources) > models.MaxExtractSources {
		return nil, nil, invalidExtractRequest("exactly one of url and one to eight sources is required")
	}
	if request.Timeout < 0 || request.Timeout > maximumMultiTimeout {
		return nil, nil, invalidExtractRequest("timeout must be between 1 and 120 seconds when set")
	}
	if request.ProxyURL != "" {
		return nil, nil, invalidExtractRequest("proxy_url is not supported for multi-source extraction")
	}
	if request.CSSSelector != "" || request.OutputFormat != "" && request.OutputFormat != "markdown" ||
		request.ExtractMode != "" && request.ExtractMode != "readability" {
		return nil, nil, invalidExtractRequest("multi-source extraction requires the default content profile")
	}

	used := 0
	seen := make(map[string]struct{}, len(request.Sources))
	sources := make([]string, 0, len(request.Sources))
	for _, rawSource := range request.Sources {
		if len(rawSource) == 0 || len(rawSource) > models.MaxExtractSourceURLBytes ||
			len(rawSource) > models.MaxExtractSourcesURLBytes-used {
			return nil, nil, invalidExtractRequest("source URLs exceed the request budget")
		}
		used += len(rawSource)
		if err := validateExtractText("source", rawSource, models.MaxExtractSourceURLBytes, false); err != nil {
			return nil, nil, err
		}
		canonical, _, err := publicnet.NormalizeHTTPURL(rawSource, nil, false)
		if err != nil || len(canonical) > models.MaxExtractSourceURLBytes {
			return nil, nil, invalidExtractRequest("sources must contain canonicalizable public HTTP URLs")
		}
		if _, duplicate := seen[canonical]; duplicate {
			continue
		}
		seen[canonical] = struct{}{}
		sources = append(sources, canonical)
	}
	if len(sources) == 0 {
		return nil, nil, invalidExtractRequest("sources contain no canonical URLs")
	}
	sort.Strings(sources)

	base := *request
	base.URL = sources[0]
	base.Sources = nil
	prepared, err := prepareRequest(&base)
	if err != nil {
		return nil, nil, err
	}
	prepared.Evidence = true
	prepared.ProxyURL = s.safeProxyURL
	return prepared, sources, nil
}

func (s *Service) extractMultiSource(
	ctx context.Context,
	prepared *models.ExtractRequest,
	sourceURL string,
) (outcome multiSourceOutcome) {
	outcome.summary = models.MultiExtractSource{
		URL:     sourceURL,
		Status:  models.MultiExtractSourceStatusExtractionFailed,
		Success: false,
	}
	outcome.usageComplete = true
	enteredExtraction := false
	startedAt := s.now()
	defer func() {
		if recover() != nil {
			outcome.consensus = nil
			outcome.usageComplete = !enteredExtraction
			outcome.summary = failedMultiSource(
				sourceURL,
				models.MultiExtractSourceStatusExtractionFailed,
				models.ErrCodeInternal,
				"source extraction failed",
				models.ExtractTimingInfo{TotalMs: elapsedMilliseconds(startedAt, s.now())},
			)
		}
	}()
	if err := s.acquireMultiSourceSlot(ctx); err != nil {
		outcome.summary = failedMultiSource(
			sourceURL,
			models.MultiExtractSourceStatusTimeout,
			models.ErrCodeTimeout,
			"source timed out",
			models.ExtractTimingInfo{TotalMs: elapsedMilliseconds(startedAt, s.now())},
		)
		return outcome
	}
	defer s.releaseMultiSourceSlot()

	request := *prepared
	request.URL = sourceURL
	request.Sources = nil
	scrapeRequest := request.ToScrapeRequest()
	scrapeRequest.MaximumBodyBytes = maximumExtractArtifactBytes
	artifact, err := s.FetchArtifact(ctx, scrapeRequest)
	if err != nil {
		status, code, message := classifyMultiFetchError(ctx, err)
		outcome.summary = failedMultiSource(sourceURL, status, code, message,
			models.ExtractTimingInfo{TotalMs: elapsedMilliseconds(startedAt, s.now())})
		return outcome
	}
	if artifact == nil || artifact.Public == nil {
		outcome.summary = failedMultiSource(
			sourceURL,
			models.MultiExtractSourceStatusFetchFailed,
			models.ErrCodeNavigation,
			"source fetch failed",
			models.ExtractTimingInfo{TotalMs: elapsedMilliseconds(startedAt, s.now())},
		)
		return outcome
	}
	if artifact.Source != nil && (artifact.Source.StatusCode < 200 || artifact.Source.StatusCode >= 300) {
		outcome.summary = failedMultiSource(
			sourceURL,
			models.MultiExtractSourceStatusFetchFailed,
			models.ErrCodeNavigation,
			"source fetch failed",
			models.ExtractTimingInfo{TotalMs: elapsedMilliseconds(startedAt, s.now())},
		)
		return outcome
	}
	if artifact.Source == nil || artifact.Source.SnapshotID == "" || artifact.Source.FetchedAt.IsZero() ||
		strings.TrimSpace(artifact.Source.RawHTML) == "" {
		outcome.summary = failedMultiSource(
			sourceURL,
			models.MultiExtractSourceStatusEvidenceUnavailable,
			models.ErrCodeEvidenceUnavailable,
			"source evidence is unavailable",
			models.ExtractTimingInfo{TotalMs: elapsedMilliseconds(startedAt, s.now())},
		)
		return outcome
	}
	enteredExtraction = true
	outcome.usageComplete = false
	response, err := s.ExtractArtifact(ctx, artifact, &request)
	if err != nil {
		timing, _ := TimingFromError(err)
		if timing.TotalMs == 0 {
			timing.TotalMs = elapsedMilliseconds(startedAt, s.now())
		}
		status, code, message := classifyMultiExtractionError(ctx, err)
		outcome.summary = failedMultiSource(sourceURL, status, code, message, timing)
		return outcome
	}
	if response == nil || !response.Success {
		outcome.summary = failedMultiSource(
			sourceURL,
			models.MultiExtractSourceStatusExtractionFailed,
			models.ErrCodeLLMFailure,
			"source extraction failed",
			models.ExtractTimingInfo{TotalMs: elapsedMilliseconds(startedAt, s.now())},
		)
		return outcome
	}
	outcome.summary.Tokens = response.Tokens
	outcome.summary.Timing = response.Timing
	outcome.summary.Timing.TotalMs = elapsedMilliseconds(startedAt, s.now())
	outcome.summary.LLMUsage = cloneLLMUsage(response.LLMUsage)
	outcome.usageComplete = response.Extractor != nil || response.LLMUsage != nil
	if err := validateMultiSourceMetrics(outcome.summary); err != nil {
		outcome.summary = failedMultiSource(
			sourceURL,
			models.MultiExtractSourceStatusExtractionFailed,
			models.ErrCodeInternal,
			"source extraction failed",
			models.ExtractTimingInfo{TotalMs: elapsedMilliseconds(startedAt, s.now())},
		)
		outcome.usageComplete = false
		return outcome
	}
	if response.Partial || len(response.Violations) > 0 {
		outcome.summary.Success = false
		outcome.summary.Status = models.MultiExtractSourceStatusPartial
		outcome.summary.Error = &models.ErrorDetail{Code: models.ErrCodeLLMFailure, Message: "source extraction was partial"}
		return outcome
	}
	violations, err := llm.ValidateAgainstSchema(prepared.Schema, response.Data)
	if err != nil || len(violations) > 0 {
		outcome.summary.Success = false
		outcome.summary.Status = models.MultiExtractSourceStatusSchemaInvalid
		outcome.summary.Error = &models.ErrorDetail{Code: models.ErrCodeInvalidInput, Message: "source output did not satisfy schema"}
		return outcome
	}
	if err := validateMultiSourceEvidence(artifact, response); err != nil {
		outcome.summary.Success = false
		outcome.summary.Status = models.MultiExtractSourceStatusEvidenceUnavailable
		outcome.summary.Error = &models.ErrorDetail{Code: models.ErrCodeEvidenceUnavailable, Message: "source evidence is unavailable"}
		return outcome
	}
	rawFinalURL := artifact.Source.FinalURL
	if strings.TrimSpace(rawFinalURL) == "" {
		rawFinalURL = sourceURL
	}
	finalURL, _, err := publicnet.NormalizeHTTPURL(rawFinalURL, nil, false)
	if err != nil || len(finalURL) > consensus.MaxURLBytes {
		outcome.summary.Success = false
		outcome.summary.Status = models.MultiExtractSourceStatusFetchFailed
		outcome.summary.Error = &models.ErrorDetail{Code: models.ErrCodeNavigation, Message: "source final URL is invalid"}
		return outcome
	}
	basis := cloneConsensusBasis(*response.Basis)
	receiptTokens := cloneConsensusReceipts(*response.Receipts)
	candidate := consensus.SourceResult{
		URL:         finalURL,
		Data:        append(json.RawMessage(nil), response.Data...),
		Basis:       basis,
		Receipts:    receiptTokens,
		SimText:     simhash.Fingerprint(artifact.Public.Content),
		CleanedText: artifact.Public.Content,
	}
	if _, err := consensus.Merge([]consensus.SourceResult{candidate}); err != nil {
		outcome.summary.Success = false
		outcome.summary.Status = models.MultiExtractSourceStatusEvidenceUnavailable
		outcome.summary.Error = &models.ErrorDetail{
			Code:    models.ErrCodeEvidenceUnavailable,
			Message: "source evidence is unavailable",
		}
		return outcome
	}
	attribution := s.judgeSourceAttribution(ctx, prepared.ExpectedSubject, finalURL, artifact)
	if applyEntityAttribution(&outcome.summary, attribution, finalURL, response.SnapshotID) {
		return outcome
	}
	outcome.summary.Success = true
	outcome.summary.Status = models.MultiExtractSourceStatusValid
	outcome.summary.FinalURL = finalURL
	outcome.summary.SnapshotID = response.SnapshotID
	outcome.fetchedAt = artifact.Source.FetchedAt
	outcome.consensus = &candidate
	return outcome
}

// judgeSourceAttribution judges one fully valid source document against the
// request's expected subject. It never fails the source: a missing judge or
// subject yields no attribution, and judge errors or timeouts degrade to an
// uncertain attribution. Judging runs against the same artifact that
// produced the source's evidence, so verdict quotes anchor in the content
// the extraction consumed.
func (s *Service) judgeSourceAttribution(
	ctx context.Context,
	spec *models.SubjectSpec,
	finalURL string,
	artifact *Artifact,
) *models.EntityAttribution {
	if s.sourceJudge == nil || spec == nil || artifact == nil || artifact.Public == nil || artifact.Source == nil {
		return nil
	}
	judgeContext, cancel := context.WithTimeout(ctx, sourceJudgeTimeout)
	defer cancel()
	judgment, err := s.sourceJudge.JudgeDocument(
		judgeContext,
		eav.Subject{Name: spec.Name, Hint: spec.Hint},
		eav.Document{
			URL:     finalURL,
			Title:   artifact.Public.Metadata.Title,
			Cleaned: artifact.Public.Content,
			RawHTML: artifact.Source.RawHTML,
		},
	)
	if err != nil {
		return &models.EntityAttribution{Verdict: models.EntityVerdictUncertain}
	}
	switch judgment.Verdict {
	case eav.VerdictMatch, eav.VerdictMismatch, eav.VerdictUncertain:
	default:
		return &models.EntityAttribution{Verdict: models.EntityVerdictUncertain}
	}
	attribution := &models.EntityAttribution{
		Verdict: string(judgment.Verdict),
		Tier:    string(judgment.Tier),
	}
	if judgment.DocEntity != nil {
		attribution.Name = judgment.DocEntity.Name
		attribution.Kind = string(judgment.DocEntity.Kind)
	}
	if judgment.Evidence != nil {
		attribution.Quote = judgment.Evidence.Quote
	}
	return attribution
}

// applyEntityAttribution attaches one attribution to a fully valid source
// summary and reports whether the source must be excluded from consensus. A
// support about the wrong entity is not support: the excluded source keeps
// its metrics and snapshot identity for accounting, but never contributes a
// consensus candidate.
func applyEntityAttribution(
	summary *models.MultiExtractSource,
	attribution *models.EntityAttribution,
	finalURL string,
	snapshotID string,
) bool {
	if attribution == nil {
		return false
	}
	summary.Entity = attribution
	if attribution.Verdict != models.EntityVerdictMismatch {
		return false
	}
	summary.Success = false
	summary.Status = models.MultiExtractSourceStatusEntityMismatch
	summary.FinalURL = finalURL
	summary.SnapshotID = snapshotID
	summary.Error = &models.ErrorDetail{
		Code:    models.ErrCodeEntityMismatch,
		Message: "source is about a different entity than the expected subject",
	}
	return true
}

func dedupeMultiFinalURLs(outcomes []multiSourceOutcome) {
	winners := make(map[string]int, len(outcomes))
	for index := range outcomes {
		if outcomes[index].consensus == nil {
			continue
		}
		finalURL := outcomes[index].summary.FinalURL
		winner, exists := winners[finalURL]
		if !exists || outcomes[index].fetchedAt.After(outcomes[winner].fetchedAt) ||
			outcomes[index].fetchedAt.Equal(outcomes[winner].fetchedAt) && outcomes[index].summary.URL < outcomes[winner].summary.URL {
			winners[finalURL] = index
		}
	}
	for index := range outcomes {
		if outcomes[index].consensus == nil {
			continue
		}
		winner := winners[outcomes[index].summary.FinalURL]
		if winner == index {
			continue
		}
		outcomes[index].consensus = nil
		outcomes[index].summary.Success = false
		outcomes[index].summary.Status = models.MultiExtractSourceStatusDuplicate
		outcomes[index].summary.DuplicateOf = outcomes[winner].summary.URL
		outcomes[index].summary.Error = nil
	}
}

func (s *Service) acquireMultiSourceSlot(ctx context.Context) error {
	select {
	case s.sourceSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) releaseMultiSourceSlot() {
	<-s.sourceSlots
}

func normalizeMultiSafeProxyURL(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if strings.TrimSpace(raw) != raw || len(raw) > maximumExtractURLBytes {
		return "", errors.New("extract: safe multi-source proxy is invalid")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "socks5" || parsed.Opaque != "" || parsed.User != nil ||
		parsed.Path != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery ||
		parsed.Fragment != "" || parsed.Host == "" {
		return "", errors.New("extract: safe multi-source proxy is invalid")
	}
	address, err := netip.ParseAddr(parsed.Hostname())
	if err != nil || address.Zone() != "" {
		return "", errors.New("extract: safe multi-source proxy must use a literal loopback address")
	}
	address = address.Unmap()
	if !address.IsLoopback() {
		return "", errors.New("extract: safe multi-source proxy must use a literal loopback address")
	}
	port, err := strconv.ParseUint(parsed.Port(), 10, 16)
	if err != nil || port == 0 {
		return "", errors.New("extract: safe multi-source proxy requires a valid port")
	}
	return "socks5://" + netip.AddrPortFrom(address, uint16(port)).String(), nil
}

func normalizeMultiSourceSlots(value int) (int, error) {
	if value == 0 {
		return defaultMultiSourceSlots, nil
	}
	if value < 1 || value > maximumMultiSourceSlots {
		return 0, fmt.Errorf("extract: source slots must be between 1 and %d", maximumMultiSourceSlots)
	}
	return value, nil
}

func failedMultiSource(
	url string,
	status models.MultiExtractSourceStatus,
	code string,
	message string,
	timing models.ExtractTimingInfo,
) models.MultiExtractSource {
	return models.MultiExtractSource{
		URL:     url,
		Success: false,
		Status:  status,
		Timing:  timing,
		Error:   &models.ErrorDetail{Code: code, Message: message},
	}
}

func multiSourceTimedOut(ctx context.Context, err error) bool {
	if ctx != nil && ctx.Err() != nil {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var scrapeError *models.ScrapeError
	return errors.As(err, &scrapeError) && scrapeError.Code == models.ErrCodeTimeout
}

func classifyMultiFetchError(ctx context.Context, err error) (models.MultiExtractSourceStatus, string, string) {
	if multiSourceTimedOut(ctx, err) {
		return models.MultiExtractSourceStatusTimeout, models.ErrCodeTimeout, "source timed out"
	}
	status := models.MultiExtractSourceStatusFetchFailed
	var scrapeError *models.ScrapeError
	if !errors.As(err, &scrapeError) {
		return status, models.ErrCodeNavigation, "source fetch failed"
	}
	switch scrapeError.Code {
	case models.ErrCodeInvalidInput:
		return models.MultiExtractSourceStatusEvidenceUnavailable, models.ErrCodeMultiSourceUnavailable, "source fetch capability is unavailable"
	case models.ErrCodeEvidenceUnavailable:
		return models.MultiExtractSourceStatusEvidenceUnavailable, scrapeError.Code, "source evidence is unavailable"
	case models.ErrCodeUnauthorized, models.ErrCodeLLMAuthFailure:
		return status, scrapeError.Code, "source authentication failed"
	case models.ErrCodeRateLimited, models.ErrCodeLLMRateLimited:
		return status, scrapeError.Code, "source rate limit exceeded"
	case models.ErrCodeExtractorUnavailable:
		return status, scrapeError.Code, "source extractor is unavailable"
	case models.ErrCodeInternal:
		return status, scrapeError.Code, "source fetch failed"
	case models.ErrCodeNavigation, models.ErrCodeBrowserCrash, models.ErrCodeReadability,
		models.ErrCodeActionFailed, models.ErrCodeContentUnusable:
		return status, scrapeError.Code, "source fetch failed"
	default:
		return status, models.ErrCodeNavigation, "source fetch failed"
	}
}

func classifyMultiExtractionError(ctx context.Context, err error) (models.MultiExtractSourceStatus, string, string) {
	if multiSourceTimedOut(ctx, err) {
		return models.MultiExtractSourceStatusTimeout, models.ErrCodeTimeout, "source timed out"
	}
	status := models.MultiExtractSourceStatusExtractionFailed
	code := models.ErrCodeLLMFailure
	message := "source extraction failed"
	var scrapeError *models.ScrapeError
	if !errors.As(err, &scrapeError) {
		return status, code, message
	}
	switch scrapeError.Code {
	case models.ErrCodeEvidenceUnavailable:
		return models.MultiExtractSourceStatusEvidenceUnavailable, scrapeError.Code, "source evidence is unavailable"
	case models.ErrCodeExtractorUnavailable:
		return status, scrapeError.Code, "source extractor is unavailable"
	case models.ErrCodeLLMAuthFailure, models.ErrCodeUnauthorized:
		return status, scrapeError.Code, "source authentication failed"
	case models.ErrCodeLLMRateLimited, models.ErrCodeRateLimited:
		return status, scrapeError.Code, "source rate limit exceeded"
	case models.ErrCodeLLMFailure:
		return status, scrapeError.Code, message
	case models.ErrCodeInvalidInput:
		return status, scrapeError.Code, "source extraction output is invalid"
	case models.ErrCodeNavigation:
		return models.MultiExtractSourceStatusFetchFailed, scrapeError.Code, "source fetch failed"
	case models.ErrCodeInternal:
		return status, scrapeError.Code, "source extraction failed"
	default:
		return status, code, message
	}
}

func aggregateMultiFailure(sources []models.MultiExtractSource) (string, string) {
	if len(sources) == 0 {
		return models.ErrCodeNoValidSource, "no valid extraction source"
	}
	allTimeout := true
	allEvidenceUnavailable := true
	hasInternal := false
	hasAuthenticationFailure := false
	hasRateLimit := false
	hasExtractorUnavailable := false
	hasMultiUnavailable := false
	for _, source := range sources {
		allTimeout = allTimeout && source.Status == models.MultiExtractSourceStatusTimeout
		allEvidenceUnavailable = allEvidenceUnavailable && source.Status == models.MultiExtractSourceStatusEvidenceUnavailable
		if source.Error == nil {
			continue
		}
		switch source.Error.Code {
		case models.ErrCodeInternal:
			hasInternal = true
		case models.ErrCodeLLMAuthFailure, models.ErrCodeUnauthorized:
			hasAuthenticationFailure = true
		case models.ErrCodeLLMRateLimited, models.ErrCodeRateLimited:
			hasRateLimit = true
		case models.ErrCodeExtractorUnavailable:
			hasExtractorUnavailable = true
		case models.ErrCodeMultiSourceUnavailable:
			hasMultiUnavailable = true
		}
	}
	switch {
	case allTimeout:
		return models.ErrCodeTimeout, "multi-source extraction timed out"
	case allEvidenceUnavailable:
		return models.ErrCodeMultiSourceUnavailable, "multi-source extraction is unavailable"
	case hasInternal:
		return models.ErrCodeInternal, "multi-source extraction failed"
	case hasMultiUnavailable:
		return models.ErrCodeMultiSourceUnavailable, "multi-source extraction is unavailable"
	case hasAuthenticationFailure:
		return models.ErrCodeLLMAuthFailure, "multi-source authentication failed"
	case hasRateLimit:
		return models.ErrCodeLLMRateLimited, "multi-source rate limit exceeded"
	case hasExtractorUnavailable:
		return models.ErrCodeExtractorUnavailable, "multi-source extractor is unavailable"
	default:
		return models.ErrCodeNoValidSource, "no valid extraction source"
	}
}

func allMultiSourcesEvidenceUnavailable(sources []models.MultiExtractSource) bool {
	if len(sources) == 0 {
		return false
	}
	for _, source := range sources {
		if source.Status != models.MultiExtractSourceStatusEvidenceUnavailable {
			return false
		}
	}
	return true
}

func validateMultiSourceEvidence(artifact *Artifact, response *models.ExtractResponse) error {
	if artifact == nil || artifact.Public == nil || artifact.Source == nil || response == nil || response.SnapshotID == "" ||
		string(artifact.Source.SnapshotID) == "" || response.SnapshotID != string(artifact.Source.SnapshotID) ||
		artifact.Source.FetchedAt.IsZero() || response.Basis == nil || response.Receipts == nil || response.UnlocatedRate == nil {
		return errors.New("missing source evidence")
	}
	unlocatedRate := *response.UnlocatedRate
	if math.IsNaN(unlocatedRate) || math.IsInf(unlocatedRate, 0) ||
		unlocatedRate < 0 || unlocatedRate > 1 || unlocatedRate != 0 {
		return errors.New("source evidence contains unlocated values")
	}
	leaves, err := evidence.LeafValues(response.Data)
	if err != nil || len(*response.Basis) != len(leaves) || len(*response.Receipts) != len(leaves) {
		return errors.New("source evidence paths do not match data")
	}
	content := artifact.Public.Content
	for path := range leaves {
		anchor, hasAnchor := (*response.Basis)[path]
		receipt, hasReceipt := (*response.Receipts)[path]
		if !hasAnchor || !hasReceipt || receipt == "" || anchor.SnapshotID != response.SnapshotID ||
			anchor.FetchedAt.IsZero() || !anchor.FetchedAt.Equal(artifact.Source.FetchedAt) {
			return errors.New("source evidence is incomplete")
		}
		switch anchor.Method {
		case evidence.MethodExact, evidence.MethodNormalized, evidence.MethodFuzzy, evidence.MethodCompiled:
		default:
			return errors.New("source evidence method is not admissible")
		}
		start, end := anchor.TextRange[0], anchor.TextRange[1]
		if anchor.Quote == "" || start < 0 || end <= start || end > len(content) || content[start:end] != anchor.Quote {
			return errors.New("source evidence range is not located in cleaned content")
		}
	}
	return nil
}

func validateMultiSourceMetrics(source models.MultiExtractSource) error {
	if source.Tokens.OriginalEstimate < 0 || source.Tokens.CleanedEstimate < 0 ||
		source.Tokens.CleanedEstimate > source.Tokens.OriginalEstimate ||
		math.IsNaN(source.Tokens.SavingsPercent) || math.IsInf(source.Tokens.SavingsPercent, 0) ||
		source.Tokens.SavingsPercent < 0 || source.Tokens.SavingsPercent > 100 ||
		source.Timing.TotalMs < 0 || source.Timing.NavigationMs < 0 ||
		source.Timing.CleaningMs < 0 || source.Timing.ExtractionMs < 0 {
		return errors.New("invalid source metrics")
	}
	if source.LLMUsage != nil {
		promptAndCompletion, ok := checkedAddInt(source.LLMUsage.PromptTokens, source.LLMUsage.CompletionTokens)
		if !ok || source.LLMUsage.TotalTokens != promptAndCompletion {
			return errors.New("invalid source usage")
		}
	}
	return nil
}

func aggregateMultiMetrics(outcomes []multiSourceOutcome) (
	models.TokenInfo,
	models.ExtractTimingInfo,
	*models.LLMUsage,
	bool,
	error,
) {
	tokens := models.TokenInfo{}
	timing := models.ExtractTimingInfo{}
	usage := &models.LLMUsage{}
	hasUsage := false
	usageComplete := true
	for _, outcome := range outcomes {
		var ok bool
		if tokens.OriginalEstimate, ok = checkedAddInt(tokens.OriginalEstimate, outcome.summary.Tokens.OriginalEstimate); !ok {
			return models.TokenInfo{}, models.ExtractTimingInfo{}, nil, false, errors.New("token total overflow")
		}
		if tokens.CleanedEstimate, ok = checkedAddInt(tokens.CleanedEstimate, outcome.summary.Tokens.CleanedEstimate); !ok {
			return models.TokenInfo{}, models.ExtractTimingInfo{}, nil, false, errors.New("token total overflow")
		}
		if timing.NavigationMs, ok = checkedAddInt64(timing.NavigationMs, outcome.summary.Timing.NavigationMs); !ok {
			return models.TokenInfo{}, models.ExtractTimingInfo{}, nil, false, errors.New("timing total overflow")
		}
		if timing.CleaningMs, ok = checkedAddInt64(timing.CleaningMs, outcome.summary.Timing.CleaningMs); !ok {
			return models.TokenInfo{}, models.ExtractTimingInfo{}, nil, false, errors.New("timing total overflow")
		}
		if timing.ExtractionMs, ok = checkedAddInt64(timing.ExtractionMs, outcome.summary.Timing.ExtractionMs); !ok {
			return models.TokenInfo{}, models.ExtractTimingInfo{}, nil, false, errors.New("timing total overflow")
		}
		if !outcome.usageComplete {
			usageComplete = false
		}
		if outcome.summary.LLMUsage == nil {
			continue
		}
		hasUsage = true
		if usage.PromptTokens, ok = checkedAddInt(usage.PromptTokens, outcome.summary.LLMUsage.PromptTokens); !ok {
			return models.TokenInfo{}, models.ExtractTimingInfo{}, nil, false, errors.New("usage total overflow")
		}
		if usage.CompletionTokens, ok = checkedAddInt(usage.CompletionTokens, outcome.summary.LLMUsage.CompletionTokens); !ok {
			return models.TokenInfo{}, models.ExtractTimingInfo{}, nil, false, errors.New("usage total overflow")
		}
		if usage.TotalTokens, ok = checkedAddInt(usage.TotalTokens, outcome.summary.LLMUsage.TotalTokens); !ok {
			return models.TokenInfo{}, models.ExtractTimingInfo{}, nil, false, errors.New("usage total overflow")
		}
	}
	if tokens.OriginalEstimate > 0 {
		if tokens.CleanedEstimate > tokens.OriginalEstimate {
			return models.TokenInfo{}, models.ExtractTimingInfo{}, nil, false, errors.New("aggregate token totals are invalid")
		}
		tokens.SavingsPercent = float64(tokens.OriginalEstimate-tokens.CleanedEstimate) /
			float64(tokens.OriginalEstimate) * 100
	}
	if !hasUsage {
		usage = nil
	}
	return tokens, timing, usage, usageComplete, nil
}

func checkedAddInt(first, second int) (int, bool) {
	if first < 0 || second < 0 || second > int(^uint(0)>>1)-first {
		return 0, false
	}
	return first + second, true
}

func checkedAddInt64(first, second int64) (int64, bool) {
	const maximum = int64(^uint64(0) >> 1)
	if first < 0 || second < 0 || second > maximum-first {
		return 0, false
	}
	return first + second, true
}

func cloneConsensusBasis(input models.EvidenceBasis) map[string]evidence.Anchor {
	output := make(map[string]evidence.Anchor, len(input))
	for path, anchor := range input {
		output[path] = anchor
	}
	return output
}

func cloneConsensusReceipts(input models.FieldReceipts) map[string]string {
	output := make(map[string]string, len(input))
	for path, receipt := range input {
		output[path] = receipt
	}
	return output
}

func cloneMultiSourceSummary(input models.MultiExtractSource) models.MultiExtractSource {
	input.LLMUsage = cloneLLMUsage(input.LLMUsage)
	if input.Error != nil {
		detail := *input.Error
		input.Error = &detail
	}
	if input.Entity != nil {
		entity := *input.Entity
		input.Entity = &entity
	}
	return input
}

func projectMultiConsensus(input consensus.Result) models.MultiExtractConsensus {
	fields := make(map[string]models.MultiExtractFieldConsensus, len(input.Fields))
	for path, field := range input.Fields {
		conflicts := make([]models.MultiExtractConflict, len(field.Conflicts))
		for index, conflict := range field.Conflicts {
			conflicts[index] = models.MultiExtractConflict{
				Value:     append(json.RawMessage(nil), conflict.Value...),
				Agreement: projectMultiAgreement(conflict.Agreement),
				Supports:  projectMultiSupports(conflict.Supports),
			}
		}
		fields[path] = models.MultiExtractFieldConsensus{
			Value:     append(json.RawMessage(nil), field.Value...),
			Agreement: projectMultiAgreement(field.Agreement),
			Supports:  projectMultiSupports(field.Supports),
			Conflicts: conflicts,
			Ambiguous: field.Ambiguous,
		}
	}
	return models.MultiExtractConsensus{Fields: fields}
}

func projectMultiAgreement(input consensus.Agreement) models.MultiExtractAgreement {
	return models.MultiExtractAgreement{
		Pages:            input.Pages,
		IndependentRoots: input.IndependentRoots,
		FoldReason:       projectMultiFoldReason(input.FoldReason),
	}
}

func projectMultiSupports(input []consensus.Support) []models.MultiExtractSupport {
	output := make([]models.MultiExtractSupport, len(input))
	for index, support := range input {
		output[index] = models.MultiExtractSupport{
			URL:        support.URL,
			Root:       support.Root,
			Receipt:    support.Receipt,
			FoldReason: projectMultiFoldReason(support.FoldReason),
		}
		if support.Evidence != nil {
			anchor := *support.Evidence
			output[index].Evidence = &anchor
		}
	}
	return output
}

func projectMultiFoldReason(input consensus.FoldReason) models.MultiExtractFoldReason {
	switch input {
	case consensus.FoldReasonSameRoot:
		return models.MultiExtractFoldReasonSameRoot
	case consensus.FoldReasonNearDuplicate:
		return models.MultiExtractFoldReasonNearDuplicate
	case consensus.FoldReasonQuoteLineage:
		return models.MultiExtractFoldReasonQuoteLineage
	default:
		return ""
	}
}

func validateMultiResponseSize(response *models.MultiExtractResponse) error {
	if response == nil {
		return models.NewScrapeError(models.ErrCodeInternal, "multi-source response exceeds its output budget", nil)
	}
	remaining := models.MaxMultiExtractResponseBytes
	if !reserveMultiResponseBytes(&remaining, 2) {
		return models.NewScrapeError(models.ErrCodeInternal, "multi-source response exceeds its output budget", nil)
	}
	fields := 0
	addField := func(name string, value any) bool {
		encodedName, err := json.Marshal(name)
		if err != nil {
			return false
		}
		encodedValue, err := json.Marshal(value)
		if err != nil {
			return false
		}
		if fields > 0 && !reserveMultiResponseBytes(&remaining, 1) {
			return false
		}
		if !reserveMultiResponseBytes(&remaining, len(encodedName)) ||
			!reserveMultiResponseBytes(&remaining, 1) ||
			!reserveMultiResponseBytes(&remaining, len(encodedValue)) {
			return false
		}
		fields++
		return true
	}
	valid := addField("success", response.Success)
	if response.Status != "" {
		valid = valid && addField("status", response.Status)
	}
	if len(response.Data) > 0 {
		valid = valid && addField("data", response.Data)
	}
	if response.Consensus != nil {
		valid = valid && addField("consensus", response.Consensus)
	}
	valid = valid && addField("sources", response.Sources)
	if len(response.Violations) > 0 {
		valid = valid && addField("violations", response.Violations)
	}
	valid = valid && addField("tokens", response.Tokens) && addField("timing", response.Timing)
	if response.LLMUsage != nil {
		valid = valid && addField("llm_usage", response.LLMUsage)
	}
	valid = valid && addField("usage_complete", response.UsageComplete)
	if response.Error != nil {
		valid = valid && addField("error", response.Error)
	}
	if !valid {
		return models.NewScrapeError(models.ErrCodeInternal, "multi-source response exceeds its output budget", nil)
	}
	return nil
}

func reserveMultiResponseBytes(remaining *int, requested int) bool {
	if remaining == nil || requested < 0 || requested > *remaining {
		return false
	}
	*remaining -= requested
	return true
}
