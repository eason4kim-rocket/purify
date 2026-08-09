package compiler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/publicnet"
	"github.com/use-agent/purify/snapshot"
)

type fakeHealSnapshot struct {
	content      []byte
	observations []snapshot.Meta
	contentErr   error
	observeErr   error
}

type fakeHealSnapshotReader struct {
	mu      sync.Mutex
	values  map[snapshot.ID]fakeHealSnapshot
	content map[snapshot.ID]int
	observe map[snapshot.ID]int
	onHas   func(context.Context, snapshot.ID, func(snapshot.Meta) bool) (bool, error)
}

func newFakeHealSnapshotReader() *fakeHealSnapshotReader {
	return &fakeHealSnapshotReader{
		values:  make(map[snapshot.ID]fakeHealSnapshot),
		content: make(map[snapshot.ID]int), observe: make(map[snapshot.ID]int),
	}
}

func (reader *fakeHealSnapshotReader) Content(id snapshot.ID, maximum int) ([]byte, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	reader.content[id]++
	value, found := reader.values[id]
	if !found {
		return nil, errors.New("missing snapshot")
	}
	if value.contentErr != nil {
		return nil, value.contentErr
	}
	if len(value.content) > maximum {
		return nil, snapshot.ErrContentTooLarge
	}
	return append([]byte(nil), value.content...), nil
}

func (reader *fakeHealSnapshotReader) HasObservationContext(
	ctx context.Context,
	id snapshot.ID,
	match func(snapshot.Meta) bool,
) (bool, error) {
	reader.mu.Lock()
	reader.observe[id]++
	onHas := reader.onHas
	value, found := reader.values[id]
	reader.mu.Unlock()
	if onHas != nil {
		return onHas(ctx, id, match)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !found {
		return false, errors.New("missing snapshot metadata")
	}
	if value.observeErr != nil {
		return false, value.observeErr
	}
	for _, observation := range value.observations {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if match(observation) {
			return true, nil
		}
	}
	return false, nil
}

func (reader *fakeHealSnapshotReader) put(
	id string,
	html string,
	observations ...snapshot.Meta,
) {
	reader.mu.Lock()
	reader.values[snapshot.ID(id)] = fakeHealSnapshot{
		content: []byte(html), observations: append([]snapshot.Meta(nil), observations...),
	}
	reader.mu.Unlock()
}

func (reader *fakeHealSnapshotReader) calls(id string) (int, int) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return reader.content[snapshot.ID(id)], reader.observe[snapshot.ID(id)]
}

type selfHealHarness struct {
	*candidateHarness
	snapshots   *fakeHealSnapshotReader
	healer      *Healer
	set         SampleSet
	sourceSet   SampleSet
	schema      json.RawMessage
	source      Extractor
	staleReason string
	candidate   Candidate
	pending     Submission
}

func newSelfHealHarness(t *testing.T, count int) *selfHealHarness {
	t.Helper()
	candidateHarness := newCandidateHarness(t)
	set, schema := candidateSampleSet(t, candidateHarness, 2000, uint64(1)<<63, count)
	initial, err := candidateHarness.store.SubmitCandidate(
		context.Background(), candidateForSet(set, schema, "h1.name"),
	)
	if err != nil {
		t.Fatalf("SubmitCandidate(initial): %v", err)
	}
	markCandidateSourceStale(t, candidateHarness, initial.Extractor.ID, "required_empty_rate")
	candidate := candidateForSet(set, schema, ".name")
	pending, err := candidateHarness.store.SubmitCandidate(context.Background(), candidate)
	if err != nil || pending.Status != SubmissionPendingHeal {
		t.Fatalf("SubmitCandidate(pending) = %+v, %v", pending, err)
	}
	snapshots := newFakeHealSnapshotReader()
	healer, err := NewHealer(candidateHarness.store, snapshots)
	if err != nil {
		t.Fatalf("NewHealer: %v", err)
	}
	return &selfHealHarness{
		candidateHarness: candidateHarness, snapshots: snapshots, healer: healer,
		set: set, sourceSet: set, schema: schema, source: initial.Extractor,
		staleReason: "required_empty_rate",
		candidate:   candidate, pending: pending,
	}
}

func newDriftSelfHealHarness(t *testing.T, count int) *selfHealHarness {
	t.Helper()
	candidateHarness := newCandidateHarness(t)
	sourceSet, schema := candidateSampleSet(t, candidateHarness, 3000, uint64(1)<<63, count)
	initial, err := candidateHarness.store.SubmitCandidate(
		context.Background(), candidateForSet(sourceSet, schema, "h1.name"),
	)
	if err != nil {
		t.Fatalf("SubmitCandidate(initial drift source): %v", err)
	}
	markCandidateSourceStale(t, candidateHarness, initial.Extractor.ID, "template_drift")
	targetSet, _ := candidateSampleSet(t, candidateHarness, 4000, 0x40000000000000ff, count)
	candidate := candidateForSet(targetSet, schema, ".name")
	if err := candidateHarness.durable.Update(context.Background(), func(tx ledger.WriteTx) error {
		return bindCandidateSamples(context.Background(), tx, candidate, initial.Extractor.ID, candidateHarness.clock.Now())
	}); err != nil {
		t.Fatalf("bind drift candidate samples: %v", err)
	}
	pending, err := candidateHarness.store.SubmitCandidate(context.Background(), candidate)
	if err != nil || pending.Status != SubmissionPendingHeal {
		t.Fatalf("SubmitCandidate(drift pending) = %+v, %v", pending, err)
	}
	snapshots := newFakeHealSnapshotReader()
	healer, err := NewHealer(candidateHarness.store, snapshots)
	if err != nil {
		t.Fatalf("NewHealer(drift): %v", err)
	}
	return &selfHealHarness{
		candidateHarness: candidateHarness, snapshots: snapshots, healer: healer,
		set: targetSet, sourceSet: sourceSet, schema: schema, source: initial.Extractor,
		staleReason: "template_drift", candidate: candidate, pending: pending,
	}
}

func (harness *selfHealHarness) seedConfirmedHistory(
	t *testing.T,
	values []string,
	match func(int) bool,
) {
	t.Helper()
	if len(values) > len(harness.set.Samples) {
		t.Fatalf("history values %d exceed samples %d", len(values), len(harness.set.Samples))
	}
	rows := make([]ledger.Verification, len(values))
	for index, expected := range values {
		url := fmt.Sprintf("https://www.example.com/history/%d", index)
		originalURL := fmt.Sprintf("https://origin.example.net/claim/%d", index)
		extracted := expected
		if match != nil && !match(index) {
			extracted = "mismatch-" + expected
		}
		sample := harness.set.Samples[index]
		harness.snapshots.put(sample.SnapshotID,
			`<html><body><h1 class="name">`+extracted+`</h1></body></html>`,
			snapshot.Meta{
				URL: url, FetchedAt: catalogTestTime.Add(-2 * time.Hour),
				StatusCode: 200, ContentType: "text/html",
			},
		)
		oldValue, err := json.Marshal(expected)
		if err != nil {
			t.Fatal(err)
		}
		rows[index] = ledger.Verification{
			VerificationID: "heal-history", ClaimIndex: index,
			URL: originalURL, FinalURL: url, Path: "name", OldValue: oldValue,
			Outcome:           ledger.OutcomeConfirmed,
			OldSnapshotID:     catalogSnapshotID(fmt.Sprintf("old-heal-%d", index)),
			NewSnapshotID:     sample.SnapshotID,
			SchemaHash:        harness.source.SchemaHash,
			TemplateClusterID: harness.source.TemplateClusterID,
			ExtractorID:       harness.source.ID,
			VerifiedAt:        catalogTestTime.Add(-time.Hour).Add(time.Duration(index) * time.Second),
		}
	}
	if err := harness.durable.RecordVerifications(context.Background(), rows); err != nil {
		t.Fatalf("RecordVerifications(history): %v", err)
	}
}

