package eav

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The golden corpus is the executable form of the PLAN.md §5.3 acceptance:
// cross-domain right-source-wrong-entity rows built by sibling-category
// perturbation. Docs are harvested snapshots (no raw HTML); LLM behavior
// replays from recordings so the full pipeline runs offline and
// deterministically. Re-record against a live model with scripts/eavcorpus.

type goldenDoc struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	Cleaned string `json:"cleaned"`
}

type goldenLabel struct {
	Subject string `json:"subject"`
	Hint    string `json:"hint,omitempty"`
	Doc     string `json:"doc"`
	Label   string `json:"label"`
	Domain  string `json:"domain"`
	Hard    bool   `json:"hard,omitempty"`
	Waived  bool   `json:"waived,omitempty"`
	Note    string `json:"note,omitempty"`
}

type goldenCorpus struct {
	docs         map[string]goldenDoc
	labels       []goldenLabel
	extractByURL map[string]json.RawMessage
	refereeByKey map[string]json.RawMessage
	urlToDocID   map[string]string
}

func goldenRefereeKey(docID, subjectNorm string) string {
	return docID + "\x00" + subjectNorm
}

func readGoldenLines(t *testing.T, path string, handle func(line []byte)) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64<<10), 256<<10)
	line := 0
	for scanner.Scan() {
		line++
		raw := scanner.Bytes()
		if len(raw) == 0 {
			continue
		}
		handle(raw)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan %s line %d: %v", path, line, err)
	}
}

func loadGoldenCorpus(t *testing.T) *goldenCorpus {
	t.Helper()
	corpus := &goldenCorpus{
		docs:         make(map[string]goldenDoc),
		extractByURL: make(map[string]json.RawMessage),
		refereeByKey: make(map[string]json.RawMessage),
		urlToDocID:   make(map[string]string),
	}
	root := filepath.Join("testdata", "golden")

	readGoldenLines(t, filepath.Join(root, "docs.jsonl"), func(line []byte) {
		var doc goldenDoc
		if err := json.Unmarshal(line, &doc); err != nil {
			t.Fatalf("decode doc row: %v", err)
		}
		if doc.ID == "" || doc.URL == "" || doc.Title == "" || doc.Cleaned == "" {
			t.Fatalf("doc row %q is incomplete", doc.ID)
		}
		if len(doc.Cleaned) > MaxHeadWindowBytes {
			t.Fatalf("doc %q cleaned exceeds the stored head-window budget", doc.ID)
		}
		if _, duplicate := corpus.docs[doc.ID]; duplicate {
			t.Fatalf("duplicate doc id %q", doc.ID)
		}
		corpus.docs[doc.ID] = doc
		corpus.urlToDocID[doc.URL] = doc.ID
	})

	readGoldenLines(t, filepath.Join(root, "labels.jsonl"), func(line []byte) {
		var label goldenLabel
		if err := json.Unmarshal(line, &label); err != nil {
			t.Fatalf("decode label row: %v", err)
		}
		if label.Subject == "" || label.Domain == "" {
			t.Fatalf("label row %+v is incomplete", label)
		}
		if _, ok := corpus.docs[label.Doc]; !ok {
			t.Fatalf("label references unknown doc %q", label.Doc)
		}
		if label.Label != "match" && label.Label != "mismatch" {
			t.Fatalf("label %q must be match or mismatch", label.Label)
		}
		corpus.labels = append(corpus.labels, label)
	})
	if len(corpus.labels) == 0 {
		t.Fatal("golden corpus has no labels")
	}

	readGoldenLines(t, filepath.Join(root, "recordings", "extract.jsonl"), func(line []byte) {
		var recording struct {
			Doc   string          `json:"doc"`
			Reply json.RawMessage `json:"reply"`
		}
		if err := json.Unmarshal(line, &recording); err != nil {
			t.Fatalf("decode extract recording: %v", err)
		}
		doc, ok := corpus.docs[recording.Doc]
		if !ok {
			t.Fatalf("extract recording references unknown doc %q", recording.Doc)
		}
		corpus.extractByURL[doc.URL] = recording.Reply
	})
	for id, doc := range corpus.docs {
		if _, ok := corpus.extractByURL[doc.URL]; !ok {
			t.Fatalf("doc %q has no extraction recording", id)
		}
	}

	readGoldenLines(t, filepath.Join(root, "recordings", "referee.jsonl"), func(line []byte) {
		var recording struct {
			Doc     string          `json:"doc"`
			Subject string          `json:"subject"`
			Reply   json.RawMessage `json:"reply"`
		}
		if err := json.Unmarshal(line, &recording); err != nil {
			t.Fatalf("decode referee recording: %v", err)
		}
		if _, ok := corpus.docs[recording.Doc]; !ok {
			t.Fatalf("referee recording references unknown doc %q", recording.Doc)
		}
		if recording.Subject != Normalize(recording.Subject) {
			t.Fatalf("referee recording subject %q must be stored in Normalize form", recording.Subject)
		}
		corpus.refereeByKey[goldenRefereeKey(recording.Doc, recording.Subject)] = recording.Reply
	})
	return corpus
}

