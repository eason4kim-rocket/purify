package eav

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/use-agent/purify/evidence"
)

const (
	// MaxExtractionReplyBytes bounds one encoded extraction reply.
	MaxExtractionReplyBytes = 16 << 10
	// maxPromptURLBytes keeps a pathological URL from flooding the prompt.
	maxPromptURLBytes = 2 << 10
)

// EntityExtractor is the transport-neutral blind-extraction boundary: given
// one document head and its candidate slate, name the document's primary
// entity. The inputs deliberately carry no query subject, so an
// implementation cannot be biased toward what the caller hopes to find.
// Provider configuration, API keys, repair retries, and HTTP all belong in
// the adapter supplied at the wiring layer.
type EntityExtractor interface {
	ExtractEntities(ctx context.Context, doc Document, slate []Candidate) (DocumentEntities, error)
}

// EntityExtractorFunc adapts a function to EntityExtractor.
type EntityExtractorFunc func(context.Context, Document, []Candidate) (DocumentEntities, error)

func (extract EntityExtractorFunc) ExtractEntities(
	ctx context.Context,
	doc Document,
	slate []Candidate,
) (DocumentEntities, error) {
	if extract == nil {
		return DocumentEntities{}, fmt.Errorf("%w: entity extractor function is nil", ErrNotConfigured)
	}
	return extract(ctx, doc, slate)
}

// AnchoredExtraction invokes one blind extraction and forces its output
// through the anchoring gates, whatever the extractor implementation is:
//
//  1. bounds — names, aliases, kinds, and quotes are size- and
//     count-limited; unknown kinds coerce to KindOther.
//  2. anchoring — the primary name and every alias must occur in the
//     document title or cleaned text (ASCII-case-insensitively, adopting the
//     document's own casing), and the quote must align into the full cleaned
//     text via evidence; an unlocated quote discards its entity.
//  3. downgrade — a discarded primary degrades the whole result to
//     DocumentEntities{Primary: nil}, which judges as uncertain.
//
// The extractor sees only the head window of the cleaned text and never the
// raw HTML; gating runs against the full cleaned text. Extraction failures
// degrade to an empty result instead of escalating: an error returns only
// for context cancellation or a missing dependency (ErrNotConfigured).
func AnchoredExtraction(
	ctx context.Context,
	extractor EntityExtractor,
	doc Document,
	slate []Candidate,
) (DocumentEntities, error) {
	if ctx == nil {
		return DocumentEntities{}, fmt.Errorf("%w: context is nil", ErrInvalidInput)
	}
	if isNilInterface(extractor) {
		return DocumentEntities{}, ErrNotConfigured
	}
	if err := ctx.Err(); err != nil {
		return DocumentEntities{}, err
	}
	title := doc.Title
	if len(title) > MaxQuoteBytes {
		title = ""
	}
	cleaned := doc.Cleaned
	if len(cleaned) > MaxDocumentBytes {
		cleaned = ""
	}
	if cleaned == "" || len(slate) == 0 {
		return DocumentEntities{}, nil
	}
	if len(slate) > MaxCandidates {
		slate = slate[:MaxCandidates]
	}
	blind := Document{
		URL:     doc.URL,
		Title:   title,
		Cleaned: headWindow(cleaned),
	}
	extracted, err := extractor.ExtractEntities(ctx, blind, slate)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return DocumentEntities{}, ctxErr
		}
		if errors.Is(err, ErrNotConfigured) {
			return DocumentEntities{}, err
		}
		return DocumentEntities{}, nil
	}
	if err := ctx.Err(); err != nil {
		return DocumentEntities{}, err
	}

	locator := newQuoteLocator(title, cleaned)
	primary, err := gateEntity(ctx, extracted.Primary, locator, cleaned)
	if err != nil {
		return DocumentEntities{}, err
	}
	if primary == nil {
		return DocumentEntities{}, nil
	}
	primaryKey := Normalize(primary.Name)
	secondary := make([]Entity, 0, min(len(extracted.Secondary), MaxSecondary))
	seen := map[string]struct{}{primaryKey: {}}
	for index := range extracted.Secondary {
		if len(secondary) == MaxSecondary {
			break
		}
		gated, gateErr := gateEntity(ctx, &extracted.Secondary[index], locator, cleaned)
		if gateErr != nil {
			return DocumentEntities{}, gateErr
		}
		if gated == nil {
			continue
		}
		key := Normalize(gated.Name)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		secondary = append(secondary, *gated)
	}
	if len(secondary) == 0 {
		secondary = nil
	}
	return DocumentEntities{Primary: primary, Secondary: secondary}, nil
}

