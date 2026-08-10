package watch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/use-agent/purify/ledger"
)

type recorderFixture struct {
	durable *ledger.Store
	store   *Store
	clock   *testClock
	claim   Claim
	base    VerificationBaseline
}

func newRecorderFixture(t *testing.T, predicate string) recorderFixture {
	t.Helper()
	clock := &testClock{now: testEpoch}
	ids := &testIDs{}
	durable, store := openWatchStore(t, WithClock(clock.Now), WithIDGenerator(ids.Generate))
	spec := validSpec("recorder subject")
	spec.Predicate = predicate
	created, made, err := store.Create(context.Background(), spec)
	if err != nil || !made {
		t.Fatalf("Create() = (%#v,%v,%v)", created, made, err)
	}
	claim, found, err := store.ClaimDue(context.Background())
	if err != nil || !found {
		t.Fatalf("ClaimDue() = (%#v,%v,%v)", claim, found, err)
	}
	clock.Set(claim.Watch.UpdatedAt.Add(time.Second))
	return recorderFixture{
		durable: durable,
		store:   store,
		clock:   clock,
		claim:   claim,
		base: VerificationBaseline{
			Path:       escapedPredicatePath(predicate),
			Value:      json.RawMessage(`"19"`),
			SourceURL:  "https://www.example.com/pricing",
			SnapshotID: recorderSnapshot("0"),
			Receipt:    "old-receipt-token",
		},
	}
}

func recorderSnapshot(character string) string {
	return "sha256:" + strings.Repeat(character, 64)
}

func (fixture recorderFixture) verification(id string, outcome ledger.Outcome) ledger.Verification {
	row := ledger.Verification{
		VerificationID: id,
		ClaimIndex:     0,
		URL:            fixture.base.SourceURL,
		FinalURL:       fixture.base.SourceURL,
		Path:           fixture.base.Path,
		OldValue:       append(json.RawMessage(nil), fixture.base.Value...),
		Outcome:        outcome,
		OldSnapshotID:  fixture.base.SnapshotID,
		NewSnapshotID:  recorderSnapshot("a"),
		OldReceipt:     fixture.base.Receipt,
		VerifiedAt:     fixture.clock.Now(),
	}
	switch outcome {
	case ledger.OutcomeConfirmed:
		row.Receipt = "refreshed-receipt-token"
	case ledger.OutcomeChanged:
		row.NewValue = json.RawMessage(`"20"`)
		row.Receipt = "changed-receipt-token"
	case ledger.OutcomeGone:
		row.GoneScope = ledger.GoneScopeField
	}
	return row
}

