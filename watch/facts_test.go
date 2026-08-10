package watch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/models"
)

func TestFactAtUsesHalfOpenHistoryAndReturnsDeepCopies(t *testing.T) {
	clock := &testClock{now: testEpoch}
	ids := &testIDs{}
	durable, store := openWatchStore(t, WithClock(clock.Now), WithIDGenerator(ids.Generate))
	created, ok, err := store.Create(context.Background(), validSpec("  durable   model  "))
	if err != nil || !ok {
		t.Fatalf("Create() = (%#v,%v,%v)", created, ok, err)
	}
	t0 := testEpoch.Add(time.Hour)
	t1 := t0.Add(time.Hour)
	t2 := t1.Add(time.Hour)
	oldID := strings.Repeat("b", 64)
	newID := strings.Repeat("c", 64)
	seedFactAtOpen(t, durable, created, t0, oldID, "fact-baseline", json.RawMessage(`"19"`),
		"example.com", "https://www.example.com/pricing", "a", "receipt-1")
	seedFactAtChange(t, durable, created, t1, oldID, newID)
	seedFactAtGone(t, durable, created, t2, newID)

	for name, asOf := range map[string]time.Time{
		"before first": t0.Add(-time.Nanosecond),
		"at gone":      t2,
		"after gone":   t2.Add(time.Hour),
	} {
		t.Run(name, func(t *testing.T) {
			got, found, err := store.FactAt(context.Background(), "durable model", created.Spec.Predicate, asOf)
			if err != nil || found || !reflect.DeepEqual(got, Fact{}) {
				t.Fatalf("FactAt(%s) = (%#v,%v,%v)", asOf, got, found, err)
			}
		})
	}

	old, found, err := store.FactAt(context.Background(), "  durable   model ", created.Spec.Predicate, t0)
	if err != nil || !found || old.ID != oldID || string(old.Value) != `"19"` ||
		old.ValidTo == nil || !old.ValidTo.Equal(t1) || old.ClosedOutcome != ledger.OutcomeChanged ||
		old.SupersededBy != newID || old.ClosedVerificationID != "fact-change" ||
		old.ClosedClaimIndex == nil || *old.ClosedClaimIndex != 0 {
		t.Fatalf("FactAt(first boundary) = (%#v,%v,%v)", old, found, err)
	}
	justBeforeChange, found, err := store.FactAt(context.Background(), created.Spec.Subject,
		created.Spec.Predicate, t1.Add(-time.Nanosecond))
	if err != nil || !found || justBeforeChange.ID != oldID {
		t.Fatalf("FactAt(before change) = (%#v,%v,%v)", justBeforeChange, found, err)
	}
	current, found, err := store.FactAt(context.Background(), created.Spec.Subject, created.Spec.Predicate, t1)
	if err != nil || !found || current.ID != newID || string(current.Value) != `"20"` ||
		current.ValidTo == nil || !current.ValidTo.Equal(t2) || current.ClosedOutcome != ledger.OutcomeGone ||
		current.GoneScope != ledger.GoneScopeField || current.SupersededBy != "" {
		t.Fatalf("FactAt(change boundary) = (%#v,%v,%v)", current, found, err)
	}

	// RawMessage and nullable timestamps are not retained across calls.
	old.Value[0] = 'x'
	*old.ValidTo = time.Time{}
	again, found, err := store.FactAt(context.Background(), created.Spec.Subject, created.Spec.Predicate, t0)
	if err != nil || !found || string(again.Value) != `"19"` || again.ValidTo == nil || !again.ValidTo.Equal(t1) {
		t.Fatalf("FactAt observed caller mutation = (%#v,%v,%v)", again, found, err)
	}
}

func TestFactAtRejectsOverlappingIntervals(t *testing.T) {
	clock := &testClock{now: testEpoch}
	ids := &testIDs{}
	durable, store := openWatchStore(t, WithClock(clock.Now), WithIDGenerator(ids.Generate))
	firstSpec := validSpec("overlap subject")
	first, _, err := store.Create(context.Background(), firstSpec)
	if err != nil {
		t.Fatal(err)
	}
	secondSpec := firstSpec
	secondSpec.Freshness = "day"
	second, _, err := store.Create(context.Background(), secondSpec)
	if err != nil {
		t.Fatal(err)
	}
	seedFactAtOpen(t, durable, first, testEpoch, strings.Repeat("1", 64), "overlap-one",
		json.RawMessage(`"one"`), "example.com", "https://one.example.com/fact", "1", "receipt-one")
	seedFactAtOpen(t, durable, second, testEpoch, strings.Repeat("2", 64), "overlap-two",
		json.RawMessage(`"two"`), "example.com", "https://two.example.com/fact", "2", "receipt-two")

	value, found, err := store.FactAt(context.Background(), first.Spec.Subject, first.Spec.Predicate, testEpoch)
	if found || !reflect.DeepEqual(value, Fact{}) || !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("FactAt(overlap) = (%#v,%v,%v)", value, found, err)
	}
}

