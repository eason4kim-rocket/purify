package rerankeval

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/use-agent/purify/search/rerank/replay"
)

const (
	// ScorecardWarmupsPerPath is the exact number of discarded observations
	// required for both the fresh-provider and rerank paths.
	ScorecardWarmupsPerPath = 10
	// ScorecardMeasuredPairs is the exact paired sample count used by the
	// latency and cost gates.
	ScorecardMeasuredPairs = 100
	// ScorecardP50Index and ScorecardP95Index lock nearest-rank selection for
	// the 100 sorted, zero-based measured samples.
	ScorecardP50Index = 49
	ScorecardP95Index = 94

	ScorecardMicrosecondsPerHour int64 = 3_600_000_000
	ScorecardMaximumLatencyUS    int64 = MaximumRecordingLatencyUS
	ScorecardMaximumTokenCount   int64 = 1_000_000
)

var (
	ErrInvalidScorecard    = errors.New("rerankeval: invalid latency scorecard")
	ErrScorecardGateFailed = errors.New("rerankeval: latency scorecard gate failed")
)

// ScorecardPairOrder records the actual A/B invocation order. A is a fresh
// provider baseline and B reranks that exact baseline input.
type ScorecardPairOrder string

const (
	ScorecardOrderProviderRerank ScorecardPairOrder = "provider_then_rerank"
	ScorecardOrderRerankProvider ScorecardPairOrder = "rerank_then_provider"
)

// ScorecardMetadata pins every environment identity needed to interpret a
// latency result. These values describe a run; they do not authenticate it.
type ScorecardMetadata struct {
	Hardware           string `json:"hardware"`
	GPU                string `json:"gpu"`
	Region             string `json:"region"`
	RuntimeImageDigest string `json:"runtime_image_digest"`
	RuntimeVersion     string `json:"runtime_version"`
	ModelID            string `json:"model_id"`
	ModelRevision      string `json:"model_revision"`
	TemplateSHA256     string `json:"template_sha256"`
	Provider           string `json:"provider"`
	ProviderVersion    string `json:"provider_version"`
}

// ScorecardManagedPrice is a managed reranker price expressed per measured
// query batch in the enclosing snapshot currency.
type ScorecardManagedPrice struct {
	RerankBatchCost float64 `json:"rerank_batch_cost"`
}

// ScorecardLocalPrice provides every price operand of the local GPU-hour
// formula. Each measured rerank observation is already one complete batch, so
// no caller-supplied amortization divisor may reduce its cost.
type ScorecardLocalPrice struct {
	GPUHourlyCost float64 `json:"gpu_hourly_cost"`
	GPUCount      int     `json:"gpu_count"`
}

// ScorecardPriceSnapshot compares like-currency provider and inference costs.
// Exactly one of Managed and Local must be present. Source and Revision bind
// the captured daily price material without asking an offline test to trust a
// moving network endpoint. EvidenceSHA256 identifies the committed source
// artifact; it is provenance, not authentication of the publisher.
type ScorecardPriceSnapshot struct {
	Date             string                 `json:"date"`
	Currency         string                 `json:"currency"`
	Source           string                 `json:"source"`
	Revision         string                 `json:"revision"`
	EvidenceSHA256   string                 `json:"evidence_sha256"`
	ProviderCallCost float64                `json:"provider_call_cost"`
	Managed          *ScorecardManagedPrice `json:"managed,omitempty"`
	Local            *ScorecardLocalPrice   `json:"local,omitempty"`
}

// ScorecardProviderObservation is one raw provider latency in microseconds.
// Fresh and BypassProviderCache must both be true and CacheHit false for every
// warmup and measured A observation.
type ScorecardProviderObservation struct {
	LatencyUS            int64  `json:"latency_us"`
	CandidateInputDigest string `json:"candidate_input_digest"`
	Fresh                bool   `json:"fresh"`
	BypassProviderCache  bool   `json:"bypass_provider_cache"`
	CacheHit             bool   `json:"cache_hit"`
}

