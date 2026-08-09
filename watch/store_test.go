package watch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/models"
)

var testEpoch = time.Date(2026, 8, 10, 1, 2, 3, 4, time.UTC)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *testClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *testClock) Set(value time.Time) {
	clock.mu.Lock()
	clock.now = value
	clock.mu.Unlock()
}

type testIDs struct {
	next  atomic.Uint64
	calls atomic.Uint64
}

func (ids *testIDs) Generate() (string, error) {
	ids.calls.Add(1)
	value := ids.next.Add(1)
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", value), nil
}

func openWatchStore(t *testing.T, options ...StoreOption) (*ledger.Store, *Store) {
	t.Helper()
	durable, err := ledger.Open(t.TempDir())
	if err != nil {
		t.Fatalf("ledger.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = durable.Close() })
	store, err := NewStore(durable, options...)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	return durable, store
}

func validSpec(subject string) models.FactSpec {
	return models.FactSpec{
		Subject: subject, Predicate: "price_per_mtok_input", Freshness: "week",
		MinIndependentSources: 2, OnConflict: models.FactConflictExpose,
	}
}

func TestNewStoreAndBoundaryErrorsAreStableAndRedacted(t *testing.T) {
	if _, err := NewStore(nil); !errors.Is(err, ErrInvalidStore) {
		t.Fatalf("NewStore(nil) error = %v", err)
	}
	durable, err := ledger.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, option := range map[string]StoreOption{
		"nil option": nil,
		"nil clock":  WithClock(nil),
		"nil ids":    WithIDGenerator(nil),
		"zero lease": WithLeaseDuration(0),
		"long lease": WithLeaseDuration(DefaultLeaseDuration + time.Nanosecond),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewStore(durable, option); !errors.Is(err, ErrInvalidStore) {
				t.Fatalf("NewStore() error = %v", err)
			}
		})
	}
	for name, option := range map[string]StoreOption{
		"custom clears clock": func(config *storeConfig) error { config.clock = nil; return nil },
		"custom clears ids":   func(config *storeConfig) error { config.idGenerator = nil; return nil },
		"custom bad lease":    func(config *storeConfig) error { config.leaseDuration = 0; return nil },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewStore(durable, option); !errors.Is(err, ErrInvalidStore) {
				t.Fatalf("NewStore(custom option) error = %v", err)
			}
		})
	}

	var nilStore *Store
	if _, _, err := nilStore.Create(context.Background(), validSpec("nil")); !errors.Is(err, ErrInvalidStore) {
		t.Fatalf("nil Store.Create() error = %v", err)
	}
	store, err := NewStore(durable, WithIDGenerator(func() (string, error) {
		return "", errors.New("secret-generator-detail")
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Create(context.Background(), validSpec("redacted")); !errors.Is(err, ErrInvalidStore) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("generator error = %q", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := store.Create(canceled, validSpec("canceled")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Create(canceled) error = %v", err)
	}
	if _, _, err := store.Create(nil, validSpec("nil context")); !errors.Is(err, ErrInvalidStore) {
		t.Fatalf("Create(nil context) error = %v", err)
	}

	if err := durable.Close(); err != nil {
		t.Fatal(err)
	}
	closed, err := NewStore(durable)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := closed.List(context.Background(), WatchListOptions{}); !errors.Is(err, ledger.ErrClosed) {
		t.Fatalf("List(closed ledger) error = %v", err)
	}
}

func TestWatchVerificationIdentityIsBoundedAndControlFreeOnRead(t *testing.T) {
	for _, test := range []struct {
		name         string
		verification string
		ignoreChecks bool
		wantCorrupt  bool
	}{
		{name: "512 bytes", verification: strings.Repeat("v", 512)},
		{name: "513 bytes", verification: strings.Repeat("v", 513), ignoreChecks: true, wantCorrupt: true},
		{name: "newline", verification: "verification\nidentity", wantCorrupt: true},
		{name: "nul", verification: "verification\x00identity", wantCorrupt: true},
		{name: "invalid utf8", verification: "verification-\xff", wantCorrupt: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			clock := &testClock{now: testEpoch}
			ids := &testIDs{}
			durable, store := openWatchStore(t, WithClock(clock.Now), WithIDGenerator(ids.Generate))
			created, _, err := store.Create(context.Background(), validSpec("verification identity"))
			if err != nil {
				t.Fatal(err)
			}
			at := testEpoch.Add(time.Second)
			seedVerification(t, durable, test.verification, at)
			err = durable.Update(context.Background(), func(tx ledger.WriteTx) error {
				if test.ignoreChecks {
					if _, err := tx.ExecContext(context.Background(), `PRAGMA ignore_check_constraints = ON`); err != nil {
						return err
					}
				}
				_, err := tx.ExecContext(context.Background(), `UPDATE watches SET
					last_verification_id = ?, last_verification_claim_index = 0,
					last_checked_at = ?, updated_at = ? WHERE id = ?`,
					test.verification, formatTime(at), formatTime(at), created.ID)
				return err
			})
			if err != nil {
				t.Fatalf("seed linked verification identity: %v", err)
			}
			loaded, err := store.Get(context.Background(), created.ID)
			if test.wantCorrupt {
				if !errors.Is(err, ErrCorruptStore) {
					t.Fatalf("Get(corrupt identity) = (%#v,%v)", loaded, err)
				}
			} else if err != nil || loaded.LastVerificationID != test.verification {
				t.Fatalf("Get(boundary identity) = (%#v,%v)", loaded, err)
			}
		})
	}
}

func TestStoreCanonicalizesClockAndReopensWithIdenticalTimes(t *testing.T) {
	directory := t.TempDir()
	durable, err := ledger.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	clockValue := time.Now().In(time.FixedZone("caller-zone", 9*60*60))
	ids := &testIDs{}
	store, err := NewStore(durable, WithClock(func() time.Time { return clockValue }), WithIDGenerator(ids.Generate))
	if err != nil {
		t.Fatal(err)
	}
	created, _, err := store.Create(context.Background(), validSpec("canonical clock"))
	if err != nil {
		t.Fatal(err)
	}
	if created.CreatedAt.Location() != time.UTC || created.CreatedAt != created.CreatedAt.Round(0) {
		t.Fatalf("Create returned non-canonical time %#v", created.CreatedAt)
	}
	if _, err := created.CreatedAt.MarshalJSON(); err != nil {
		t.Fatalf("Create returned non-JSON time: %v", err)
	}
	if err := durable.Close(); err != nil {
		t.Fatal(err)
	}
	reopenedLedger, err := ledger.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopenedLedger.Close() })
	reopened, err := NewStore(reopenedLedger)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := reopened.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CreatedAt != created.CreatedAt || loaded.UpdatedAt != created.UpdatedAt ||
		loaded.NextCheckAt == nil || created.NextCheckAt == nil || *loaded.NextCheckAt != *created.NextCheckAt {
		t.Fatalf("reopened times differ: created=%#v loaded=%#v", created, loaded)
	}

	badClock, err := NewStore(reopenedLedger, WithClock(func() time.Time { return time.Time{} }))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := badClock.Create(context.Background(), validSpec("bad clock")); !errors.Is(err, ErrInvalidStore) {
		t.Fatalf("Create(zero clock) error = %v", err)
	}
}

