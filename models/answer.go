package models

import (
	"encoding/json"
	"time"

	"github.com/use-agent/purify/evidence"
)

const (
	// DefaultAnswerFreshness is the neutral Search freshness used when a fact
	// specification omits its freshness requirement.
	DefaultAnswerFreshness = "7d"
	// DefaultAnswerMinIndependentSources is the minimum evidence-component
	// agreement required before Answer may return a belief.
	DefaultAnswerMinIndependentSources = 2
	// MaxAnswerMinIndependentSources matches multi-source extraction's fan-out.
	MaxAnswerMinIndependentSources = MaxExtractSources

	// MaxAnswerSubjectRunes and MaxAnswerSubjectWords leave room for the
	// predicate in Search's complete query budget.
	MaxAnswerSubjectRunes = 300
	MaxAnswerSubjectWords = 40
	// MaxAnswerSubjectBytes is checked before normalization so trusted direct
	// callers cannot force an unbounded Fields allocation.
	MaxAnswerSubjectBytes = MaxAnswerSubjectRunes * 4
	// MaxAnswerPredicateBytes bounds the generated schema property and the
	// consensus field lookup.
	MaxAnswerPredicateBytes = 512
	MaxAnswerFreshnessBytes = 16

	// MaxAnswerRequestBytes and MaxAnswerResponseBytes are the transport and
	// encoding budgets shared by core and adapters.
	MaxAnswerRequestBytes  = 1 << 20
	MaxAnswerResponseBytes = 32 << 20

	DefaultAnswerTimeoutSeconds = 30
	MaxAnswerTimeoutSeconds     = 120

	// DefaultAnswerLeaseSeconds is Phase 5's fixed lease. Phase 7 replaces this
	// with an adaptive interval without changing the response shape.
	DefaultAnswerLeaseSeconds = 24 * 60 * 60
	// DefaultAnswerConfidenceHalflifeSeconds is the initial one-week decay hint.
	DefaultAnswerConfidenceHalflifeSeconds = 7 * 24 * 60 * 60
	// DefaultAnswerRenewURL is deliberately a relative, credential-free target.
	DefaultAnswerRenewURL = "/api/v1/verify"

	ErrCodeAnswerUnavailable = "ANSWER_UNAVAILABLE"
	ErrCodeAnswerFailed      = "ANSWER_FAILED"
)

// FactConflictPolicy controls how competing values are represented. Phase 5
// deliberately supports only exposure; silently choosing a value is forbidden.
type FactConflictPolicy string

const (
	FactConflictExpose FactConflictPolicy = "expose"
)

// FactSpec identifies one scalar fact. The Phase 5 core extracts a required
// string value; an additive typed-schema boundary is needed for other JSON
// scalar types.
type FactSpec struct {
	Subject               string             `json:"subject" binding:"required"`
	Predicate             string             `json:"predicate" binding:"required"`
	Freshness             string             `json:"freshness,omitempty"`
	MinIndependentSources int                `json:"min_independent_sources,omitempty" binding:"omitempty,min=1,max=8"`
	OnConflict            FactConflictPolicy `json:"on_conflict,omitempty" binding:"omitempty,oneof=expose"`
}

// Defaults applies semantic defaults without validating or normalizing input.
func (spec *FactSpec) Defaults() {
	if spec == nil {
		return
	}
	if spec.Freshness == "" {
		spec.Freshness = DefaultAnswerFreshness
	}
	if spec.MinIndependentSources == 0 {
		spec.MinIndependentSources = DefaultAnswerMinIndependentSources
	}
	if spec.OnConflict == "" {
		spec.OnConflict = FactConflictExpose
	}
}

// AnswerRequest is the payload for POST /api/v1/answer.
type AnswerRequest struct {
	Spec    FactSpec `json:"spec" binding:"required"`
	Timeout int      `json:"timeout,omitempty" binding:"omitempty,min=1,max=120"`
}

// Defaults applies the fixed Phase 5 timeout and the nested fact defaults.
func (request *AnswerRequest) Defaults() {
	if request == nil {
		return
	}
	request.Spec.Defaults()
	if request.Timeout == 0 {
		request.Timeout = DefaultAnswerTimeoutSeconds
	}
}

// AnswerStatus makes the successful known and successful unknown shapes
// explicit. Unknown is not a transport or service failure.
type AnswerStatus string

const (
	AnswerStatusKnown   AnswerStatus = "known"
	AnswerStatusUnknown AnswerStatus = "unknown"
)