type goldenReplayExtractor struct {
	replies map[string]json.RawMessage
	misses  map[string]struct{}
}

func (replay *goldenReplayExtractor) ExtractEntities(
	_ context.Context,
	doc Document,
	_ []Candidate,
) (DocumentEntities, error) {
	raw, ok := replay.replies[doc.URL]
	if !ok {
		replay.misses[doc.URL] = struct{}{}
		return DocumentEntities{}, nil
	}
	return DecodeExtractionReply(raw)
}

type goldenReplayReferee struct {
	urlToDocID map[string]string
	replies    map[string]json.RawMessage
	misses     map[string]struct{}
}

func (replay *goldenReplayReferee) SameReferent(
	_ context.Context,
	subject Subject,
	_ Entity,
	doc Document,
) (RefereeVerdict, error) {
	key := goldenRefereeKey(replay.urlToDocID[doc.URL], Normalize(subject.Name))
	raw, ok := replay.replies[key]
	if !ok {
		replay.misses[key] = struct{}{}
		return RefereeVerdict{Answer: RefereeUnsure}, nil
	}
	return DecodeRefereeReply(raw)
}

// goldenTally accumulates alarm quality for one slice of rows.
type goldenTally struct {
	matchTotal     int // clean rows
	matchAlarmed   int // clean rows judged mismatch: the false-positive count
	matchUncertain int
	misTotal       int // mismatch rows
	misAlarmed     int // true alarms
	misMatched     int // silent failures: wrong entity accepted
	misUncertain   int
}

func (tally *goldenTally) add(label goldenLabel, verdict Verdict) {
	if label.Label == "match" {
		tally.matchTotal++
		switch verdict {
		case VerdictMismatch:
			tally.matchAlarmed++
		case VerdictUncertain:
			tally.matchUncertain++
		}
		return
	}
	tally.misTotal++
	switch verdict {
	case VerdictMismatch:
		tally.misAlarmed++
	case VerdictMatch:
		tally.misMatched++
	default:
		tally.misUncertain++
	}
}

func (tally *goldenTally) precision() (float64, bool) {
	alarms := tally.misAlarmed + tally.matchAlarmed
	if alarms == 0 {
		return 0, false
	}
	return float64(tally.misAlarmed) / float64(alarms), true
}

func (tally *goldenTally) recall() float64 {
	if tally.misTotal == 0 {
		return 0
	}
	return float64(tally.misAlarmed) / float64(tally.misTotal)
}

func (tally *goldenTally) cleanFalsePositiveRate() float64 {
	if tally.matchTotal == 0 {
		return 0
	}
	return float64(tally.matchAlarmed) / float64(tally.matchTotal)
}

func (tally *goldenTally) uncertainRate() float64 {
	total := tally.matchTotal + tally.misTotal
	if total == 0 {
		return 0
	}
	return float64(tally.matchUncertain+tally.misUncertain) / float64(total)
}

