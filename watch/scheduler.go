package watch

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"sync"
	"time"

	"github.com/use-agent/purify/models"
)

const (
	// DefaultSchedulerTick is the fixed production polling cadence. Due-ness
	// itself is durable: a restarted scheduler resumes from next_check_at.
	DefaultSchedulerTick = 30 * time.Second
	// DefaultSchedulerClaimsPerTick bounds work started by one tick so a large
	// due backlog drains across ticks instead of monopolizing verification.
	DefaultSchedulerClaimsPerTick = 16
	// schedulerLeaseSafetyMargin is subtracted from the lease deadline so a
	// verification cannot commit against an already-expired capability.
	schedulerLeaseSafetyMargin = 2 * time.Second
	// schedulerReleaseTimeout bounds the failure-path lease release. It is
	// deliberately independent of the scheduler lifecycle context so failed
	// work still reschedules during shutdown.
	schedulerReleaseTimeout = 5 * time.Second
	// schedulerBaseRetryInterval doubles per consecutive failure up to the
	// store's maximum retry horizon.
	schedulerBaseRetryInterval = 10 * time.Minute
	// schedulerMaximumRetryShift caps exponential growth before the interval
	// clamp so the shift itself cannot overflow.
	schedulerMaximumRetryShift = 10

	// Stable last_error_code values. They are part of the public watch
	// projection and must satisfy the durable error-code constraint.
	schedulerCodeClaimInvalid      = "WATCH_CLAIM_INVALID"
	schedulerCodeAnswerFailed      = "WATCH_ANSWER_FAILED"
	schedulerCodeAnswerUnknown     = "WATCH_ANSWER_UNKNOWN"
	schedulerCodeBaselineInvalid   = "WATCH_BASELINE_INVALID"
	schedulerCodeBootstrapGone     = "WATCH_BOOTSTRAP_GONE"
	schedulerCodeBootstrapUnstable = "WATCH_BOOTSTRAP_UNSTABLE"
	schedulerCodeVerifyFailed      = "WATCH_VERIFY_FAILED"
	schedulerCodeVerifyTimeout     = "WATCH_VERIFY_TIMEOUT"
)

// ErrInvalidScheduler reports an unusable scheduler configuration.
var ErrInvalidScheduler = errors.New("watch: invalid scheduler configuration")

// VerificationSubmitter runs one verification and commits it through the
// supplied watch-scoped recorder. The production implementation adapts
// verify.Service.VerifyWithRecorder.
type VerificationSubmitter interface {
	VerifyWithRecorder(context.Context, models.VerifyRequest, *VerificationRecorder) (*models.VerifyResponse, error)
}

// BaselineAnswerer produces the evidence-backed belief used to begin a watch.
// The production implementation is the Answer service.
type BaselineAnswerer interface {
	Answer(context.Context, *models.AnswerRequest) (*models.AnswerResponse, error)
}

// SchedulerOptions controls polling and logging. Zero values select fixed
// production defaults.
type SchedulerOptions struct {
	Tick          time.Duration
	ClaimsPerTick int
	Logger        *slog.Logger
}

// Scheduler owns one durable polling goroutine that claims due watches and
// drives them through the verification pipeline. All fact and schedule state
// changes happen inside the recorder's ledger transaction; the scheduler
// itself only claims, submits, and releases failed leases.
type Scheduler struct {
	store         *Store
	verifier      VerificationSubmitter
	answerer      BaselineAnswerer
	tick          time.Duration
	claimsPerTick int
	logger        *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc

	wg        sync.WaitGroup
	closeOnce sync.Once
}

// NewScheduler starts exactly one polling goroutine. It performs no work
// before the first tick beyond an immediate drain of already-due watches.
func NewScheduler(
	parent context.Context,
	store *Store,
	verifier VerificationSubmitter,
	answerer BaselineAnswerer,
	options SchedulerOptions,
) (*Scheduler, error) {
	if parent == nil || !validRecorderStore(store) ||
		isNilSchedulerDependency(verifier) || isNilSchedulerDependency(answerer) {
		return nil, ErrInvalidScheduler
	}
	if err := parent.Err(); err != nil {
		return nil, err
	}
	if options.Tick < 0 || options.ClaimsPerTick < 0 {
		return nil, ErrInvalidScheduler
	}
	if options.Tick == 0 {
		options.Tick = DefaultSchedulerTick
	}
	if options.ClaimsPerTick == 0 {
		options.ClaimsPerTick = DefaultSchedulerClaimsPerTick
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}

	lifecycle, cancel := context.WithCancel(parent)
	scheduler := &Scheduler{
		store:         store,
		verifier:      verifier,
		answerer:      answerer,
		tick:          options.Tick,
		claimsPerTick: options.ClaimsPerTick,
		logger:        options.Logger,
		ctx:           lifecycle,
		cancel:        cancel,
	}
	scheduler.wg.Add(1)
	go scheduler.run()
	return scheduler, nil
}