// AnswerConfidence is an intentionally non-statistical ordinal tier.
type AnswerConfidence string

const (
	AnswerConfidenceLow    AnswerConfidence = "low"
	AnswerConfidenceMedium AnswerConfidence = "medium"
	AnswerConfidenceHigh   AnswerConfidence = "high"
)

// AnswerUnknownReason is a stable, non-sensitive reason for withholding a
// belief.
type AnswerUnknownReason string

const (
	AnswerUnknownNoSearchResults AnswerUnknownReason = "no_search_results"
	AnswerUnknownNoValidSources  AnswerUnknownReason = "no_valid_sources"
	AnswerUnknownMissingValue    AnswerUnknownReason = "missing_consensus_value"
	AnswerUnknownInsufficient    AnswerUnknownReason = "insufficient_independent_sources"
	AnswerUnknownConflict        AnswerUnknownReason = "conflicting_independent_sources"
	// AnswerUnknownEntityMismatch reports that sources were found but were
	// judged to be about a different entity than the requested subject, and
	// the belief was withheld because of those exclusions.
	AnswerUnknownEntityMismatch AnswerUnknownReason = "entity_mismatch"
)

// AnswerEvidence is the credential-free public projection of one winning
// consensus support. EntityVerdict, when present, is the source's entity
// attribution ("entity_match" or "entity_uncertain"); mismatched sources
// never reach a belief because consensus excludes them.
type AnswerEvidence struct {
	URL           string          `json:"url"`
	Root          string          `json:"root"`
	Quote         string          `json:"quote"`
	TextRange     [2]int          `json:"text_range"`
	Selector      string          `json:"selector,omitempty"`
	Method        evidence.Method `json:"method"`
	SnapshotID    string          `json:"snapshot_id"`
	FetchedAt     time.Time       `json:"fetched_at"`
	EntityVerdict string          `json:"entity_verdict,omitempty"`
}

// AnswerCandidate exposes one competing value without recomputing its
// independence score.
type AnswerCandidate struct {
	Value     json.RawMessage       `json:"value"`
	Agreement MultiExtractAgreement `json:"agreement"`
}

// AnswerBelief is emitted only after the winner clears the independent-root
// threshold and no competing value has the same independent-root count.
type AnswerBelief struct {
	Value      json.RawMessage       `json:"value"`
	Confidence AnswerConfidence      `json:"confidence"`
	Agreement  MultiExtractAgreement `json:"agreement"`
	AsOf       time.Time             `json:"as_of"`
	Evidence   []AnswerEvidence      `json:"evidence"`
	Receipts   map[string]string     `json:"receipts"`
}

// AnswerNeeds quantifies the extra independent agreement required to promote
// the closest candidate. A tie always needs at least one additional root.
type AnswerNeeds struct {
	MoreIndependentSources int `json:"more_independent_sources"`
}

// AnswerClosest reports the best evidence-backed candidate without presenting
// it as a belief.
type AnswerClosest struct {
	Value            json.RawMessage `json:"value"`
	IndependentRoots int             `json:"independent_roots"`
	Note             string          `json:"note"`
}

// AnswerLease is Phase 5's fixed validity hint. Its shape is intentionally
// compatible with Phase 7's adaptive lease.
type AnswerLease struct {
	ExpiresAt          time.Time `json:"expires_at"`
	RenewURL           string    `json:"renew_url"`
	ConfidenceHalflife int64     `json:"confidence_halflife_s"`
}

// AnswerResponse has two successful shapes. Belief is always encoded so an
// unknown response is unambiguously `"belief": null`.
type AnswerResponse struct {
	Status  AnswerStatus        `json:"status"`
	Belief  *AnswerBelief       `json:"belief"`
	Reason  AnswerUnknownReason `json:"reason,omitempty"`
	Needs   *AnswerNeeds        `json:"needs,omitempty"`
	Closest *AnswerClosest      `json:"closest,omitempty"`
	// Conflicts contains score-ordered alternatives only. It never repeats the
	// selected Belief value or the separately projected Closest candidate.
	Conflicts []AnswerCandidate `json:"conflicts,omitempty"`
	Lease     *AnswerLease      `json:"lease,omitempty"`
}

// AnswerErrorResponse is the model-owned transport envelope for failed Answer
// operations. Successful unknown responses use AnswerResponse instead.
type AnswerErrorResponse struct {
	Error *ErrorDetail `json:"error"`
}