func TestHealerPromotesExactNinetyPercentAndIsTerminallyIdempotent(t *testing.T) {
	harness := newSelfHealHarness(t, 10)
	values := make([]string, 10)
	for index := range values {
		values[index] = fmt.Sprintf("value-%d", index)
	}
	harness.seedConfirmedHistory(t, values, func(index int) bool { return index != 9 })

	result, err := harness.healer.HealRun(context.Background(), harness.pending.HealRunID)
	if err != nil {
		t.Fatalf("HealRun: %v", err)
	}
	if result.State != HealPromoted || result.TerminalReason != "replay_passed" ||
		result.ReplayTotal != 10 || result.ReplayMatched != 9 || result.ReplayRatio == nil ||
		*result.ReplayRatio != 0.9 || !validExtractorID(result.PromotedExtractorID) {
		t.Fatalf("promoted result = %+v", result)
	}
	source, err := harness.store.Get(context.Background(), harness.source.ID)
	if err != nil || source.State != StateRetired || source.StaleReason != "required_empty_rate" {
		t.Fatalf("retired source = %+v, %v", source, err)
	}
	promoted, err := harness.store.Get(context.Background(), result.PromotedExtractorID)
	if err != nil || promoted.State != StateActive || promoted.Version != 2 ||
		promoted.TemplateClusterID != harness.source.TemplateClusterID ||
		promoted.IR.Fields[0].Selector != ".name" {
		t.Fatalf("promoted extractor = %+v, %v", promoted, err)
	}
	assertHealBindings(t, harness, result.PromotedExtractorID, harness.set)

	retry, err := harness.healer.Heal(context.Background(), harness.candidate.Key)
	if err != nil || retry.ID != result.ID || retry.PromotedExtractorID != result.PromotedExtractorID ||
		retry.State != HealPromoted {
		t.Fatalf("terminal Heal retry = %+v, %v", retry, err)
	}
	assertHealCounts(t, harness, 2, 1, 0)
	for _, sample := range harness.set.Samples {
		contentCalls, observationCalls := harness.snapshots.calls(sample.SnapshotID)
		if contentCalls != 1 || observationCalls != 1 {
			t.Fatalf("snapshot %q calls content/observation = %d/%d, want 1/1",
				sample.SnapshotID, contentCalls, observationCalls)
		}
	}
}

func TestHealerDegradesBelowThresholdWithoutExtractorMutation(t *testing.T) {
	harness := newSelfHealHarness(t, 10)
	values := make([]string, 10)
	for index := range values {
		values[index] = fmt.Sprintf("value-%d", index)
	}
	harness.seedConfirmedHistory(t, values, func(index int) bool { return index < 8 })

	result, err := harness.healer.HealRun(context.Background(), harness.pending.HealRunID)
	if err != nil {
		t.Fatalf("HealRun: %v", err)
	}
	if result.State != HealDegraded || result.TerminalReason != "replay_below_threshold" ||
		result.ReplayTotal != 10 || result.ReplayMatched != 8 || result.ReplayRatio == nil ||
		*result.ReplayRatio != 0.8 || result.PromotedExtractorID != "" {
		t.Fatalf("degraded result = %+v", result)
	}
	assertHealSourceUnchanged(t, harness)
	assertHealBindings(t, harness, harness.source.ID, harness.set)
	assertHealCounts(t, harness, 1, 1, 0)
}

func TestHealerEightOfNineDoesNotRoundUpToNinetyPercent(t *testing.T) {
	harness := newSelfHealHarness(t, 9)
	values := make([]string, 9)
	for index := range values {
		values[index] = fmt.Sprintf("value-%d", index)
	}
	harness.seedConfirmedHistory(t, values, func(index int) bool { return index < 8 })
	result, err := harness.healer.HealRun(context.Background(), harness.pending.HealRunID)
	if err != nil || result.State != HealDegraded || result.ReplayTotal != 9 || result.ReplayMatched != 8 ||
		result.ReplayRatio == nil || *result.ReplayRatio != 8.0/9.0 {
		t.Fatalf("eight-of-nine result = %+v, %v", result, err)
	}
	assertHealSourceUnchanged(t, harness)
}

func TestHealerTreatsMissingContentAndObservationAsFactFailures(t *testing.T) {
	harness := newSelfHealHarness(t, 3)
	harness.seedConfirmedHistory(t, []string{"one", "two", "three"}, func(int) bool { return true })

	harness.snapshots.mu.Lock()
	delete(harness.snapshots.values, snapshot.ID(harness.set.Samples[1].SnapshotID))
	third := harness.snapshots.values[snapshot.ID(harness.set.Samples[2].SnapshotID)]
	third.observations = nil
	harness.snapshots.values[snapshot.ID(harness.set.Samples[2].SnapshotID)] = third
	harness.snapshots.mu.Unlock()

	result, err := harness.healer.HealRun(context.Background(), harness.pending.HealRunID)
	if err != nil {
		t.Fatalf("HealRun: %v", err)
	}
	if result.State != HealDegraded || result.ReplayTotal != 3 || result.ReplayMatched != 1 ||
		result.ReplayRatio == nil || *result.ReplayRatio != 1.0/3.0 {
		t.Fatalf("missing snapshot result = %+v", result)
	}
	assertHealSourceUnchanged(t, harness)
}

func TestHealerRejectsObservationFetchedAfterVerification(t *testing.T) {
	harness := newSelfHealHarness(t, 3)
	harness.seedConfirmedHistory(t, []string{"one", "two", "three"}, func(int) bool { return true })
	firstID := snapshot.ID(harness.set.Samples[0].SnapshotID)
	harness.snapshots.mu.Lock()
	first := harness.snapshots.values[firstID]
	first.observations[0].FetchedAt = catalogTestTime.Add(-30 * time.Minute)
	harness.snapshots.values[firstID] = first
	harness.snapshots.mu.Unlock()

	result, err := harness.healer.HealRun(context.Background(), harness.pending.HealRunID)
	if err != nil || result.State != HealDegraded || result.ReplayTotal != 3 || result.ReplayMatched != 2 {
		t.Fatalf("late observation result = %+v, %v", result, err)
	}
	assertHealSourceUnchanged(t, harness)
}