// Close stops polling and waits for in-flight claim work to finish. It is
// idempotent and safe on a nil scheduler.
func (scheduler *Scheduler) Close() error {
	if scheduler == nil {
		return nil
	}
	scheduler.closeOnce.Do(func() {
		scheduler.cancel()
		scheduler.wg.Wait()
	})
	return nil
}

func (scheduler *Scheduler) run() {
	defer scheduler.wg.Done()
	scheduler.drain()
	ticker := time.NewTicker(scheduler.tick)
	defer ticker.Stop()
	for {
		select {
		case <-scheduler.ctx.Done():
			return
		case <-ticker.C:
			scheduler.drain()
		}
	}
}

// drain claims and processes due watches until the tick budget is spent, no
// work remains, or the scheduler is closing.
func (scheduler *Scheduler) drain() {
	for processed := 0; processed < scheduler.claimsPerTick; processed++ {
		if scheduler.ctx.Err() != nil {
			return
		}
		claim, found, err := scheduler.store.ClaimDue(scheduler.ctx)
		if err != nil {
			if !isSchedulerShutdownError(err) {
				scheduler.logger.Warn("watch claim failed", "error", err)
			}
			return
		}
		if !found {
			return
		}
		scheduler.process(claim)
	}
}

func (scheduler *Scheduler) process(claim Claim) {
	// The budget is computed against the store clock and applied as a wall
	// duration, so lease semantics survive an injected test clock unchanged.
	now, err := scheduler.store.operationTime()
	if err != nil {
		scheduler.release(claim, schedulerCodeClaimInvalid)
		return
	}
	remaining := claim.Lease.Until.Sub(now) - schedulerLeaseSafetyMargin
	if remaining <= 0 {
		scheduler.release(claim, schedulerCodeClaimInvalid)
		return
	}
	ctx, cancel := context.WithTimeout(scheduler.ctx, remaining)
	defer cancel()

	switch {
	case claim.Fact != nil && claim.Watch.State == StateActive:
		scheduler.verifyFact(ctx, claim)
	case claim.Fact == nil && (claim.Watch.State == StatePending || claim.Watch.State == StateActive):
		scheduler.bootstrap(ctx, claim)
	default:
		scheduler.release(claim, schedulerCodeClaimInvalid)
	}
}

// verifyFact revisits the open fact through its own portable receipt. The
// recorder refreshes, supersedes, or closes the fact atomically.
func (scheduler *Scheduler) verifyFact(ctx context.Context, claim Claim) {
	recorder, err := NewFactVerificationRecorder(scheduler.store, claim)
	if err != nil {
		scheduler.release(claim, schedulerCodeClaimInvalid)
		return
	}
	result, committed := scheduler.submit(ctx, models.VerifyRequest{Receipt: claim.Fact.Receipt}, recorder, claim)
	if committed && !result.LeaseReleased {
		// Fact-mode commits always update the watch schedule; reaching this
		// branch means a durable invariant broke, so surrender the lease.
		scheduler.release(claim, schedulerCodeVerifyFailed)
	}
}

// bootstrap obtains an evidence-backed baseline from Answer and verifies it.
// A changed first verification permits exactly one follow-up bound to the
// refreshed evidence; a second change abandons the attempt until the retry.
func (scheduler *Scheduler) bootstrap(ctx context.Context, claim Claim) {
	response, err := scheduler.answerer.Answer(ctx, &models.AnswerRequest{Spec: claim.Watch.Spec})
	if err != nil || response == nil {
		code := schedulerCodeAnswerFailed
		if err != nil && isSchedulerTimeout(ctx, err) {
			code = schedulerCodeVerifyTimeout
		}
		scheduler.release(claim, code)
		return
	}
	if response.Status != models.AnswerStatusKnown || response.Belief == nil {
		scheduler.release(claim, schedulerCodeAnswerUnknown)
		return
	}

	recorder, baseline, ok := scheduler.bootstrapRecorder(claim, response.Belief)
	if !ok {
		scheduler.release(claim, schedulerCodeBaselineInvalid)
		return
	}
	result, committed := scheduler.submit(ctx, models.VerifyRequest{Receipt: baseline.Receipt}, recorder, claim)
	if !committed {
		return
	}
	if result.Retry == nil {
		scheduler.finishBootstrap(claim, result)
		return
	}

	followUp, err := NewBootstrapVerificationRecorder(scheduler.store, claim, *result.Retry)
	if err != nil {
		scheduler.release(claim, schedulerCodeBaselineInvalid)
		return
	}
	retried, committed := scheduler.submit(ctx, models.VerifyRequest{Receipt: result.Retry.Receipt}, followUp, claim)
	if !committed {
		return
	}
	if retried.Retry != nil {
		scheduler.release(claim, schedulerCodeBootstrapUnstable)
		return
	}
	scheduler.finishBootstrap(claim, retried)
}

