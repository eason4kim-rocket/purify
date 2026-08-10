package answer

import (
	"context"
	"testing"
	"time"

	"github.com/use-agent/purify/models"
)

func excludedMismatchSource(rawURL, entityName string) models.MultiExtractSource {
	return models.MultiExtractSource{
		URL:      rawURL,
		FinalURL: rawURL,
		Success:  false,
		Status:   models.MultiExtractSourceStatusEntityMismatch,
		Entity: &models.EntityAttribution{
			Verdict: models.EntityVerdictMismatch,
			Name:    entityName,
			Kind:    "organization",
			Tier:    "referee",
		},
		Error: &models.ErrorDetail{
			Code:    models.ErrCodeEntityMismatch,
			Message: "source is about a different entity than the expected subject",
		},
	}
}

// The fact subject and predicate must flow to extraction as the expected
// entity, so the extract layer can judge sources without a second contract.
func TestAnswerPassesExpectedSubjectToExtraction(t *testing.T) {
	fetchedAt := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	searcher := &stubAnswerSearcher{response: successfulSearch(
		"https://one.example.com/fact",
		"https://two.example.net/fact",
	)}
	searcher.response.Query = "anthropic price"
	extractor := &stubAnswerExtractor{response: successfulConsensus("price", knownField(`"$3.00"`, 2, 2, fetchedAt))}
	service, err := NewService(searcher, extractor)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	request := &models.AnswerRequest{Spec: models.FactSpec{Subject: " anthropic ", Predicate: "price"}}
	if _, err := service.Answer(context.Background(), request); err != nil {
		t.Fatalf("Answer() error = %v", err)
	}
	subject := extractor.request.ExpectedSubject
	if subject == nil || subject.Name != "anthropic" || subject.Hint != "price" {
		t.Fatalf("extraction expected subject = %#v, want the normalized fact spec", subject)
	}
}

// Exclusions that cause the support shortfall must surface as
// entity_mismatch, not as a generic insufficiency.
func TestAnswerReportsEntityMismatchAfterExclusions(t *testing.T) {
	fetchedAt := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	searcher := &stubAnswerSearcher{response: successfulSearch(
		"https://one.example.com/fact",
		"https://two.example.net/fact",
		"https://three.example.org/fact",
	)}
	searcher.response.Query = "anthropic price"
	response := successfulConsensus("price", knownField(`"$3.00"`, 1, 1, fetchedAt))
	response.Sources = append(response.Sources,
		excludedMismatchSource("https://two.example.net/fact", "Anthropic Insurance Group"),
		excludedMismatchSource("https://three.example.org/fact", "Anthropic Insurance Group"),
	)
	extractor := &stubAnswerExtractor{response: response}
	service, err := NewService(searcher, extractor)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	request := &models.AnswerRequest{Spec: models.FactSpec{Subject: "anthropic", Predicate: "price"}}
	got, err := service.Answer(context.Background(), request)
	if err != nil {
		t.Fatalf("Answer() error = %v", err)
	}
	if got.Status != models.AnswerStatusUnknown || got.Reason != models.AnswerUnknownEntityMismatch {
		t.Fatalf("response = (%s, %s), want unknown entity_mismatch", got.Status, got.Reason)
	}
	if got.Closest == nil || got.Closest.Note != "insufficient independent roots after entity mismatch exclusions" {
		t.Fatalf("closest = %#v, want the exclusion note", got.Closest)
	}
	if encoded, encodeErr := service.EncodeResponse(context.Background(), got); encodeErr != nil || len(encoded) == 0 {
		t.Fatalf("EncodeResponse() = (%d bytes, %v)", len(encoded), encodeErr)
	}
}

// A generic shortfall without any exclusions must keep its existing reason.
func TestAnswerKeepsInsufficientWithoutExclusions(t *testing.T) {
	fetchedAt := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	searcher := &stubAnswerSearcher{response: successfulSearch("https://one.example.com/fact")}
	searcher.response.Query = "anthropic price"
	extractor := &stubAnswerExtractor{response: successfulConsensus("price", knownField(`"$3.00"`, 1, 1, fetchedAt))}
	service, err := NewService(searcher, extractor)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	request := &models.AnswerRequest{Spec: models.FactSpec{Subject: "anthropic", Predicate: "price"}}
	got, err := service.Answer(context.Background(), request)
	if err != nil {
		t.Fatalf("Answer() error = %v", err)
	}
	if got.Reason != models.AnswerUnknownInsufficient {
		t.Fatalf("reason = %s, want plain insufficiency", got.Reason)
	}
}