func TestHealerCreatedAtCutoffExcludesFutureConfirmedRows(t *testing.T) {
	harness := newSelfHealHarness(t, 3)
	harness.seedConfirmedHistory(t, []string{"one", "two"}, func(int) bool { return true })
	futureSnapshot := catalogSnapshotID("future-confirmed")
	futureURL := "https://www.example.com/future"
	harness.snapshots.put(futureSnapshot, `<h1 class="name">future</h1>`,
		snapshot.Meta{URL: futureURL, FetchedAt: catalogTestTime.Add(30 * time.Minute), StatusCode: 200})
	future := ledger.Verification{
		VerificationID: "future-confirmed", ClaimIndex: 0,
		URL: "https://origin.example.net/future", FinalURL: futureURL,
		Path: "name", OldValue: json.RawMessage(`"future"`), Outcome: ledger.OutcomeConfirmed,
		OldSnapshotID: catalogSnapshotID("future-old"), NewSnapshotID: futureSnapshot,
		SchemaHash: harness.source.SchemaHash, TemplateClusterID: harness.source.TemplateClusterID,
		ExtractorID: harness.source.ID, VerifiedAt: catalogTestTime.Add(time.Hour),
	}
	if err := harness.durable.RecordVerifications(context.Background(), []ledger.Verification{future}); err != nil {
		t.Fatal(err)
	}
	result, err := harness.healer.HealRun(context.Background(), harness.pending.HealRunID)
	if err != nil || result.State != HealPromoted || result.ReplayTotal != 2 || result.ReplayMatched != 2 {
		t.Fatalf("future cutoff result = %+v, %v", result, err)
	}
	if content, observations := harness.snapshots.calls(futureSnapshot); content != 0 || observations != 0 {
		t.Fatalf("future snapshot was replayed = %d/%d", content, observations)
	}
}

func TestHealerRequiresExactSourceVerificationProvenance(t *testing.T) {
	harness := newSelfHealHarness(t, 3)
	snapshotID := catalogSnapshotID("wrong-provenance")
	url := "https://www.example.com/wrong-provenance"
	harness.snapshots.put(snapshotID, `<h1 class="name">ignored</h1>`,
		snapshot.Meta{URL: url, FetchedAt: catalogTestTime.Add(-2 * time.Hour), StatusCode: 200})
	rows := []ledger.Verification{
		{
			VerificationID: "wrong-provenance", ClaimIndex: 0,
			URL: url, FinalURL: url, Path: "name", OldValue: json.RawMessage(`"ignored"`),
			Outcome: ledger.OutcomeConfirmed, OldSnapshotID: catalogSnapshotID("wrong-old-0"),
			NewSnapshotID: snapshotID, SchemaHash: strings.Repeat("a", 64),
			TemplateClusterID: harness.source.TemplateClusterID, ExtractorID: harness.source.ID,
			VerifiedAt: catalogTestTime.Add(-time.Hour),
		},
		{
			VerificationID: "wrong-provenance", ClaimIndex: 1,
			URL: url, FinalURL: url, Path: "name", OldValue: json.RawMessage(`"ignored"`),
			Outcome: ledger.OutcomeConfirmed, OldSnapshotID: catalogSnapshotID("wrong-old-1"),
			NewSnapshotID: snapshotID, SchemaHash: harness.source.SchemaHash,
			TemplateClusterID: strings.Repeat("b", 64), ExtractorID: harness.source.ID,
			VerifiedAt: catalogTestTime.Add(-time.Hour),
		},
		{
			VerificationID: "wrong-provenance", ClaimIndex: 2,
			URL: url, FinalURL: url, Path: "name", OldValue: json.RawMessage(`"ignored"`),
			Outcome: ledger.OutcomeConfirmed, OldSnapshotID: catalogSnapshotID("wrong-old-2"),
			NewSnapshotID: snapshotID, SchemaHash: harness.source.SchemaHash,
			TemplateClusterID: harness.source.TemplateClusterID,
			ExtractorID:       "00000000-0000-4000-8000-000000000099",
			VerifiedAt:        catalogTestTime.Add(-time.Hour),
		},
	}
	if err := harness.durable.RecordVerifications(context.Background(), rows); err != nil {
		t.Fatal(err)
	}
	result, err := harness.healer.HealRun(context.Background(), harness.pending.HealRunID)
	if err != nil || result.State != HealDegraded || result.TerminalReason != "insufficient_history" ||
		result.ReplayTotal != 0 {
		t.Fatalf("wrong provenance result = %+v, %v", result, err)
	}
	if content, observations := harness.snapshots.calls(snapshotID); content != 0 || observations != 0 {
		t.Fatalf("wrong provenance touched snapshot = %d/%d", content, observations)
	}
}

func TestHealerZeroHistoryIsDeterministicallyDegraded(t *testing.T) {
	harness := newSelfHealHarness(t, 3)
	result, err := harness.healer.HealRun(context.Background(), harness.pending.HealRunID)
	if err != nil {
		t.Fatalf("HealRun: %v", err)
	}
	if result.State != HealDegraded || result.TerminalReason != "insufficient_history" ||
		result.ReplayTotal != 0 || result.ReplayMatched != 0 || result.ReplayRatio != nil {
		t.Fatalf("zero-history result = %+v", result)
	}
	assertHealSourceUnchanged(t, harness)
}

func TestHealerCanonicalScalarDedupeRetainsLexicalNumberStrictness(t *testing.T) {
	harness := newSelfHealHarness(t, 3)
	sample := harness.set.Samples[0]
	finalURL := "https://www.example.com/canonical"
	harness.snapshots.put(sample.SnapshotID,
		`<h1 class="name">same</h1>`,
		snapshot.Meta{URL: finalURL, FetchedAt: catalogTestTime.Add(-2 * time.Hour), StatusCode: 200},
	)
	rows := []ledger.Verification{
		{
			VerificationID: "canonical-dedupe", ClaimIndex: 0,
			URL: "https://origin.example.net/a", FinalURL: finalURL,
			Path: "name", OldValue: json.RawMessage(`"same"`), Outcome: ledger.OutcomeConfirmed,
			OldSnapshotID: catalogSnapshotID("canonical-old-a"), NewSnapshotID: sample.SnapshotID,
			SchemaHash: harness.source.SchemaHash, TemplateClusterID: harness.source.TemplateClusterID,
			ExtractorID: harness.source.ID, VerifiedAt: catalogTestTime.Add(-time.Hour),
		},
		{
			VerificationID: "canonical-dedupe", ClaimIndex: 1,
			URL: "https://origin.example.net/b", FinalURL: finalURL,
			Path: "name", OldValue: json.RawMessage(`"\u0073ame"`), Outcome: ledger.OutcomeConfirmed,
			OldSnapshotID: catalogSnapshotID("canonical-old-b"), NewSnapshotID: sample.SnapshotID,
			SchemaHash: harness.source.SchemaHash, TemplateClusterID: harness.source.TemplateClusterID,
			ExtractorID: harness.source.ID, VerifiedAt: catalogTestTime.Add(-time.Hour - time.Second),
		},
	}
	if err := harness.durable.RecordVerifications(context.Background(), rows); err != nil {
		t.Fatal(err)
	}
	result, err := harness.healer.HealRun(context.Background(), harness.pending.HealRunID)
	if err != nil || result.State != HealPromoted || result.ReplayTotal != 1 || result.ReplayMatched != 1 {
		t.Fatalf("canonical dedupe result = %+v, %v", result, err)
	}
	if !strictReplayScalarEqual(json.RawMessage(`"same"`), json.RawMessage(`"\u0073ame"`)) {
		t.Fatal("equivalent JSON strings did not compare strictly equal after canonicalization")
	}
	if strictReplayScalarEqual(json.RawMessage(`1`), json.RawMessage(`1.0`)) {
		t.Fatal("lexically distinct JSON numbers compared equal")
	}
}

