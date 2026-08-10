package extract

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/scraper"
	"github.com/use-agent/purify/verify/eav"
)

type fakeSourceJudge struct {
	calls    int
	subject  eav.Subject
	doc      eav.Document
	judgment eav.Judgment
	err      error
}

func (fake *fakeSourceJudge) JudgeDocument(
	_ context.Context,
	subject eav.Subject,
	doc eav.Document,
) (eav.Judgment, error) {
	fake.calls++
	fake.subject = subject
	fake.doc = doc
	return fake.judgment, fake.err
}

func attributionTestArtifact() *Artifact {
	return &Artifact{
		Public: &models.ScrapeResponse{
			Content:  "Apple Bank was founded in 1863.",
			Metadata: models.Metadata{Title: "Apple Bank - About"},
		},
		Source: &scraper.ScrapeResult{RawHTML: "<html><body>Apple Bank</body></html>"},
	}
}

// The wire literals and the eav domain literals must stay in lockstep; the
// extract layer converts between them by string identity.
func TestEntityVerdictLiteralsMatchEAV(t *testing.T) {
	if models.EntityVerdictMatch != string(eav.VerdictMatch) ||
		models.EntityVerdictMismatch != string(eav.VerdictMismatch) ||
		models.EntityVerdictUncertain != string(eav.VerdictUncertain) {
		t.Fatal("models entity verdict literals must equal the eav verdict literals")
	}
}

func TestJudgeSourceAttribution(t *testing.T) {
	spec := &models.SubjectSpec{Name: "Apple", Hint: "savings account"}

	t.Run("disabled without judge", func(t *testing.T) {
		service := &Service{}
		if got := service.judgeSourceAttribution(context.Background(), spec, "https://a.example/", attributionTestArtifact()); got != nil {
			t.Fatalf("no judge must yield no attribution, got %#v", got)
		}
	})
	t.Run("disabled without subject", func(t *testing.T) {
		judge := &fakeSourceJudge{}
		service := &Service{sourceJudge: judge}
		if got := service.judgeSourceAttribution(context.Background(), nil, "https://a.example/", attributionTestArtifact()); got != nil {
			t.Fatalf("no subject must yield no attribution, got %#v", got)
		}
		if judge.calls != 0 {
			t.Fatal("no subject must not call the judge")
		}
	})
	t.Run("judge error degrades to uncertain", func(t *testing.T) {
		judge := &fakeSourceJudge{err: errors.New("provider returned HTTP 500")}
		service := &Service{sourceJudge: judge}
		got := service.judgeSourceAttribution(context.Background(), spec, "https://a.example/", attributionTestArtifact())
		if got == nil || got.Verdict != models.EntityVerdictUncertain {
			t.Fatalf("judge failure must degrade to uncertain, got %#v", got)
		}
	})
	t.Run("invalid verdict degrades to uncertain", func(t *testing.T) {
		judge := &fakeSourceJudge{judgment: eav.Judgment{Verdict: eav.Verdict("banana")}}
		service := &Service{sourceJudge: judge}
		got := service.judgeSourceAttribution(context.Background(), spec, "https://a.example/", attributionTestArtifact())
		if got == nil || got.Verdict != models.EntityVerdictUncertain {
			t.Fatalf("invalid verdict must degrade to uncertain, got %#v", got)
		}
	})
	t.Run("judgment maps onto the wire", func(t *testing.T) {
		judge := &fakeSourceJudge{judgment: eav.Judgment{
			Verdict:   eav.VerdictMismatch,
			Tier:      eav.TierReferee,
			DocEntity: &eav.Entity{Name: "Apple Bank", Kind: eav.KindOrganization},
			Evidence:  &evidence.Anchor{Quote: "Apple Bank was founded in 1863."},
		}}
		service := &Service{sourceJudge: judge}
		got := service.judgeSourceAttribution(context.Background(), spec, "https://a.example/final", attributionTestArtifact())
		want := models.EntityAttribution{
			Verdict: models.EntityVerdictMismatch,
			Name:    "Apple Bank",
			Kind:    "organization",
			Quote:   "Apple Bank was founded in 1863.",
			Tier:    "referee",
		}
		if got == nil || *got != want {
			t.Fatalf("attribution = %#v, want %#v", got, want)
		}
		if judge.subject.Name != "Apple" || judge.subject.Hint != "savings account" {
			t.Fatalf("judge must receive the request subject, got %#v", judge.subject)
		}
		if judge.doc.URL != "https://a.example/final" ||
			judge.doc.Title != "Apple Bank - About" ||
			judge.doc.Cleaned != "Apple Bank was founded in 1863." ||
			judge.doc.RawHTML == "" {
			t.Fatalf("judge must receive the artifact document, got %#v", judge.doc)
		}
	})
}

