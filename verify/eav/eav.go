// Package eav judges entity attribution: whether a document is about the same
// real-world entity a caller asked about. It exists to catch
// right-source-wrong-entity failures ("deceptive grounding"), where every
// claim faithfully quotes a real, correctly cited document that is simply
// about a different entity than the query subject — a failure mode invisible
// to hallucination, faithfulness, and citation checks.
//
// The deterministic core is pure functions over surface forms. Entity
// extraction and gray-zone refereeing are dependency boundaries supplied by
// callers, so the package stays transport neutral: no LLM client, no HTTP,
// and no persistence live here. An alarm (entity_mismatch) is only ever
// produced from hard evidence; everything unprovable stays entity_uncertain.
package eav

import (
	"errors"

	"github.com/use-agent/purify/evidence"
)

const (
	// MaxSubjectBytes bounds the query-side entity name. It matches the
	// answer subject budget (300 runes at up to 4 bytes each) without
	// importing the models package.
	MaxSubjectBytes = 1200
	// MaxHintBytes bounds the optional disambiguation context and matches
	// the answer predicate budget.
	MaxHintBytes = 512
	// MaxDocumentBytes bounds one judged document and matches the
	// repository-wide page budget.
	MaxDocumentBytes = 4 << 20
	// MaxHeadWindowBytes is the slice of title plus cleaned text an entity
	// extractor receives. Documents state their subject early; the window
	// keeps extraction cost flat regardless of page size.
	MaxHeadWindowBytes = 8 << 10
	// MaxCandidates bounds one harvested candidate slate.
	MaxCandidates = 24
	// MaxAliases bounds the alias list of one extracted entity.
	MaxAliases = 8
	// MaxSecondary bounds the non-primary entities of one document.
	MaxSecondary = 8
	// MaxEntityBytes bounds one entity surface form.
	MaxEntityBytes = 512
	// MaxQuoteBytes bounds one supporting quote.
	MaxQuoteBytes = 1 << 10
)

var (
	// ErrInvalidInput marks malformed subjects, documents, and extractions.
	ErrInvalidInput = errors.New("eav: invalid input")
	// ErrNotConfigured marks a judge missing a required dependency.
	ErrNotConfigured = errors.New("eav: judge is not configured")
)

// Kind is the coarse open-web entity ontology. It is diagnostic in v1: the
// deterministic ladder never branches on it.
type Kind string

const (
	KindOrganization Kind = "organization"
	KindPerson       Kind = "person"
	KindProduct      Kind = "product"
	KindPlace        Kind = "place"
	KindEvent        Kind = "event"
	KindWork         Kind = "work"
	KindSubstance    Kind = "substance"
	KindOther        Kind = "other"
)

// Subject is the query-side entity. Hint is optional disambiguation context
// (answer passes the fact predicate); it is shown to a gray-zone referee and
// is deliberately never part of blind extraction or deterministic matching.
type Subject struct {
	Name string
	Hint string
}

// Document is one document under judgment. Cleaned is required; RawHTML lets
// the candidate harvest read og:* and JSON-LD signals and may be empty.
type Document struct {
	URL     string
	Title   string
	Cleaned string
	RawHTML string
}

// Candidate is one slate entry produced by the deterministic harvest. Quote
// must occur verbatim in the document title or cleaned text.
type Candidate struct {
	Surface string
	Signal  string
	Quote   string
}

// Entity is one named thing with the aliases the same document uses for it
// (ticker, abbreviation, former name). Quote is the shortest verbatim
// citation evidencing the primary-entity choice.
type Entity struct {
	Name    string
	Kind    Kind
	Aliases []string
	Quote   string
}

// DocumentEntities is one blind extraction result. A nil Primary means no
// single entity dominates the document (list pages, comparisons, forums);
// that is a legitimate outcome, not an error.
type DocumentEntities struct {
	Primary   *Entity
	Secondary []Entity
}

// Verdict is the three-state attribution outcome. The only alarm state is
// VerdictMismatch; unprovable cases must remain VerdictUncertain.
type Verdict string

const (
	VerdictMatch     Verdict = "entity_match"
	VerdictMismatch  Verdict = "entity_mismatch"
	VerdictUncertain Verdict = "entity_uncertain"
)

// MatchTier records which rung of the ladder decided a verdict, for responses
// and layered evaluation. Deterministic alarms only ever carry TierFloor;
// TierReferee is reserved for gray-zone adjudication above this package's
// pure core.
type MatchTier string

const (
	TierExact   MatchTier = "exact"
	TierAlias   MatchTier = "alias"
	TierSuffix  MatchTier = "suffix"
	TierFloor   MatchTier = "floor"
	TierReferee MatchTier = "referee"
	TierNone    MatchTier = ""
)

// MatchResult is the deterministic ladder outcome. Similarity is diagnostic;
// the contract is Verdict and Tier.
type MatchResult struct {
	Verdict    Verdict
	Tier       MatchTier
	Similarity float64
}

// Judgment is one complete attribution decision. Evidence anchors the
// deciding quote in the document's cleaned text and is present whenever the
// verdict is match or mismatch; DocEntity may be nil only when uncertain.
type Judgment struct {
	Verdict    Verdict
	Tier       MatchTier
	Subject    string
	DocEntity  *Entity
	Evidence   *evidence.Anchor
	Similarity float64
}