func TestHealCompiledFieldNameCanonicalRoundTrip(t *testing.T) {
	tests := []struct {
		path    string
		want    string
		wantErr bool
	}{
		{path: `price\.usd`, want: "price.usd"},
		{path: `literal\backslash`, want: `literal\backslash`},
		{path: "nested.child", wantErr: true},
	}
	for _, test := range tests {
		got, err := healCompiledFieldName(test.path)
		if test.wantErr {
			if err == nil {
				t.Errorf("healCompiledFieldName(%q) = %q, want error", test.path, got)
			}
			continue
		}
		if err != nil || got != test.want {
			t.Errorf("healCompiledFieldName(%q) = (%q, %v), want %q", test.path, got, err, test.want)
		}
	}
}

func TestReplayProgramBatchesAttrRegexTransformAndKeepsSingleMatchSemantics(t *testing.T) {
	ir := IR{Version: CurrentIRVersion, Fields: []FieldRule{
		{
			Name: "price", Selector: ".price", Attr: "data-price", Regex: `USD\s+(.+)`,
			Transforms: []string{"currency_amount"}, Type: TypeNumber,
		},
		{
			Name: "label", Selector: ".label", Regex: `value:(.*)`,
			Transforms: []string{"trim", "collapse_ws", "lower"}, Type: TypeString,
		},
	}}
	program, err := newReplayProgram(ir)
	if err != nil {
		t.Fatal(err)
	}
	results, err := program.replay(
		`<span class="price" data-price="USD 1,299.50"></span><p class="label">value:  HELLO   WORLD </p>`,
		[]string{"price", "label", "price"},
	)
	if err != nil || len(results) != 2 || results["price"].err != nil ||
		!results["price"].result.Found || string(results["price"].result.Value) != "1299.50" ||
		results["label"].err != nil || !results["label"].result.Found ||
		string(results["label"].result.Value) != `"hello world"` {
		t.Fatalf("batched replay = %#v, %v", results, err)
	}

	// SingleMatcher deliberately evaluates only the first matching node. A
	// later valid node cannot rescue a non-unique selector whose first node
	// fails the immutable regex.
	results, err = program.replay(
		`<span class="price" data-price="not-a-price"></span>`+
			`<span class="price" data-price="USD 1,299.50"></span>`,
		[]string{"price"},
	)
	if err != nil || results["price"].err != nil || results["price"].result.Found {
		t.Fatalf("non-unique SingleMatcher replay = %#v, %v", results["price"], err)
	}
}

func TestHealerOversizedHistoryProjectionCountsUnmatchedWithoutReadingSnapshot(t *testing.T) {
	harness := newSelfHealHarness(t, 3)
	harness.seedConfirmedHistory(t, []string{"one", "two"}, func(int) bool { return true })
	oversizedSnapshot := catalogSnapshotID("oversized-history")
	row := ledger.Verification{
		VerificationID: "oversized-history", ClaimIndex: 0,
		URL: "https://origin.example.net/oversized", FinalURL: "https://www.example.com/oversized",
		Path: strings.Repeat("p", MaxFieldNameBytes+1), OldValue: json.RawMessage(`"ignored"`),
		Outcome:       ledger.OutcomeConfirmed,
		OldSnapshotID: catalogSnapshotID("oversized-old"), NewSnapshotID: oversizedSnapshot,
		SchemaHash: harness.source.SchemaHash, TemplateClusterID: harness.source.TemplateClusterID,
		ExtractorID: harness.source.ID, VerifiedAt: catalogTestTime.Add(-30 * time.Minute),
	}
	if err := harness.durable.RecordVerifications(context.Background(), []ledger.Verification{row}); err != nil {
		t.Fatal(err)
	}
	result, err := harness.healer.HealRun(context.Background(), harness.pending.HealRunID)
	if err != nil || result.State != HealDegraded || result.ReplayTotal != 3 || result.ReplayMatched != 2 {
		t.Fatalf("oversized history result = %+v, %v", result, err)
	}
	if content, observations := harness.snapshots.calls(oversizedSnapshot); content != 0 || observations != 0 {
		t.Fatalf("oversized projection touched snapshot = %d/%d", content, observations)
	}
}

func TestHealerCanonicalURLExpansionCountsUnmatchedWithoutReadingSnapshot(t *testing.T) {
	harness := newSelfHealHarness(t, 3)
	harness.seedConfirmedHistory(t, []string{"one", "two"}, func(int) bool { return true })
	expandedSnapshot := catalogSnapshotID("canonical-url-expansion")
	rawFinalURL := "https://www.example.com/" + strings.Repeat("界", 1900)
	canonical, _, err := publicnet.NormalizeHTTPURL(rawFinalURL, nil, true)
	if err != nil || len(rawFinalURL) > MaxExtractorURLBytes || len(canonical) <= MaxExtractorURLBytes {
		t.Fatalf("URL expansion fixture = raw %d canonical %d, %v", len(rawFinalURL), len(canonical), err)
	}
	row := ledger.Verification{
		VerificationID: "canonical-url-expansion", ClaimIndex: 0,
		URL: "https://origin.example.net/expansion", FinalURL: rawFinalURL,
		Path: "name", OldValue: json.RawMessage(`"ignored"`), Outcome: ledger.OutcomeConfirmed,
		OldSnapshotID: catalogSnapshotID("canonical-url-expansion-old"), NewSnapshotID: expandedSnapshot,
		SchemaHash: harness.source.SchemaHash, TemplateClusterID: harness.source.TemplateClusterID,
		ExtractorID: harness.source.ID, VerifiedAt: catalogTestTime.Add(-30 * time.Minute),
	}
	if err := harness.durable.RecordVerifications(context.Background(), []ledger.Verification{row}); err != nil {
		t.Fatal(err)
	}
	result, err := harness.healer.HealRun(context.Background(), harness.pending.HealRunID)
	if err != nil || result.State != HealDegraded || result.ReplayTotal != 3 || result.ReplayMatched != 2 {
		t.Fatalf("canonical URL expansion result = %+v, %v", result, err)
	}
	if content, observations := harness.snapshots.calls(expandedSnapshot); content != 0 || observations != 0 {
		t.Fatalf("expanded canonical URL touched snapshot = %d/%d", content, observations)
	}
}