func TestCancellationDuringIDGenerationIsPreservedAndRollsBack(t *testing.T) {
	clock := &testClock{now: testEpoch}
	ctx, cancel := context.WithCancel(context.Background())
	_, store := openWatchStore(t, WithClock(clock.Now), WithIDGenerator(func() (string, error) {
		cancel()
		return "00000000-0000-4000-8000-000000000001", nil
	}))
	if _, created, err := store.Create(ctx, validSpec("cancel during create")); created || !errors.Is(err, context.Canceled) {
		t.Fatalf("Create(mid-operation cancel) = (created=%v,error=%v)", created, err)
	}
	page, err := store.List(context.Background(), WatchListOptions{})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("canceled Create persisted a row: %#v, %v", page, err)
	}
}

func TestCreateNormalizesHashesAndIsIdempotent(t *testing.T) {
	clock := &testClock{now: testEpoch}
	ids := &testIDs{}
	_, store := openWatchStore(t, WithClock(clock.Now), WithIDGenerator(ids.Generate))
	spec := models.FactSpec{
		Subject: "  anthropic   claude-fable-5  ", Predicate: "price_per_mtok_input",
		Freshness: " 7D ",
	}
	created, wasCreated, err := store.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if !wasCreated || created.ID != "00000000-0000-4000-8000-000000000001" ||
		created.Spec.Subject != "anthropic claude-fable-5" || created.Spec.Freshness != "week" ||
		created.Spec.MinIndependentSources != 2 || created.Spec.OnConflict != models.FactConflictExpose ||
		created.State != StatePending || created.NextCheckAt == nil || !created.NextCheckAt.Equal(testEpoch) ||
		created.EWMAInterval != 7*24*time.Hour || !created.CreatedAt.Equal(testEpoch) ||
		!created.UpdatedAt.Equal(testEpoch) {
		t.Fatalf("created watch = %#v", created)
	}

	// Returned pointer fields are not retained by the store.
	*created.NextCheckAt = time.Time{}
	created.Spec.Subject = "mutated"
	loaded, err := store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Spec.Subject != "anthropic claude-fable-5" || loaded.NextCheckAt == nil || !loaded.NextCheckAt.Equal(testEpoch) {
		t.Fatalf("Get() observed caller mutation: %#v", loaded)
	}

	retry, wasCreated, err := store.Create(context.Background(), validSpec("anthropic claude-fable-5"))
	if err != nil {
		t.Fatal(err)
	}
	if wasCreated || retry.ID != loaded.ID || ids.calls.Load() != 1 {
		t.Fatalf("idempotent Create() = (%q,%v), generator calls=%d", retry.ID, wasCreated, ids.calls.Load())
	}

	left, _, err := normalizeFactSpec(validSpec("a"))
	if err != nil {
		t.Fatal(err)
	}
	left.Predicate = "bc"
	right, _, err := normalizeFactSpec(validSpec("ab"))
	if err != nil {
		t.Fatal(err)
	}
	right.Predicate = "c"
	if factSpecHash(left) == factSpecHash(right) {
		t.Fatal("framed fact specification hash has a concatenation collision")
	}
}