func TestFactAtValidatesInputsTimesContextsAndClosedStore(t *testing.T) {
	clock := &testClock{now: testEpoch}
	_, store := openWatchStore(t, WithClock(clock.Now))

	var nilStore *Store
	if _, _, err := nilStore.FactAt(context.Background(), "subject", "predicate", testEpoch); !errors.Is(err, ErrInvalidStore) {
		t.Fatalf("nil Store.FactAt() error = %v", err)
	}
	if _, _, err := store.FactAt(nil, "subject", "predicate", testEpoch); !errors.Is(err, ErrInvalidStore) {
		t.Fatalf("FactAt(nil context) error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := store.FactAt(canceled, "subject", "predicate", testEpoch); !errors.Is(err, context.Canceled) {
		t.Fatalf("FactAt(canceled) error = %v", err)
	}

	maximumSQLiteTime := time.Date(9999, 12, 31, 23, 59, 59, 999499999, time.UTC)
	for name, value := range map[string]time.Time{
		"zero":              {},
		"year zero":         time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC),
		"beyond sqlite":     maximumSQLiteTime.Add(time.Nanosecond),
		"year 10000":        time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
		"non UTC":           testEpoch.In(time.FixedZone("offset", 3600)),
		"monotonic payload": time.Now(),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := store.FactAt(context.Background(), "subject", "predicate", value); !errors.Is(err, ErrInvalidWatchSpec) {
				t.Fatalf("FactAt(%#v) error = %v", value, err)
			}
		})
	}
	if _, found, err := store.FactAt(context.Background(), "subject", "predicate", maximumSQLiteTime); err != nil || found {
		t.Fatalf("FactAt(max SQLite time) = (found=%v,error=%v)", found, err)
	}

	maxSubject := strings.Repeat("😀", models.MaxAnswerSubjectRunes)
	maxPredicate := strings.Repeat("界", models.MaxAnswerPredicateBytes/len("界"))
	// Use a four-byte rune to hit the byte edge exactly.
	maxPredicate = strings.Repeat("😀", models.MaxAnswerPredicateBytes/len("😀"))
	for name, input := range []struct {
		subject   string
		predicate string
		valid     bool
	}{
		{subject: maxSubject, predicate: "p", valid: true},
		{subject: maxSubject + "x", predicate: "p"},
		{subject: "s", predicate: maxPredicate, valid: true},
		{subject: "s", predicate: maxPredicate + "x"},
		{subject: "private\nsubject", predicate: "p"},
		{subject: "subject", predicate: "bad predicate"},
		{subject: "subject", predicate: "bad\xff"},
	} {
		name := fmt.Sprintf("case-%d", name)
		t.Run(name, func(t *testing.T) {
			_, found, err := store.FactAt(context.Background(), input.subject, input.predicate, testEpoch)
			if input.valid {
				if err != nil || found {
					t.Fatalf("FactAt(valid bounds) = (found=%v,error=%v)", found, err)
				}
			} else if !errors.Is(err, ErrInvalidWatchSpec) {
				t.Fatalf("FactAt(invalid bounds) error = %v", err)
			}
		})
	}

	durable, closedStore := openWatchStore(t)
	if err := durable.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := closedStore.FactAt(context.Background(), "subject", "predicate", testEpoch); !errors.Is(err, ledger.ErrClosed) {
		t.Fatalf("FactAt(closed ledger) error = %v", err)
	}
}

func TestFactAtPrioritizesCancellationAfterTheIndexedRead(t *testing.T) {
	durable, store, created := openFactAtRelationFixture(t, "late cancellation")
	seedFactAtOpen(t, durable, created, testEpoch, strings.Repeat("e", 64), "late-cancel-base",
		json.RawMessage(`"value"`), "example.com", "https://example.com/fact", "e", "receipt")
	ctx := &factAtLateCancelContext{Context: context.Background(), cancelAt: 5}
	value, found, err := store.FactAt(ctx, created.Spec.Subject, created.Spec.Predicate, testEpoch)
	if found || !reflect.DeepEqual(value, Fact{}) || !errors.Is(err, context.Canceled) || ctx.calls.Load() < ctx.cancelAt {
		t.Fatalf("FactAt(late cancellation) = (%#v,%v,%v), Err calls=%d", value, found, err, ctx.calls.Load())
	}
}

