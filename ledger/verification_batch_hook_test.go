package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type typedNilHookPanic struct{}

func (*typedNilHookPanic) Error() string {
	return "typed-nil-hook-secret-must-not-leak"
}

func TestRecordVerificationBatchFunctionTypeIsStable(t *testing.T) {
	var method func(*Store, context.Context, []Verification, *OutboxEvent) error = (*Store).RecordVerificationBatch
	if method == nil {
		t.Fatal("RecordVerificationBatch method expression = nil")
	}
}

func TestRecordVerificationBatchWithHookSeesNormalizedRowsBeforeOutbox(t *testing.T) {
	store := openTestStore(t)
	row := outboxVerification(" verification-hook-new ", 0)
	row.URL = " https://EXAMPLE.com./item "
	row.Path = " /price "
	row.VerifiedAt = time.Date(2026, 8, 9, 20, 13, 14, 567890123, time.FixedZone("fixture", 8*60*60))
	event := validOutboxEvent("verification-hook-new", "event-hook-new", outboxTestTime())

	called := 0
	err := store.RecordVerificationBatchWithHook(context.Background(), []Verification{row}, &event,
		func(ctx context.Context, tx WriteTx, state VerificationBatchState) error {
			called++
			if state != (VerificationBatchState{VerificationID: "verification-hook-new", Existing: false}) {
				t.Fatalf("state = %#v", state)
			}
			var (
				verificationID string
				pageURL        string
				host           string
				path           string
				verifiedAt     string
			)
			if err := tx.QueryRowContext(ctx, `SELECT verification_id, url, host, path, verified_at
				FROM verifications WHERE verification_id = ? AND claim_index = 0`, state.VerificationID).
				Scan(&verificationID, &pageURL, &host, &path, &verifiedAt); err != nil {
				return err
			}
			if verificationID != "verification-hook-new" || pageURL != "https://EXAMPLE.com./item" ||
				host != "example.com" || path != "/price" || verifiedAt != "2026-08-09T12:13:14.567890123Z" {
				t.Fatalf("normalized row = id=%q url=%q host=%q path=%q verified_at=%q",
					verificationID, pageURL, host, path, verifiedAt)
			}
			var outboxCount int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM outbox_events WHERE id = ?", event.ID).Scan(&outboxCount); err != nil {
				return err
			}
			if outboxCount != 0 {
				t.Fatalf("outbox count inside new hook = %d, want 0", outboxCount)
			}
			return nil
		})
	if err != nil {
		t.Fatalf("RecordVerificationBatchWithHook() error = %v", err)
	}
	if called != 1 {
		t.Fatalf("hook calls = %d, want 1", called)
	}
	assertVerificationCount(t, store.db, "verification-hook-new", 1)
	assertTableCount(t, store.db, "outbox_events", 1)
}

func TestRecordVerificationBatchWithHookRunsForExactRetry(t *testing.T) {
	store := openTestStore(t)
	row := outboxVerification("verification-hook-retry", 0)
	event := validOutboxEvent(row.VerificationID, "event-hook-retry", outboxTestTime())
	if err := store.RecordVerificationBatch(context.Background(), []Verification{row}, &event); err != nil {
		t.Fatalf("seed batch error = %v", err)
	}

	retry := event
	retry.ID = "ignored-id-on-exact-retry"
	called := 0
	err := store.RecordVerificationBatchWithHook(context.Background(), []Verification{row}, &retry,
		func(ctx context.Context, tx WriteTx, state VerificationBatchState) error {
			called++
			if state.VerificationID != row.VerificationID || !state.Existing {
				t.Fatalf("retry state = %#v", state)
			}
			var rowCount, eventCount int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM verifications WHERE verification_id = ?", state.VerificationID).Scan(&rowCount); err != nil {
				return err
			}
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM outbox_events WHERE verification_id = ?", state.VerificationID).Scan(&eventCount); err != nil {
				return err
			}
			if rowCount != 1 || eventCount != 1 {
				t.Fatalf("retry-visible counts = rows %d events %d", rowCount, eventCount)
			}
			return nil
		})
	if err != nil {
		t.Fatalf("exact retry error = %v", err)
	}
	if called != 1 {
		t.Fatalf("retry hook calls = %d, want 1", called)
	}
	assertVerificationCount(t, store.db, row.VerificationID, 1)
	assertTableCount(t, store.db, "outbox_events", 1)
}

func TestRecordVerificationBatchWithHookRejectsDriftBeforeHook(t *testing.T) {
	store := openTestStore(t)
	row := outboxVerification("verification-hook-drift", 0)
	if err := store.RecordVerificationBatch(context.Background(), []Verification{row}, nil); err != nil {
		t.Fatalf("seed batch error = %v", err)
	}
	drifted := row
	drifted.Path = "/different"
	called := false
	err := store.RecordVerificationBatchWithHook(context.Background(), []Verification{drifted}, nil,
		func(context.Context, WriteTx, VerificationBatchState) error {
			called = true
			return nil
		})
	if !errors.Is(err, ErrInvalidVerification) {
		t.Fatalf("drift error = %v, want ErrInvalidVerification", err)
	}
	if called {
		t.Fatal("hook ran for conflicting retry")
	}
}