func TestApplyEntityAttribution(t *testing.T) {
	base := models.MultiExtractSource{URL: "https://a.example/", Success: false}

	t.Run("nil attribution changes nothing", func(t *testing.T) {
		summary := base
		if applyEntityAttribution(&summary, nil, "https://a.example/final", "sha256:aa") {
			t.Fatal("nil attribution must not exclude")
		}
		if summary.Entity != nil {
			t.Fatal("nil attribution must not annotate")
		}
	})
	t.Run("match annotates without excluding", func(t *testing.T) {
		summary := base
		attribution := &models.EntityAttribution{Verdict: models.EntityVerdictMatch, Tier: "exact"}
		if applyEntityAttribution(&summary, attribution, "https://a.example/final", "sha256:aa") {
			t.Fatal("a match must not exclude the source")
		}
		if summary.Entity != attribution || summary.Status == models.MultiExtractSourceStatusEntityMismatch {
			t.Fatalf("match annotation is wrong: %#v", summary)
		}
	})
	t.Run("mismatch excludes with full accounting", func(t *testing.T) {
		summary := base
		attribution := &models.EntityAttribution{Verdict: models.EntityVerdictMismatch, Name: "Apple Bank"}
		if !applyEntityAttribution(&summary, attribution, "https://a.example/final", "sha256:aa") {
			t.Fatal("a mismatch must exclude the source")
		}
		if summary.Success || summary.Status != models.MultiExtractSourceStatusEntityMismatch ||
			summary.FinalURL != "https://a.example/final" || summary.SnapshotID != "sha256:aa" ||
			summary.Entity != attribution || summary.Error == nil ||
			summary.Error.Code != models.ErrCodeEntityMismatch {
			t.Fatalf("exclusion accounting is wrong: %#v", summary)
		}
	})
}

func TestPrepareRequestExpectedSubject(t *testing.T) {
	valid := func() *models.ExtractRequest {
		return &models.ExtractRequest{
			URL:             "https://example.com/page",
			Schema:          []byte(`{"price":"string"}`),
			ExpectedSubject: &models.SubjectSpec{Name: "  Apple Inc.  ", Hint: " stock price "},
		}
	}

	prepared, err := prepareRequest(valid())
	if err != nil {
		t.Fatalf("prepareRequest returned error: %v", err)
	}
	if prepared.ExpectedSubject == nil || prepared.ExpectedSubject.Name != "Apple Inc." ||
		prepared.ExpectedSubject.Hint != "stock price" {
		t.Fatalf("expected subject must be trimmed and cloned, got %#v", prepared.ExpectedSubject)
	}
	original := valid()
	prepared, err = prepareRequest(original)
	if err != nil {
		t.Fatalf("prepareRequest returned error: %v", err)
	}
	if prepared.ExpectedSubject == original.ExpectedSubject {
		t.Fatal("prepared request must not share the caller's subject pointer")
	}

	invalid := []models.SubjectSpec{
		{Name: "   "},
		{Name: strings.Repeat("a", models.MaxAnswerSubjectBytes+1)},
		{Name: "Apple", Hint: strings.Repeat("h", models.MaxAnswerPredicateBytes+1)},
		{Name: "Apple\x00Inc"},
	}
	for index, spec := range invalid {
		request := valid()
		subject := spec
		request.ExpectedSubject = &subject
		if _, err := prepareRequest(request); err == nil {
			t.Fatalf("invalid subject %d must be rejected", index)
		}
	}
}

func TestExtractRejectsExpectedSubjectForSingleSource(t *testing.T) {
	service := &Service{now: time.Now}
	_, err := service.Extract(context.Background(), &models.ExtractRequest{
		URL:             "https://example.com/page",
		Schema:          []byte(`{"price":"string"}`),
		ExpectedSubject: &models.SubjectSpec{Name: "Apple"},
	})
	if err == nil || !strings.Contains(err.Error(), "expected_subject requires multi-source extraction") {
		t.Fatalf("single-source extraction must reject expected_subject, got %v", err)
	}
}