func recorderFacts(t *testing.T, durable *ledger.Store, watchID string) []Fact {
	t.Helper()
	values := []Fact{}
	err := durable.View(context.Background(), func(tx ledger.ReadTx) error {
		rows, err := tx.QueryContext(context.Background(), `SELECT `+factColumns+`
			FROM facts WHERE watch_id = ? ORDER BY valid_from, id`, watchID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			value, err := scanFact(rows)
			if err != nil {
				return err
			}
			values = append(values, cloneFact(value))
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("read facts: %v", err)
	}
	return values
}

func recorderRowCount(t *testing.T, durable *ledger.Store, table string) int {
	t.Helper()
	var count int
	err := durable.View(context.Background(), func(tx ledger.ReadTx) error {
		return tx.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

func recorderLease(t *testing.T, durable *ledger.Store, watchID string) (string, string) {
	t.Helper()
	var id, until sqlNullString
	err := durable.View(context.Background(), func(tx ledger.ReadTx) error {
		return tx.QueryRowContext(context.Background(),
			`SELECT lease_id, lease_until FROM watches WHERE id = ?`, watchID).Scan(&id, &until)
	})
	if err != nil {
		t.Fatalf("read lease: %v", err)
	}
	return id.String, until.String
}

// sqlNullString is the minimal Scanner needed by recorderLease without
// exposing database/sql values to the rest of the test helpers.
type sqlNullString struct {
	String string
	Valid  bool
}

func (value *sqlNullString) Scan(source any) error {
	if source == nil {
		value.String, value.Valid = "", false
		return nil
	}
	switch typed := source.(type) {
	case string:
		value.String, value.Valid = typed, true
	case []byte:
		value.String, value.Valid = string(typed), true
	default:
		return fmt.Errorf("unexpected SQL string %T", source)
	}
	return nil
}

func materializeRecorderBaseline(t *testing.T, fixture recorderFixture) VerificationRecordResult {
	t.Helper()
	recorder, err := NewBootstrapVerificationRecorder(fixture.store, fixture.claim, fixture.base)
	if err != nil {
		t.Fatal(err)
	}
	row := fixture.verification("bootstrap-confirmed", ledger.OutcomeConfirmed)
	if err := recorder.RecordVerificationBatch(context.Background(), []ledger.Verification{row}, nil); err != nil {
		t.Fatalf("RecordVerificationBatch() error = %v", err)
	}
	result, ok := recorder.Result()
	if !ok || result.Fact == nil {
		t.Fatalf("Result() = (%#v,%v)", result, ok)
	}
	return result
}

func claimExistingRecorderFact(t *testing.T, fixture *recorderFixture) Claim {
	t.Helper()
	watch, err := fixture.store.Get(context.Background(), fixture.claim.Watch.ID)
	if err != nil || watch.NextCheckAt == nil {
		t.Fatalf("Get() = (%#v,%v)", watch, err)
	}
	fixture.clock.Set(*watch.NextCheckAt)
	claim, found, err := fixture.store.ClaimDue(context.Background())
	if err != nil || !found || claim.Fact == nil || claim.Watch.State != StateActive {
		t.Fatalf("ClaimDue(active) = (%#v,%v,%v)", claim, found, err)
	}
	fixture.clock.Set(claim.Watch.UpdatedAt.Add(time.Second))
	fixture.claim = claim
	fixture.base = VerificationBaseline{
		Path: claim.Fact.Path, Value: append(json.RawMessage(nil), claim.Fact.Value...),
		SourceURL: claim.Fact.SourceURL, SnapshotID: claim.Fact.SnapshotID,
		Receipt: claim.Fact.Receipt,
	}
	return claim
}

func TestBootstrapConfirmedMaterializesFactAndScheduleAtomically(t *testing.T) {
	fixture := newRecorderFixture(t, `price.v1`)
	recorder, err := NewBootstrapVerificationRecorder(fixture.store, fixture.claim, fixture.base)
	if err != nil {
		t.Fatal(err)
	}
	row := fixture.verification("verification-bootstrap-confirmed", ledger.OutcomeConfirmed)
	row.FinalURL = "https://shop.example.co.uk/current"
	if err := recorder.RecordVerificationBatch(context.Background(), []ledger.Verification{row}, nil); err != nil {
		t.Fatalf("RecordVerificationBatch() error = %v", err)
	}
	result, ok := recorder.Result()
	if !ok || !result.LeaseReleased || result.Retry != nil || result.Fact == nil ||
		result.Outcome != ledger.OutcomeConfirmed {
		t.Fatalf("Result() = (%#v,%v)", result, ok)
	}
	wantID := deterministicFactID(fixture.claim.Watch.ID, row.VerificationID, 0, row.Path)
	if result.Fact.ID != wantID || result.Fact.Root != "example.co.uk" ||
		result.Fact.SourceURL != row.FinalURL || string(result.Fact.Value) != `"19"` ||
		result.Fact.Receipt != row.Receipt || result.Fact.SnapshotID != row.NewSnapshotID {
		t.Fatalf("materialized fact = %#v", result.Fact)
	}
	stored, err := fixture.store.Get(context.Background(), fixture.claim.Watch.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantNext := row.VerifiedAt.Add(7 * 24 * time.Hour / 2)
	if stored.State != StateActive || stored.LastCheckedAt == nil || !stored.LastCheckedAt.Equal(row.VerifiedAt) ||
		stored.LastChangeAt == nil || !stored.LastChangeAt.Equal(row.VerifiedAt) ||
		stored.NextCheckAt == nil || !stored.NextCheckAt.Equal(wantNext) ||
		stored.EWMAInterval != 7*24*time.Hour || stored.LastVerificationID != row.VerificationID ||
		stored.LastVerificationClaimIndex == nil || *stored.LastVerificationClaimIndex != 0 ||
		stored.ConsecutiveFailures != 0 || stored.LastErrorCode != "" {
		t.Fatalf("scheduled watch = %#v", stored)
	}
	leaseID, leaseUntil := recorderLease(t, fixture.durable, fixture.claim.Watch.ID)
	if leaseID != "" || leaseUntil != "" || recorderRowCount(t, fixture.durable, "verifications") != 1 {
		t.Fatalf("lease/count = %q/%q/%d", leaseID, leaseUntil, recorderRowCount(t, fixture.durable, "verifications"))
	}

	// Returned mutable values are isolated from both recorder state and durable state.
	result.Fact.Value[0] = 'x'
	again, ok := recorder.Result()
	if !ok || again.Fact == nil || string(again.Fact.Value) != `"19"` {
		t.Fatalf("Result() retained caller mutation: (%#v,%v)", again, ok)
	}
}

func TestBootstrapChangedSupportsExactRetryThenConfirmedPromotion(t *testing.T) {
	fixture := newRecorderFixture(t, "price")
	recorder, err := NewBootstrapVerificationRecorder(fixture.store, fixture.claim, fixture.base)
	if err != nil {
		t.Fatal(err)
	}
	changed := fixture.verification("verification-bootstrap-changed", ledger.OutcomeChanged)
	changed.FinalURL = "https://pricing.example.net/new"
	if err := recorder.RecordVerificationBatch(context.Background(), []ledger.Verification{changed}, nil); err != nil {
		t.Fatal(err)
	}
	first, ok := recorder.Result()
	if !ok || first.Retry == nil || first.Fact != nil || first.LeaseReleased ||
		string(first.Retry.Value) != `"20"` || first.Retry.SourceURL != changed.FinalURL ||
		first.Retry.SnapshotID != changed.NewSnapshotID || first.Retry.Receipt != changed.Receipt {
		t.Fatalf("changed result = (%#v,%v)", first, ok)
	}
	if facts := recorderFacts(t, fixture.durable, fixture.claim.Watch.ID); len(facts) != 0 {
		t.Fatalf("bootstrap changed wrote facts: %#v", facts)
	}
	leaseID, leaseUntil := recorderLease(t, fixture.durable, fixture.claim.Watch.ID)
	if leaseID != fixture.claim.Lease.ID || leaseUntil != formatTime(fixture.claim.Lease.Until) {
		t.Fatalf("bootstrap changed lease = %q/%q", leaseID, leaseUntil)
	}

	// ledger Existing=true re-runs the hook and reconstructs the same retry.
	if err := recorder.RecordVerificationBatch(context.Background(), []ledger.Verification{changed}, nil); err != nil {
		t.Fatalf("exact changed retry: %v", err)
	}
	retried, ok := recorder.Result()
	if !ok || !reflect.DeepEqual(first, retried) || recorderRowCount(t, fixture.durable, "verifications") != 1 {
		t.Fatalf("exact retry = (%#v,%v), first=%#v", retried, ok, first)
	}

	second, err := NewBootstrapVerificationRecorder(fixture.store, fixture.claim, *first.Retry)
	if err != nil {
		t.Fatal(err)
	}
	fixture.base = *first.Retry
	confirmed := fixture.verification("verification-bootstrap-stable", ledger.OutcomeConfirmed)
	confirmed.VerifiedAt = changed.VerifiedAt.Add(time.Second)
	fixture.clock.Set(confirmed.VerifiedAt)
	if err := second.RecordVerificationBatch(context.Background(), []ledger.Verification{confirmed}, nil); err != nil {
		t.Fatalf("second formal verification: %v", err)
	}
	result, ok := second.Result()
	if !ok || result.Fact == nil || string(result.Fact.Value) != `"20"` || !result.LeaseReleased {
		t.Fatalf("promoted result = (%#v,%v)", result, ok)
	}
}

func TestBootstrapGoneRetainsPendingLeaseWithoutFact(t *testing.T) {
	fixture := newRecorderFixture(t, "availability")
	recorder, err := NewBootstrapVerificationRecorder(fixture.store, fixture.claim, fixture.base)
	if err != nil {
		t.Fatal(err)
	}
	row := fixture.verification("verification-bootstrap-gone", ledger.OutcomeGone)
	row.GoneScope = ledger.GoneScopePage
	if err := recorder.RecordVerificationBatch(context.Background(), []ledger.Verification{row}, nil); err != nil {
		t.Fatal(err)
	}
	result, ok := recorder.Result()
	if !ok || result.Fact != nil || result.Retry != nil || result.LeaseReleased ||
		result.Outcome != ledger.OutcomeGone || result.GoneScope != ledger.GoneScopePage {
		t.Fatalf("gone result = (%#v,%v)", result, ok)
	}
	watch, err := fixture.store.Get(context.Background(), fixture.claim.Watch.ID)
	leaseID, _ := recorderLease(t, fixture.durable, fixture.claim.Watch.ID)
	if err != nil || watch.State != StatePending || watch.LastCheckedAt != nil ||
		leaseID != fixture.claim.Lease.ID || len(recorderFacts(t, fixture.durable, watch.ID)) != 0 {
		t.Fatalf("bootstrap gone state = watch:%#v lease:%q err:%v", watch, leaseID, err)
	}
}

func TestExistingFactConfirmedChangedAndGone(t *testing.T) {
	for _, outcome := range []ledger.Outcome{ledger.OutcomeConfirmed, ledger.OutcomeChanged, ledger.OutcomeGone} {
		t.Run(string(outcome), func(t *testing.T) {
			fixture := newRecorderFixture(t, "price")
			initial := materializeRecorderBaseline(t, fixture)
			original := cloneFact(*initial.Fact)
			claimExistingRecorderFact(t, &fixture)
			recorder, err := NewFactVerificationRecorder(fixture.store, fixture.claim)
			if err != nil {
				t.Fatal(err)
			}
			row := fixture.verification("verification-existing-"+string(outcome), outcome)
			row.FinalURL = "https://cdn.example.net/pricing"
			if outcome == ledger.OutcomeGone {
				row.FinalURL = row.URL
			}
			if err := recorder.RecordVerificationBatch(context.Background(), []ledger.Verification{row}, nil); err != nil {
				t.Fatalf("RecordVerificationBatch() error = %v", err)
			}
			result, ok := recorder.Result()
			if !ok || result.Fact == nil || !result.LeaseReleased || result.Outcome != outcome {
				t.Fatalf("Result() = (%#v,%v)", result, ok)
			}
			facts := recorderFacts(t, fixture.durable, fixture.claim.Watch.ID)
			watch, err := fixture.store.Get(context.Background(), fixture.claim.Watch.ID)
			if err != nil || watch.State != StateActive || watch.LastCheckedAt == nil ||
				!watch.LastCheckedAt.Equal(row.VerifiedAt) || watch.ConsecutiveFailures != 0 {
				t.Fatalf("watch after %s = (%#v,%v)", outcome, watch, err)
			}

			switch outcome {
			case ledger.OutcomeConfirmed:
				if len(facts) != 1 || facts[0].ID != original.ID || facts[0].Root != "example.net" ||
					facts[0].LatestVerificationID != row.VerificationID || facts[0].Receipt != row.Receipt ||
					facts[0].ValidTo != nil || watch.LastChangeAt == nil ||
					!watch.LastChangeAt.Equal(original.ObservedAt) || watch.NextCheckAt == nil ||
					!watch.NextCheckAt.Equal(row.VerifiedAt.Add(7*24*time.Hour)) {
					t.Fatalf("confirmed facts/watch = %#v / %#v", facts, watch)
				}
			case ledger.OutcomeChanged:
				if len(facts) != 2 || facts[0].ID != original.ID || facts[0].ValidTo == nil ||
					!facts[0].ValidTo.Equal(row.VerifiedAt) || facts[0].ClosedOutcome != ledger.OutcomeChanged ||
					facts[0].SupersededBy != facts[1].ID || facts[1].ID != result.Fact.ID ||
					string(facts[1].Value) != `"20"` || facts[1].Root != "example.net" ||
					watch.LastChangeAt == nil || !watch.LastChangeAt.Equal(row.VerifiedAt) {
					t.Fatalf("changed facts/watch = %#v / %#v", facts, watch)
				}
				observed := row.VerifiedAt.Sub(original.ObservedAt)
				wantEWMA, _ := weightedEWMA(observed, 7*24*time.Hour)
				if watch.EWMAInterval != wantEWMA || watch.NextCheckAt == nil ||
					!watch.NextCheckAt.Equal(row.VerifiedAt.Add(clampScheduleInterval(wantEWMA/2))) {
					t.Fatalf("changed schedule = %#v want ewma %s", watch, wantEWMA)
				}
			case ledger.OutcomeGone:
				if len(facts) != 1 || facts[0].ID != original.ID || facts[0].ValidTo == nil ||
					facts[0].ClosedOutcome != ledger.OutcomeGone || facts[0].GoneScope != ledger.GoneScopeField ||
					facts[0].SupersededBy != "" || result.Fact.ValidTo == nil {
					t.Fatalf("gone fact = %#v / %#v", facts, result.Fact)
				}
			}
		})
	}
}

func TestCommittedTerminalVerificationExactRetryAcrossRecorders(t *testing.T) {
	fixture := newRecorderFixture(t, "price")
	recorder, err := NewBootstrapVerificationRecorder(fixture.store, fixture.claim, fixture.base)
	if err != nil {
		t.Fatal(err)
	}
	row := fixture.verification("verification-response-lost", ledger.OutcomeConfirmed)
	if err := recorder.RecordVerificationBatch(context.Background(), []ledger.Verification{row}, nil); err != nil {
		t.Fatal(err)
	}
	want, _ := recorder.Result()

	// Same in-memory recorder reconstructs from committed rows after lease clear.
	if err := recorder.RecordVerificationBatch(context.Background(), []ledger.Verification{row}, nil); err != nil {
		t.Fatalf("same-recorder exact retry: %v", err)
	}
	got, ok := recorder.Result()
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("same-recorder retry = (%#v,%v), want %#v", got, ok, want)
	}

	// A fresh Store and recorder can recover the same result after a response loss.
	secondDurable, err := ledger.Open(filepath.Dir(fixture.durable.Path()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondDurable.Close() })
	secondStore, err := NewStore(secondDurable, WithClock(fixture.clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := NewBootstrapVerificationRecorder(secondStore, fixture.claim, fixture.base)
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.RecordVerificationBatch(context.Background(), []ledger.Verification{row}, nil); err != nil {
		t.Fatalf("cross-store exact retry: %v", err)
	}
	cross, ok := fresh.Result()
	if !ok || !reflect.DeepEqual(cross, want) || recorderRowCount(t, fixture.durable, "facts") != 1 {
		t.Fatalf("cross-store retry = (%#v,%v), want %#v", cross, ok, want)
	}
}

func TestDeterministicFactIDIsFramedAndDomainSeparated(t *testing.T) {
	base := deterministicFactID("watch", "verification", 12, `price\.v1`)
	if len(base) != 64 || base != deterministicFactID("watch", "verification", 12, `price\.v1`) ||
		!validHex(base, 64) {
		t.Fatalf("fact ID is not stable lowercase SHA-256: %q", base)
	}
	for name, value := range map[string]string{
		"watch":        deterministicFactID("watch-2", "verification", 12, `price\.v1`),
		"verification": deterministicFactID("watch", "verification-2", 12, `price\.v1`),
		"index":        deterministicFactID("watch", "verification", 13, `price\.v1`),
		"path":         deterministicFactID("watch", "verification", 12, `price\.v2`),
	} {
		if value == base {
			t.Fatalf("%s did not affect fact ID", name)
		}
	}
	// These tuples collide under delimiter-free concatenation but not framing.
	left := deterministicFactID("ab", "c", 1, "d")
	right := deterministicFactID("a", "bc", 1, "d")
	if left == right {
		t.Fatalf("framing collision: %q", left)
	}
}

func TestWeightedEWMABoundsAndIntegerRounding(t *testing.T) {
	for _, test := range []struct {
		name     string
		observed time.Duration
		old      time.Duration
		want     time.Duration
		wantErr  bool
	}{
		{name: "non-positive", observed: 0, old: time.Hour, wantErr: true},
		{name: "old below bound", observed: time.Hour, old: 10*time.Minute - 1, wantErr: true},
		{name: "minimum clamp", observed: time.Nanosecond, old: 10 * time.Minute, want: 10 * time.Minute},
		{name: "integer floor", observed: 11*time.Minute + 9, old: 13*time.Minute + 7,
			want: (3*(11*time.Minute+9) + 7*(13*time.Minute+7)) / 10},
		{name: "maximum saturation", observed: time.Duration(1<<63 - 1), old: 7 * 24 * time.Hour, want: 7 * 24 * time.Hour},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := weightedEWMA(test.observed, test.old)
			if test.wantErr {
				if !errors.Is(err, ErrVerificationMismatch) {
					t.Fatalf("weightedEWMA() error = %v", err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("weightedEWMA() = (%s,%v), want %s", got, err, test.want)
			}
		})
	}
}