func TestFactAtAcceptsCanonicalRootsAndRejectsCorruptDurableRows(t *testing.T) {
	for name, test := range map[string]struct {
		url  string
		root string
	}{
		"registrable IDN": {url: "https://xn--bcher-kva.example/pricing", root: "xn--bcher-kva.example"},
		"public IPv4":     {url: "https://8.8.8.8/pricing", root: "8.8.8.8"},
		"public IPv6":     {url: "https://[2606:4700:4700::1111]/pricing", root: "2606:4700:4700::1111"},
	} {
		t.Run(name, func(t *testing.T) {
			clock := &testClock{now: testEpoch}
			ids := &testIDs{}
			durable, store := openWatchStore(t, WithClock(clock.Now), WithIDGenerator(ids.Generate))
			created, _, err := store.Create(context.Background(), validSpec("canonical root"))
			if err != nil {
				t.Fatal(err)
			}
			seedFactAtOpen(t, durable, created, testEpoch, strings.Repeat("3", 64), "canonical-root",
				json.RawMessage(`"value"`), test.root, test.url, "3", "receipt")
			fact, found, err := store.FactAt(context.Background(), created.Spec.Subject, created.Spec.Predicate, testEpoch)
			if err != nil || !found || fact.Root != test.root || fact.SourceURL != test.url {
				t.Fatalf("FactAt() = (%#v,%v,%v)", fact, found, err)
			}
		})
	}

	for name, test := range map[string]struct {
		value json.RawMessage
		url   string
		root  string
	}{
		"noncanonical JSON": {value: json.RawMessage(`"\u0076alue"`), url: "https://example.com/fact", root: "example.com"},
		"private IP":        {value: json.RawMessage(`"value"`), url: "https://127.0.0.1/fact", root: "127.0.0.1"},
		"root mismatch":     {value: json.RawMessage(`"value"`), url: "https://example.com/fact", root: "attacker.example"},
		"noncanonical IDN":  {value: json.RawMessage(`"value"`), url: "https://bücher.example/fact", root: "xn--bcher-kva.example"},
	} {
		t.Run(name, func(t *testing.T) {
			clock := &testClock{now: testEpoch}
			ids := &testIDs{}
			durable, store := openWatchStore(t, WithClock(clock.Now), WithIDGenerator(ids.Generate))
			created, _, err := store.Create(context.Background(), validSpec("corrupt durable row"))
			if err != nil {
				t.Fatal(err)
			}
			seedFactAtOpen(t, durable, created, testEpoch, strings.Repeat("4", 64), "corrupt-row",
				test.value, test.root, test.url, "4", "receipt")
			fact, found, err := store.FactAt(context.Background(), created.Spec.Subject, created.Spec.Predicate, testEpoch)
			if found || !reflect.DeepEqual(fact, Fact{}) || !errors.Is(err, ErrCorruptStore) {
				t.Fatalf("FactAt(corrupt) = (%#v,%v,%v)", fact, found, err)
			}
		})
	}
}