func TestCreateRejectsInvalidSpecsAndIDsWithoutEchoingInput(t *testing.T) {
	clock := &testClock{now: testEpoch}
	_, invalidIDStore := openWatchStore(t, WithClock(clock.Now), WithIDGenerator(func() (string, error) {
		return "00000000-0000-5000-8000-000000000001", nil
	}))
	if _, _, err := invalidIDStore.Create(context.Background(), validSpec("uuid")); !errors.Is(err, ErrInvalidStore) {
		t.Fatalf("invalid generated UUID error = %v", err)
	}

	_, store := openWatchStore(t, WithClock(clock.Now))
	tooLongSubject := strings.Repeat("x", models.MaxAnswerSubjectBytes+1)
	tooLongPredicate := strings.Repeat("p", models.MaxAnswerPredicateBytes+1)
	for name, spec := range map[string]models.FactSpec{
		"empty subject":        validSpec(""),
		"control subject":      validSpec("private\nsubject"),
		"long subject":         validSpec(tooLongSubject),
		"empty predicate":      {Subject: "subject", Freshness: "week", MinIndependentSources: 2, OnConflict: models.FactConflictExpose},
		"space predicate":      {Subject: "subject", Predicate: "bad predicate", Freshness: "week", MinIndependentSources: 2, OnConflict: models.FactConflictExpose},
		"long predicate":       {Subject: "subject", Predicate: tooLongPredicate, Freshness: "week", MinIndependentSources: 2, OnConflict: models.FactConflictExpose},
		"any freshness":        {Subject: "subject", Predicate: "price", Freshness: " ", MinIndependentSources: 2, OnConflict: models.FactConflictExpose},
		"invalid freshness":    {Subject: "subject", Predicate: "price", Freshness: "hour", MinIndependentSources: 2, OnConflict: models.FactConflictExpose},
		"too many roots":       {Subject: "subject", Predicate: "price", Freshness: "week", MinIndependentSources: 9, OnConflict: models.FactConflictExpose},
		"unsupported conflict": {Subject: "subject", Predicate: "price", Freshness: "week", MinIndependentSources: 2, OnConflict: "hide"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := store.Create(context.Background(), spec)
			if !errors.Is(err, ErrInvalidWatchSpec) {
				t.Fatalf("Create() error = %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "private") {
				t.Fatalf("error echoed input: %q", err)
			}
		})
	}
	boundary := validSpec("freshness boundary")
	boundary.Freshness = "week" + strings.Repeat(" ", models.MaxAnswerFreshnessBytes-len("week"))
	normalized, _, err := normalizeFactSpec(boundary)
	if err != nil || normalized.Freshness != "week" {
		t.Fatalf("normalize freshness at %d bytes = (%#v,%v)", models.MaxAnswerFreshnessBytes, normalized, err)
	}
	boundary.Freshness += " "
	if _, _, err := normalizeFactSpec(boundary); !errors.Is(err, ErrInvalidWatchSpec) {
		t.Fatalf("normalize freshness at %d bytes error = %v", models.MaxAnswerFreshnessBytes+1, err)
	}
	boundary.Freshness = strings.Repeat("W", 1<<20)
	if _, _, err := normalizeFactSpec(boundary); !errors.Is(err, ErrInvalidWatchSpec) {
		t.Fatalf("normalize oversized freshness error = %v", err)
	}
}

func TestConcurrentCreateIsExactlyIdempotent(t *testing.T) {
	clock := &testClock{now: testEpoch}
	ids := &testIDs{}
	_, store := openWatchStore(t, WithClock(clock.Now), WithIDGenerator(ids.Generate))
	const callers = 32
	type result struct {
		watch   Watch
		created bool
		err     error
	}
	results := make(chan result, callers)
	var group sync.WaitGroup
	for index := 0; index < callers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			watch, created, err := store.Create(context.Background(), validSpec("concurrent subject"))
			results <- result{watch: watch, created: created, err: err}
		}()
	}
	group.Wait()
	close(results)
	createdCount := 0
	id := ""
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent Create() error = %v", result.err)
		}
		if result.created {
			createdCount++
		}
		if id == "" {
			id = result.watch.ID
		} else if result.watch.ID != id {
			t.Fatalf("Create IDs differ: %q and %q", id, result.watch.ID)
		}
	}
	if createdCount != 1 || ids.calls.Load() != 1 {
		t.Fatalf("created=%d generator calls=%d", createdCount, ids.calls.Load())
	}
}

func TestConcurrentCreateAcrossLedgerConnectionsHasOneWinner(t *testing.T) {
	directory := t.TempDir()
	firstLedger, err := ledger.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer firstLedger.Close()
	secondLedger, err := ledger.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer secondLedger.Close()
	clock := &testClock{now: testEpoch}
	ids := &testIDs{}
	first, err := NewStore(firstLedger, WithClock(clock.Now), WithIDGenerator(ids.Generate))
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewStore(secondLedger, WithClock(clock.Now), WithIDGenerator(ids.Generate))
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		watch   Watch
		created bool
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, store := range []*Store{first, second} {
		go func(store *Store) {
			<-start
			value, created, err := store.Create(context.Background(), validSpec("cross connection create"))
			results <- result{watch: value, created: created, err: err}
		}(store)
	}
	close(start)
	left, right := <-results, <-results
	if left.err != nil || right.err != nil || left.watch.ID == "" || left.watch.ID != right.watch.ID {
		t.Fatalf("cross-connection Create results = %#v / %#v", left, right)
	}
	if left.created == right.created || ids.calls.Load() != 1 {
		t.Fatalf("created flags = %v/%v, generator calls=%d", left.created, right.created, ids.calls.Load())
	}
}