func (scheduler *Scheduler) finishBootstrap(claim Claim, result VerificationRecordResult) {
	if result.LeaseReleased {
		return
	}
	// A gone verdict leaves the watch untouched: no fact exists yet, so the
	// only durable consequence is a bounded retry.
	scheduler.release(claim, schedulerCodeBootstrapGone)
}

// bootstrapRecorder binds the first belief evidence whose receipt yields a
// canonical baseline. Evidence order follows the winning consensus supports.
func (scheduler *Scheduler) bootstrapRecorder(
	claim Claim,
	belief *models.AnswerBelief,
) (*VerificationRecorder, VerificationBaseline, bool) {
	for _, item := range belief.Evidence {
		receipt := belief.Receipts[item.URL]
		if receipt == "" {
			continue
		}
		baseline := VerificationBaseline{
			Path:       escapedPredicatePath(claim.Watch.Spec.Predicate),
			Value:      belief.Value,
			SourceURL:  item.URL,
			SnapshotID: item.SnapshotID,
			Receipt:    receipt,
		}
		recorder, err := NewBootstrapVerificationRecorder(scheduler.store, claim, baseline)
		if err == nil {
			return recorder, baseline, true
		}
	}
	return nil, VerificationBaseline{}, false
}

// submit runs one verification. The committed result is authoritative even
// when the verifier returns an error after its transaction: durable effects
// are never re-released. A failed, uncommitted submission releases the lease.
func (scheduler *Scheduler) submit(
	ctx context.Context,
	request models.VerifyRequest,
	recorder *VerificationRecorder,
	claim Claim,
) (VerificationRecordResult, bool) {
	_, err := scheduler.verifier.VerifyWithRecorder(ctx, request, recorder)
	if result, committed := recorder.Result(); committed {
		return result, true
	}
	code := schedulerCodeVerifyFailed
	if isSchedulerTimeout(ctx, err) {
		code = schedulerCodeVerifyTimeout
	}
	scheduler.release(claim, code)
	return VerificationRecordResult{}, false
}

// release reschedules a failed claim with bounded exponential backoff. The
// release context is independent of the scheduler lifecycle so shutdown
// cannot strand a claimed watch until its lease expires.
func (scheduler *Scheduler) release(claim Claim, code string) {
	now, err := scheduler.store.operationTime()
	if err != nil {
		scheduler.logger.Warn("watch release clock failed", "watch", claim.Watch.ID, "error", err)
		return
	}
	retryAt := now.Add(schedulerRetryInterval(claim.Watch.ConsecutiveFailures))
	ctx, cancel := context.WithTimeout(context.Background(), schedulerReleaseTimeout)
	defer cancel()
	released, err := scheduler.store.ReleaseLease(ctx, claim.Lease, retryAt, code)
	if err != nil {
		scheduler.logger.Warn("watch release failed", "watch", claim.Watch.ID, "code", code, "error", err)
		return
	}
	if released {
		scheduler.logger.Info("watch rescheduled after failure", "watch", claim.Watch.ID, "code", code)
	}
}

func schedulerRetryInterval(consecutiveFailures int) time.Duration {
	shift := consecutiveFailures
	if shift < 0 {
		shift = 0
	}
	if shift > schedulerMaximumRetryShift {
		shift = schedulerMaximumRetryShift
	}
	interval := schedulerBaseRetryInterval << shift
	if interval > maximumRetryInterval {
		return maximumRetryInterval
	}
	return interval
}

func isSchedulerTimeout(ctx context.Context, err error) bool {
	if ctx != nil && ctx.Err() != nil {
		return true
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func isSchedulerShutdownError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func isNilSchedulerDependency(dependency any) bool {
	if dependency == nil {
		return true
	}
	value := reflect.ValueOf(dependency)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