// gateEntity applies the bounds and anchoring gates to one extracted entity.
// A failed gate returns (nil, nil); an error is context cancellation only. A
// missing quote falls back to the located name, which is already anchored,
// so the fallback can never weaken the anchoring guarantee.
func gateEntity(
	ctx context.Context,
	entity *Entity,
	locator *quoteLocator,
	cleaned string,
) (*Entity, error) {
	if entity == nil {
		return nil, nil
	}
	name := strings.TrimSpace(entity.Name)
	if name == "" || len(name) > MaxEntityBytes {
		return nil, nil
	}
	locatedName, located := locator.locate(name)
	if !located {
		return nil, nil
	}
	quote := strings.TrimSpace(entity.Quote)
	if quote == "" {
		quote = locatedName
	}
	if len(quote) > MaxQuoteBytes {
		return nil, nil
	}
	anchor, err := evidence.AlignValueContext(ctx, quote, cleaned, "")
	if err != nil {
		return nil, err
	}
	if anchor.Method == evidence.MethodUnlocated {
		return nil, nil
	}
	// Prefer the aligned document span over the extractor's rendering, but a
	// fuzzy window may run slightly longer than the given quote, so keep the
	// already-bounded original if adoption would break the quote budget.
	if anchor.Quote != "" && len(anchor.Quote) <= MaxQuoteBytes {
		quote = anchor.Quote
	}

	nameKey := Normalize(locatedName)
	seen := map[string]struct{}{nameKey: {}}
	aliases := make([]string, 0, min(len(entity.Aliases), MaxAliases))
	for _, alias := range entity.Aliases {
		if len(aliases) == MaxAliases {
			break
		}
		trimmed := strings.TrimSpace(alias)
		if trimmed == "" || len(trimmed) > MaxEntityBytes {
			continue
		}
		locatedAlias, aliasLocated := locator.locate(trimmed)
		if !aliasLocated {
			continue
		}
		key := Normalize(locatedAlias)
		if key == "" {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		aliases = append(aliases, locatedAlias)
	}
	if len(aliases) == 0 {
		aliases = nil
	}
	return &Entity{
		Name:    locatedName,
		Kind:    knownKind(entity.Kind),
		Aliases: aliases,
		Quote:   quote,
	}, nil
}

func knownKind(kind Kind) Kind {
	switch kind {
	case KindOrganization, KindPerson, KindProduct, KindPlace,
		KindEvent, KindWork, KindSubstance, KindOther:
		return kind
	default:
		return KindOther
	}
}

// ExtractionSystemPrompt is the blind-extraction instruction shared by every
// LLM adapter. It deliberately says nothing about any query subject: the
// document is judged on its own, and the comparison happens later in
// deterministic code.
const ExtractionSystemPrompt = `You identify the single primary entity that one web document is about.

Rules:
- Choose the primary entity name from the CANDIDATES list. Never output a name that does not appear in the document.
- kind is one of: organization, person, product, place, event, work, substance, other.
- aliases are other surface forms THIS document uses for the same entity (ticker symbol, abbreviation, former name, translation). Copy them exactly from the document; do not invent aliases.
- quote is the shortest verbatim excerpt from the document that proves the primary choice. Copy it exactly.
- If no single entity dominates the document (list pages, comparisons, forums, category hubs), set primary to null and explain briefly in reason_if_none.
Reply only with JSON matching the provided schema.`

// ExtractionReplySchema is the strict reply schema for one blind extraction.
// Its aliases maxItems mirrors MaxAliases and its kind enum mirrors the Kind
// literals; eav_test locks the correspondence.
const ExtractionReplySchema = `{
  "type": "object",
  "properties": {
    "primary": {
      "type": ["object", "null"],
      "properties": {
        "name": {"type": "string"},
        "kind": {"type": "string", "enum": ["organization", "person", "product", "place", "event", "work", "substance", "other"]},
        "aliases": {"type": "array", "items": {"type": "string"}, "maxItems": 8},
        "quote": {"type": "string"}
      },
      "required": ["name", "kind", "aliases", "quote"],
      "additionalProperties": false
    },
    "reason_if_none": {"type": ["string", "null"]}
  },
  "required": ["primary", "reason_if_none"],
  "additionalProperties": false
}`

// BuildExtractionInput renders the deterministic user payload for one blind
// extraction: URL, title, the numbered candidate slate, and the cleaned head
// window. Blindness is structural — there is no subject parameter to leak.
func BuildExtractionInput(doc Document, slate []Candidate) string {
	if len(slate) > MaxCandidates {
		slate = slate[:MaxCandidates]
	}
	var input strings.Builder
	input.Grow(MaxHeadWindowBytes + 1024)
	input.WriteString("URL: ")
	input.WriteString(runeSafePrefix(doc.URL, maxPromptURLBytes))
	input.WriteString("\nTITLE: ")
	input.WriteString(runeSafePrefix(doc.Title, MaxQuoteBytes))
	input.WriteString("\nCANDIDATES:\n")
	for index, candidate := range slate {
		fmt.Fprintf(&input, "%d. %q (%s)\n", index+1, candidate.Surface, candidate.Signal)
	}
	input.WriteString("CONTENT:\n")
	input.WriteString(headWindow(doc.Cleaned))
	return input.String()
}

type extractionReply struct {
	Primary      *extractionEntity `json:"primary"`
	ReasonIfNone string            `json:"reason_if_none"`
}

type extractionEntity struct {
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	Aliases []string `json:"aliases"`
	Quote   string   `json:"quote"`
}

// DecodeExtractionReply parses one raw LLM reply into ungated
// DocumentEntities. It validates shape and size only; anchoring and bounds
// gating always happen in AnchoredExtraction against the real document, so a
// decoder can never be the last line of defense.
func DecodeExtractionReply(raw []byte) (DocumentEntities, error) {
	if len(raw) == 0 {
		return DocumentEntities{}, fmt.Errorf("%w: extraction reply is empty", ErrInvalidInput)
	}
	if len(raw) > MaxExtractionReplyBytes {
		return DocumentEntities{}, fmt.Errorf(
			"%w: extraction reply exceeds %d bytes",
			ErrInvalidInput,
			MaxExtractionReplyBytes,
		)
	}
	var reply extractionReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return DocumentEntities{}, fmt.Errorf("%w: decode extraction reply: %v", ErrInvalidInput, err)
	}
	if reply.Primary == nil {
		return DocumentEntities{}, nil
	}
	return DocumentEntities{
		Primary: &Entity{
			Name:    reply.Primary.Name,
			Kind:    knownKind(Kind(reply.Primary.Kind)),
			Aliases: append([]string(nil), reply.Primary.Aliases...),
			Quote:   reply.Primary.Quote,
		},
	}, nil
}