func TestListUsesStableCursorAndExcludesDeleted(t *testing.T) {
	clock := &testClock{now: testEpoch}
	ids := &testIDs{}
	_, store := openWatchStore(t, WithClock(clock.Now), WithIDGenerator(ids.Generate))
	created := make([]Watch, 5)
	for index := range created {
		value, ok, err := store.Create(context.Background(), validSpec(fmt.Sprintf("subject %d", index)))
		if err != nil || !ok {
			t.Fatalf("Create(%d) = (%v,%v)", index, ok, err)
		}
		created[index] = value
	}
	if err := store.Delete(context.Background(), created[2].ID); err != nil {
		t.Fatal(err)
	}

	first, err := store.List(context.Background(), WatchListOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 2 || first.Next == nil || first.Items[0].ID != created[0].ID || first.Items[1].ID != created[1].ID {
		t.Fatalf("first page = %#v", first)
	}
	cursor := *first.Next
	second, err := store.List(context.Background(), WatchListOptions{Limit: 2, Cursor: &cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 2 || second.Next != nil || second.Items[0].ID != created[3].ID || second.Items[1].ID != created[4].ID {
		t.Fatalf("second page = %#v", second)
	}
	first.Items[0].Spec.Subject = "mutated"
	first.Next.ID = "mutated"
	again, err := store.List(context.Background(), WatchListOptions{Limit: 2})
	if err != nil || again.Items[0].Spec.Subject == "mutated" || again.Next == nil || again.Next.ID == "mutated" {
		t.Fatalf("List retained caller memory: %#v, %v", again, err)
	}

	emptyStoreClock := &testClock{now: testEpoch}
	_, emptyStore := openWatchStore(t, WithClock(emptyStoreClock.Now))
	empty, err := emptyStore.List(context.Background(), WatchListOptions{})
	if err != nil || empty.Items == nil || len(empty.Items) != 0 || empty.Next != nil {
		t.Fatalf("empty List() = %#v, %v", empty, err)
	}
	for _, options := range []WatchListOptions{
		{Limit: -1}, {Limit: MaxListLimit + 1},
		{Cursor: &WatchCursor{CreatedAt: time.Time{}, ID: created[0].ID}},
		{Cursor: &WatchCursor{CreatedAt: testEpoch, ID: "not-a-uuid"}},
		{Cursor: &WatchCursor{CreatedAt: time.Now(), ID: created[0].ID}},
	} {
		if _, err := store.List(context.Background(), options); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("List(%#v) error = %v", options, err)
		}
	}
	if _, err := store.Get(context.Background(), created[2].ID); !errors.Is(err, ErrWatchNotFound) {
		t.Fatalf("Get(deleted) error = %v", err)
	}
}

func TestPauseResumeDeleteAreIdempotentAndAdvanceTime(t *testing.T) {
	clock := &testClock{now: testEpoch}
	ids := &testIDs{}
	_, store := openWatchStore(t, WithClock(clock.Now), WithIDGenerator(ids.Generate))
	created, _, err := store.Create(context.Background(), validSpec("lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	paused, err := store.Pause(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if paused.State != StatePaused || paused.NextCheckAt != nil || paused.PausedAt == nil ||
		!paused.UpdatedAt.Equal(testEpoch.Add(time.Nanosecond)) || !paused.PausedAt.Equal(paused.UpdatedAt) {
		t.Fatalf("paused = %#v", paused)
	}
	pausedAgain, err := store.Pause(context.Background(), created.ID)
	if err != nil || !pausedAgain.UpdatedAt.Equal(paused.UpdatedAt) {
		t.Fatalf("idempotent Pause() = %#v, %v", pausedAgain, err)
	}
	resumed, err := store.Resume(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.State != StateActive || resumed.NextCheckAt == nil || resumed.PausedAt != nil ||
		!resumed.UpdatedAt.Equal(testEpoch.Add(2*time.Nanosecond)) || !resumed.NextCheckAt.Equal(resumed.UpdatedAt) {
		t.Fatalf("resumed = %#v", resumed)
	}
	resumedAgain, err := store.Resume(context.Background(), created.ID)
	if err != nil || !resumedAgain.UpdatedAt.Equal(resumed.UpdatedAt) {
		t.Fatalf("idempotent Resume() = %#v, %v", resumedAgain, err)
	}
	if err := store.Delete(context.Background(), created.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(context.Background(), created.ID); err != nil {
		t.Fatalf("idempotent Delete() error = %v", err)
	}
	replacement, replaced, err := store.Create(context.Background(), validSpec("lifecycle"))
	if err != nil || !replaced || replacement.ID == created.ID {
		t.Fatalf("Create(replacement after delete) = (%#v,%v,%v)", replacement, replaced, err)
	}
	if _, err := store.Pause(context.Background(), created.ID); !errors.Is(err, ErrWatchNotFound) {
		t.Fatalf("Pause(deleted) error = %v", err)
	}
	if err := store.Delete(context.Background(), "00000000-0000-4000-8000-000000999999"); !errors.Is(err, ErrWatchNotFound) {
		t.Fatalf("Delete(missing) error = %v", err)
	}
	if _, err := store.Get(context.Background(), "not-a-uuid"); !errors.Is(err, ErrInvalidWatchID) {
		t.Fatalf("Get(invalid ID) error = %v", err)
	}
}

func TestClaimAndFailedReleaseUseExactLeases(t *testing.T) {
	clock := &testClock{now: testEpoch}
	ids := &testIDs{}
	_, store := openWatchStore(t, WithClock(clock.Now), WithIDGenerator(ids.Generate), WithLeaseDuration(3*time.Minute))
	created, _, err := store.Create(context.Background(), validSpec("lease"))
	if err != nil {
		t.Fatal(err)
	}
	claim, found, err := store.ClaimDue(context.Background())
	if err != nil || !found {
		t.Fatalf("ClaimDue() = (%#v,%v,%v)", claim, found, err)
	}
	if claim.Watch.ID != created.ID || claim.Fact != nil || claim.Lease.WatchID != created.ID ||
		claim.Lease.ID != "00000000-0000-4000-8000-000000000002" ||
		!claim.Watch.UpdatedAt.Equal(testEpoch.Add(time.Nanosecond)) ||
		!claim.Lease.Until.Equal(claim.Watch.UpdatedAt.Add(3*time.Minute)) {
		t.Fatalf("claim = %#v", claim)
	}
	encoded, err := json.Marshal(claim.Lease)
	if err != nil || string(encoded) != `{}` || strings.Contains(string(encoded), claim.Lease.ID) {
		t.Fatalf("lease JSON = %q, %v", encoded, err)
	}
	if _, found, err := store.ClaimDue(context.Background()); err != nil || found {
		t.Fatalf("live lease was reclaimed: found=%v err=%v", found, err)
	}

	wrong := claim.Lease
	wrong.ID = "00000000-0000-4000-8000-000000999999"
	if matched, err := store.ReleaseLease(context.Background(), wrong, testEpoch.Add(time.Minute), "FETCH_FAILED"); err != nil || matched {
		t.Fatalf("ReleaseLease(wrong) = (%v,%v)", matched, err)
	}
	retryAt := testEpoch.Add(time.Minute)
	matched, err := store.ReleaseLease(context.Background(), claim.Lease, retryAt, "FETCH_FAILED")
	if err != nil || !matched {
		t.Fatalf("ReleaseLease() = (%v,%v)", matched, err)
	}
	failed, err := store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.ConsecutiveFailures != 1 || failed.LastErrorCode != "FETCH_FAILED" ||
		failed.NextCheckAt == nil || !failed.NextCheckAt.Equal(retryAt) {
		t.Fatalf("failed watch = %#v", failed)
	}
	if matched, err := store.ReleaseLease(context.Background(), claim.Lease, retryAt.Add(time.Minute), "FETCH_FAILED"); err != nil || matched {
		t.Fatalf("repeated ReleaseLease() = (%v,%v)", matched, err)
	}
	if _, found, err := store.ClaimDue(context.Background()); err != nil || found {
		t.Fatalf("watch claimed before retry: found=%v err=%v", found, err)
	}

	clock.Set(retryAt)
	second, found, err := store.ClaimDue(context.Background())
	if err != nil || !found {
		t.Fatalf("second ClaimDue() = (%#v,%v,%v)", second, found, err)
	}
	clock.Set(second.Lease.Until)
	takeover, found, err := store.ClaimDue(context.Background())
	if err != nil || !found || takeover.Lease.ID == second.Lease.ID {
		t.Fatalf("expired takeover = (%#v,%v,%v)", takeover, found, err)
	}
	if matched, err := store.ReleaseLease(context.Background(), second.Lease,
		second.Lease.Until.Add(time.Minute), "FETCH_FAILED"); err != nil || matched {
		t.Fatalf("old lease release after takeover = (%v,%v)", matched, err)
	}
	if _, err := store.Pause(context.Background(), created.ID); err != nil {
		t.Fatal(err)
	}
	if matched, err := store.ReleaseLease(context.Background(), takeover.Lease,
		takeover.Lease.Until.Add(time.Minute), "FETCH_FAILED"); err != nil || matched {
		t.Fatalf("paused lease release = (%v,%v)", matched, err)
	}
}

func TestConcurrentClaimAcrossLedgerConnectionsHasOneWinner(t *testing.T) {
	directory := t.TempDir()
	firstLedger, err := ledger.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer firstLedger.Close()
	secondLedger, err := ledger.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer secondLedger.Close()
	clock := &testClock{now: testEpoch}
	ids := &testIDs{}
	first, err := NewStore(firstLedger, WithClock(clock.Now), WithIDGenerator(ids.Generate))
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewStore(secondLedger, WithClock(clock.Now), WithIDGenerator(ids.Generate))
	if err != nil {
		t.Fatal(err)
	}
	created, ok, err := first.Create(context.Background(), validSpec("cross connection claim"))
	if err != nil || !ok {
		t.Fatalf("Create() = (%#v,%v,%v)", created, ok, err)
	}
	type result struct {
		claim Claim
		found bool
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, store := range []*Store{first, second} {
		go func(store *Store) {
			<-start
			claim, found, err := store.ClaimDue(context.Background())
			results <- result{claim: claim, found: found, err: err}
		}(store)
	}
	close(start)
	left, right := <-results, <-results
	if left.err != nil || right.err != nil || left.found == right.found {
		t.Fatalf("cross-connection ClaimDue results = %#v / %#v", left, right)
	}
	winner := left
	if !winner.found {
		winner = right
	}
	if winner.claim.Watch.ID != created.ID || winner.claim.Lease.ID == "" || ids.calls.Load() != 2 {
		t.Fatalf("claim winner = %#v, generator calls=%d", winner, ids.calls.Load())
	}
}

func TestClaimRejectsLeaseIdentityReuse(t *testing.T) {
	clock := &testClock{now: testEpoch}
	var calls atomic.Int32
	reused := "00000000-0000-4000-8000-000000000001"
	_, store := openWatchStore(t, WithClock(clock.Now), WithIDGenerator(func() (string, error) {
		calls.Add(1)
		return reused, nil
	}))
	created, _, err := store.Create(context.Background(), validSpec("lease identity reuse"))
	if err != nil || created.ID != reused {
		t.Fatalf("Create() = (%#v,%v)", created, err)
	}
	if _, found, err := store.ClaimDue(context.Background()); found || !errors.Is(err, ErrInvalidStore) {
		t.Fatalf("ClaimDue(reused public ID) = (found=%v,error=%v)", found, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("ID generator calls = %d", calls.Load())
	}
}

func TestClaimDueQueryUsesDueIndexWithoutTemporarySort(t *testing.T) {
	durable, _ := openWatchStore(t)
	details := make([]string, 0, 2)
	err := durable.View(context.Background(), func(tx ledger.ReadTx) error {
		rows, err := tx.QueryContext(context.Background(), `EXPLAIN QUERY PLAN SELECT `+watchColumns+`
			FROM watches WHERE state IN ('pending','active') AND next_check_at <= ?
				AND (lease_id IS NULL OR lease_until <= ?)
			ORDER BY next_check_at, lease_until, id LIMIT 1`, formatTime(testEpoch), formatTime(testEpoch))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, parent, notUsed int
			var detail string
			if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
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
	if !strings.Contains(plan, "USING INDEX idx_watches_due") || strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("due query plan does not use the ordered partial index:\n%s", plan)
	}
}

func TestReleaseLeaseRejectsMalformedOrUnboundedRetry(t *testing.T) {
	clock := &testClock{now: testEpoch}
	ids := &testIDs{}
	_, store := openWatchStore(t, WithClock(clock.Now), WithIDGenerator(ids.Generate))
	_, _, err := store.Create(context.Background(), validSpec("retry bounds"))
	if err != nil {
		t.Fatal(err)
	}
	claim, found, err := store.ClaimDue(context.Background())
	if err != nil || !found {
		t.Fatal("expected due claim", err)
	}
	for name, test := range map[string]struct {
		lease Lease
		retry time.Time
		code  string
	}{
		"bad watch":      {Lease{WatchID: "bad", ID: claim.Lease.ID, Until: claim.Lease.Until}, testEpoch.Add(time.Minute), "FETCH_FAILED"},
		"bad lease":      {Lease{WatchID: claim.Lease.WatchID, ID: "bad", Until: claim.Lease.Until}, testEpoch.Add(time.Minute), "FETCH_FAILED"},
		"bad until":      {Lease{WatchID: claim.Lease.WatchID, ID: claim.Lease.ID}, testEpoch.Add(time.Minute), "FETCH_FAILED"},
		"empty code":     {claim.Lease, testEpoch.Add(time.Minute), ""},
		"lowercase code": {claim.Lease, testEpoch.Add(time.Minute), "secret_detail"},
		"control code":   {claim.Lease, testEpoch.Add(time.Minute), "FETCH\nFAILED"},
		"oversize code":  {claim.Lease, testEpoch.Add(time.Minute), "F" + strings.Repeat("A", 64)},
		"past retry":     {claim.Lease, testEpoch, "FETCH_FAILED"},
		"far retry":      {claim.Lease, testEpoch.Add(7*24*time.Hour + time.Nanosecond), "FETCH_FAILED"},
		"non UTC retry":  {claim.Lease, testEpoch.In(time.FixedZone("offset", 3600)).Add(time.Minute), "FETCH_FAILED"},
	} {
		t.Run(name, func(t *testing.T) {
			matched, err := store.ReleaseLease(context.Background(), test.lease, test.retry, test.code)
			if matched || !errors.Is(err, ErrInvalidLease) || strings.Contains(fmt.Sprint(err), "secret") {
				t.Fatalf("ReleaseLease() = (%v,%q)", matched, err)
			}
		})
	}
	if matched, err := store.ReleaseLease(context.Background(), claim.Lease,
		testEpoch.Add(time.Minute), "HTTP_429"); err != nil || !matched {
		t.Fatalf("ReleaseLease(HTTP_429) = (%v,%v)", matched, err)
	}
}

func TestReleaseLeaseCannotMutateAnExpiredClaim(t *testing.T) {
	t.Run("one nanosecond before expiry", func(t *testing.T) {
		clock := &testClock{now: testEpoch}
		ids := &testIDs{}
		_, store := openWatchStore(t, WithClock(clock.Now), WithIDGenerator(ids.Generate))
		created, _, err := store.Create(context.Background(), validSpec("release before expiry"))
		if err != nil {
			t.Fatal(err)
		}
		claim, found, err := store.ClaimDue(context.Background())
		if err != nil || !found {
			t.Fatalf("ClaimDue() = (%#v,%v,%v)", claim, found, err)
		}
		clock.Set(claim.Lease.Until.Add(-time.Nanosecond))
		retryAt := claim.Lease.Until.Add(time.Nanosecond)
		matched, err := store.ReleaseLease(context.Background(), claim.Lease, retryAt, "HTTP_429")
		if err != nil || !matched {
			t.Fatalf("ReleaseLease(before expiry) = (%v,%v)", matched, err)
		}
		loaded, err := store.Get(context.Background(), created.ID)
		if err != nil || loaded.ConsecutiveFailures != 1 || loaded.NextCheckAt == nil || !loaded.NextCheckAt.Equal(retryAt) {
			t.Fatalf("released watch = (%#v,%v)", loaded, err)
		}
	})

	t.Run("at expiry and after takeover", func(t *testing.T) {
		clock := &testClock{now: testEpoch}
		ids := &testIDs{}
		_, store := openWatchStore(t, WithClock(clock.Now), WithIDGenerator(ids.Generate))
		created, _, err := store.Create(context.Background(), validSpec("release at expiry"))
		if err != nil {
			t.Fatal(err)
		}
		old, found, err := store.ClaimDue(context.Background())
		if err != nil || !found {
			t.Fatalf("ClaimDue() = (%#v,%v,%v)", old, found, err)
		}
		clock.Set(old.Lease.Until)
		matched, err := store.ReleaseLease(context.Background(), old.Lease,
			old.Lease.Until.Add(time.Minute), "FETCH_FAILED")
		if err != nil || matched {
			t.Fatalf("ReleaseLease(at expiry) = (%v,%v)", matched, err)
		}
		unchanged, err := store.Get(context.Background(), created.ID)
		if err != nil || unchanged.ConsecutiveFailures != 0 || unchanged.LastErrorCode != "" ||
			unchanged.NextCheckAt == nil || !unchanged.NextCheckAt.Equal(testEpoch) {
			t.Fatalf("expired release changed schedule: (%#v,%v)", unchanged, err)
		}
		takeover, found, err := store.ClaimDue(context.Background())
		if err != nil || !found || takeover.Lease.ID == old.Lease.ID {
			t.Fatalf("expired takeover = (%#v,%v,%v)", takeover, found, err)
		}
		matched, err = store.ReleaseLease(context.Background(), old.Lease,
			old.Lease.Until.Add(time.Minute), "FETCH_FAILED")
		if err != nil || matched {
			t.Fatalf("ReleaseLease(old after takeover) = (%v,%v)", matched, err)
		}
		stillLeased, err := store.Get(context.Background(), created.ID)
		if err != nil || stillLeased.ConsecutiveFailures != 0 || stillLeased.UpdatedAt != takeover.Watch.UpdatedAt {
			t.Fatalf("old release altered takeover: (%#v,%v)", stillLeased, err)
		}
	})
}

func TestClaimLoadsAndDeepCopiesCanonicalOpenFact(t *testing.T) {
	clock := &testClock{now: testEpoch}
	ids := &testIDs{}
	durable, store := openWatchStore(t, WithClock(clock.Now), WithIDGenerator(ids.Generate))
	created, _, err := store.Create(context.Background(), validSpec("fact subject"))
	if err != nil {
		t.Fatal(err)
	}
	observed := testEpoch.Add(time.Hour)
	seedOpenFact(t, durable, created, observed, "example.com", "https://www.example.com/pricing")
	clock.Set(observed)
	claim, found, err := store.ClaimDue(context.Background())
	if err != nil || !found || claim.Fact == nil {
		t.Fatalf("ClaimDue() = (%#v,%v,%v)", claim, found, err)
	}
	fact := claim.Fact
	if fact.WatchID != created.ID || fact.Subject != created.Spec.Subject ||
		fact.Predicate != created.Spec.Predicate || string(fact.Value) != `"19"` ||
		fact.Root != "example.com" || fact.SourceURL != "https://www.example.com/pricing" ||
		fact.CreatedVerificationID != "baseline-verification" || fact.LatestVerificationID != "baseline-verification" ||
		!fact.ObservedAt.Equal(observed) || !fact.ValidFrom.Equal(observed) || !fact.LastVerifiedAt.Equal(observed) ||
		fact.ValidTo != nil || fact.ClosedOutcome != "" || fact.GoneScope != "" {
		t.Fatalf("claimed fact = %#v", fact)
	}
	copy := cloneClaim(claim)
	fact.Value[0] = 'x'
	fact.ValidTo = timePointer(time.Time{})
	if string(copy.Fact.Value) != `"19"` || copy.Fact.ValidTo != nil {
		t.Fatalf("claim fact was not deeply copied: %#v", copy.Fact)
	}
}

func TestClaimRejectsMismatchedFactRootAndRollsBack(t *testing.T) {
	clock := &testClock{now: testEpoch}
	ids := &testIDs{}
	durable, store := openWatchStore(t, WithClock(clock.Now), WithIDGenerator(ids.Generate))
	created, _, err := store.Create(context.Background(), validSpec("corrupt root"))
	if err != nil {
		t.Fatal(err)
	}
	seedOpenFact(t, durable, created, testEpoch, "attacker.example", "https://www.example.com/pricing")
	if _, found, err := store.ClaimDue(context.Background()); found || !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("ClaimDue(corrupt root) = (found=%v,error=%v)", found, err)
	}
	var leaseID, leaseUntil any
	if err := durable.View(context.Background(), func(tx ledger.ReadTx) error {
		return tx.QueryRowContext(context.Background(),
			`SELECT lease_id, lease_until FROM watches WHERE id = ?`, created.ID).Scan(&leaseID, &leaseUntil)
	}); err != nil {
		t.Fatal(err)
	}
	if leaseID != nil || leaseUntil != nil {
		t.Fatalf("corrupt claim retained lease: id=%v until=%v", leaseID, leaseUntil)
	}
}

func TestClaimRejectsFactVerificationControlCharacters(t *testing.T) {
	for _, verificationID := range []string{"fact\nverification", "fact\x00verification", "fact-\xff"} {
		t.Run(fmt.Sprintf("%x", verificationID), func(t *testing.T) {
			clock := &testClock{now: testEpoch}
			ids := &testIDs{}
			durable, store := openWatchStore(t, WithClock(clock.Now), WithIDGenerator(ids.Generate))
			created, _, err := store.Create(context.Background(), validSpec("corrupt fact verification"))
			if err != nil {
				t.Fatal(err)
			}
			seedOpenFactWithVerification(t, durable, created, testEpoch, "example.com",
				"https://www.example.com/pricing", verificationID)
			if _, found, err := store.ClaimDue(context.Background()); found || !errors.Is(err, ErrCorruptStore) {
				t.Fatalf("ClaimDue(corrupt fact verification) = (found=%v,error=%v)", found, err)
			}
		})
	}
	if !validVerificationIdentity(strings.Repeat("v", 512)) ||
		validVerificationIdentity(strings.Repeat("v", 513)) {
		t.Fatal("verification identity byte boundary is not exact")
	}
}

func TestLiveWatchLimitIsAtomicAndExactRetryStillSucceeds(t *testing.T) {
	clock := &testClock{now: testEpoch}
	ids := &testIDs{}
	durable, store := openWatchStore(t, WithClock(clock.Now), WithIDGenerator(ids.Generate))
	ctx := context.Background()
	var first models.FactSpec
	err := durable.Update(ctx, func(tx ledger.WriteTx) error {
		stamp := formatTime(testEpoch)
		for index := 0; index < MaxLiveWatches; index++ {
			spec := validSpec(fmt.Sprintf("capacity subject %05d", index))
			normalized, hash, err := normalizeFactSpec(spec)
			if err != nil {
				return err
			}
			if index == 0 {
				first = normalized
			}
			id := fmt.Sprintf("10000000-0000-4000-8000-%012x", index+1)
			if _, err := tx.ExecContext(ctx, `INSERT INTO watches (
				id, spec_hash, subject, predicate, freshness, min_independent_sources,
				on_conflict, state, next_check_at, ewma_interval_s, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?, ?)`,
				id, hash, normalized.Subject, normalized.Predicate, normalized.Freshness,
				normalized.MinIndependentSources, normalized.OnConflict, stamp, 604800.0, stamp, stamp); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed capacity: %v", err)
	}
	existing, created, err := store.Create(ctx, first)
	if err != nil || created || existing.Spec != first {
		t.Fatalf("Create(existing at cap) = (%#v,%v,%v)", existing, created, err)
	}
	if _, created, err := store.Create(ctx, validSpec("over capacity")); created || !errors.Is(err, ErrWatchLimit) {
		t.Fatalf("Create(over cap) = (created=%v,error=%v)", created, err)
	}
}

func seedOpenFact(
	t *testing.T,
	durable *ledger.Store,
	watch Watch,
	observed time.Time,
	root string,
	sourceURL string,
) {
	t.Helper()
	seedOpenFactWithVerification(t, durable, watch, observed, root, sourceURL, "baseline-verification")
}

func seedOpenFactWithVerification(
	t *testing.T,
	durable *ledger.Store,
	watch Watch,
	observed time.Time,
	root string,
	sourceURL string,
	verificationID string,
) {
	t.Helper()
	verification := ledger.Verification{
		VerificationID: verificationID, ClaimIndex: 0, URL: sourceURL,
		FinalURL: sourceURL, Path: watch.Spec.Predicate, OldValue: json.RawMessage(`"19"`),
		Outcome:       ledger.OutcomeConfirmed,
		OldSnapshotID: "sha256:" + strings.Repeat("0", 64),
		NewSnapshotID: "sha256:" + strings.Repeat("a", 64),
		OldReceipt:    "old-receipt", Receipt: "receipt-1", VerifiedAt: observed,
	}
	err := durable.RecordVerificationBatchWithHook(context.Background(), []ledger.Verification{verification}, nil,
		func(ctx context.Context, tx ledger.WriteTx, state ledger.VerificationBatchState) error {
			if state.Existing {
				return errors.New("unexpected retry")
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO facts (
				id, watch_id, subject, predicate, path, value, root, source_url, receipt,
				snapshot_id, created_verification_id, created_claim_index,
				latest_verification_id, latest_claim_index, observed_at, valid_from,
				last_verified_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				strings.Repeat("b", 64), watch.ID, watch.Spec.Subject, watch.Spec.Predicate,
				watch.Spec.Predicate, string(verification.OldValue), root, sourceURL,
				verification.Receipt, verification.NewSnapshotID, verification.VerificationID,
				verification.ClaimIndex, verification.VerificationID, verification.ClaimIndex,
				formatTime(observed), formatTime(observed), formatTime(observed))
			return err
		})
	if err != nil {
		t.Fatalf("seed open fact: %v", err)
	}
}

func seedVerification(t *testing.T, durable *ledger.Store, verificationID string, at time.Time) {
	t.Helper()
	err := durable.RecordVerifications(context.Background(), []ledger.Verification{{
		VerificationID: verificationID, ClaimIndex: 0, URL: "https://example.com/pricing",
		Path: "price_per_mtok_input", OldValue: json.RawMessage(`"19"`),
		Outcome:       ledger.OutcomeConfirmed,
		OldSnapshotID: "sha256:" + strings.Repeat("0", 64), VerifiedAt: at,
	}})
	if err != nil {
		t.Fatalf("seed verification: %v", err)
	}
}
