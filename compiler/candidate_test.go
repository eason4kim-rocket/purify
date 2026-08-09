package compiler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/use-agent/purify/ledger"
)

type candidateHarness struct {
	store   *Store
	catalog *SampleCatalog
	durable *ledger.Store
	clock   *coordinatorTestClock
}

func TestSubmitCandidateInitialExactAndHealthyDifferent(t *testing.T) {
	harness := newCandidateHarness(t)
	set, schema := candidateSampleSet(t, harness, 10, uint64(1)<<63, MinCompileSamples)
	candidate := candidateForSet(set, schema, "h1.name")

	initial, err := harness.store.SubmitCandidate(context.Background(), candidate)
	if err != nil {
		t.Fatalf("SubmitCandidate(initial) error = %v", err)
	}
	if initial.Status != SubmissionInitialActive || initial.Extractor.Version != 1 ||
		initial.Extractor.TemplateClusterID != set.Key.TemplateClusterID {
		t.Fatalf("initial submission = %+v", initial)
	}
	assertCandidateCounts(t, harness.durable, 1, 0, len(set.Samples))

	retry, err := harness.store.SubmitCandidate(context.Background(), candidate)
	if err != nil {
		t.Fatalf("SubmitCandidate(exact retry) error = %v", err)
	}
	if retry.Status != SubmissionCurrent || retry.Extractor.ID != initial.Extractor.ID {
		t.Fatalf("exact retry = %+v, initial = %+v", retry, initial)
	}

	different := candidateForSet(set, schema, ".name")
	ignored, err := harness.store.SubmitCandidate(context.Background(), different)
	if err != nil {
		t.Fatalf("SubmitCandidate(healthy different) error = %v", err)
	}
	if ignored.Status != SubmissionActiveUnchanged || ignored.Extractor.ID != initial.Extractor.ID {
		t.Fatalf("healthy different submission = %+v", ignored)
	}
	assertCandidateCounts(t, harness.durable, 1, 0, len(set.Samples))
	current, err := harness.store.Get(context.Background(), initial.Extractor.ID)
	if err != nil || current.State != StateActive || current.IR.Fields[0].Selector != "h1.name" {
		t.Fatalf("healthy active changed = %+v, %v", current, err)
	}
}

func TestSubmitCandidateStaleCreatesPendingWithoutPromotion(t *testing.T) {
	harness := newCandidateHarness(t)
	set, schema := candidateSampleSet(t, harness, 100, uint64(1)<<63, MinCompileSamples)
	initial, err := harness.store.SubmitCandidate(context.Background(), candidateForSet(set, schema, "h1.name"))
	if err != nil {
		t.Fatal(err)
	}
	markCandidateSourceStale(t, harness, initial.Extractor.ID, "required_empty_rate")

	candidate := candidateForSet(set, schema, ".name")
	pending, err := harness.store.SubmitCandidate(context.Background(), candidate)
	if err != nil {
		t.Fatalf("SubmitCandidate(stale) error = %v", err)
	}
	if pending.Status != SubmissionPendingHeal || !validExtractorID(pending.HealRunID) ||
		pending.SourceExtractorID != initial.Extractor.ID || pending.SourceVersion != 1 {
		t.Fatalf("pending submission = %+v", pending)
	}
	assertCandidateCounts(t, harness.durable, 1, 1, len(set.Samples))
	assertCandidateSourceAndBindings(t, harness, initial.Extractor.ID, StateStale, set)

	// An exact retry after a process crash must resolve to the same immutable
	// pending run, not create a second audit record.
	retry, err := harness.store.SubmitCandidate(context.Background(), candidate)
	if err != nil || retry.Status != SubmissionPendingHeal || retry.HealRunID != pending.HealRunID {
		t.Fatalf("pending retry = %+v, %v", retry, err)
	}
	assertCandidateCounts(t, harness.durable, 1, 1, len(set.Samples))
}

