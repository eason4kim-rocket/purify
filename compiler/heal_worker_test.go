package compiler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/snapshot"
)

func TestHealWorkerPollsPendingAndExpiredRunsDurably(t *testing.T) {
	t.Run("pending", func(t *testing.T) {
		harness := newSelfHealHarness(t, 3)
		worker := newHealTestWorker(t, harness.healer)
		awaitHealState(t, harness.durable, harness.pending.HealRunID, HealDegraded)
		if err := worker.Close(); err != nil {
			t.Fatal(err)
		}
		result, err := harness.healer.HealRun(context.Background(), harness.pending.HealRunID)
		if err != nil || result.TerminalReason != "insufficient_history" {
			t.Fatalf("pending poll result = %+v, %v", result, err)
		}
	})

	t.Run("expired replaying", func(t *testing.T) {
		harness := newSelfHealHarness(t, 3)
		claimed, _, terminal, err := harness.healer.claim(
			context.Background(), healSelector{id: harness.pending.HealRunID},
		)
		if err != nil || terminal || claimed.result.State != HealReplaying {
			t.Fatalf("claim = %+v, %v/%v", claimed.result, terminal, err)
		}
		harness.clock.Add(HealLeaseDuration + time.Second)
		worker := newHealTestWorker(t, harness.healer)
		awaitHealState(t, harness.durable, harness.pending.HealRunID, HealDegraded)
		if err := worker.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestHealWorkerIgnoresLiveReplayLeaseButManualScheduleReturnsTicket(t *testing.T) {
	harness := newSelfHealHarness(t, 3)
	claimed, leaseID, terminal, err := harness.healer.claim(
		context.Background(), healSelector{id: harness.pending.HealRunID},
	)
	if err != nil || terminal || claimed.result.State != HealReplaying {
		t.Fatalf("claim = %+v, %v/%v", claimed.result, terminal, err)
	}
	worker := newHealTestWorker(t, harness.healer)
	if cap(worker.wake) != 1 {
		t.Fatalf("wake capacity = %d, want 1", cap(worker.wake))
	}
	schedule, err := worker.ScheduleExtractor(context.Background(), harness.source.ID)
	if err != nil || schedule.ExtractorID != harness.source.ID || schedule.HealRunID != harness.pending.HealRunID {
		t.Fatalf("ScheduleExtractor(live) = %+v, %v", schedule, err)
	}
	time.Sleep(4 * time.Millisecond)
	state, storedLease := loadHealState(t, harness.durable, harness.pending.HealRunID)
	if state != HealReplaying || storedLease != leaseID {
		t.Fatalf("live run changed = %q/%q, want replaying/%q", state, storedLease, leaseID)
	}
	if err := worker.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHealWorkerScheduleUsesExactCurrentSourceRevisionAndFailsClosed(t *testing.T) {
	t.Run("invalid missing and active", func(t *testing.T) {
		harness := newCandidateHarness(t)
		set, schema := candidateSampleSet(t, harness, 20, uint64(1)<<63, MinCompileSamples)
		initial, err := harness.store.SubmitCandidate(
			context.Background(), candidateForSet(set, schema, "h1.name"),
		)
		if err != nil {
			t.Fatal(err)
		}
		snapshots := newFakeHealSnapshotReader()
		healer, err := NewHealer(harness.store, snapshots)
		if err != nil {
			t.Fatal(err)
		}
		worker := newHealTestWorker(t, healer)
		defer worker.Close()
		if _, err := worker.ScheduleExtractor(context.Background(), "not-a-uuid"); !errors.Is(err, ErrInvalidHealSchedule) {
			t.Fatalf("invalid schedule error = %v", err)
		}
		if _, err := worker.ScheduleExtractor(context.Background(),
			"00000000-0000-4000-8000-000000000099"); !errors.Is(err, ErrHealExtractorNotFound) {
			t.Fatalf("missing extractor error = %v", err)
		}
		if _, err := worker.ScheduleExtractor(context.Background(), initial.Extractor.ID); !errors.Is(err, ErrHealScheduleNotReady) {
			t.Fatalf("active extractor schedule error = %v", err)
		}
	})

	t.Run("stale without actionable run", func(t *testing.T) {
		harness := newSelfHealHarness(t, 3)
		result, err := harness.healer.HealRun(context.Background(), harness.pending.HealRunID)
		if err != nil || result.State != HealDegraded {
			t.Fatalf("terminalize pending run = %+v, %v", result, err)
		}
		worker := newHealTestWorker(t, harness.healer)
		defer worker.Close()
		if _, err := worker.ScheduleExtractor(context.Background(), harness.source.ID); !errors.Is(err, ErrHealScheduleNotReady) {
			t.Fatalf("stale extractor without actionable run error = %v", err)
		}
	})

	t.Run("retired with actionable run", func(t *testing.T) {
		harness := newSelfHealHarness(t, 3)
		claimed, _, terminal, err := harness.healer.claim(
			context.Background(), healSelector{id: harness.pending.HealRunID},
		)
		if err != nil || terminal || claimed.result.State != HealReplaying {
			t.Fatalf("claim = %+v, %v/%v", claimed.result, terminal, err)
		}
		if err := harness.durable.Update(context.Background(), func(tx ledger.WriteTx) error {
			_, err := tx.ExecContext(context.Background(),
				"UPDATE extractors SET state = 'retired' WHERE id = ?", harness.source.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		worker := newHealTestWorker(t, harness.healer)
		defer worker.Close()
		if _, err := worker.ScheduleExtractor(context.Background(), harness.source.ID); !errors.Is(err, ErrHealScheduleNotReady) {
			t.Fatalf("retired extractor schedule error = %v", err)
		}
	})

	t.Run("mismatched persisted source version", func(t *testing.T) {
		harness := newSelfHealHarness(t, 3)
		if err := harness.durable.Update(context.Background(), func(tx ledger.WriteTx) error {
			if _, err := tx.ExecContext(context.Background(),
				"DROP TRIGGER trg_extractor_heal_runs_source_exact"); err != nil {
				return err
			}
			if _, err := tx.ExecContext(context.Background(),
				"DROP TRIGGER trg_extractor_heal_runs_identity_immutable"); err != nil {
				return err
			}
			_, err := tx.ExecContext(context.Background(), `UPDATE extractor_heal_runs
				SET source_version = source_version + 1, state = 'replaying', lease_id = ?,
					lease_until = ?, updated_at = ? WHERE id = ?`,
				"00000000-0000-4000-8000-000000000077",
				formatExtractorTime(harness.clock.Now().Add(10*time.Minute)),
				formatExtractorTime(harness.clock.Now()), harness.pending.HealRunID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		worker := newHealTestWorker(t, harness.healer)
		defer worker.Close()
		if _, err := worker.ScheduleExtractor(context.Background(), harness.source.ID); !errors.Is(err, ErrHealScheduleNotReady) {
			t.Fatalf("version-mismatched schedule error = %v", err)
		}
	})

	t.Run("ambiguous actionable runs", func(t *testing.T) {
		harness := newSelfHealHarness(t, 3)
		insertSecondLiveHealRun(t, harness)
		worker := newHealTestWorker(t, harness.healer)
		defer worker.Close()
		if _, err := worker.ScheduleExtractor(context.Background(), harness.source.ID); !errors.Is(err, ErrHealScheduleAmbiguous) {
			t.Fatalf("ambiguous schedule error = %v", err)
		}
	})
}

func TestHealWorkerClosesRunningReplayAndReleasesDurableLease(t *testing.T) {
	harness := newSelfHealHarness(t, 3)
	harness.seedConfirmedHistory(t, []string{"one", "two", "three"}, func(int) bool { return true })
	entered := make(chan struct{})
	var once sync.Once
	harness.snapshots.onHas = func(
		ctx context.Context,
		_ snapshot.ID,
		_ func(snapshot.Meta) bool,
	) (bool, error) {
		if err := harness.durable.View(context.Background(), func(tx ledger.ReadTx) error {
			var one int
			return tx.QueryRowContext(context.Background(), "SELECT 1").Scan(&one)
		}); err != nil {
			return false, err
		}
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return false, ctx.Err()
	}
	worker := newHealTestWorker(t, harness.healer)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not reach snapshot observation")
	}
	if err := worker.Close(); err != nil {
		t.Fatal(err)
	}
	state, leaseID := loadHealState(t, harness.durable, harness.pending.HealRunID)
	if state != HealPending || leaseID != "" {
		t.Fatalf("closed worker left run = %q lease %q", state, leaseID)
	}
	if _, err := worker.ScheduleExtractor(context.Background(), harness.source.ID); !errors.Is(err, ErrHealWorkerClosed) {
		t.Fatalf("schedule after Close error = %v", err)
	}
	if err := worker.Close(); err != nil {
		t.Fatalf("idempotent Close error = %v", err)
	}
}

func TestNewHealWorkerRejectsInvalidConfiguration(t *testing.T) {
	if worker, err := NewHealWorker(context.Background(), nil, HealWorkerOptions{}); worker != nil ||
		!errors.Is(err, ErrInvalidHealWorker) {
		t.Fatalf("nil healer = %#v, %v", worker, err)
	}
	harness := newSelfHealHarness(t, 3)
	if worker, err := NewHealWorker(nil, harness.healer, HealWorkerOptions{}); worker != nil ||
		!errors.Is(err, ErrInvalidHealWorker) {
		t.Fatalf("nil parent = %#v, %v", worker, err)
	}
	if worker, err := NewHealWorker(context.Background(), harness.healer,
		HealWorkerOptions{PollInterval: -time.Second}); worker != nil || !errors.Is(err, ErrInvalidHealWorker) {
		t.Fatalf("negative interval = %#v, %v", worker, err)
	}
	if err := (*HealWorker)(nil).Close(); err != nil {
		t.Fatalf("nil Close error = %v", err)
	}
}

func newHealTestWorker(t *testing.T, healer *Healer) *HealWorker {
	t.Helper()
	worker, err := NewHealWorker(context.Background(), healer, HealWorkerOptions{
		PollInterval: time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewHealWorker: %v", err)
	}
	t.Cleanup(func() { _ = worker.Close() })
	return worker
}

func awaitHealState(t *testing.T, durable *ledger.Store, runID string, want HealState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		state, _ := loadHealState(t, durable, runID)
		if state == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	state, leaseID := loadHealState(t, durable, runID)
	t.Fatalf("heal state = %q lease %q, want %q", state, leaseID, want)
}

func insertSecondLiveHealRun(t *testing.T, harness *selfHealHarness) {
	t.Helper()
	const secondRunID = "00000000-0000-4000-8000-000000000099"
	now := formatExtractorTime(harness.clock.Now())
	leaseUntil := formatExtractorTime(harness.clock.Now().Add(10 * time.Minute))
	if err := harness.durable.Update(context.Background(), func(tx ledger.WriteTx) error {
		if _, err := tx.ExecContext(context.Background(), `INSERT INTO extractor_heal_runs (
			id, source_extractor_id, source_version, host, schema_json, schema_hash,
			content_profile, target_template_cluster_id, target_cluster_simhash,
			catalog_revision, candidate_ir, candidate_ir_hash, candidate_ir_format_version,
			validation_report, validation, samples_json, state, terminal_reason,
			lease_id, lease_until, replay_total, replay_matched, replay_ratio,
			promoted_extractor_id, created_at, updated_at, completed_at
		) SELECT ?, source_extractor_id, source_version, host, schema_json, schema_hash,
			content_profile, ?, ?, catalog_revision, candidate_ir, candidate_ir_hash,
			candidate_ir_format_version, validation_report, validation, samples_json,
			'pending', '', NULL, NULL, 0, 0, NULL, NULL, created_at, updated_at, NULL
			FROM extractor_heal_runs WHERE id = ?`, secondRunID, strings.Repeat("e", 64),
			[]byte{0x40, 0, 0, 0, 0, 0, 0, 0xff}, harness.pending.HealRunID); err != nil {
			return err
		}
		_, err := tx.ExecContext(context.Background(), `UPDATE extractor_heal_runs SET
			state = 'replaying', lease_id = ?, lease_until = ?, updated_at = ?
			WHERE id IN (?, ?)`, "00000000-0000-4000-8000-000000000088",
			leaseUntil, now, harness.pending.HealRunID, secondRunID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
