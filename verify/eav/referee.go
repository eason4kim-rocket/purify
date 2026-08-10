package eav

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// MaxRefereeReplyBytes bounds one encoded referee reply.
const MaxRefereeReplyBytes = 4 << 10

// Referee adjudicates the gray zone: two surface forms that are confusably
// close but not alias-verified. Unlike extraction it legitimately sees the
// subject — same-referent resolution is an entity-linking judgment, not an
// extraction — but it can only ever move an outcome between match and
// uncertain-or-mismatch inside the gray zone; the deterministic ladder has
// already returned every clear case before a referee is consulted.
type Referee interface {
	SameReferent(ctx context.Context, subject Subject, entity Entity, doc Document) (RefereeVerdict, error)
}

// RefereeFunc adapts a function to Referee.
type RefereeFunc func(context.Context, Subject, Entity, Document) (RefereeVerdict, error)

func (referee RefereeFunc) SameReferent(
	ctx context.Context,
	subject Subject,
	entity Entity,
	doc Document,
) (RefereeVerdict, error) {
	if referee == nil {
		return RefereeVerdict{}, fmt.Errorf("%w: referee function is nil", ErrNotConfigured)
	}
	return referee(ctx, subject, entity, doc)
}

// RefereeAnswer is the three-way same-referent outcome.
type RefereeAnswer string

const (
	RefereeSame      RefereeAnswer = "same"
	RefereeDifferent RefereeAnswer = "different"
	RefereeUnsure    RefereeAnswer = "unsure"
)

// RefereeVerdict carries the answer and, for RefereeDifferent, the verbatim
// distinguishing excerpt. The judge downgrades an unanchorable
// distinguishing quote to unsure, so a referee can never alarm on its word
// alone.
type RefereeVerdict struct {
	Answer RefereeAnswer
	Quote  string
}

// RefereeSystemPrompt is the same-referent instruction shared by every LLM
// referee adapter.
const RefereeSystemPrompt = `You decide whether two names refer to the same real-world entity.

You are given a SUBJECT (what a user asked about, possibly with a HINT of
surrounding context), a DOCUMENT ENTITY (what one web document is about,
with its kind, aliases, and an evidencing quote), and the document itself.

Rules:
- Answer "same" only when the subject and the document entity are the same
  real-world thing: an alias, abbreviation, ticker, former name, translation,
  or the same name with a legal suffix.
- Answer "different" only when the document entity is clearly a different
  thing than the subject: a sibling product line, a similarly named company,
  a different person or place. Then quote must be a verbatim excerpt from the
  document that distinguishes the document entity from the subject. Copy it
  exactly; do not paraphrase.
- Answer "unsure" when the document does not let you decide.
Reply only with JSON matching the provided schema.`

// RefereeReplySchema is the strict reply schema for one referee call. Its
// answer enum mirrors the RefereeAnswer literals; eav_test locks the
// correspondence.
const RefereeReplySchema = `{
  "type": "object",
  "properties": {
    "answer": {"type": "string", "enum": ["same", "different", "unsure"]},
    "quote": {"type": ["string", "null"]}
  },
  "required": ["answer", "quote"],
  "additionalProperties": false
}`

// BuildRefereeInput renders the deterministic user payload for one referee
// call: the subject with its optional hint, the gated document entity, and
// the document head window.
func BuildRefereeInput(subject Subject, entity Entity, doc Document) string {
	var input strings.Builder
	input.Grow(MaxHeadWindowBytes + 1024)
	input.WriteString("SUBJECT: ")
	input.WriteString(runeSafePrefix(subject.Name, MaxSubjectBytes))
	if hint := strings.TrimSpace(subject.Hint); hint != "" {
		input.WriteString("\nHINT: ")
		input.WriteString(runeSafePrefix(hint, MaxHintBytes))
	}
	input.WriteString("\nDOCUMENT ENTITY: ")
	input.WriteString(runeSafePrefix(entity.Name, MaxEntityBytes))
	input.WriteString("\nKIND: ")
	input.WriteString(string(knownKind(entity.Kind)))
	if len(entity.Aliases) > 0 {
		aliases := entity.Aliases
		if len(aliases) > MaxAliases {
			aliases = aliases[:MaxAliases]
		}
		input.WriteString("\nALIASES: ")
		for index, alias := range aliases {
			if index > 0 {
				input.WriteString(", ")
			}
			input.WriteString(runeSafePrefix(alias, MaxEntityBytes))
		}
	}
	if quote := strings.TrimSpace(entity.Quote); quote != "" {
		input.WriteString("\nENTITY QUOTE: ")
		input.WriteString(runeSafePrefix(quote, MaxQuoteBytes))
	}
	input.WriteString("\nURL: ")
	input.WriteString(runeSafePrefix(doc.URL, maxPromptURLBytes))
	input.WriteString("\nTITLE: ")
	input.WriteString(runeSafePrefix(doc.Title, MaxQuoteBytes))
	input.WriteString("\nCONTENT:\n")
	input.WriteString(headWindow(doc.Cleaned))
	return input.String()
}

type refereeReply struct {
	Answer string `json:"answer"`
	Quote  string `json:"quote"`
}

// DecodeRefereeReply parses one raw referee reply. Malformed or oversized
// replies error; an unrecognized answer value coerces to unsure, which is
// the direction that can never create an alarm.
func DecodeRefereeReply(raw []byte) (RefereeVerdict, error) {
	if len(raw) == 0 {
		return RefereeVerdict{}, fmt.Errorf("%w: referee reply is empty", ErrInvalidInput)
	}
	if len(raw) > MaxRefereeReplyBytes {
		return RefereeVerdict{}, fmt.Errorf(
			"%w: referee reply exceeds %d bytes",
			ErrInvalidInput,
			MaxRefereeReplyBytes,
		)
	}
	var reply refereeReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return RefereeVerdict{}, fmt.Errorf("%w: decode referee reply: %v", ErrInvalidInput, err)
	}
	answer := RefereeAnswer(strings.ToLower(strings.TrimSpace(reply.Answer)))
	switch answer {
	case RefereeSame, RefereeDifferent, RefereeUnsure:
	default:
		answer = RefereeUnsure
	}
	return RefereeVerdict{Answer: answer, Quote: reply.Quote}, nil
}
