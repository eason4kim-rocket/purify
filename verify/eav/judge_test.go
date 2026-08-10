package eav

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/use-agent/purify/evidence"
)

type fakeReferee struct {
	calls   int
	subject Subject
	entity  Entity
	doc     Document
	verdict RefereeVerdict
	err     error
}

func (fake *fakeReferee) SameReferent(
	_ context.Context,
	subject Subject,
	entity Entity,
	doc Document,
) (RefereeVerdict, error) {
	fake.calls++
	fake.subject = subject
	fake.entity = entity
	fake.doc = doc
	return fake.verdict, fake.err
}

func judgeTestDocument() Document {
	return Document{
		URL:   "https://applebank.example/personal-banking",
		Title: "Apple Bank - Personal Banking",
		Cleaned: "Apple Bank offers personal savings accounts across New York. " +
			"Apple Bank was founded in 1863. The ticker ABNK is listed on the exchange. " +
			"Contact Apple Bank today.",
	}
}

func judgeTestExtractor() *fakeExtractor {
	return &fakeExtractor{result: DocumentEntities{Primary: &Entity{
		Name:    "Apple Bank",
		Kind:    KindOrganization,
		Aliases: []string{"ABNK"},
		Quote:   "Apple Bank was founded in 1863.",
	}}}
}

func newTestJudge(t *testing.T, extractor EntityExtractor, referee Referee) *Judge {
	t.Helper()
	judge, err := NewJudge(Config{Extractor: extractor, Referee: referee})
	if err != nil {
		t.Fatalf("NewJudge returned error: %v", err)
	}
	return judge
}

func TestJudgeDocumentPaths(t *testing.T) {
	primaryQuote := "Apple Bank was founded in 1863."
	distinguishing := "Apple Bank offers personal savings accounts across New York."
	cases := []struct {
		name             string
		subject          Subject
		refereeVerdict   *RefereeVerdict // nil = no referee configured
		refereeErr       error
		wantVerdict      Verdict
		wantTier         MatchTier
		wantQuote        string // "" = no evidence expected
		wantRefereeCalls int
	}{
		{
			name: "exact match", subject: Subject{Name: "Apple Bank"},
			refereeVerdict: &RefereeVerdict{Answer: RefereeUnsure},
			wantVerdict:    VerdictMatch, wantTier: TierExact, wantQuote: primaryQuote,
		},
		{
			name: "suffix match", subject: Subject{Name: "Apple Bank Inc"},
			refereeVerdict: &RefereeVerdict{Answer: RefereeUnsure},
			wantVerdict:    VerdictMatch, wantTier: TierSuffix, wantQuote: primaryQuote,
		},
		{
			name: "alias match", subject: Subject{Name: "ABNK"},
			refereeVerdict: &RefereeVerdict{Answer: RefereeUnsure},
			wantVerdict:    VerdictMatch, wantTier: TierAlias, wantQuote: primaryQuote,
		},
		{
			name: "floor mismatch skips referee", subject: Subject{Name: "Microsoft Azure"},
			refereeVerdict: &RefereeVerdict{Answer: RefereeSame},
			wantVerdict:    VerdictMismatch, wantTier: TierFloor, wantQuote: primaryQuote,
		},
		{
			name: "gray without referee", subject: Subject{Name: "Apple"},
			wantVerdict: VerdictUncertain, wantTier: TierNone,
		},
		{
			name: "gray referee same", subject: Subject{Name: "Apple"},
			refereeVerdict: &RefereeVerdict{Answer: RefereeSame},
			wantVerdict:    VerdictMatch, wantTier: TierReferee, wantQuote: primaryQuote,
			wantRefereeCalls: 1,
		},
		{
			name: "gray referee different anchored", subject: Subject{Name: "Apple"},
			refereeVerdict: &RefereeVerdict{Answer: RefereeDifferent, Quote: distinguishing},
			wantVerdict:    VerdictMismatch, wantTier: TierReferee, wantQuote: distinguishing,
			wantRefereeCalls: 1,
		},
		{
			name: "gray referee different hallucinated quote", subject: Subject{Name: "Apple"},
			refereeVerdict: &RefereeVerdict{
				Answer: RefereeDifferent,
				Quote:  "fabricated distinguishing narrative zzz",
			},
			wantVerdict: VerdictUncertain, wantTier: TierNone, wantRefereeCalls: 1,
		},
		{
			name: "gray referee different empty quote", subject: Subject{Name: "Apple"},
			refereeVerdict: &RefereeVerdict{Answer: RefereeDifferent},
			wantVerdict:    VerdictUncertain, wantTier: TierNone, wantRefereeCalls: 1,
		},
		{
			name: "gray referee unsure", subject: Subject{Name: "Apple"},
			refereeVerdict: &RefereeVerdict{Answer: RefereeUnsure},
			wantVerdict:    VerdictUncertain, wantTier: TierNone, wantRefereeCalls: 1,
		},
		{
			name: "gray referee error", subject: Subject{Name: "Apple"},
			refereeVerdict: &RefereeVerdict{}, refereeErr: errors.New("provider returned HTTP 500"),
			wantVerdict: VerdictUncertain, wantTier: TierNone, wantRefereeCalls: 1,
		},
	}

	judgments := make([]Judgment, 0, len(cases))
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			doc := judgeTestDocument()
			extractor := judgeTestExtractor()
			var referee Referee
			var fake *fakeReferee
			if testCase.refereeVerdict != nil {
				fake = &fakeReferee{verdict: *testCase.refereeVerdict, err: testCase.refereeErr}
				referee = fake
			}
			judge := newTestJudge(t, extractor, referee)
			got, err := judge.JudgeDocument(context.Background(), testCase.subject, doc)
			if err != nil {
				t.Fatalf("JudgeDocument returned error: %v", err)
			}
			if got.Verdict != testCase.wantVerdict || got.Tier != testCase.wantTier {
				t.Fatalf(
					"verdict = (%s, %q), want (%s, %q)",
					got.Verdict, got.Tier, testCase.wantVerdict, testCase.wantTier,
				)
			}
			if got.Subject != Normalize(testCase.subject.Name) {
				t.Fatalf("judgment subject %q, want %q", got.Subject, Normalize(testCase.subject.Name))
			}
			if got.DocEntity == nil || got.DocEntity.Name != "Apple Bank" {
				t.Fatalf("judgment must carry the gated document entity, got %#v", got.DocEntity)
			}
			if testCase.wantQuote == "" {
				if got.Evidence != nil {
					t.Fatalf("uncertain judgment must carry no evidence, got %#v", got.Evidence)
				}
			} else {
				if got.Evidence == nil || got.Evidence.Quote != testCase.wantQuote {
					t.Fatalf("evidence = %#v, want quote %q", got.Evidence, testCase.wantQuote)
				}
				if got.Evidence.Method == evidence.MethodUnlocated {
					t.Fatal("decided evidence must be located")
				}
			}
			if fake != nil && fake.calls != testCase.wantRefereeCalls {
				t.Fatalf("referee calls = %d, want %d", fake.calls, testCase.wantRefereeCalls)
			}
			judgments = append(judgments, got)
		})
	}

	// False-positive discipline: an alarm can only come from the similarity
	// floor or an anchored referee "different" — never any other tier.
	for _, judgment := range judgments {
		if judgment.Verdict == VerdictMismatch &&
			judgment.Tier != TierFloor && judgment.Tier != TierReferee {
			t.Fatalf("mismatch carried tier %q; only floor and referee may alarm", judgment.Tier)
		}
		if judgment.Verdict != VerdictUncertain && judgment.Evidence == nil {
			t.Fatalf("decided verdict %s carried no evidence", judgment.Verdict)
		}
	}
}