func TestFactAtClosesRowsAfterScanCorruption(t *testing.T) {
	durable, store, created := openFactAtRelationFixture(t, "scan corruption")
	seedFactAtOpen(t, durable, created, testEpoch, strings.Repeat("f", 64), "scan-corrupt-base",
		json.RawMessage(`"value"`), "example.com", "https://example.com/fact", "f", "receipt")
	poisonFactAtDatabase(t, durable, `DROP TRIGGER trg_facts_open_transition`)
	poisonFactAtDatabase(t, durable, `DROP TRIGGER trg_facts_identity_immutable`)
	if err := durable.Update(context.Background(), func(tx ledger.WriteTx) error {
		if _, err := tx.ExecContext(context.Background(), `PRAGMA ignore_check_constraints = ON`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(context.Background(), `UPDATE facts
			SET observed_at = 'malformed' WHERE created_verification_id = 'scan-corrupt-base'`); err != nil {
			return err
		}
		_, err := tx.ExecContext(context.Background(), `PRAGMA ignore_check_constraints = OFF`)
		return err
	}); err != nil {
		t.Fatalf("poison scan column: %v", err)
	}

	value, found, err := store.FactAt(context.Background(), created.Spec.Subject, created.Spec.Predicate, testEpoch)
	if found || !reflect.DeepEqual(value, Fact{}) || !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("FactAt(scan corruption) = (%#v,%v,%v)", value, found, err)
	}

	operationDone := make(chan error, 1)
	go func() {
		_, operationErr := store.Get(context.Background(), created.ID)
		operationDone <- operationErr
	}()
	select {
	case operationErr := <-operationDone:
		if operationErr != nil {
			t.Fatalf("Get after scan corruption: %v", operationErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Store operation blocked after scan corruption")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- durable.Close() }()
	select {
	case closeErr := <-closeDone:
		if closeErr != nil {
			t.Fatalf("Close after scan corruption: %v", closeErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked after scan corruption")
	}
}

func TestFactAtRejectsRelationalProvenanceCorruption(t *testing.T) {
	t.Run("created and latest verification tuple", func(t *testing.T) {
		for name, statement := range map[string]string{
			"path":          `UPDATE verifications SET path = 'poisoned' WHERE verification_id = 'relation-base'`,
			"value":         `UPDATE verifications SET old_value = '"poisoned"' WHERE verification_id = 'relation-base'`,
			"snapshot":      `UPDATE verifications SET new_snapshot_id = 'sha256:bad' WHERE verification_id = 'relation-base'`,
			"receipt":       `UPDATE verifications SET receipt = 'poisoned' WHERE verification_id = 'relation-base'`,
			"effective URL": `UPDATE verifications SET final_url = 'https://other.example/fact' WHERE verification_id = 'relation-base'`,
			"verified at":   `UPDATE verifications SET verified_at = '2026-08-10T09:09:09Z' WHERE verification_id = 'relation-base'`,
		} {
			t.Run(name, func(t *testing.T) {
				durable, store, created := openFactAtRelationFixture(t, "created relation")
				seedFactAtOpen(t, durable, created, testEpoch, strings.Repeat("5", 64), "relation-base",
					json.RawMessage(`"value"`), "example.com", "https://example.com/fact", "5", "receipt")
				poisonFactAtDatabase(t, durable, statement)
				assertFactAtCorrupt(t, store, created.Spec.Subject, created.Spec.Predicate, testEpoch)
			})
		}
	})

	t.Run("distinct latest verification tuple", func(t *testing.T) {
		durable, store, created := openFactAtRelationFixture(t, "latest relation")
		factID := strings.Repeat("6", 64)
		seedFactAtOpen(t, durable, created, testEpoch, factID, "latest-base",
			json.RawMessage(`"value"`), "example.com", "https://example.com/fact", "6", "receipt-1")
		confirmedAt := testEpoch.Add(time.Hour)
		seedFactAtConfirmation(t, durable, created, factID, confirmedAt)
		poisonFactAtDatabase(t, durable,
			`UPDATE verifications SET receipt = 'poisoned' WHERE verification_id = 'latest-confirmed'`)
		assertFactAtCorrupt(t, store, created.Spec.Subject, created.Spec.Predicate, testEpoch)
	})

	t.Run("closed verification tuple", func(t *testing.T) {
		durable, store, created := openFactAtRelationFixture(t, "closed relation")
		oldID := strings.Repeat("7", 64)
		newID := strings.Repeat("8", 64)
		seedFactAtOpen(t, durable, created, testEpoch, oldID, "closed-base",
			json.RawMessage(`"19"`), "example.com", "https://www.example.com/pricing", "a", "receipt-1")
		seedFactAtChange(t, durable, created, testEpoch.Add(time.Hour), oldID, newID)
		poisonFactAtDatabase(t, durable,
			`UPDATE verifications SET old_receipt = 'poisoned' WHERE verification_id = 'fact-change'`)
		assertFactAtCorrupt(t, store, created.Spec.Subject, created.Spec.Predicate, testEpoch)
	})

	t.Run("watch relation after trigger poisoning", func(t *testing.T) {
		durable, store, created := openFactAtRelationFixture(t, "watch relation")
		seedFactAtOpen(t, durable, created, testEpoch, strings.Repeat("9", 64), "watch-base",
			json.RawMessage(`"value"`), "example.com", "https://example.com/fact", "9", "receipt")
		poisonFactAtDatabase(t, durable, `DROP TRIGGER trg_watches_spec_immutable`)
		poisonFactAtDatabase(t, durable, `UPDATE watches SET predicate = 'poisoned', updated_at =
			'2026-08-10T01:02:04.000000004Z' WHERE id = '`+created.ID+`'`)
		assertFactAtCorrupt(t, store, created.Spec.Subject, created.Spec.Predicate, testEpoch)
	})

	t.Run("changed successor after trigger poisoning", func(t *testing.T) {
		durable, store, created := openFactAtRelationFixture(t, "successor relation")
		oldID := strings.Repeat("a", 64)
		newID := strings.Repeat("b", 64)
		seedFactAtOpen(t, durable, created, testEpoch, oldID, "successor-base",
			json.RawMessage(`"19"`), "example.com", "https://www.example.com/pricing", "a", "receipt-1")
		seedFactAtChange(t, durable, created, testEpoch.Add(time.Hour), oldID, newID)
		poisonFactAtDatabase(t, durable, `DROP TRIGGER trg_facts_open_transition`)
		poisonFactAtDatabase(t, durable, `UPDATE facts SET root = 'other.example',
			source_url = 'https://other.example/fact', receipt = 'other-receipt',
			snapshot_id = 'sha256:`+strings.Repeat("f", 64)+`' WHERE id = '`+newID+`'`)
		assertFactAtCorrupt(t, store, created.Spec.Subject, created.Spec.Predicate, testEpoch)
	})

	t.Run("gone cannot have a successor", func(t *testing.T) {
		durable, store, created := openFactAtRelationFixture(t, "gone successor")
		factID := strings.Repeat("c", 64)
		seedFactAtOpen(t, durable, created, testEpoch, factID, "gone-base",
			json.RawMessage(`"20"`), "example.com", "https://www.example.com/pricing", "c", "receipt-2")
		closedAt := testEpoch.Add(time.Hour)
		seedFactAtGone(t, durable, created, closedAt, factID)
		for _, trigger := range []string{"trg_facts_created_provenance", "trg_facts_changed_successor_insert"} {
			poisonFactAtDatabase(t, durable, `DROP TRIGGER `+trigger)
		}
		poisonFactAtDatabase(t, durable, `INSERT INTO facts (
			id, watch_id, subject, predicate, path, value, root, source_url, receipt,
			snapshot_id, created_verification_id, created_claim_index,
			latest_verification_id, latest_claim_index, observed_at, valid_from, last_verified_at
		) VALUES ('`+strings.Repeat("d", 64)+`', '`+created.ID+`', '`+created.Spec.Subject+`',
			'`+created.Spec.Predicate+`', '`+created.Spec.Predicate+`', '"20"', 'example.com',
			'https://www.example.com/pricing', 'receipt-3', 'sha256:`+strings.Repeat("d", 64)+`',
			'fact-gone', 0, 'fact-gone', 0, '`+formatTime(closedAt)+`', '`+formatTime(closedAt)+`',
			'`+formatTime(closedAt)+`')`)
		assertFactAtCorrupt(t, store, created.Spec.Subject, created.Spec.Predicate, testEpoch)
	})
}

func TestDurableFactValidationIsStrictAndBounded(t *testing.T) {
	base := validFactAtRow()
	if !validDurableFact(base, base.Subject, base.Predicate) {
		t.Fatal("validDurableFact rejected a canonical open row")
	}
	for _, raw := range []json.RawMessage{
		json.RawMessage(`"text"`), json.RawMessage(`"<>&"`), json.RawMessage(`"line\n"`),
		json.RawMessage(`true`), json.RawMessage(`false`), json.RawMessage(`0`),
		json.RawMessage(`-1`), json.RawMessage(`0.25`), json.RawMessage(`1000`),
	} {
		value := base
		value.Value = raw
		if !validDurableFact(value, value.Subject, value.Predicate) {
			t.Fatalf("validDurableFact rejected canonical scalar %s", raw)
		}
	}
	for _, raw := range []json.RawMessage{
		json.RawMessage(`"\u0074ext"`), json.RawMessage(`"\/"`), json.RawMessage(`  "text"`),
		json.RawMessage(`1.0`), json.RawMessage(`1e3`), json.RawMessage(`-0`),
		json.RawMessage(`null`), json.RawMessage(`[]`), json.RawMessage(`{}`),
	} {
		value := base
		value.Value = raw
		if validDurableFact(value, value.Subject, value.Predicate) {
			t.Fatalf("validDurableFact accepted noncanonical scalar %s", raw)
		}
	}

	changed := base
	changed.ValidTo = timePointer(base.ValidFrom.Add(time.Hour))
	changed.ClosedVerificationID = "closed-verification"
	changed.ClosedClaimIndex = intPointer(0)
	changed.ClosedOutcome = ledger.OutcomeChanged
	changed.SupersededBy = strings.Repeat("d", 64)
	if !validDurableFact(changed, changed.Subject, changed.Predicate) {
		t.Fatal("validDurableFact rejected a canonical changed row")
	}
	gone := changed
	gone.ClosedOutcome = ledger.OutcomeGone
	gone.GoneScope = ledger.GoneScopePage
	gone.SupersededBy = ""
	if !validDurableFact(gone, gone.Subject, gone.Predicate) {
		t.Fatal("validDurableFact rejected a canonical gone row")
	}

	mutations := map[string]func(*Fact){
		"fact ID":              func(value *Fact) { value.ID = strings.Repeat("A", 64) },
		"watch ID":             func(value *Fact) { value.WatchID = "00000000-0000-5000-8000-000000000001" },
		"subject":              func(value *Fact) { value.Subject = " subject " },
		"predicate":            func(value *Fact) { value.Predicate = "bad predicate" },
		"path control":         func(value *Fact) { value.Path = "bad\npath" },
		"path N+1":             func(value *Fact) { value.Path = strings.Repeat("p", 4097) },
		"value N+1":            func(value *Fact) { value.Value = json.RawMessage(`"` + strings.Repeat("v", maximumFactValue) + `"`) },
		"root":                 func(value *Fact) { value.Root = "attacker.example" },
		"private URL":          func(value *Fact) { value.SourceURL, value.Root = "https://127.0.0.1/", "127.0.0.1" },
		"receipt control":      func(value *Fact) { value.Receipt = "bad\nreceipt" },
		"receipt N+1":          func(value *Fact) { value.Receipt = strings.Repeat("r", maximumFactReceipt+1) },
		"snapshot":             func(value *Fact) { value.SnapshotID = "sha256:" + strings.Repeat("A", 64) },
		"created verification": func(value *Fact) { value.CreatedVerificationID = "bad\nverification" },
		"latest verification":  func(value *Fact) { value.LatestVerificationID = strings.Repeat("v", 513) },
		"created index":        func(value *Fact) { value.CreatedClaimIndex = -1 },
		"latest index":         func(value *Fact) { value.LatestClaimIndex = -1 },
		"observed time":        func(value *Fact) { value.ObservedAt = time.Time{} },
		"valid mismatch":       func(value *Fact) { value.ValidFrom = value.ValidFrom.Add(time.Nanosecond) },
		"verified before":      func(value *Fact) { value.LastVerifiedAt = value.ObservedAt.Add(-time.Nanosecond) },
		"open closure":         func(value *Fact) { value.ClosedOutcome = ledger.OutcomeGone },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			value := cloneFact(base)
			mutate(&value)
			if validDurableFact(value, value.Subject, value.Predicate) {
				t.Fatalf("validDurableFact accepted mutation: %#v", value)
			}
		})
	}

	pathBoundary := base
	pathBoundary.Path = strings.Repeat("p", 4096)
	valueBoundary := base
	valueBoundary.Value = json.RawMessage(`"` + strings.Repeat("v", maximumFactValue-2) + `"`)
	receiptBoundary := base
	receiptBoundary.Receipt = strings.Repeat("r", maximumFactReceipt)
	verificationBoundary := base
	verificationBoundary.CreatedVerificationID = strings.Repeat("v", 512)
	for name, value := range map[string]Fact{
		"path N": pathBoundary, "value N": valueBoundary, "receipt N": receiptBoundary,
		"verification N": verificationBoundary,
	} {
		if !validDurableFact(value, value.Subject, value.Predicate) {
			t.Fatalf("validDurableFact rejected %s boundary", name)
		}
	}
}

func TestFactAtQueryUsesAsOfIndexWithoutTemporarySort(t *testing.T) {
	durable, _ := openWatchStore(t)
	details := make([]string, 0, 2)
	err := durable.View(context.Background(), func(tx ledger.ReadTx) error {
		rows, err := tx.QueryContext(context.Background(), `EXPLAIN QUERY PLAN SELECT `+factColumns+`
			FROM facts
			WHERE subject = ? AND predicate = ? AND valid_from <= ?
				AND (valid_to IS NULL OR ? < valid_to)
			ORDER BY valid_from DESC, id LIMIT 2`, "subject", "predicate", formatTime(testEpoch), formatTime(testEpoch))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				return err
			}
			details = append(details, detail)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(details, "\n")
	if !strings.Contains(plan, "USING INDEX idx_facts_subject_predicate_valid") || strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("FactAt query does not use the committed as-of index:\n%s", plan)
	}
}

func validFactAtRow() Fact {
	observed := testEpoch
	return Fact{
		ID: strings.Repeat("a", 64), WatchID: "00000000-0000-4000-8000-000000000001",
		Subject: "canonical subject", Predicate: "price_per_mtok_input", Path: "price_per_mtok_input",
		Value: json.RawMessage(`"19"`), Root: "example.com", SourceURL: "https://example.com/fact",
		Receipt: "receipt", SnapshotID: "sha256:" + strings.Repeat("a", 64),
		CreatedVerificationID: "created-verification", CreatedClaimIndex: 0,
		LatestVerificationID: "latest-verification", LatestClaimIndex: 0,
		ObservedAt: observed, ValidFrom: observed, LastVerifiedAt: observed,
	}
}

func intPointer(value int) *int { return &value }

type factAtLateCancelContext struct {
	context.Context
	calls    atomic.Int32
	cancelAt int32
}

func (ctx *factAtLateCancelContext) Err() error {
	if ctx.calls.Add(1) >= ctx.cancelAt {
		return context.Canceled
	}
	return nil
}

func openFactAtRelationFixture(t *testing.T, subject string) (*ledger.Store, *Store, Watch) {
	t.Helper()
	clock := &testClock{now: testEpoch}
	ids := &testIDs{}
	durable, store := openWatchStore(t, WithClock(clock.Now), WithIDGenerator(ids.Generate))
	created, ok, err := store.Create(context.Background(), validSpec(subject))
	if err != nil || !ok {
		t.Fatalf("Create() = (%#v,%v,%v)", created, ok, err)
	}
	return durable, store, created
}

func poisonFactAtDatabase(t *testing.T, durable *ledger.Store, statement string) {
	t.Helper()
	if err := durable.Update(context.Background(), func(tx ledger.WriteTx) error {
		_, err := tx.ExecContext(context.Background(), statement)
		return err
	}); err != nil {
		t.Fatalf("poison durable relation: %v", err)
	}
}

func assertFactAtCorrupt(t *testing.T, store *Store, subject, predicate string, asOf time.Time) {
	t.Helper()
	value, found, err := store.FactAt(context.Background(), subject, predicate, asOf)
	if found || !reflect.DeepEqual(value, Fact{}) || !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("FactAt(corrupt relation) = (%#v,%v,%v)", value, found, err)
	}
}

func seedFactAtOpen(
	t *testing.T,
	durable *ledger.Store,
	watch Watch,
	observed time.Time,
	factID string,
	verificationID string,
	value json.RawMessage,
	root string,
	sourceURL string,
	snapshotCharacter string,
	receipt string,
) {
	t.Helper()
	verification := ledger.Verification{
		VerificationID: verificationID, ClaimIndex: 0, URL: sourceURL, FinalURL: sourceURL,
		Path: watch.Spec.Predicate, OldValue: append(json.RawMessage(nil), value...),
		Outcome: ledger.OutcomeConfirmed, OldSnapshotID: "sha256:" + strings.Repeat("0", 64),
		NewSnapshotID: "sha256:" + strings.Repeat(snapshotCharacter, 64), Receipt: receipt,
		VerifiedAt: observed,
	}
	err := durable.RecordVerificationBatchWithHook(context.Background(), []ledger.Verification{verification}, nil,
		func(ctx context.Context, tx ledger.WriteTx, state ledger.VerificationBatchState) error {
			if state.Existing {
				return errors.New("unexpected verification retry")
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO facts (
				id, watch_id, subject, predicate, path, value, root, source_url, receipt,
				snapshot_id, created_verification_id, created_claim_index,
				latest_verification_id, latest_claim_index, observed_at, valid_from,
				last_verified_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				factID, watch.ID, watch.Spec.Subject, watch.Spec.Predicate, watch.Spec.Predicate,
				string(value), root, sourceURL, receipt, verification.NewSnapshotID,
				verificationID, 0, verificationID, 0, formatTime(observed), formatTime(observed),
				formatTime(observed))
			return err
		})
	if err != nil {
		t.Fatalf("seed open fact: %v", err)
	}
}

func seedFactAtConfirmation(t *testing.T, durable *ledger.Store, watch Watch, factID string, at time.Time) {
	t.Helper()
	verification := ledger.Verification{
		VerificationID: "latest-confirmed", ClaimIndex: 0, URL: "https://example.com/fact",
		FinalURL: "https://example.com/fact", Path: watch.Spec.Predicate,
		OldValue: json.RawMessage(`"value"`), Outcome: ledger.OutcomeConfirmed,
		OldSnapshotID: "sha256:" + strings.Repeat("6", 64),
		NewSnapshotID: "sha256:" + strings.Repeat("e", 64),
		OldReceipt:    "receipt-1", Receipt: "receipt-2", VerifiedAt: at,
	}
	err := durable.RecordVerificationBatchWithHook(context.Background(), []ledger.Verification{verification}, nil,
		func(ctx context.Context, tx ledger.WriteTx, _ ledger.VerificationBatchState) error {
			_, err := tx.ExecContext(ctx, `UPDATE facts SET latest_verification_id = ?, latest_claim_index = 0,
				last_verified_at = ?, root = 'example.com', source_url = ?, receipt = ?, snapshot_id = ?
				WHERE id = ? AND valid_to IS NULL`, verification.VerificationID, formatTime(at),
				verification.FinalURL, verification.Receipt, verification.NewSnapshotID, factID)
			return err
		})
	if err != nil {
		t.Fatalf("seed confirmed fact: %v", err)
	}
}

func seedFactAtChange(t *testing.T, durable *ledger.Store, watch Watch, at time.Time, oldID, newID string) {
	t.Helper()
	verification := ledger.Verification{
		VerificationID: "fact-change", ClaimIndex: 0, URL: "https://www.example.com/pricing",
		FinalURL: "https://www.example.com/pricing", Path: watch.Spec.Predicate,
		OldValue: json.RawMessage(`"19"`), NewValue: json.RawMessage(`"20"`), Outcome: ledger.OutcomeChanged,
		OldSnapshotID: "sha256:" + strings.Repeat("a", 64), NewSnapshotID: "sha256:" + strings.Repeat("c", 64),
		OldReceipt: "receipt-1", Receipt: "receipt-2", VerifiedAt: at,
	}
	err := durable.RecordVerificationBatchWithHook(context.Background(), []ledger.Verification{verification}, nil,
		func(ctx context.Context, tx ledger.WriteTx, _ ledger.VerificationBatchState) error {
			if _, err := tx.ExecContext(ctx, `UPDATE facts SET valid_to = ?, closed_verification_id = ?,
				closed_claim_index = 0, superseded_by = ?, closed_outcome = 'changed'
				WHERE id = ? AND valid_to IS NULL`, formatTime(at), verification.VerificationID, newID, oldID); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO facts (
				id, watch_id, subject, predicate, path, value, root, source_url, receipt,
				snapshot_id, created_verification_id, created_claim_index,
				latest_verification_id, latest_claim_index, observed_at, valid_from,
				last_verified_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, 0, ?, ?, ?)`,
				newID, watch.ID, watch.Spec.Subject, watch.Spec.Predicate, watch.Spec.Predicate,
				string(verification.NewValue), "example.com", verification.FinalURL, verification.Receipt,
				verification.NewSnapshotID, verification.VerificationID, verification.VerificationID,
				formatTime(at), formatTime(at), formatTime(at))
			return err
		})
	if err != nil {
		t.Fatalf("seed changed fact: %v", err)
	}
}

func seedFactAtGone(t *testing.T, durable *ledger.Store, watch Watch, at time.Time, factID string) {
	t.Helper()
	verification := ledger.Verification{
		VerificationID: "fact-gone", ClaimIndex: 0, URL: "https://www.example.com/pricing",
		FinalURL: "https://www.example.com/pricing", Path: watch.Spec.Predicate,
		OldValue: json.RawMessage(`"20"`), Outcome: ledger.OutcomeGone, GoneScope: ledger.GoneScopeField,
		OldSnapshotID: "sha256:" + strings.Repeat("c", 64), NewSnapshotID: "sha256:" + strings.Repeat("d", 64),
		OldReceipt: "receipt-2", Receipt: "receipt-3", VerifiedAt: at,
	}
	err := durable.RecordVerificationBatchWithHook(context.Background(), []ledger.Verification{verification}, nil,
		func(ctx context.Context, tx ledger.WriteTx, _ ledger.VerificationBatchState) error {
			_, err := tx.ExecContext(ctx, `UPDATE facts SET valid_to = ?, closed_verification_id = ?,
				closed_claim_index = 0, closed_outcome = 'gone', gone_scope = 'field'
				WHERE id = ? AND valid_to IS NULL`, formatTime(at), verification.VerificationID, factID)
			return err
		})
	if err != nil {
		t.Fatalf("seed gone fact: %v", err)
	}
}