func TestRecordVerificationBatchWithHookFailuresAreAtomicAndRedacted(t *testing.T) {
	tests := []struct {
		name string
		hook func(context.CancelFunc, error) VerificationBatchHook
		want error
	}{
		{
			name: "error",
			hook: func(_ context.CancelFunc, secret error) VerificationBatchHook {
				return func(context.Context, WriteTx, VerificationBatchState) error { return secret }
			},
		},
		{
			name: "panic",
			hook: func(_ context.CancelFunc, secret error) VerificationBatchHook {
				return func(context.Context, WriteTx, VerificationBatchState) error { panic(secret) }
			},
			want: ErrVerificationBatchHookPanic,
		},
		{
			name: "cancel",
			hook: func(cancel context.CancelFunc, _ error) VerificationBatchHook {
				return func(ctx context.Context, _ WriteTx, _ VerificationBatchState) error {
					cancel()
					return ctx.Err()
				}
			},
			want: context.Canceled,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			row := outboxVerification("verification-hook-failure-"+test.name, 0)
			event := validOutboxEvent(row.VerificationID, "event-hook-failure-"+test.name, outboxTestTime())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			secret := errors.New("secret-hook-cause-must-not-leak")
			err := store.RecordVerificationBatchWithHook(ctx, []Verification{row}, &event, test.hook(cancel, secret))
			if !errors.Is(err, ErrVerificationBatchHook) {
				t.Fatalf("error = %v, want ErrVerificationBatchHook", err)
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if test.name == "error" && !errors.Is(err, secret) {
				t.Fatalf("error = %v, want hidden cause identity", err)
			}
			if strings.Contains(err.Error(), secret.Error()) {
				t.Fatalf("error leaked hook secret: %q", err)
			}
			assertVerificationCount(t, store.db, row.VerificationID, 0)
			assertTableCount(t, store.db, "outbox_events", 0)
		})
	}
}

func TestRecordVerificationBatchWithHookRecoversLiteralNilPanic(t *testing.T) {
	t.Setenv("GODEBUG", "panicnil=1")
	store := openTestStore(t)
	row := outboxVerification("verification-hook-literal-nil-panic", 0)
	event := validOutboxEvent(row.VerificationID, "event-hook-literal-nil-panic", outboxTestTime())

	err := store.RecordVerificationBatchWithHook(context.Background(), []Verification{row}, &event,
		func(context.Context, WriteTx, VerificationBatchState) error {
			panic(nil)
		})
	if !errors.Is(err, ErrVerificationBatchHookPanic) || !errors.Is(err, ErrVerificationBatchHook) {
		t.Fatalf("literal nil panic error = %v, want hook panic sentinels", err)
	}
	assertVerificationCount(t, store.db, row.VerificationID, 0)
	assertTableCount(t, store.db, "outbox_events", 0)
}

func TestRecordVerificationBatchWithHookRedactsTypedNilPanic(t *testing.T) {
	store := openTestStore(t)
	row := outboxVerification("verification-hook-typed-nil-panic", 0)
	event := validOutboxEvent(row.VerificationID, "event-hook-typed-nil-panic", outboxTestTime())
	var secret *typedNilHookPanic

	err := store.RecordVerificationBatchWithHook(context.Background(), []Verification{row}, &event,
		func(context.Context, WriteTx, VerificationBatchState) error {
			panic(secret)
		})
	if !errors.Is(err, ErrVerificationBatchHookPanic) || !errors.Is(err, ErrVerificationBatchHook) {
		t.Fatalf("typed nil panic error = %v, want hook panic sentinels", err)
	}
	if strings.Contains(err.Error(), secret.Error()) {
		t.Fatalf("error leaked typed nil panic value: %q", err)
	}
	assertVerificationCount(t, store.db, row.VerificationID, 0)
	assertTableCount(t, store.db, "outbox_events", 0)
}