func TestHealerBoundsFactsAtOneHundredBeforeSnapshotIO(t *testing.T) {
	harness := newSelfHealHarness(t, 3)
	snapshotID := catalogSnapshotID("fact-cap")
	rows := make([]ledger.Verification, MaxHealReplayFacts+1)
	for index := range rows {
		rows[index] = ledger.Verification{
			VerificationID: "fact-cap", ClaimIndex: index,
			URL: "https://origin.example.net/cap", FinalURL: "https://www.example.com/cap",
			Path: fmt.Sprintf("nested.%03d", index), OldValue: json.RawMessage(`"ignored"`),
			Outcome:       ledger.OutcomeConfirmed,
			OldSnapshotID: catalogSnapshotID(fmt.Sprintf("fact-cap-old-%d", index)),
			NewSnapshotID: snapshotID, SchemaHash: harness.source.SchemaHash,
			TemplateClusterID: harness.source.TemplateClusterID, ExtractorID: harness.source.ID,
			VerifiedAt: catalogTestTime.Add(-time.Minute - time.Duration(index)*time.Nanosecond),
		}
	}
	if err := harness.durable.RecordVerifications(context.Background(), rows); err != nil {
		t.Fatal(err)
	}
	result, err := harness.healer.HealRun(context.Background(), harness.pending.HealRunID)
	if err != nil || result.State != HealDegraded || result.ReplayTotal != MaxHealReplayFacts || result.ReplayMatched != 0 {
		t.Fatalf("fact cap result = %+v, %v", result, err)
	}
	if content, observations := harness.snapshots.calls(snapshotID); content != 0 || observations != 0 {
		t.Fatalf("invalid capped facts touched snapshot = %d/%d", content, observations)
	}
}

func TestHealerBoundsSnapshotsAtTwentyAndCountsOverflowUnmatched(t *testing.T) {
	harness := newSelfHealHarness(t, 3)
	rows := make([]ledger.Verification, MaxHealReplaySnapshots+1)
	for index := range rows {
		snapshotID := catalogSnapshotID(fmt.Sprintf("snapshot-cap-%d", index))
		url := fmt.Sprintf("https://www.example.com/snapshot-cap/%d", index)
		value := fmt.Sprintf("value-%d", index)
		harness.snapshots.put(snapshotID, `<h1 class="name">`+value+`</h1>`,
			snapshot.Meta{URL: url, FetchedAt: catalogTestTime.Add(-2 * time.Hour), StatusCode: 200})
		encoded, _ := json.Marshal(value)
		rows[index] = ledger.Verification{
			VerificationID: "snapshot-cap", ClaimIndex: index,
			URL: "https://origin.example.net/cap", FinalURL: url,
			Path: "name", OldValue: encoded, Outcome: ledger.OutcomeConfirmed,
			OldSnapshotID: catalogSnapshotID(fmt.Sprintf("snapshot-cap-old-%d", index)),
			NewSnapshotID: snapshotID, SchemaHash: harness.source.SchemaHash,
			TemplateClusterID: harness.source.TemplateClusterID, ExtractorID: harness.source.ID,
			VerifiedAt: catalogTestTime.Add(-time.Hour).Add(time.Duration(index) * time.Second),
		}
	}
	if err := harness.durable.RecordVerifications(context.Background(), rows); err != nil {
		t.Fatal(err)
	}
	result, err := harness.healer.HealRun(context.Background(), harness.pending.HealRunID)
	if err != nil || result.State != HealPromoted || result.ReplayTotal != 21 || result.ReplayMatched != 20 {
		t.Fatalf("snapshot cap result = %+v, %v", result, err)
	}
	readSnapshots := 0
	for index := range rows {
		content, observations := harness.snapshots.calls(catalogSnapshotID(fmt.Sprintf("snapshot-cap-%d", index)))
		if content != 0 || observations != 0 {
			if content != 1 || observations != 1 {
				t.Fatalf("snapshot %d calls = %d/%d", index, content, observations)
			}
			readSnapshots++
		}
	}
	if readSnapshots != MaxHealReplaySnapshots {
		t.Fatalf("read snapshots = %d, want %d", readSnapshots, MaxHealReplaySnapshots)
	}
}

func TestHealerTaskCancellationReleasesLeaseWithBackgroundContext(t *testing.T) {
	harness := newSelfHealHarness(t, 3)
	harness.seedConfirmedHistory(t, []string{"one", "two", "three"}, func(int) bool { return true })
	harness.healer.taskTimeout = 20 * time.Millisecond
	harness.snapshots.onHas = func(
		ctx context.Context,
		_ snapshot.ID,
		_ func(snapshot.Meta) bool,
	) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}

	_, err := harness.healer.HealRun(context.Background(), harness.pending.HealRunID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("HealRun cancellation error = %v", err)
	}
	state, leaseID := loadHealState(t, harness.durable, harness.pending.HealRunID)
	if state != HealPending || leaseID != "" {
		t.Fatalf("released run state/lease = %q/%q", state, leaseID)
	}
	assertHealSourceUnchanged(t, harness)
}

func TestHealerBusyLeaseAndExpiredTakeover(t *testing.T) {
	harness := newSelfHealHarness(t, 3)
	harness.seedConfirmedHistory(t, []string{"one", "two", "three"}, func(int) bool { return true })
	run, firstLease, terminal, err := harness.healer.claim(
		context.Background(), healSelector{id: harness.pending.HealRunID},
	)
	if err != nil || terminal || firstLease == "" {
		t.Fatalf("initial claim = lease %q terminal %v err %v", firstLease, terminal, err)
	}
	if _, err := harness.healer.HealRun(context.Background(), harness.pending.HealRunID); !errors.Is(err, ErrHealLeaseBusy) {
		t.Fatalf("active lease retry error = %v", err)
	}

	harness.clock.Add(HealLeaseDuration + time.Second)
	result, err := harness.healer.HealRun(context.Background(), harness.pending.HealRunID)
	if err != nil || result.State != HealPromoted {
		t.Fatalf("expired takeover = %+v, %v", result, err)
	}
	if err := harness.healer.releaseClaim(run, firstLease); err != nil {
		t.Fatalf("release superseded lease: %v", err)
	}
	terminalResult, err := harness.healer.HealRun(context.Background(), harness.pending.HealRunID)
	if err != nil || terminalResult.PromotedExtractorID != result.PromotedExtractorID {
		t.Fatalf("terminal after old release = %+v, %v", terminalResult, err)
	}
}

func TestHealerTemplateDriftRebindsOnlyCandidateSamples(t *testing.T) {
	harness := newDriftSelfHealHarness(t, 3)
	harness.seedConfirmedHistory(t, []string{"one", "two", "three"}, func(int) bool { return true })
	result, err := harness.healer.HealRun(context.Background(), harness.pending.HealRunID)
	if err != nil || result.State != HealPromoted {
		t.Fatalf("HealRun(drift) = %+v, %v", result, err)
	}
	promoted, err := harness.store.Get(context.Background(), result.PromotedExtractorID)
	if err != nil || promoted.Version != harness.source.Version+1 ||
		promoted.TemplateClusterID != harness.candidate.Key.TemplateClusterID ||
		promoted.TemplateSimHash != harness.candidate.Key.ClusterSimHash {
		t.Fatalf("drift promoted extractor = %+v, %v", promoted, err)
	}
	source, err := harness.store.Get(context.Background(), harness.source.ID)
	if err != nil || source.State != StateRetired || source.StaleReason != "template_drift" {
		t.Fatalf("drift source = %+v, %v", source, err)
	}
	assertHealBindings(t, harness, result.PromotedExtractorID, harness.set)
	assertHealBindings(t, harness, harness.source.ID, harness.sourceSet)
	assertHealCounts(t, harness, 2, 1, 0)
}

