package eav

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/use-agent/purify/evidence"
)

// Config supplies the judge's dependency boundaries. Extractor is required;
// Referee is optional — without one, every gray-zone case stays uncertain.
type Config struct {
	Extractor EntityExtractor
	Referee   Referee
}

// Judge orchestrates one complete attribution decision: harvest the
// candidate slate, extract the primary entity blind, run the deterministic
// ladder, and consult the referee only for the gray zone.
type Judge struct {
	extractor EntityExtractor
	referee   Referee
}

// NewJudge validates the required extractor boundary. A typed-nil referee is
// treated as absent rather than trusted.
func NewJudge(config Config) (*Judge, error) {
	if isNilInterface(config.Extractor) {
		return nil, ErrNotConfigured
	}
	referee := config.Referee
	if isNilInterface(referee) {
		referee = nil
	}
	return &Judge{extractor: config.Extractor, referee: referee}, nil
}

// JudgeDocument decides whether doc is about subject. The decision priority
// is fixed: unusable input judges uncertain before any dependency is called;
// a blind extraction without a primary entity judges uncertain; ladder tiers
// 1–4 return immediately; only the remaining gray zone consults the referee,
// and a nil, failing, or unsure referee leaves the gray zone uncertain.
//
// Every match or mismatch carries Evidence anchored in doc.Cleaned; when the
// deciding quote cannot be anchored the verdict degrades to uncertain rather
// than shipping an unevidenced decision. Errors are returned only for
// context cancellation and missing dependencies — a judgment over bad data
// is always some verdict, never an error.
func (judge *Judge) JudgeDocument(ctx context.Context, subject Subject, doc Document) (Judgment, error) {
	if judge == nil || isNilInterface(judge.extractor) {
		return Judgment{}, ErrNotConfigured
	}
	if ctx == nil {
		return Judgment{}, fmt.Errorf("%w: context is nil", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return Judgment{}, err
	}
	if len(subject.Name) > MaxSubjectBytes {
		return Judgment{Verdict: VerdictUncertain}, nil
	}
	subjectNorm := Normalize(subject.Name)
	uncertain := Judgment{Verdict: VerdictUncertain, Subject: subjectNorm}
	if subjectNorm == "" {
		return uncertain, nil
	}
	subject.Hint = runeSafePrefix(strings.TrimSpace(subject.Hint), MaxHintBytes)
	if doc.Cleaned == "" || len(doc.Cleaned) > MaxDocumentBytes {
		return uncertain, nil
	}

	slate := HarvestCandidates(doc)
	entities, err := AnchoredExtraction(ctx, judge.extractor, doc, slate)
	if err != nil {
		return Judgment{}, err
	}
	if entities.Primary == nil {
		return uncertain, nil
	}
	primary := entities.Primary
	uncertain.DocEntity = primary

	result := Match(subject, *primary)
	uncertain.Similarity = result.Similarity
	if result.Verdict == VerdictMatch || result.Verdict == VerdictMismatch {
		return decideJudgment(ctx, result.Verdict, result.Tier, uncertain, primary.Quote, doc)
	}

	if judge.referee == nil {
		return uncertain, nil
	}
	refereeDoc := Document{
		URL:     doc.URL,
		Title:   boundedTitle(doc.Title),
		Cleaned: headWindow(doc.Cleaned),
	}
	verdict, refereeErr := judge.referee.SameReferent(ctx, subject, *primary, refereeDoc)
	if refereeErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Judgment{}, ctxErr
		}
		if errors.Is(refereeErr, ErrNotConfigured) {
			return Judgment{}, refereeErr
		}
		return uncertain, nil
	}
	if err := ctx.Err(); err != nil {
		return Judgment{}, err
	}
	switch verdict.Answer {
	case RefereeSame:
		return decideJudgment(ctx, VerdictMatch, TierReferee, uncertain, primary.Quote, doc)
	case RefereeDifferent:
		quote := strings.TrimSpace(verdict.Quote)
		if quote == "" || len(quote) > MaxQuoteBytes {
			return uncertain, nil
		}
		return decideJudgment(ctx, VerdictMismatch, TierReferee, uncertain, quote, doc)
	default:
		return uncertain, nil
	}
}

// decideJudgment assembles a match or mismatch judgment around its deciding
// quote. Both alarm-capable verdicts must carry evidence, so an unanchorable
// quote degrades the decision to the prepared uncertain judgment instead —
// for a referee "different" this is the required downgrade to unsure.
func decideJudgment(
	ctx context.Context,
	verdict Verdict,
	tier MatchTier,
	uncertain Judgment,
	quote string,
	doc Document,
) (Judgment, error) {
	rawHTML := doc.RawHTML
	if len(rawHTML) > MaxDocumentBytes {
		rawHTML = ""
	}
	anchor, err := evidence.AlignValueContext(ctx, quote, doc.Cleaned, rawHTML)
	if err != nil {
		return Judgment{}, err
	}
	if anchor.Method == evidence.MethodUnlocated {
		return uncertain, nil
	}
	return Judgment{
		Verdict:    verdict,
		Tier:       tier,
		Subject:    uncertain.Subject,
		DocEntity:  uncertain.DocEntity,
		Evidence:   &anchor,
		Similarity: uncertain.Similarity,
	}, nil
}

func boundedTitle(title string) string {
	if len(title) > MaxQuoteBytes {
		return ""
	}
	return title
}