func TestRecordVerificationBatchWithHookRollsBackHookWritesOnOutboxFailure(t *testing.T) {
	store := openTestStore(t)
	seed := outboxVerification("verification-hook-rollback-seed", 0)
	seedEvent := validOutboxEvent(seed.VerificationID, "event-hook-rollback-shared", outboxTestTime())
	if err := store.RecordVerificationBatch(context.Background(), []Verification{seed}, &seedEvent); err != nil {
		t.Fatalf("seed batch error = %v", err)
	}

	row := outboxVerification("verification-hook-rollback-new", 0)
	conflict := validOutboxEvent(row.VerificationID, seedEvent.ID, outboxTestTime())
	err := store.RecordVerificationBatchWithHook(context.Background(), []Verification{row}, &conflict,
		func(ctx context.Context, tx WriteTx, _ VerificationBatchState) error {
			_, err := tx.ExecContext(ctx, "UPDATE verifications SET receipt = ? WHERE verification_id = ?", "hook-mutated", seed.VerificationID)
			return err
		})
	if err == nil {
		t.Fatal("outbox conflict error = nil")
	}
	assertVerificationCount(t, store.db, row.VerificationID, 0)
	var receipt sql.NullString
	if err := store.db.QueryRow("SELECT receipt FROM verifications WHERE verification_id = ?", seed.VerificationID).Scan(&receipt); err != nil {
		t.Fatalf("load seed receipt: %v", err)
	}
	if receipt.Valid {
		t.Fatalf("hook mutation survived outbox rollback: %#v", receipt)
	}
	assertTableCount(t, store.db, "outbox_events", 1)
}

func TestRecordVerificationBatchWithHookRollsBackExactRetryHookOnEventDrift(t *testing.T) {
	store := openTestStore(t)
	row := outboxVerification("verification-hook-retry-event-drift", 0)
	event := validOutboxEvent(row.VerificationID, "event-hook-retry-event-drift", outboxTestTime())
	if err := store.RecordVerificationBatch(context.Background(), []Verification{row}, &event); err != nil {
		t.Fatalf("seed batch error = %v", err)
	}
	drifted := event
	drifted.Payload = []byte(`{"different":true}`)
	err := store.RecordVerificationBatchWithHook(context.Background(), []Verification{row}, &drifted,
		func(ctx context.Context, tx WriteTx, state VerificationBatchState) error {
			if !state.Existing {
				t.Fatal("event-drift retry was not marked existing")
			}
			_, err := tx.ExecContext(ctx, "UPDATE verifications SET receipt = ? WHERE verification_id = ?", "hook-mutated", state.VerificationID)
			return err
		})
	if !errors.Is(err, ErrOutboxConflict) {
		t.Fatalf("event drift error = %v, want ErrOutboxConflict", err)
	}
	var receipt sql.NullString
	if err := store.db.QueryRow("SELECT receipt FROM verifications WHERE verification_id = ?", row.VerificationID).Scan(&receipt); err != nil {
		t.Fatalf("load retry receipt: %v", err)
	}
	if receipt.Valid {
		t.Fatalf("retry hook mutation survived event rollback: %#v", receipt)
	}
}

func TestRecordVerificationBatchWithHookConcurrentAcrossStores(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir)
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := Open(dir)
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	row := outboxVerification("verification-hook-concurrent", 0)
	event := validOutboxEvent(row.VerificationID, "event-hook-concurrent", outboxTestTime())
	const workers = 20
	start := make(chan struct{})
	errorsSeen := make(chan error, workers)
	var newCount, retryCount atomic.Int32
	var group sync.WaitGroup
	for index := 0; index < workers; index++ {
		index := index
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			store := first
			if index%2 == 1 {
				store = second
			}
			errorsSeen <- store.RecordVerificationBatchWithHook(context.Background(), []Verification{row}, &event,
				func(ctx context.Context, tx WriteTx, state VerificationBatchState) error {
					var count int
					if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM verifications WHERE verification_id = ?", state.VerificationID).Scan(&count); err != nil {
						return err
					}
					if count != 1 {
						return fmt.Errorf("hook-visible row count = %d", count)
					}
					if state.Existing {
						retryCount.Add(1)
					} else {
						newCount.Add(1)
					}
					return nil
				})
		}()
	}
	close(start)
	group.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("concurrent hook batch error = %v", err)
		}
	}
	if got := newCount.Load(); got != 1 {
		t.Fatalf("new hook count = %d, want 1", got)
	}
	if got := retryCount.Load(); got != workers-1 {
		t.Fatalf("retry hook count = %d, want %d", got, workers-1)
	}
	assertVerificationCount(t, first.db, row.VerificationID, 1)
	assertTableCount(t, first.db, "outbox_events", 1)
}

func TestRecordVerificationBatchWithHookHonorsContextAndClose(t *testing.T) {
	store := openTestStore(t)
	row := outboxVerification("verification-hook-closed", 0)
	called := false
	hook := func(context.Context, WriteTx, VerificationBatchState) error {
		called = true
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.RecordVerificationBatchWithHook(ctx, []Verification{row}, nil, hook); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled batch error = %v", err)
	}
	if called {
		t.Fatal("hook ran for canceled call")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := store.RecordVerificationBatchWithHook(context.Background(), []Verification{row}, nil, hook); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed batch error = %v", err)
	}
	if called {
		t.Fatal("hook ran for closed store")
	}
}