func TestJudgeDocumentUncertainWithoutPrimary(t *testing.T) {
	doc := judgeTestDocument()
	subject := Subject{Name: "Apple Bank"}

	empty := &fakeExtractor{}
	judge := newTestJudge(t, empty, nil)
	got, err := judge.JudgeDocument(context.Background(), subject, doc)
	if err != nil || got.Verdict != VerdictUncertain || got.DocEntity != nil {
		t.Fatalf("no primary must judge uncertain, got (%#v, %v)", got, err)
	}
	if empty.calls != 1 {
		t.Fatalf("extractor calls = %d, want 1", empty.calls)
	}

	failing := &fakeExtractor{err: errors.New("provider returned HTTP 500")}
	judge = newTestJudge(t, failing, nil)
	got, err = judge.JudgeDocument(context.Background(), subject, doc)
	if err != nil || got.Verdict != VerdictUncertain {
		t.Fatalf("extraction failure must judge uncertain, got (%#v, %v)", got, err)
	}
}

func TestJudgeDocumentRefusesUnusableInput(t *testing.T) {
	doc := judgeTestDocument()
	cases := []struct {
		name    string
		subject Subject
		doc     Document
	}{
		{"empty subject", Subject{}, doc},
		{"decoration subject", Subject{Name: "..."}, doc},
		{"oversized subject", Subject{Name: strings.Repeat("a", MaxSubjectBytes+1)}, doc},
		{"empty cleaned", Subject{Name: "Apple Bank"}, Document{Title: doc.Title}},
		{
			"oversized cleaned", Subject{Name: "Apple Bank"},
			Document{Title: doc.Title, Cleaned: strings.Repeat("a", MaxDocumentBytes+1)},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			extractor := judgeTestExtractor()
			judge := newTestJudge(t, extractor, nil)
			got, err := judge.JudgeDocument(context.Background(), testCase.subject, testCase.doc)
			if err != nil || got.Verdict != VerdictUncertain {
				t.Fatalf("unusable input must judge uncertain, got (%#v, %v)", got, err)
			}
			if extractor.calls != 0 {
				t.Fatal("unusable input must not reach the extractor")
			}
		})
	}
}