func TestSubmitCandidateExactRetryDoesNotStealAnotherLineageBinding(t *testing.T) {
	harness := newCandidateHarness(t)
	firstSet, schema := candidateSampleSet(t, harness, 150, uint64(1)<<63, MinCompileSamples)
	secondSet, _ := candidateSampleSet(t, harness, 175, 0x40000000000000ff, MinCompileSamples)
	firstCandidate := candidateForSet(firstSet, schema, "h1.name")
	first, err := harness.store.SubmitCandidate(context.Background(), firstCandidate)
	if err != nil {
		t.Fatal(err)
	}
	second, err := harness.store.SubmitCandidate(context.Background(), candidateForSet(secondSet, schema, "h1.name"))
	if err != nil {
		t.Fatal(err)
	}
	if err := harness.durable.Update(context.Background(), func(tx ledger.WriteTx) error {
		_, err := tx.ExecContext(context.Background(), `UPDATE extractor_page_bindings
			SET extractor_id = ? WHERE page_hash = ? AND schema_hash = ?`,
			second.Extractor.ID, firstSet.Samples[0].PageHash, firstSet.Key.SchemaHash)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	result, err := harness.store.SubmitCandidate(context.Background(), firstCandidate)
	if !errors.Is(err, ErrHealingRequired) || !errors.Is(err, ErrCandidateAmbiguous) ||
		result.Status != SubmissionRejectedAmbiguous {
		t.Fatalf("exact retry with foreign binding = %+v, %v", result, err)
	}
	var bound string
	if err := harness.durable.View(context.Background(), func(tx ledger.ReadTx) error {
		return tx.QueryRowContext(context.Background(), `SELECT extractor_id FROM extractor_page_bindings
			WHERE page_hash = ? AND schema_hash = ?`, firstSet.Samples[0].PageHash,
			firstSet.Key.SchemaHash).Scan(&bound)
	}); err != nil {
		t.Fatal(err)
	}
	if bound != second.Extractor.ID {
		t.Fatalf("foreign binding changed to %q, want %q (first %q)", bound, second.Extractor.ID, first.Extractor.ID)
	}
}

func TestSubmitCandidatePendingRetryRequiresCompleteImmutableTuple(t *testing.T) {
	harness := newCandidateHarness(t)
	set, schema := candidateSampleSet(t, harness, 180, uint64(1)<<63, 10)
	initial := candidateForSet(set, schema, "h1.name")
	initial.Validation = validationForCandidateSamples(10)
	active, err := harness.store.SubmitCandidate(context.Background(), initial)
	if err != nil {
		t.Fatal(err)
	}
	markCandidateSourceStale(t, harness, active.Extractor.ID, "required_empty_rate")

	pendingCandidate := candidateForSet(set, schema, ".name")
	pendingCandidate.Validation = ValidationReport{
		PerField: []FieldValidation{{Name: "name", Matches: 9, Samples: 10, Score: 0.9}},
		Overall:  0.9, Samples: 10, ValidSamples: 10,
		Threshold: ValidationThreshold, CanEnable: true,
	}
	pending, err := harness.store.SubmitCandidate(context.Background(), pendingCandidate)
	if err != nil {
		t.Fatal(err)
	}
	differentReport := pendingCandidate
	differentReport.Validation = validationForCandidateSamples(10)
	if result, err := harness.store.SubmitCandidate(context.Background(), differentReport); !errors.Is(err, ErrHealingRequired) || result.HealRunID != "" {
		t.Fatalf("different validation retry = %+v, %v", result, err)
	}
	var count int
	if err := harness.durable.View(context.Background(), func(tx ledger.ReadTx) error {
		return tx.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM extractor_heal_runs
			WHERE id = ? AND validation = 0.9`, pending.HealRunID).Scan(&count)
	}); err != nil || count != 1 {
		t.Fatalf("original pending audit count = %d, %v", count, err)
	}
}

func TestSubmitCandidateAttributesTemplateDriftThroughSampleBindings(t *testing.T) {
	harness := newCandidateHarness(t)
	oldSet, schema := candidateSampleSet(t, harness, 200, uint64(1)<<63, MinCompileSamples)
	initial, err := harness.store.SubmitCandidate(context.Background(), candidateForSet(oldSet, schema, "h1.name"))
	if err != nil {
		t.Fatal(err)
	}
	newSet, _ := candidateSampleSet(t, harness, 300, 0x40000000000000ff, MinCompileSamples)
	newCandidate := candidateForSet(newSet, schema, ".name")
	markCandidateSourceStale(t, harness, initial.Extractor.ID, "template_drift")
	if err := harness.durable.Update(context.Background(), func(tx ledger.WriteTx) error {
		return bindCandidateSamples(context.Background(), tx, newCandidate, initial.Extractor.ID, harness.clock.Now())
	}); err != nil {
		t.Fatalf("bind drift samples: %v", err)
	}

	pending, err := harness.store.SubmitCandidate(context.Background(), newCandidate)
	if err != nil {
		t.Fatalf("SubmitCandidate(template drift) error = %v", err)
	}
	if pending.Status != SubmissionPendingHeal || pending.SourceExtractorID != initial.Extractor.ID {
		t.Fatalf("template drift submission = %+v", pending)
	}
	assertCandidateCounts(t, harness.durable, 1, 1, len(oldSet.Samples)+len(newSet.Samples))
	assertCandidateSourceAndBindings(t, harness, initial.Extractor.ID, StateStale, newSet)
}

func TestSubmitCandidateNearHistoryDistanceBoundaryWithoutBindings(t *testing.T) {
	base := uint64(1) << 63
	for _, test := range []struct {
		name         string
		state        ExtractorState
		fingerprint  uint64
		wantStatus   SubmissionStatus
		wantError    error
		wantHeals    int
		wantRows     int
		wantBindings int
	}{
		{"distance six active", StateActive, base ^ 0x3f, SubmissionActiveUnchanged, nil, 0, 1, 3},
		{"distance six stale", StateStale, base ^ 0x3f, SubmissionPendingHeal, nil, 1, 1, 3},
		{"distance six retired", StateRetired, base ^ 0x3f, SubmissionRejectedRetired, ErrCandidateRetired, 0, 1, 3},
		{"distance seven is new", StateActive, base ^ 0x7f, SubmissionInitialActive, nil, 0, 2, 6},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newCandidateHarness(t)
			oldSet, schema := candidateSampleSet(t, harness, 325, base, MinCompileSamples)
			source, err := harness.store.SubmitCandidate(context.Background(), candidateForSet(oldSet, schema, "h1.name"))
			if err != nil {
				t.Fatal(err)
			}
			switch test.state {
			case StateStale:
				markCandidateSourceStale(t, harness, source.Extractor.ID, "template_drift")
			case StateRetired:
				if _, err := harness.store.Retire(context.Background(), source.Extractor.ID); err != nil {
					t.Fatal(err)
				}
			}
			clearCandidateCatalog(t, harness)
			newSet, _ := candidateSampleSet(t, harness, 350, test.fingerprint, MinCompileSamples)
			result, err := harness.store.SubmitCandidate(context.Background(), candidateForSet(newSet, schema, ".name"))
			if test.wantError != nil {
				if !errors.Is(err, ErrHealingRequired) || !errors.Is(err, test.wantError) {
					t.Fatalf("SubmitCandidate() error = %v, want %v", err, test.wantError)
				}
			} else if err != nil {
				t.Fatalf("SubmitCandidate() error = %v", err)
			}
			if result.Status != test.wantStatus {
				t.Fatalf("status = %q, want %q", result.Status, test.wantStatus)
			}
			assertCandidateCounts(t, harness.durable, test.wantRows, test.wantHeals, test.wantBindings)
		})
	}
}

func TestSubmitCandidateRetiredAndAmbiguousFailClosed(t *testing.T) {
	t.Run("retired only", func(t *testing.T) {
		harness := newCandidateHarness(t)
		set, schema := candidateSampleSet(t, harness, 400, uint64(1)<<63, MinCompileSamples)
		initial, err := harness.store.SubmitCandidate(context.Background(), candidateForSet(set, schema, "h1.name"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := harness.store.Retire(context.Background(), initial.Extractor.ID); err != nil {
			t.Fatal(err)
		}
		result, err := harness.store.SubmitCandidate(context.Background(), candidateForSet(set, schema, ".name"))
		if !errors.Is(err, ErrHealingRequired) || !errors.Is(err, ErrCandidateRetired) ||
			result.Status != SubmissionRejectedRetired {
			t.Fatalf("retired submission = %+v, %v", result, err)
		}
		assertCandidateCounts(t, harness.durable, 1, 0, len(set.Samples))
	})

	t.Run("ambiguous bindings", func(t *testing.T) {
		harness := newCandidateHarness(t)
		firstSet, schema := candidateSampleSet(t, harness, 500, uint64(1)<<63, MinCompileSamples)
		secondSet, _ := candidateSampleSet(t, harness, 600, 0x40000000000000ff, MinCompileSamples)
		first, err := harness.store.SubmitCandidate(context.Background(), candidateForSet(firstSet, schema, "h1.name"))
		if err != nil {
			t.Fatal(err)
		}
		second, err := harness.store.SubmitCandidate(context.Background(), candidateForSet(secondSet, schema, "h1.name"))
		if err != nil {
			t.Fatal(err)
		}
		markCandidateSourceStale(t, harness, first.Extractor.ID, "template_drift")
		markCandidateSourceStale(t, harness, second.Extractor.ID, "template_drift")

		targetSet, _ := candidateSampleSet(t, harness, 700, 0x2000000000ff0000, MinCompileSamples)
		target := candidateForSet(targetSet, schema, ".name")
		if err := harness.durable.Update(context.Background(), func(tx ledger.WriteTx) error {
			if err := bindCandidateSamples(context.Background(), tx, target, first.Extractor.ID, harness.clock.Now()); err != nil {
				return err
			}
			_, err := tx.ExecContext(context.Background(), `UPDATE extractor_page_bindings
				SET extractor_id = ? WHERE page_hash = ? AND schema_hash = ?`,
				second.Extractor.ID, target.Samples[0].PageHash, target.Key.SchemaHash)
			return err
		}); err != nil {
			t.Fatal(err)
		}

		result, err := harness.store.SubmitCandidate(context.Background(), target)
		if !errors.Is(err, ErrHealingRequired) || !errors.Is(err, ErrCandidateAmbiguous) ||
			result.Status != SubmissionRejectedAmbiguous {
			t.Fatalf("ambiguous submission = %+v, %v", result, err)
		}
		assertCandidateCounts(t, harness.durable, 2, 0,
			len(firstSet.Samples)+len(secondSet.Samples)+len(targetSet.Samples))
	})
}

func TestSubmitCandidateConcurrentAndCrashIdempotent(t *testing.T) {
	t.Run("concurrent initial", func(t *testing.T) {
		harness := newCandidateHarness(t)
		set, schema := candidateSampleSet(t, harness, 800, uint64(1)<<63, MinCompileSamples)
		candidate := candidateForSet(set, schema, "h1.name")
		const workers = 24
		start := make(chan struct{})
		results := make(chan Submission, workers)
		errs := make(chan error, workers)
		var group sync.WaitGroup
		for range workers {
			group.Add(1)
			go func() {
				defer group.Done()
				<-start
				result, err := harness.store.SubmitCandidate(context.Background(), candidate)
				if err != nil {
					errs <- err
					return
				}
				results <- result
			}()
		}
		close(start)
		group.Wait()
		close(results)
		close(errs)
		for err := range errs {
			t.Fatalf("concurrent SubmitCandidate error = %v", err)
		}
		id := ""
		initials := 0
		for result := range results {
			if result.Status == SubmissionInitialActive {
				initials++
			}
			if id == "" {
				id = result.Extractor.ID
			}
			if result.Extractor.ID != id {
				t.Fatalf("concurrent extractor IDs differ: %q/%q", result.Extractor.ID, id)
			}
		}
		if initials != 1 {
			t.Fatalf("initial submissions = %d, want 1", initials)
		}
		assertCandidateCounts(t, harness.durable, 1, 0, len(set.Samples))
	})

	t.Run("pending survives reopen", func(t *testing.T) {
		dataDir := t.TempDir()
		clock := &coordinatorTestClock{now: catalogTestTime}
		durable, err := ledger.Open(dataDir)
		if err != nil {
			t.Fatal(err)
		}
		catalog, _ := NewSampleCatalog(durable, clock.Now)
		store, _ := NewStore(durable, WithStoreClock(clock.Now))
		harness := &candidateHarness{store: store, catalog: catalog, durable: durable, clock: clock}
		set, schema := candidateSampleSet(t, harness, 900, uint64(1)<<63, MinCompileSamples)
		initial, err := store.SubmitCandidate(context.Background(), candidateForSet(set, schema, "h1.name"))
		if err != nil {
			t.Fatal(err)
		}
		markCandidateSourceStale(t, harness, initial.Extractor.ID, "template_drift")
		candidate := candidateForSet(set, schema, ".name")
		pending, err := store.SubmitCandidate(context.Background(), candidate)
		if err != nil {
			t.Fatal(err)
		}
		if err := durable.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := ledger.Open(dataDir)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		reopenedStore, _ := NewStore(reopened, WithStoreClock(clock.Now))
		retry, err := reopenedStore.SubmitCandidate(context.Background(), candidate)
		if err != nil || retry.HealRunID != pending.HealRunID || retry.Status != SubmissionPendingHeal {
			t.Fatalf("reopened retry = %+v, %v; pending = %+v", retry, err, pending)
		}
	})
}

func TestSubmitCandidateSampleBoundNPlusOne(t *testing.T) {
	harness := newCandidateHarness(t)
	set, schema := candidateSampleSet(t, harness, 1_000, uint64(1)<<63, MaxCompileSamples)
	maximum := candidateForSet(set, schema, "h1.name")
	maximum.Validation = validationForCandidateSamples(MaxCompileSamples)
	if result, err := harness.store.SubmitCandidate(context.Background(), maximum); err != nil ||
		result.Status != SubmissionInitialActive {
		t.Fatalf("SubmitCandidate(maximum) = %+v, %v", result, err)
	}

	overflow := maximum
	overflow.Samples = append(append([]SampleRef(nil), maximum.Samples...), SampleRef{
		PageHash: catalogDigest("overflow-page"), SnapshotID: catalogSnapshotID("overflow-snapshot"),
		SampleSimHash: set.Key.ClusterSimHash, FetchedAt: catalogTestTime,
	})
	overflow.Validation = validationForCandidateSamples(MaxCompileSamples + 1)
	overflow.CatalogRevision = sampleSetRevision(SampleSet{Key: overflow.Key, Samples: overflow.Samples})
	if _, err := harness.store.SubmitCandidate(context.Background(), overflow); !errors.Is(err, ErrInvalidStoreInput) {
		t.Fatalf("SubmitCandidate(N+1) error = %v", err)
	}
	assertCandidateCounts(t, harness.durable, 1, 0, MaxCompileSamples)
}

func TestSubmitCandidateRejectsStaleCatalogRevisionAndClonesInput(t *testing.T) {
	harness := newCandidateHarness(t)
	set, schema := candidateSampleSet(t, harness, 1_100, uint64(1)<<63, MinCompileSamples)
	candidate := candidateForSet(set, schema, "h1.name")
	candidate.CatalogRevision = catalogDigest("forged")
	if _, err := harness.store.SubmitCandidate(context.Background(), candidate); !errors.Is(err, ErrInvalidStoreInput) {
		t.Fatalf("forged revision error = %v", err)
	}
	assertCandidateCounts(t, harness.durable, 0, 0, 0)

	candidate = candidateForSet(set, schema, "h1.name")
	result, err := harness.store.SubmitCandidate(context.Background(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Schema[0] = 'x'
	candidate.IR.Fields[0].Selector = "mutated"
	candidate.Samples[0].PageHash = catalogDigest("mutated")
	stored, err := harness.store.Get(context.Background(), result.Extractor.ID)
	if err != nil || stored.IR.Fields[0].Selector != "h1.name" || stored.Schema[0] != '{' {
		t.Fatalf("stored candidate aliases caller input: %+v, %v", stored, err)
	}
}

func newCandidateHarness(t *testing.T) *candidateHarness {
	t.Helper()
	clock := &coordinatorTestClock{now: catalogTestTime}
	durable, err := ledger.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(durable, WithStoreClock(clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := NewSampleCatalog(durable, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = durable.Close() })
	return &candidateHarness{store: store, catalog: catalog, durable: durable, clock: clock}
}

func candidateSampleSet(
	t *testing.T,
	harness *candidateHarness,
	base int,
	fingerprint uint64,
	count int,
) (SampleSet, json.RawMessage) {
	t.Helper()
	var key CompileKey
	for index := 0; index < count; index++ {
		page := catalogPageKey(t, base+index, fingerprint)
		if index == 0 {
			key = CompileKey{
				Host: page.Host, SchemaHash: page.SchemaHash, ContentProfile: DefaultCompilerProfile,
				TemplateClusterID: templateClusterID(fingerprint), ClusterSimHash: fingerprint,
			}
		}
		_, _, err := harness.catalog.Observe(context.Background(), page,
			catalogSnapshotID(fmt.Sprintf("candidate-%d", base+index)),
			catalogTestTime.Add(time.Duration(base+index)*time.Second))
		if err != nil {
			t.Fatalf("Observe(candidate %d) error = %v", index, err)
		}
	}
	set, ready, err := harness.catalog.Load(context.Background(), key, MaxCompileSamples)
	if err != nil || !ready || len(set.Samples) != count {
		t.Fatalf("Load(candidate set) = %d/%v/%v", len(set.Samples), ready, err)
	}
	return set, readySetSchema(t, catalogSchema())
}

func candidateForSet(set SampleSet, schema json.RawMessage, selector string) Candidate {
	ir, _ := extractorFixture(selector)
	return Candidate{
		Key: set.Key, CatalogRevision: set.Revision,
		Schema: append(json.RawMessage(nil), schema...), IR: ir,
		Validation: validationForCandidateSamples(len(set.Samples)),
		Samples:    append([]SampleRef(nil), set.Samples...),
	}
}

func validationForCandidateSamples(count int) ValidationReport {
	return ValidationReport{
		PerField: []FieldValidation{{Name: "name", Matches: count, Samples: count, Score: 1}},
		Overall:  1, Samples: count, ValidSamples: count,
		Threshold: ValidationThreshold, CanEnable: true,
	}
}

func markCandidateSourceStale(t *testing.T, harness *candidateHarness, id, reason string) {
	t.Helper()
	if err := harness.durable.Update(context.Background(), func(tx ledger.WriteTx) error {
		_, err := tx.ExecContext(context.Background(), `UPDATE extractors
			SET state = 'stale', stale_reason = ?, updated_at = ? WHERE id = ? AND state = 'active'`,
			reason, formatExtractorTime(harness.clock.Now()), id)
		return err
	}); err != nil {
		t.Fatalf("mark candidate source stale: %v", err)
	}
}

func clearCandidateCatalog(t *testing.T, harness *candidateHarness) {
	t.Helper()
	if err := harness.durable.Update(context.Background(), func(tx ledger.WriteTx) error {
		_, err := tx.ExecContext(context.Background(), "DELETE FROM compiler_samples")
		return err
	}); err != nil {
		t.Fatalf("clear candidate catalog: %v", err)
	}
}

func assertCandidateCounts(t *testing.T, durable *ledger.Store, extractors, heals, bindings int) {
	t.Helper()
	var gotExtractors, gotHeals, gotBindings int
	if err := durable.View(context.Background(), func(tx ledger.ReadTx) error {
		if err := tx.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM extractors").Scan(&gotExtractors); err != nil {
			return err
		}
		if err := tx.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM extractor_heal_runs").Scan(&gotHeals); err != nil {
			return err
		}
		return tx.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM extractor_page_bindings").Scan(&gotBindings)
	}); err != nil {
		t.Fatal(err)
	}
	if gotExtractors != extractors || gotHeals != heals || gotBindings != bindings {
		t.Fatalf("candidate counts extractors/heals/bindings = %d/%d/%d, want %d/%d/%d",
			gotExtractors, gotHeals, gotBindings, extractors, heals, bindings)
	}
}

func assertCandidateSourceAndBindings(
	t *testing.T,
	harness *candidateHarness,
	id string,
	state ExtractorState,
	set SampleSet,
) {
	t.Helper()
	value, err := harness.store.Get(context.Background(), id)
	if err != nil || value.State != state {
		t.Fatalf("source extractor = %+v, %v, want state %q", value, err, state)
	}
	if err := harness.durable.View(context.Background(), func(tx ledger.ReadTx) error {
		for _, sample := range set.Samples {
			var bound string
			if err := tx.QueryRowContext(context.Background(), `SELECT extractor_id
				FROM extractor_page_bindings WHERE page_hash = ? AND schema_hash = ?`,
				sample.PageHash, set.Key.SchemaHash).Scan(&bound); err != nil {
				return err
			}
			if bound != id {
				return fmt.Errorf("page %s bound to %s, want %s", sample.PageHash, bound, id)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