func TestHealerTemplateDriftForeignBindingFailsClosedWithoutPartialRebind(t *testing.T) {
	harness := newDriftSelfHealHarness(t, 3)
	harness.seedConfirmedHistory(t, []string{"one", "two", "three"}, func(int) bool { return true })
	foreignFingerprint := uint64(0x2000ffff00000000)
	foreignID := insertHealTestExtractor(
		t, harness, foreignFingerprint, 1, StateActive, "", harness.source.IR, harness.source.Validation,
	)
	if err := harness.durable.Update(context.Background(), func(tx ledger.WriteTx) error {
		_, err := tx.ExecContext(context.Background(), `UPDATE extractor_page_bindings
			SET extractor_id = ? WHERE page_hash = ? AND schema_hash = ?`,
			foreignID, harness.set.Samples[0].PageHash, harness.set.Key.SchemaHash)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	result, err := harness.healer.HealRun(context.Background(), harness.pending.HealRunID)
	if err != nil || result.State != HealDegraded || result.TerminalReason != "binding_conflict" ||
		result.PromotedExtractorID != "" {
		t.Fatalf("foreign binding result = %+v, %v", result, err)
	}
	assertHealSourceUnchanged(t, harness)
	assertOneHealBinding(t, harness, harness.set.Samples[0], foreignID)
	for _, sample := range harness.set.Samples[1:] {
		assertOneHealBinding(t, harness, sample, harness.source.ID)
	}
	assertHealBindings(t, harness, harness.source.ID, harness.sourceSet)
	assertHealCounts(t, harness, 2, 1, 0)
}

func TestHealerFinalPromotionCASRejectsSourceStateRace(t *testing.T) {
	harness := newSelfHealHarness(t, 3)
	harness.seedConfirmedHistory(t, []string{"one", "two", "three"}, func(int) bool { return true })
	var retireOnce sync.Once
	harness.snapshots.onHas = func(
		ctx context.Context,
		id snapshot.ID,
		match func(snapshot.Meta) bool,
	) (bool, error) {
		harness.snapshots.mu.Lock()
		observations := append([]snapshot.Meta(nil), harness.snapshots.values[id].observations...)
		harness.snapshots.mu.Unlock()
		found := false
		for _, observation := range observations {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if match(observation) {
				found = true
				break
			}
		}
		retireOnce.Do(func() {
			if _, err := harness.store.Retire(context.Background(), harness.source.ID); err != nil {
				t.Errorf("race retirement: %v", err)
			}
		})
		return found, nil
	}
	result, err := harness.healer.HealRun(context.Background(), harness.pending.HealRunID)
	if err != nil || result.State != HealDegraded || result.TerminalReason != "source_changed" ||
		result.PromotedExtractorID != "" {
		t.Fatalf("source race result = %+v, %v", result, err)
	}
	source, err := harness.store.Get(context.Background(), harness.source.ID)
	if err != nil || source.State != StateRetired || source.StaleReason != harness.staleReason {
		t.Fatalf("raced source = %+v, %v", source, err)
	}
	assertHealBindings(t, harness, harness.source.ID, harness.set)
	assertHealCounts(t, harness, 1, 1, 0)
}

func TestHealerNearActiveConflictNeverSilentlyReplacesCurrent(t *testing.T) {
	harness := newSelfHealHarness(t, 3)
	harness.seedConfirmedHistory(t, []string{"one", "two", "three"}, func(int) bool { return true })
	nearFingerprint := harness.candidate.Key.ClusterSimHash ^ 1
	currentID := insertHealTestExtractor(
		t, harness, nearFingerprint, 1, StateActive, "",
		harness.source.IR, harness.source.Validation,
	)
	result, err := harness.healer.HealRun(context.Background(), harness.pending.HealRunID)
	if err != nil || result.State != HealDegraded || result.TerminalReason != "active_conflict" {
		t.Fatalf("near-active result = %+v, %v", result, err)
	}
	assertHealSourceUnchanged(t, harness)
	current, err := harness.store.Get(context.Background(), currentID)
	if err != nil || current.State != StateActive || current.Version != 1 ||
		current.TemplateSimHash != nearFingerprint {
		t.Fatalf("near active changed = %+v, %v", current, err)
	}
	assertHealBindings(t, harness, harness.source.ID, harness.set)
	assertHealCounts(t, harness, 2, 1, 0)
}

func TestHealerSameClusterRejectsObsoleteSourceWhenNewerRevisionExists(t *testing.T) {
	harness := newSelfHealHarness(t, 3)
	harness.seedConfirmedHistory(t, []string{"one", "two", "three"}, func(int) bool { return true })
	newerID := insertHealTestExtractor(
		t, harness, harness.candidate.Key.ClusterSimHash, 2, StateRetired,
		"required_empty_rate", harness.source.IR, harness.source.Validation,
	)
	result, err := harness.healer.HealRun(context.Background(), harness.pending.HealRunID)
	if err != nil || result.State != HealDegraded || result.TerminalReason != "source_changed" {
		t.Fatalf("obsolete-source result = %+v, %v", result, err)
	}
	assertHealSourceUnchanged(t, harness)
	newer, err := harness.store.Get(context.Background(), newerID)
	if err != nil || newer.State != StateRetired || newer.Version != 2 {
		t.Fatalf("newer revision changed = %+v, %v", newer, err)
	}
	assertHealCounts(t, harness, 2, 1, 0)
}

func TestHealerTemplateDriftRejectsObsoleteSourceLineage(t *testing.T) {
	harness := newDriftSelfHealHarness(t, 3)
	harness.seedConfirmedHistory(t, []string{"one", "two", "three"}, func(int) bool { return true })
	newerID := insertHealTestExtractor(
		t, harness, harness.source.TemplateSimHash, 2, StateRetired,
		"template_drift", harness.source.IR, harness.source.Validation,
	)
	result, err := harness.healer.HealRun(context.Background(), harness.pending.HealRunID)
	if err != nil || result.State != HealDegraded || result.TerminalReason != "source_changed" {
		t.Fatalf("obsolete drift source result = %+v, %v", result, err)
	}
	assertHealSourceUnchanged(t, harness)
	newer, err := harness.store.Get(context.Background(), newerID)
	if err != nil || newer.State != StateRetired || newer.Version != 2 {
		t.Fatalf("newer drift revision changed = %+v, %v", newer, err)
	}
	assertHealBindings(t, harness, harness.source.ID, harness.set)
	assertHealCounts(t, harness, 2, 1, 0)
}

func TestHealerImmutableCandidateCorruptionTerminatesFailedWithoutPromotion(t *testing.T) {
	harness := newSelfHealHarness(t, 3)
	if err := harness.durable.Update(context.Background(), func(tx ledger.WriteTx) error {
		if _, err := tx.ExecContext(context.Background(),
			"DROP TRIGGER trg_extractor_heal_runs_identity_immutable"); err != nil {
			return err
		}
		_, err := tx.ExecContext(context.Background(), `UPDATE extractor_heal_runs
			SET candidate_ir_hash = ? WHERE id = ?`, strings.Repeat("f", 64), harness.pending.HealRunID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	result, err := harness.healer.HealRun(context.Background(), harness.pending.HealRunID)
	if err != nil || result.State != HealFailed || result.TerminalReason != "invalid_candidate" ||
		result.ReplayTotal != 0 || result.PromotedExtractorID != "" {
		t.Fatalf("corrupt candidate result = %+v, %v", result, err)
	}
	assertHealSourceUnchanged(t, harness)
	assertHealBindings(t, harness, harness.source.ID, harness.set)
}

func TestHealerWebhookIsAtomicAndPayloadIsBoundedToLifecycleIdentity(t *testing.T) {
	harness := newSelfHealHarness(t, 3)
	harness.seedConfirmedHistory(t, []string{"one", "two", "three"}, func(int) bool { return true })
	healer, err := NewHealer(harness.store, harness.snapshots,
		WithHealWebhook("HTTPS://Hooks.Example.COM:443/heal", "process-secret"))
	if err != nil {
		t.Fatalf("NewHealer(webhook): %v", err)
	}
	result, err := healer.HealRun(context.Background(), harness.pending.HealRunID)
	if err != nil || result.State != HealPromoted {
		t.Fatalf("HealRun(webhook) = %+v, %v", result, err)
	}
	events, err := harness.durable.PendingOutbox(
		context.Background(), result.UpdatedAt, ledger.MaxPendingOutbox, ledger.MaxPendingOutboxBytes,
	)
	if err != nil || len(events) != 1 {
		t.Fatalf("PendingOutbox = %d, %v", len(events), err)
	}
	event := events[0]
	if event.SubjectType != ledger.SubjectExtractorHeal || event.SubjectID != result.ID ||
		event.Type != ledger.ExtractorPromotedEvent || event.URL != "https://hooks.example.com/heal" ||
		event.Secret != "process-secret" {
		t.Fatalf("heal event = %+v", event)
	}
	payload := string(event.Payload)
	for _, forbidden := range []string{
		"process-secret", "hooks.example.com", string(harness.schema),
		harness.candidate.IR.Fields[0].Selector, harness.set.Samples[0].SnapshotID,
	} {
		if strings.Contains(payload, forbidden) {
			t.Fatalf("heal payload leaked %q: %s", forbidden, payload)
		}
	}
	if !strings.Contains(payload, result.ID) || !strings.Contains(payload, result.PromotedExtractorID) ||
		!strings.Contains(payload, harness.source.ID) {
		t.Fatalf("heal payload omits lifecycle identities: %s", payload)
	}
	if _, err := healer.HealRun(context.Background(), result.ID); err != nil {
		t.Fatalf("terminal webhook retry: %v", err)
	}
	events, err = harness.durable.PendingOutbox(
		context.Background(), result.UpdatedAt, ledger.MaxPendingOutbox, ledger.MaxPendingOutboxBytes,
	)
	if err != nil || len(events) != 1 {
		t.Fatalf("terminal retry outbox = %d, %v", len(events), err)
	}
}

func TestHealerWebhookPersistsDegradedTerminalEventsWithoutChangingSource(t *testing.T) {
	for _, test := range []struct {
		name       string
		seed       bool
		wantReason string
	}{
		{name: "insufficient history", wantReason: "insufficient_history"},
		{name: "below threshold", seed: true, wantReason: "replay_below_threshold"},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newSelfHealHarness(t, 3)
			if test.seed {
				harness.seedConfirmedHistory(t, []string{"one", "two", "three"},
					func(index int) bool { return index < 2 })
			}
			healer, err := NewHealer(
				harness.store, harness.snapshots,
				WithHealWebhook("https://hooks.example.com/heal", "process-secret"),
			)
			if err != nil {
				t.Fatal(err)
			}
			result, err := healer.HealRun(context.Background(), harness.pending.HealRunID)
			if err != nil || result.State != HealDegraded || result.TerminalReason != test.wantReason {
				t.Fatalf("degraded webhook result = %+v, %v", result, err)
			}
			assertHealSourceUnchanged(t, harness)
			assertHealBindings(t, harness, harness.source.ID, harness.set)
			events, err := harness.durable.PendingOutbox(
				context.Background(), result.UpdatedAt,
				ledger.MaxPendingOutbox, ledger.MaxPendingOutboxBytes,
			)
			if err != nil || len(events) != 1 {
				t.Fatalf("degraded PendingOutbox = %d, %v", len(events), err)
			}
			event := events[0]
			if event.Type != ledger.ExtractorDegradedEvent ||
				event.SubjectType != ledger.SubjectExtractorHeal || event.SubjectID != result.ID ||
				!strings.Contains(string(event.Payload), test.wantReason) {
				t.Fatalf("degraded event = %+v payload=%s", event, event.Payload)
			}
		})
	}
}

func TestHealerOutboxFailureRollsBackPromotionAndReleasesPending(t *testing.T) {
	harness := newSelfHealHarness(t, 3)
	harness.seedConfirmedHistory(t, []string{"one", "two", "three"}, func(int) bool { return true })
	harness.healer.webhookURL = "http://"
	harness.healer.webhookSecret = "must-not-leak"

	_, err := harness.healer.HealRun(context.Background(), harness.pending.HealRunID)
	if err == nil {
		t.Fatal("HealRun with invalid outbox URL succeeded")
	}
	if strings.Contains(err.Error(), harness.healer.webhookURL) || strings.Contains(err.Error(), harness.healer.webhookSecret) {
		t.Fatalf("outbox error leaked config: %v", err)
	}
	state, leaseID := loadHealState(t, harness.durable, harness.pending.HealRunID)
	if state != HealPending || leaseID != "" {
		t.Fatalf("rolled-back run = %q lease %q", state, leaseID)
	}
	assertHealSourceUnchanged(t, harness)
	assertHealBindings(t, harness, harness.source.ID, harness.set)
	assertHealCounts(t, harness, 1, 1, 0)
}

func TestNewHealerRejectsUnsafeWebhookWithoutEchoingConfiguration(t *testing.T) {
	harness := newSelfHealHarness(t, 3)
	for _, rawURL := range []string{
		"http://127.0.0.1/hook", "http://localhost/hook", "https://user:pass@example.com/hook",
		"ftp://example.com/hook", "https://example.com:/hook", "https://example.com/\xff",
	} {
		_, err := NewHealer(harness.store, harness.snapshots, WithHealWebhook(rawURL, "secret"))
		if !errors.Is(err, ErrInvalidHealer) {
			t.Fatalf("NewHealer(%q) error = %v", rawURL, err)
		}
		if strings.Contains(err.Error(), rawURL) || strings.Contains(err.Error(), "secret") {
			t.Fatalf("NewHealer(%q) leaked config: %v", rawURL, err)
		}
	}
	oversizedSecret := strings.Repeat("s", ledger.MaxOutboxSecretBytes+1)
	_, err := NewHealer(
		harness.store, harness.snapshots,
		WithHealWebhook("https://hooks.example.com/heal", oversizedSecret),
	)
	if !errors.Is(err, ErrInvalidHealer) || strings.Contains(err.Error(), oversizedSecret) {
		t.Fatalf("oversized secret error = %v", err)
	}
}

func TestHealerExactLookupAndInputErrorsFailClosed(t *testing.T) {
	harness := newSelfHealHarness(t, 3)
	terminal, err := harness.healer.Heal(context.Background(), harness.candidate.Key)
	if err != nil || terminal.State != HealDegraded || terminal.TerminalReason != "insufficient_history" {
		t.Fatalf("exact Heal = %+v, %v", terminal, err)
	}
	adjacent := harness.candidate.Key
	adjacent.ClusterSimHash ^= 0x7f
	adjacent.TemplateClusterID = templateClusterID(adjacent.ClusterSimHash)
	if _, err := harness.healer.Heal(context.Background(), adjacent); !errors.Is(err, ErrHealRunNotFound) {
		t.Fatalf("adjacent Heal error = %v", err)
	}
	if _, err := harness.healer.HealRun(
		context.Background(), "00000000-0000-4000-8000-000000000099",
	); !errors.Is(err, ErrHealRunNotFound) {
		t.Fatalf("missing HealRun error = %v", err)
	}
	if _, err := harness.healer.HealRun(context.Background(), "not-a-uuid"); !errors.Is(err, ErrInvalidHealer) {
		t.Fatalf("invalid HealRun error = %v", err)
	}
	if _, err := harness.healer.Heal(nil, harness.candidate.Key); !errors.Is(err, ErrInvalidHealer) {
		t.Fatalf("nil-context Heal error = %v", err)
	}
	var nilSnapshots *fakeHealSnapshotReader
	if _, err := NewHealer(harness.store, nilSnapshots); !errors.Is(err, ErrInvalidHealer) {
		t.Fatalf("typed-nil NewHealer error = %v", err)
	}
}

func assertHealSourceUnchanged(t *testing.T, harness *selfHealHarness) {
	t.Helper()
	source, err := harness.store.Get(context.Background(), harness.source.ID)
	if err != nil || source.State != StateStale || source.StaleReason != harness.staleReason ||
		source.Version != harness.source.Version {
		t.Fatalf("source changed = %+v, %v", source, err)
	}
}

func assertHealBindings(t *testing.T, harness *selfHealHarness, extractorID string, set SampleSet) {
	t.Helper()
	if err := harness.durable.View(context.Background(), func(tx ledger.ReadTx) error {
		for _, sample := range set.Samples {
			var bound string
			if err := tx.QueryRowContext(context.Background(), `SELECT extractor_id
				FROM extractor_page_bindings WHERE page_hash = ? AND schema_hash = ?`,
				sample.PageHash, set.Key.SchemaHash).Scan(&bound); err != nil {
				return err
			}
			if bound != extractorID {
				return fmt.Errorf("page %q bound to %q, want %q", sample.PageHash, bound, extractorID)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func assertOneHealBinding(t *testing.T, harness *selfHealHarness, sample SampleRef, extractorID string) {
	t.Helper()
	var bound string
	if err := harness.durable.View(context.Background(), func(tx ledger.ReadTx) error {
		return tx.QueryRowContext(context.Background(), `SELECT extractor_id
			FROM extractor_page_bindings WHERE page_hash = ? AND schema_hash = ?`,
			sample.PageHash, harness.set.Key.SchemaHash).Scan(&bound)
	}); err != nil {
		t.Fatal(err)
	}
	if bound != extractorID {
		t.Fatalf("page %q bound to %q, want %q", sample.PageHash, bound, extractorID)
	}
}

func insertHealTestExtractor(
	t *testing.T,
	harness *selfHealHarness,
	fingerprint uint64,
	version int,
	state ExtractorState,
	reason string,
	ir IR,
	report ValidationReport,
) string {
	t.Helper()
	id, err := newExtractorID()
	if err != nil {
		t.Fatal(err)
	}
	irJSON, reportJSON, irHash, err := validateRegistration(harness.schema, ir, report)
	if err != nil {
		t.Fatal(err)
	}
	now := formatExtractorTime(harness.clock.Now())
	if err := harness.durable.Update(context.Background(), func(tx ledger.WriteTx) error {
		_, err := tx.ExecContext(context.Background(), `INSERT INTO extractors (
			id, host, schema_json, schema_hash, template_cluster_id,
			template_simhash, ir, ir_hash, ir_format_version, version,
			validation_report, validation, state, empty_window,
			stale_reason, created_at, last_used_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '[]', ?, ?, NULL, ?)`,
			id, harness.source.Host, string(harness.schema), harness.source.SchemaHash,
			templateClusterID(fingerprint), encodeSimHash(fingerprint), string(irJSON), irHash,
			ir.Version, version, string(reportJSON), report.Overall, string(state), reason, now, now)
		return err
	}); err != nil {
		t.Fatalf("insert test extractor: %v", err)
	}
	return id
}

func assertHealCounts(t *testing.T, harness *selfHealHarness, extractors, runs, outbox int) {
	t.Helper()
	var gotExtractors, gotRuns, gotOutbox int
	if err := harness.durable.View(context.Background(), func(tx ledger.ReadTx) error {
		if err := tx.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM extractors").Scan(&gotExtractors); err != nil {
			return err
		}
		if err := tx.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM extractor_heal_runs").Scan(&gotRuns); err != nil {
			return err
		}
		return tx.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM outbox_events").Scan(&gotOutbox)
	}); err != nil {
		t.Fatal(err)
	}
	if gotExtractors != extractors || gotRuns != runs || gotOutbox != outbox {
		t.Fatalf("counts extractor/run/outbox = %d/%d/%d, want %d/%d/%d",
			gotExtractors, gotRuns, gotOutbox, extractors, runs, outbox)
	}
}

func loadHealState(t *testing.T, durable *ledger.Store, runID string) (HealState, string) {
	t.Helper()
	var state string
	var lease sqlNullString
	if err := durable.View(context.Background(), func(tx ledger.ReadTx) error {
		return tx.QueryRowContext(context.Background(), `SELECT state, lease_id
			FROM extractor_heal_runs WHERE id = ?`, runID).Scan(&state, &lease)
	}); err != nil {
		t.Fatal(err)
	}
	return HealState(state), lease.String
}

// sqlNullString is the small Scanner surface these tests need without exposing
// database handles from ledger.Store.
type sqlNullString struct {
	String string
	Valid  bool
}

func (value *sqlNullString) Scan(source any) error {
	if source == nil {
		value.String, value.Valid = "", false
		return nil
	}
	text, ok := source.(string)
	if !ok {
		return fmt.Errorf("scan nullable string from %T", source)
	}
	value.String, value.Valid = text, true
	return nil
}