func TestJudgeConfigurationErrors(t *testing.T) {
	if _, err := NewJudge(Config{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("NewJudge without extractor must fail, got %v", err)
	}
	var typedNil EntityExtractorFunc
	if _, err := NewJudge(Config{Extractor: typedNil}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("NewJudge with typed-nil extractor must fail, got %v", err)
	}

	var nilJudge *Judge
	if _, err := nilJudge.JudgeDocument(context.Background(), Subject{Name: "x"}, judgeTestDocument()); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("nil judge must return ErrNotConfigured, got %v", err)
	}

	// A typed-nil referee is treated as absent: the gray zone stays
	// uncertain without a call or a panic.
	judge := newTestJudge(t, judgeTestExtractor(), (*fakeReferee)(nil))
	got, err := judge.JudgeDocument(context.Background(), Subject{Name: "Apple"}, judgeTestDocument())
	if err != nil || got.Verdict != VerdictUncertain {
		t.Fatalf("typed-nil referee must leave the gray zone uncertain, got (%#v, %v)", got, err)
	}

	misconfigured := &fakeExtractor{err: fmt.Errorf("adapter: %w", ErrNotConfigured)}
	judge = newTestJudge(t, misconfigured, nil)
	if _, err := judge.JudgeDocument(context.Background(), Subject{Name: "Apple Bank"}, judgeTestDocument()); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("extractor configuration errors must escalate, got %v", err)
	}

	brokenReferee := &fakeReferee{err: fmt.Errorf("adapter: %w", ErrNotConfigured)}
	judge = newTestJudge(t, judgeTestExtractor(), brokenReferee)
	if _, err := judge.JudgeDocument(context.Background(), Subject{Name: "Apple"}, judgeTestDocument()); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("referee configuration errors must escalate, got %v", err)
	}
}

func TestJudgeDocumentContextDiscipline(t *testing.T) {
	doc := judgeTestDocument()
	subject := Subject{Name: "Apple Bank"}

	extractor := judgeTestExtractor()
	judge := newTestJudge(t, extractor, nil)
	if _, err := judge.JudgeDocument(nil, subject, doc); !errors.Is(err, ErrInvalidInput) { //nolint:staticcheck
		t.Fatalf("nil context must return ErrInvalidInput, got %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := judge.JudgeDocument(canceled, subject, doc); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled context must return its error, got %v", err)
	}
	if extractor.calls != 0 {
		t.Fatal("a canceled context must not reach the extractor")
	}

	midExtract, cancelExtract := context.WithCancel(context.Background())
	interrupted := EntityExtractorFunc(func(context.Context, Document, []Candidate) (DocumentEntities, error) {
		cancelExtract()
		return DocumentEntities{}, errors.New("interrupted")
	})
	judge = newTestJudge(t, interrupted, nil)
	if _, err := judge.JudgeDocument(midExtract, subject, doc); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation during extraction must surface, got %v", err)
	}

	midReferee, cancelReferee := context.WithCancel(context.Background())
	cancelingReferee := RefereeFunc(func(context.Context, Subject, Entity, Document) (RefereeVerdict, error) {
		cancelReferee()
		return RefereeVerdict{}, errors.New("interrupted")
	})
	judge = newTestJudge(t, judgeTestExtractor(), cancelingReferee)
	if _, err := judge.JudgeDocument(midReferee, Subject{Name: "Apple"}, doc); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation during refereeing must surface, got %v", err)
	}
}

func TestJudgeDocumentRefereeInput(t *testing.T) {
	tail := " Apple Bank was founded in 1863. The ticker ABNK is listed here."
	doc := Document{
		URL:     "https://applebank.example/about",
		Title:   "Apple Bank - Personal Banking",
		Cleaned: strings.Repeat("a", MaxHeadWindowBytes) + tail,
		RawHTML: "<html><body>raw</body></html>",
	}
	referee := &fakeReferee{verdict: RefereeVerdict{Answer: RefereeUnsure}}
	judge := newTestJudge(t, judgeTestExtractor(), referee)
	subject := Subject{Name: "Apple", Hint: strings.Repeat("h", MaxHintBytes+64)}
	if _, err := judge.JudgeDocument(context.Background(), subject, doc); err != nil {
		t.Fatalf("JudgeDocument returned error: %v", err)
	}
	if referee.calls != 1 {
		t.Fatalf("referee calls = %d, want 1", referee.calls)
	}
	if referee.subject.Name != "Apple" || len(referee.subject.Hint) != MaxHintBytes {
		t.Fatalf("referee subject must carry the truncated hint, got %d bytes", len(referee.subject.Hint))
	}
	if referee.entity.Name != "Apple Bank" || len(referee.entity.Aliases) != 1 ||
		referee.entity.Aliases[0] != "ABNK" {
		t.Fatalf("referee must see the gated entity, got %#v", referee.entity)
	}
	if referee.doc.Cleaned != doc.Cleaned[:MaxHeadWindowBytes] {
		t.Fatal("the referee must only see the head window")
	}
	if referee.doc.RawHTML != "" {
		t.Fatal("the referee must never see raw HTML")
	}
}