func runGolden(
	t *testing.T,
	corpus *goldenCorpus,
	extractor EntityExtractor,
	referee Referee,
) (*goldenTally, map[string]*goldenTally) {
	t.Helper()
	judge, err := NewJudge(Config{Extractor: extractor, Referee: referee})
	if err != nil {
		t.Fatalf("NewJudge returned error: %v", err)
	}
	overall := &goldenTally{}
	buckets := make(map[string]*goldenTally)
	bucket := func(key string) *goldenTally {
		if _, ok := buckets[key]; !ok {
			buckets[key] = &goldenTally{}
		}
		return buckets[key]
	}
	ctx := context.Background()
	for _, label := range corpus.labels {
		fixture := corpus.docs[label.Doc]
		judgment, judgeErr := judge.JudgeDocument(
			ctx,
			Subject{Name: label.Subject, Hint: label.Hint},
			Document{URL: fixture.URL, Title: fixture.Title, Cleaned: fixture.Cleaned},
		)
		if judgeErr != nil {
			t.Fatalf("JudgeDocument(%q, %q) returned error: %v", label.Subject, label.Doc, judgeErr)
		}
		if label.Waived {
			t.Logf(
				"waived (known limit): %q vs %s judged %s/%s — %s",
				label.Subject, label.Doc, judgment.Verdict, judgment.Tier, label.Note,
			)
			continue
		}
		misjudged := (label.Label == "match" && judgment.Verdict == VerdictMismatch) ||
			(label.Label == "mismatch" && judgment.Verdict != VerdictMismatch)
		if misjudged {
			t.Logf(
				"misjudged: %q vs %s labeled %s, judged %s/%s (similarity %.3f)",
				label.Subject, label.Doc, label.Label, judgment.Verdict, judgment.Tier,
				judgment.Similarity,
			)
		}
		overall.add(label, judgment.Verdict)
		bucket("domain/"+label.Domain).add(label, judgment.Verdict)
		hardness := "easy"
		if label.Hard {
			hardness = "hard"
		}
		bucket("split/"+hardness).add(label, judgment.Verdict)
	}
	return overall, buckets
}

