package compiler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/use-agent/purify/ledger"
)

const DefaultHealWorkerPollInterval = time.Second

var (
	ErrInvalidHealWorker        = errors.New("compiler: invalid heal worker configuration")
	ErrHealWorkerClosed         = errors.New("compiler: heal worker is closed")
	ErrInvalidHealSchedule      = errors.New("compiler: invalid extractor heal schedule")
	ErrHealExtractorNotFound    = errors.New("compiler: extractor for heal scheduling not found")
	ErrHealScheduleNotReady     = errors.New("compiler: extractor heal schedule is not ready")
	ErrHealScheduleAmbiguous    = errors.New("compiler: extractor heal schedule is ambiguous")
	errHealWorkerPanicRecovered = errors.New("compiler: heal worker recovered a task panic")
)

// HealSchedule is the stable ticket returned when a durable run is already
// pending or replaying for one exact source extractor revision.
type HealSchedule struct {
	ExtractorID string
	HealRunID   string
}

// HealWorkerOptions controls polling and logging. Zero values select fixed
// production defaults; work and concurrency limits are not configurable.
type HealWorkerOptions struct {
	PollInterval time.Duration
	Logger       *slog.Logger
}

// HealWorker owns one durable polling goroutine. The wake channel is only a
// coalesced hint: run identity and lifecycle always come from the ledger.
type HealWorker struct {
	healer       *Healer
	ledger       *ledger.Store
	clock        func() time.Time
	pollInterval time.Duration
	logger       *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}

	mu        sync.Mutex
	closed    bool
	wg        sync.WaitGroup
	closeOnce sync.Once
	done      chan struct{}
}

// NewHealWorker starts exactly one durable replay worker. Snapshot reads and
// Healer replay always happen after the polling read transaction has ended.
func NewHealWorker(
	parent context.Context,
	healer *Healer,
	options HealWorkerOptions,
) (*HealWorker, error) {
	if parent == nil {
		return nil, fmt.Errorf("%w: parent context is nil", ErrInvalidHealWorker)
	}
	if healer == nil {
		return nil, fmt.Errorf("%w: healer is nil", ErrInvalidHealWorker)
	}
	if err := healer.validateContext(parent); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: healer is uninitialized", ErrInvalidHealWorker)
	}
	if options.PollInterval < 0 {
		return nil, fmt.Errorf("%w: poll interval cannot be negative", ErrInvalidHealWorker)
	}
	if options.PollInterval == 0 {
		options.PollInterval = DefaultHealWorkerPollInterval
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}

	lifecycle, cancel := context.WithCancel(parent)
	worker := &HealWorker{
		healer: healer, ledger: healer.ledger, clock: healer.clock,
		pollInterval: options.PollInterval, logger: options.Logger,
		ctx: lifecycle, cancel: cancel, wake: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
	worker.wg.Add(1)
	go worker.run()
	return worker, nil
}

// ScheduleExtractor resolves an actionable durable run using the exact
// (source_extractor_id, source_version) stored on the current extractor row.
// A live replay lease is already accepted and returns the same ticket. No
// CompileKey is inferred from an extractor identifier.
func (w *HealWorker) ScheduleExtractor(ctx context.Context, extractorID string) (HealSchedule, error) {
	if ctx == nil {
		return HealSchedule{}, fmt.Errorf("%w: context is nil", ErrInvalidHealSchedule)
	}
	if err := ctx.Err(); err != nil {
		return HealSchedule{}, err
	}
	if !validExtractorID(extractorID) {
		return HealSchedule{}, fmt.Errorf("%w: extractor ID is invalid", ErrInvalidHealSchedule)
	}

	queryCtx, finish, err := w.beginSchedule(ctx)
	if err != nil {
		return HealSchedule{}, err
	}
	defer finish()

	runIDs := make([]string, 0, 2)
	err = w.ledger.View(queryCtx, func(tx ledger.ReadTx) error {
		var (
			sourceVersion int
			storedState   string
			staleReason   string
		)
		scanErr := tx.QueryRowContext(queryCtx, `SELECT version, state, stale_reason
			FROM extractors WHERE id = ?`, extractorID).Scan(
			&sourceVersion, &storedState, &staleReason,
		)
		if errors.Is(scanErr, sql.ErrNoRows) {
			return ErrHealExtractorNotFound
		}
		if scanErr != nil {
			return fmt.Errorf("compiler: query extractor heal source: %w", scanErr)
		}
		sourceState := ExtractorState(storedState)
		if sourceVersion < 1 || !validStoredState(sourceState, staleReason) {
			return storeCorruption("extractor heal source contains invalid lifecycle metadata")
		}
		if sourceState != StateStale {
			return ErrHealScheduleNotReady
		}

		rows, err := tx.QueryContext(queryCtx, `SELECT id FROM extractor_heal_runs
			WHERE source_extractor_id = ? AND source_version = ?
				AND state IN ('pending', 'replaying')
			ORDER BY created_at DESC, id DESC LIMIT 2`, extractorID, sourceVersion)
		if err != nil {
			return fmt.Errorf("compiler: query extractor heal schedule: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var runID string
			if err := rows.Scan(&runID); err != nil {
				return fmt.Errorf("compiler: scan extractor heal schedule: %w", err)
			}
			if !validExtractorID(runID) {
				return storeCorruption("extractor heal schedule contains an invalid run ID")
			}
			runIDs = append(runIDs, runID)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("compiler: iterate extractor heal schedule: %w", err)
		}
		return nil
	})
	if err != nil {
		if callerErr := ctx.Err(); callerErr != nil {
			return HealSchedule{}, callerErr
		}
		if w.ctx.Err() != nil {
			return HealSchedule{}, ErrHealWorkerClosed
		}
		return HealSchedule{}, err
	}
	switch len(runIDs) {
	case 0:
		return HealSchedule{}, ErrHealScheduleNotReady
	case 1:
		w.signal()
		return HealSchedule{ExtractorID: extractorID, HealRunID: runIDs[0]}, nil
	default:
		return HealSchedule{}, ErrHealScheduleAmbiguous
	}
}

func (w *HealWorker) beginSchedule(caller context.Context) (context.Context, func(), error) {
	if w == nil || w.healer == nil || w.ledger == nil || w.clock == nil || w.cancel == nil ||
		w.wake == nil || w.done == nil || w.pollInterval <= 0 || w.logger == nil {
		return nil, nil, fmt.Errorf("%w: worker is nil or uninitialized", ErrInvalidHealWorker)
	}
	w.mu.Lock()
	if w.closed || w.ctx == nil || w.ctx.Err() != nil {
		w.mu.Unlock()
		return nil, nil, ErrHealWorkerClosed
	}
	w.wg.Add(1)
	lifecycle := w.ctx
	w.mu.Unlock()

	ctx, cancel := context.WithCancel(lifecycle)
	stopCaller := context.AfterFunc(caller, cancel)
	finish := func() {
		stopCaller()
		cancel()
		w.wg.Done()
	}
	return ctx, finish, nil
}

func (w *HealWorker) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *HealWorker) run() {
	defer w.wg.Done()
	for {
		if w.ctx.Err() != nil {
			return
		}
		progressed, err := w.processNext()
		if w.ctx.Err() != nil {
			return
		}
		if err != nil {
			w.logger.Warn("extractor heal worker iteration failed", "failure", stableHealWorkerFailure(err))
		}
		if progressed && err == nil {
			continue
		}
		if !w.wait() {
			return
		}
	}
}