// ScorecardRerankObservation is one raw rerank latency and its unaggregated
// response usage. The reference protocol requires prompt and total counts to
// be equal and bounded.
type ScorecardRerankObservation struct {
	LatencyUS    int64 `json:"latency_us"`
	PromptTokens int64 `json:"prompt_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

// ScorecardWarmupPair contains exactly one discarded observation from each
// path for the same production-built input.
type ScorecardWarmupPair struct {
	CaseID          string                       `json:"case_id"`
	InputDigest     string                       `json:"input_digest"`
	CandidateDigest string                       `json:"candidate_digest"`
	Provider        ScorecardProviderObservation `json:"provider"`
	Rerank          ScorecardRerankObservation   `json:"rerank"`
}

// ScorecardMeasuredPair contains exactly one A and one B observation. Index,
// CaseID, and Order make retries, cherry-picked ordering, and schedule drift
// observable in the committed record.
type ScorecardMeasuredPair struct {
	Index           int                          `json:"index"`
	CaseID          string                       `json:"case_id"`
	InputDigest     string                       `json:"input_digest"`
	CandidateDigest string                       `json:"candidate_digest"`
	Order           ScorecardPairOrder           `json:"order"`
	Provider        ScorecardProviderObservation `json:"provider"`
	Rerank          ScorecardRerankObservation   `json:"rerank"`
}

// ScorecardRun is the complete offline latency/cost record. CaseIDs may arrive
// in any order; the measured schedule is always checked against their byte-
// lexical order and pair i uses case i mod n.
type ScorecardRun struct {
	ManifestID                    string                  `json:"manifest_id"`
	RecordedAt                    string                  `json:"recorded_at"`
	CaseIDs                       []string                `json:"case_ids"`
	Metadata                      ScorecardMetadata       `json:"metadata"`
	PriceSnapshot                 ScorecardPriceSnapshot  `json:"price_snapshot"`
	DeploymentEvidenceID          string                  `json:"deployment_evidence_id,omitempty"`
	PrefixCachingDisabledObserved bool                    `json:"prefix_caching_disabled_observed,omitempty"`
	Warmups                       []ScorecardWarmupPair   `json:"warmups"`
	Pairs                         []ScorecardMeasuredPair `json:"pairs"`
	ProviderCacheHitSamplesUS     []int64                 `json:"provider_cache_hit_samples_us"`
}

type ScorecardPercentiles struct {
	P50US int64 `json:"p50_us"`
	P95US int64 `json:"p95_us"`
}

// ScorecardReport keeps the raw measured latency series alongside unrounded
// nearest-rank summaries. Cache-hit samples remain separate and never enter
// the fresh-provider comparison.
type ScorecardReport struct {
	ManifestID                    string                  `json:"manifest_id"`
	RecordedAt                    string                  `json:"recorded_at"`
	CaseIDs                       []string                `json:"case_ids"`
	Metadata                      ScorecardMetadata       `json:"metadata"`
	PriceSnapshot                 ScorecardPriceSnapshot  `json:"price_snapshot"`
	Warmups                       []ScorecardWarmupPair   `json:"warmups"`
	MeasuredPairs                 []ScorecardMeasuredPair `json:"measured_pairs"`
	ProviderSamplesUS             []int64                 `json:"provider_samples_us"`
	RerankSamplesUS               []int64                 `json:"rerank_samples_us"`
	ProviderCacheHitSamplesUS     []int64                 `json:"provider_cache_hit_samples_us"`
	ProviderLatency               ScorecardPercentiles    `json:"provider_latency"`
	RerankLatency                 ScorecardPercentiles    `json:"rerank_latency"`
	ProviderCostPerCall           float64                 `json:"provider_cost_per_call"`
	RerankCostPerBatch            float64                 `json:"rerank_cost_per_batch"`
	DeploymentEvidenceID          string                  `json:"deployment_evidence_id,omitempty"`
	PrefixCachingDisabledObserved bool                    `json:"prefix_caching_disabled_observed,omitempty"`
	LatencyPassed                 bool                    `json:"latency_passed"`
	CostPassed                    bool                    `json:"cost_passed"`
	Provisional                   bool                    `json:"provisional"`
	ProductionPassed              bool                    `json:"production_passed"`
}

// ValidateScorecardRun validates the complete record without applying the
// production gate. This deliberately permits an unauthenticated run to be
// saved as provisional evidence before R-6a exists.
func ValidateScorecardRun(corpus Corpus, run ScorecardRun) error {
	if _, err := Evaluate(corpus); err != nil {
		return scorecardInvalid("quality-gated corpus")
	}
	referenceManifest, err := replay.ReferenceManifest()
	if err != nil || run.ManifestID != referenceManifest.ManifestID {
		return scorecardInvalid("reference manifest")
	}
	if run.DeploymentEvidenceID != "" && !validDigest(run.DeploymentEvidenceID) {
		return scorecardInvalid("deployment evidence id")
	}
	recordedAt, err := time.Parse(time.RFC3339, run.RecordedAt)
	if err != nil || recordedAt.UTC().Format(time.RFC3339) != run.RecordedAt || len(run.RecordedAt) < len("2006-01-02") ||
		run.RecordedAt[:len("2006-01-02")] != run.PriceSnapshot.Date {
		return scorecardInvalid("recorded at")
	}
	if len(run.CaseIDs) != len(corpus.Cases) {
		return scorecardInvalid("case count")
	}
	type corpusIdentity struct {
		input     string
		candidate string
	}
	corpusDigests := make(map[string]corpusIdentity, len(corpus.Cases))
	for _, goldenCase := range corpus.Cases {
		corpusDigests[goldenCase.ID] = corpusIdentity{input: goldenCase.InputDigest, candidate: goldenCase.CandidateDigest}
	}
	knownCases := make(map[string]struct{}, len(run.CaseIDs))
	orderedCases := append([]string(nil), run.CaseIDs...)
	for _, caseID := range orderedCases {
		if !validPlainID(caseID) {
			return scorecardInvalid("case id")
		}
		if _, duplicate := knownCases[caseID]; duplicate {
			return scorecardInvalid("duplicate case id")
		}
		if _, present := corpusDigests[caseID]; !present {
			return scorecardInvalid("case does not belong to corpus")
		}
		knownCases[caseID] = struct{}{}
	}
	sort.Strings(orderedCases)
	if err := validateScorecardMetadata(run.Metadata, referenceManifest); err != nil {
		return err
	}
	if err := validateScorecardPrice(run.PriceSnapshot); err != nil {
		return err
	}
	if len(run.Warmups) != ScorecardWarmupsPerPath {
		return scorecardInvalid("warmup count")
	}
	if len(run.Pairs) != ScorecardMeasuredPairs {
		return scorecardInvalid("measured pair count")
	}

	digestByCase := make(map[string]corpusIdentity, len(knownCases))
	validateIdentity := func(caseID, inputDigest, candidateDigest string) error {
		identity := corpusIdentity{input: inputDigest, candidate: candidateDigest}
		if _, known := knownCases[caseID]; !known || !validDigest(inputDigest) || !validDigest(candidateDigest) || corpusDigests[caseID] != identity {
			return scorecardInvalid("sample identity")
		}
		if prior, present := digestByCase[caseID]; present && prior != identity {
			return scorecardInvalid("case input digest changed")
		}
		digestByCase[caseID] = identity
		return nil
	}
	for _, warmup := range run.Warmups {
		if err := validateIdentity(warmup.CaseID, warmup.InputDigest, warmup.CandidateDigest); err != nil {
			return err
		}
		if !validScorecardProviderObservation(warmup.Provider, warmup.CandidateDigest) || !validScorecardRerankObservation(warmup.Rerank) {
			return scorecardInvalid("warmup observation")
		}
	}
	for index, pair := range run.Pairs {
		wantOrder := ScorecardOrderProviderRerank
		if index%2 == 1 {
			wantOrder = ScorecardOrderRerankProvider
		}
		if pair.Index != index || pair.CaseID != orderedCases[index%len(orderedCases)] || pair.Order != wantOrder {
			return scorecardInvalid("measured schedule")
		}
		if err := validateIdentity(pair.CaseID, pair.InputDigest, pair.CandidateDigest); err != nil {
			return err
		}
		if !validScorecardProviderObservation(pair.Provider, pair.CandidateDigest) || !validScorecardRerankObservation(pair.Rerank) {
			return scorecardInvalid("measured observation")
		}
	}
	if len(run.ProviderCacheHitSamplesUS) == 0 || len(run.ProviderCacheHitSamplesUS) > MaxJSONArrayElements {
		return scorecardInvalid("provider cache-hit samples")
	}
	for _, latency := range run.ProviderCacheHitSamplesUS {
		if latency < 0 || latency > ScorecardMaximumLatencyUS {
			return scorecardInvalid("provider cache-hit latency")
		}
	}
	return nil
}

// EvaluateScorecardRun returns an auditable report even when a performance or
// deployment gate fails. R-6 has no authenticated deployment registry: an
// evidence identifier or observed prefix-cache setting is retained for audit
// only and cannot self-certify a production pass before R-6a exists.
func EvaluateScorecardRun(corpus Corpus, run ScorecardRun) (ScorecardReport, error) {
	if err := ValidateScorecardRun(corpus, run); err != nil {
		return ScorecardReport{}, err
	}
	report := cloneScorecardReport(run)
	report.ProviderSamplesUS = make([]int64, len(run.Pairs))
	report.RerankSamplesUS = make([]int64, len(run.Pairs))
	for index, pair := range run.Pairs {
		report.ProviderSamplesUS[index] = pair.Provider.LatencyUS
		report.RerankSamplesUS[index] = pair.Rerank.LatencyUS
	}
	report.ProviderLatency = scorecardNearestRanks(report.ProviderSamplesUS)
	report.RerankLatency = scorecardNearestRanks(report.RerankSamplesUS)
	report.ProviderCostPerCall = run.PriceSnapshot.ProviderCallCost
	if run.PriceSnapshot.Managed != nil {
		report.RerankCostPerBatch = run.PriceSnapshot.Managed.RerankBatchCost
	} else {
		cost, err := ScorecardLocalCostPerBatch(*run.PriceSnapshot.Local, report.RerankSamplesUS)
		if err != nil {
			return report, err
		}
		report.RerankCostPerBatch = cost
	}
	report.LatencyPassed = report.RerankLatency.P50US < report.ProviderLatency.P50US
	report.CostPassed = report.RerankCostPerBatch < report.ProviderCostPerCall
	report.Provisional = true
	report.ProductionPassed = false
	return report, fmt.Errorf("%w: authenticated deployment unavailable before R-6a; latency=%t cost=%t",
		ErrScorecardGateFailed, report.LatencyPassed, report.CostPassed)
}

// ScorecardLocalCostPerBatch applies the locked local formula to all measured
// raw rerank samples:
//
//	GPU-hour price * GPU count * mean batch microseconds
//	---------------------------------------------------
//	                  microseconds/hour
func ScorecardLocalCostPerBatch(price ScorecardLocalPrice, latencyUS []int64) (float64, error) {
	if !validPositiveFinite(price.GPUHourlyCost) || price.GPUCount < 1 || price.GPUCount > MaxJSONArrayElements || len(latencyUS) == 0 {
		return 0, scorecardInvalid("local price formula")
	}
	total := 0.0
	for _, latency := range latencyUS {
		if !validScorecardMeasuredLatency(latency) {
			return 0, scorecardInvalid("local price latency")
		}
		total += float64(latency)
	}
	meanLatency := total / float64(len(latencyUS))
	cost := price.GPUHourlyCost * float64(price.GPUCount) * meanLatency / float64(ScorecardMicrosecondsPerHour)
	if !validPositiveFinite(cost) {
		return 0, scorecardInvalid("local price result")
	}
	return cost, nil
}

func validateScorecardMetadata(metadata ScorecardMetadata, reference replay.Manifest) error {
	values := [...]string{
		metadata.Hardware,
		metadata.GPU,
		metadata.Region,
		metadata.RuntimeVersion,
		metadata.ModelID,
		metadata.ModelRevision,
		metadata.Provider,
		metadata.ProviderVersion,
	}
	for _, value := range values {
		if !validPlainID(value) {
			return scorecardInvalid("metadata")
		}
	}
	if metadata.RuntimeImageDigest != reference.ImageDigest || metadata.RuntimeVersion != reference.VLLMVersion ||
		metadata.ModelID != reference.ServedModelID || metadata.ModelRevision != reference.ModelRevision ||
		metadata.TemplateSHA256 != reference.TemplateSHA256 {
		return scorecardInvalid("metadata reference tuple")
	}
	return nil
}

func validateScorecardPrice(snapshot ScorecardPriceSnapshot) error {
	parsedDate, err := time.Parse("2006-01-02", snapshot.Date)
	if err != nil || parsedDate.Format("2006-01-02") != snapshot.Date || !validScorecardCurrency(snapshot.Currency) ||
		!validPlainID(snapshot.Source) || !validPlainID(snapshot.Revision) || !validDigest(snapshot.EvidenceSHA256) ||
		!validPositiveFinite(snapshot.ProviderCallCost) {
		return scorecardInvalid("price snapshot")
	}
	if (snapshot.Managed == nil) == (snapshot.Local == nil) {
		return scorecardInvalid("price mode")
	}
	if snapshot.Managed != nil && !validPositiveFinite(snapshot.Managed.RerankBatchCost) {
		return scorecardInvalid("managed price")
	}
	if snapshot.Local != nil && (!validPositiveFinite(snapshot.Local.GPUHourlyCost) ||
		snapshot.Local.GPUCount < 1 || snapshot.Local.GPUCount > MaxJSONArrayElements) {
		return scorecardInvalid("local price")
	}
	return nil
}

func validScorecardCurrency(currency string) bool {
	if len(currency) != 3 {
		return false
	}
	for _, character := range []byte(currency) {
		if character < 'A' || character > 'Z' {
			return false
		}
	}
	return true
}

func validScorecardProviderObservation(observation ScorecardProviderObservation, inputDigest string) bool {
	return validScorecardMeasuredLatency(observation.LatencyUS) && observation.CandidateInputDigest == inputDigest &&
		validDigest(observation.CandidateInputDigest) && observation.Fresh && observation.BypassProviderCache && !observation.CacheHit
}

func validScorecardRerankObservation(observation ScorecardRerankObservation) bool {
	return validScorecardMeasuredLatency(observation.LatencyUS) && observation.PromptTokens >= 0 &&
		observation.PromptTokens <= ScorecardMaximumTokenCount && observation.TotalTokens == observation.PromptTokens
}

func validScorecardMeasuredLatency(latency int64) bool {
	return latency > 0 && latency <= ScorecardMaximumLatencyUS
}

func validPositiveFinite(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func scorecardNearestRanks(raw []int64) ScorecardPercentiles {
	ordered := append([]int64(nil), raw...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left] < ordered[right] })
	return ScorecardPercentiles{P50US: ordered[ScorecardP50Index], P95US: ordered[ScorecardP95Index]}
}

func cloneScorecardReport(run ScorecardRun) ScorecardReport {
	caseIDs := append([]string(nil), run.CaseIDs...)
	sort.Strings(caseIDs)
	return ScorecardReport{
		ManifestID:                    run.ManifestID,
		RecordedAt:                    run.RecordedAt,
		CaseIDs:                       caseIDs,
		Metadata:                      run.Metadata,
		PriceSnapshot:                 cloneScorecardPrice(run.PriceSnapshot),
		Warmups:                       append([]ScorecardWarmupPair(nil), run.Warmups...),
		MeasuredPairs:                 append([]ScorecardMeasuredPair(nil), run.Pairs...),
		ProviderCacheHitSamplesUS:     append([]int64(nil), run.ProviderCacheHitSamplesUS...),
		DeploymentEvidenceID:          run.DeploymentEvidenceID,
		PrefixCachingDisabledObserved: run.PrefixCachingDisabledObserved,
	}
}

func cloneScorecardPrice(source ScorecardPriceSnapshot) ScorecardPriceSnapshot {
	cloned := source
	if source.Managed != nil {
		managed := *source.Managed
		cloned.Managed = &managed
	}
	if source.Local != nil {
		local := *source.Local
		cloned.Local = &local
	}
	return cloned
}

func scorecardInvalid(detail string) error {
	return fmt.Errorf("%w: %s", ErrInvalidScorecard, detail)
}