// When every found source is about the wrong entity, the no-valid-source
// outcome must say so instead of hiding behind a generic reason.
func TestAnswerReportsEntityMismatchWhenAllSourcesExcluded(t *testing.T) {
	searcher := &stubAnswerSearcher{response: successfulSearch(
		"https://one.example.com/fact",
		"https://two.example.net/fact",
	)}
	searcher.response.Query = "anthropic price"
	extractor := &stubAnswerExtractor{
		response: &models.MultiExtractResponse{
			Success: false,
			Sources: []models.MultiExtractSource{
				excludedMismatchSource("https://one.example.com/fact", "Anthropic Insurance Group"),
				excludedMismatchSource("https://two.example.net/fact", "Anthropic Insurance Group"),
			},
			Error: &models.ErrorDetail{Code: models.ErrCodeNoValidSource, Message: "no valid extraction source"},
		},
		err: models.NewScrapeError(models.ErrCodeNoValidSource, "no valid extraction source", nil),
	}
	service, err := NewService(searcher, extractor)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	request := &models.AnswerRequest{Spec: models.FactSpec{Subject: "anthropic", Predicate: "price"}}
	got, err := service.Answer(context.Background(), request)
	if err != nil {
		t.Fatalf("Answer() error = %v", err)
	}
	if got.Status != models.AnswerStatusUnknown || got.Reason != models.AnswerUnknownEntityMismatch {
		t.Fatalf("response = (%s, %s), want unknown entity_mismatch", got.Status, got.Reason)
	}
	if got.Closest != nil || len(got.Conflicts) != 0 {
		t.Fatalf("all-excluded response must carry no candidates, got %#v", got)
	}
	if encoded, encodeErr := service.EncodeResponse(context.Background(), got); encodeErr != nil || len(encoded) == 0 {
		t.Fatalf("EncodeResponse() = (%d bytes, %v)", len(encoded), encodeErr)
	}
}

// Belief evidence carries each source's attribution verdict; uncertain stays
// visible instead of being laundered into a match.
func TestAnswerAnnotatesBeliefEvidenceWithEntityVerdicts(t *testing.T) {
	fetchedAt := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	searcher := &stubAnswerSearcher{response: successfulSearch(
		"https://one.example.com/fact",
		"https://two.example.net/fact",
	)}
	searcher.response.Query = "anthropic price"
	response := successfulConsensus("price", knownField(`"$3.00"`, 2, 2, fetchedAt))
	response.Sources[0].Entity = &models.EntityAttribution{Verdict: models.EntityVerdictMatch, Tier: "exact"}
	response.Sources[1].Entity = &models.EntityAttribution{Verdict: models.EntityVerdictUncertain}
	extractor := &stubAnswerExtractor{response: response}
	service, err := NewService(searcher, extractor)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	request := &models.AnswerRequest{Spec: models.FactSpec{Subject: "anthropic", Predicate: "price"}}
	got, err := service.Answer(context.Background(), request)
	if err != nil {
		t.Fatalf("Answer() error = %v", err)
	}
	if got.Status != models.AnswerStatusKnown || got.Belief == nil {
		t.Fatalf("response = %#v, want a known belief", got)
	}
	verdicts := make(map[string]string, len(got.Belief.Evidence))
	for _, item := range got.Belief.Evidence {
		verdicts[item.URL] = item.EntityVerdict
	}
	if verdicts["https://one.example.com/fact"] != models.EntityVerdictMatch ||
		verdicts["https://two.example.net/fact"] != models.EntityVerdictUncertain {
		t.Fatalf("evidence verdicts = %#v", verdicts)
	}
	if encoded, encodeErr := service.EncodeResponse(context.Background(), got); encodeErr != nil || len(encoded) == 0 {
		t.Fatalf("EncodeResponse() = (%d bytes, %v)", len(encoded), encodeErr)
	}
}