func (w *HealWorker) processNext() (progressed bool, err error) {
	now, err := normalizeCatalogTime(w.clock())
	if err != nil {
		return false, fmt.Errorf("%w: worker clock is invalid", ErrInvalidHealWorker)
	}
	runID := ""
	err = w.ledger.View(w.ctx, func(tx ledger.ReadTx) error {
		scanErr := tx.QueryRowContext(w.ctx, `SELECT id FROM extractor_heal_runs
			WHERE state IN ('pending', 'replaying') AND
				(state = 'pending' OR lease_until <= ?)
			ORDER BY created_at, id LIMIT 1`, formatExtractorTime(now)).Scan(&runID)
		if errors.Is(scanErr, sql.ErrNoRows) {
			return nil
		}
		if scanErr != nil {
			return fmt.Errorf("compiler: poll extractor heal run: %w", scanErr)
		}
		if !validExtractorID(runID) {
			return storeCorruption("extractor heal poll returned an invalid run ID")
		}
		return nil
	})
	if err != nil || runID == "" {
		return false, err
	}

	_, err = invokeHealRun(w.ctx, w.healer, runID)
	if err == nil || errors.Is(err, ErrHealLeaseBusy) || errors.Is(err, ErrHealLeaseLost) ||
		errors.Is(err, ErrHealRunNotFound) {
		return true, nil
	}
	return false, err
}

func invokeHealRun(ctx context.Context, healer *Healer, runID string) (result HealResult, err error) {
	defer func() {
		if recover() != nil {
			result = HealResult{}
			err = errHealWorkerPanicRecovered
		}
	}()
	return healer.HealRun(ctx, runID)
}

func (w *HealWorker) wait() bool {
	timer := time.NewTimer(w.pollInterval)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()
	select {
	case <-w.ctx.Done():
		return false
	case <-w.wake:
		return true
	case <-timer.C:
		return true
	}
}

// Close cancels polling and replay, waits for accepted ScheduleExtractor calls,
// and rejects all later scheduling. It is safe to call repeatedly.
func (w *HealWorker) Close() error {
	if w == nil {
		return nil
	}
	if w.cancel == nil || w.done == nil {
		return fmt.Errorf("%w: worker is uninitialized", ErrInvalidHealWorker)
	}
	w.closeOnce.Do(func() {
		w.mu.Lock()
		w.closed = true
		w.cancel()
		w.mu.Unlock()
		w.wg.Wait()
		close(w.done)
	})
	<-w.done
	return nil
}

func stableHealWorkerFailure(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "context canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "task timeout"
	case errors.Is(err, ledger.ErrClosed):
		return "store closed"
	case errors.Is(err, ErrExtractorStore):
		return "stored state invalid"
	case errors.Is(err, ErrInvalidHealer), errors.Is(err, ErrInvalidHealWorker):
		return "worker configuration invalid"
	case errors.Is(err, errHealWorkerPanicRecovered):
		return "task panic recovered"
	default:
		return "heal task failure"
	}
}