func logGoldenTallies(t *testing.T, mode string, overall *goldenTally, buckets map[string]*goldenTally) {
	t.Helper()
	report := func(name string, tally *goldenTally) {
		precision, hasAlarms := tally.precision()
		precisionText := "n/a"
		if hasAlarms {
			precisionText = fmt.Sprintf("%.3f", precision)
		}
		t.Logf(
			"%s %-18s clean=%d fp=%d | mismatch=%d tp=%d silent=%d unsure=%d | P=%s R=%.3f FP=%.3f U=%.3f",
			mode, name,
			tally.matchTotal, tally.matchAlarmed,
			tally.misTotal, tally.misAlarmed, tally.misMatched, tally.misUncertain,
			precisionText, tally.recall(), tally.cleanFalsePositiveRate(), tally.uncertainRate(),
		)
	}
	report("overall", overall)
	keys := make([]string, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	for _, key := range sortedStrings(keys) {
		report(key, buckets[key])
	}
}

func sortedStrings(values []string) []string {
	for first := 1; first < len(values); first++ {
		for second := first; second > 0 && values[second] < values[second-1]; second-- {
			values[second], values[second-1] = values[second-1], values[second]
		}
	}
	return values
}

// TestGoldenAttributionFullPipeline runs harvest -> blind extraction ->
// ladder -> referee over the whole corpus with recorded LLM replies and
// enforces the PLAN.md §5.3 gates: alarm precision > 0.90, alarm recall >
// 0.90 (uncertain counts as a miss), clean false positives < 0.02.
func TestGoldenAttributionFullPipeline(t *testing.T) {
	corpus := loadGoldenCorpus(t)
	extractor := &goldenReplayExtractor{replies: corpus.extractByURL, misses: map[string]struct{}{}}
	referee := &goldenReplayReferee{
		urlToDocID: corpus.urlToDocID,
		replies:    corpus.refereeByKey,
		misses:     map[string]struct{}{},
	}
	overall, buckets := runGolden(t, corpus, extractor, referee)
	logGoldenTallies(t, "full", overall, buckets)
	for miss := range extractor.misses {
		t.Errorf("missing extraction recording for %s", miss)
	}
	for miss := range referee.misses {
		t.Errorf("missing referee recording for key %q", miss)
	}

	precision, hasAlarms := overall.precision()
	if !hasAlarms {
		t.Fatal("full pipeline produced no alarms at all")
	}
	if precision <= 0.90 {
		t.Errorf("alarm precision %.3f must exceed 0.90", precision)
	}
	if recall := overall.recall(); recall <= 0.90 {
		t.Errorf("alarm recall %.3f must exceed 0.90", recall)
	}
	if rate := overall.cleanFalsePositiveRate(); rate >= 0.02 {
		t.Errorf("clean false-positive rate %.3f must stay below 0.02", rate)
	}
}

// TestGoldenAttributionDeterministicFloor runs the same corpus with the
// referee disabled: the gray zone stays uncertain, so recall is expected to
// drop (that drop is the referee's reason to exist) while precision and the
// clean false-positive rate must hold on the floor alarms alone.
func TestGoldenAttributionDeterministicFloor(t *testing.T) {
	corpus := loadGoldenCorpus(t)
	extractor := &goldenReplayExtractor{replies: corpus.extractByURL, misses: map[string]struct{}{}}
	overall, buckets := runGolden(t, corpus, extractor, nil)
	logGoldenTallies(t, "det-only", overall, buckets)
	for miss := range extractor.misses {
		t.Errorf("missing extraction recording for %s", miss)
	}

	precision, hasAlarms := overall.precision()
	if !hasAlarms {
		t.Fatal("deterministic floor produced no alarms at all")
	}
	if precision <= 0.90 {
		t.Errorf("deterministic alarm precision %.3f must exceed 0.90", precision)
	}
	if rate := overall.cleanFalsePositiveRate(); rate >= 0.02 {
		t.Errorf("clean false-positive rate %.3f must stay below 0.02", rate)
	}
	t.Logf("deterministic-only recall %.3f (not gated; the referee closes this gap)", overall.recall())
}

func TestGoldenExtractionQuotesPreserveVerbatimDetails(t *testing.T) {
	corpus := loadGoldenCorpus(t)
	docIDs := make([]string, 0, len(corpus.docs))
	for docID := range corpus.docs {
		docIDs = append(docIDs, docID)
	}
	for _, docID := range sortedStrings(docIDs) {
		t.Run(docID, func(t *testing.T) {
			fixture := corpus.docs[docID]
			raw := corpus.extractByURL[fixture.URL]
			extracted, err := DecodeExtractionReply(raw)
			if err != nil {
				t.Fatalf("decode recorded extraction: %v", err)
			}
			if extracted.Primary == nil {
				t.Fatal("entity-page recording has no primary entity")
			}
			if extracted.Primary.Quote == "" {
				t.Fatal("recorded primary entity has no evidence quote")
			}
			if !strings.Contains(fixture.Cleaned, extracted.Primary.Quote) {
				t.Fatalf("recorded quote was cleaned instead of copied verbatim: %q", extracted.Primary.Quote)
			}

			doc := Document{URL: fixture.URL, Title: fixture.Title, Cleaned: fixture.Cleaned}
			gated, err := AnchoredExtraction(
				context.Background(),
				EntityExtractorFunc(func(context.Context, Document, []Candidate) (DocumentEntities, error) {
					return extracted, nil
				}),
				doc,
				HarvestCandidates(doc),
			)
			if err != nil || gated.Primary == nil {
				t.Fatalf("verbatim recording did not survive anchoring: primary=%#v error=%v", gated.Primary, err)
			}
		})
	}
}
