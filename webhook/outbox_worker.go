package webhook

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/use-agent/purify/ledger"
)

const (
	// MaxOutboxDeliveryAttempts includes the initial delivery.
	MaxOutboxDeliveryAttempts    = 4
	DefaultOutboxPollInterval    = time.Second
	DefaultOutboxDeliveryTimeout = 10 * time.Second

	terminalStateRetryDelay = time.Nanosecond
)

var retryBackoff = [...]time.Duration{
	time.Second,
	5 * time.Second,
	30 * time.Second,
}

// OutboxStore is the durable state boundary required by OutboxWorker.
type OutboxStore interface {
	PendingOutbox(context.Context, time.Time, int, int) ([]ledger.OutboxEvent, error)
	MarkOutboxAttempt(context.Context, string, time.Time, time.Time, string) error
	MarkOutboxDelivered(context.Context, string, time.Time) error
	MarkOutboxFailed(context.Context, string, time.Time, string) error
}

// Clock supplies persisted delivery timestamps and due cutoffs.
type Clock interface {
	Now() time.Time
}

// Timer provides the cancelable delay between bounded polling rounds.
type Timer interface {
	Wait(context.Context, time.Duration) error
}

// OutboxWorkerOptions controls bounded polling and supports deterministic tests.
// Zero values select production defaults.
type OutboxWorkerOptions struct {
	Clock           Clock
	Timer           Timer
	PollInterval    time.Duration
	BatchSize       int
	MaximumBytes    int
	DeliveryTimeout time.Duration
	Logger          *slog.Logger
}

// OutboxWorker owns exactly one coordinator goroutine. It has no Submit API;
// durable events are discovered exclusively through OutboxStore.
type OutboxWorker struct {
	store           OutboxStore
	deliverer       Deliverer
	clock           Clock
	timer           Timer
	pollInterval    time.Duration
	batchSize       int
	maximumBytes    int
	deliveryTimeout time.Duration
	logger          *slog.Logger

	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
}

// NewOutboxWorker validates its bounds and immediately starts one coordinator.
func NewOutboxWorker(
	parent context.Context,
	store OutboxStore,
	deliverer Deliverer,
	options OutboxWorkerOptions,
) (*OutboxWorker, error) {
	if parent == nil {
		return nil, fmt.Errorf("%w: parent context is required", ErrInvalidOutboxConfig)
	}
	if store == nil {
		return nil, fmt.Errorf("%w: outbox store is required", ErrInvalidOutboxConfig)
	}
	if deliverer == nil {
		return nil, fmt.Errorf("%w: deliverer is required", ErrInvalidOutboxConfig)
	}
	if options.PollInterval < 0 || options.DeliveryTimeout < 0 ||
		options.BatchSize < 0 || options.MaximumBytes < 0 {
		return nil, fmt.Errorf("%w: worker bounds cannot be negative", ErrInvalidOutboxConfig)
	}
	if options.PollInterval == 0 {
		options.PollInterval = DefaultOutboxPollInterval
	}
	if options.DeliveryTimeout == 0 {
		options.DeliveryTimeout = DefaultOutboxDeliveryTimeout
	}
	if options.BatchSize == 0 {
		options.BatchSize = ledger.MaxPendingOutbox
	}
	if options.MaximumBytes == 0 {
		options.MaximumBytes = ledger.MaxPendingOutboxBytes
	}
	if options.BatchSize > ledger.MaxPendingOutbox {
		return nil, fmt.Errorf(
			"%w: batch size must be between 1 and %d",
			ErrInvalidOutboxConfig,
			ledger.MaxPendingOutbox,
		)
	}
	if options.MaximumBytes < ledger.MaxOutboxPayloadBytes ||
		options.MaximumBytes > ledger.MaxPendingOutboxBytes {
		return nil, fmt.Errorf(
			"%w: maximum bytes must be between %d and %d",
			ErrInvalidOutboxConfig,
			ledger.MaxOutboxPayloadBytes,
			ledger.MaxPendingOutboxBytes,
		)
	}
	if options.Clock == nil {
		options.Clock = systemClock{}
	}
	if options.Timer == nil {
		options.Timer = systemTimer{}
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}

	ctx, cancel := context.WithCancel(parent)
	worker := &OutboxWorker{
		store:           store,
		deliverer:       deliverer,
		clock:           options.Clock,
		timer:           options.Timer,
		pollInterval:    options.PollInterval,
		batchSize:       options.BatchSize,
		maximumBytes:    options.MaximumBytes,
		deliveryTimeout: options.DeliveryTimeout,
		logger:          options.Logger,
		cancel:          cancel,
		done:            make(chan struct{}),
	}
	go worker.run(ctx)
	return worker, nil
}

// Close cancels an in-flight poll or delivery and waits for the coordinator.
// It is safe to call concurrently and repeatedly.
func (w *OutboxWorker) Close() error {
	if w == nil {
		return nil
	}
	w.closeOnce.Do(w.cancel)
	<-w.done
	return nil
}

func (w *OutboxWorker) run(ctx context.Context) {
	defer close(w.done)
	for {
		if ctx.Err() != nil {
			return
		}
		w.poll(ctx)
		if ctx.Err() != nil {
			return
		}
		if err := w.timer.Wait(ctx, w.pollInterval); err != nil {
			if ctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				w.logger.Error("webhook outbox timer failed", "failure", "timer failure")
			}
			return
		}
	}
}

func (w *OutboxWorker) poll(ctx context.Context) {
	events, err := w.store.PendingOutbox(
		ctx,
		w.clock.Now().UTC(),
		w.batchSize,
		w.maximumBytes,
	)
	if err != nil {
		if ctx.Err() == nil {
			w.logger.Warn("webhook outbox poll failed", "failure", "store read failure")
		}
		return
	}
	for _, event := range events {
		if ctx.Err() != nil {
			return
		}
		w.process(ctx, event)
	}
}

func (w *OutboxWorker) process(ctx context.Context, event ledger.OutboxEvent) {
	if event.Attempts >= MaxOutboxDeliveryAttempts {
		if err := w.store.MarkOutboxFailed(
			ctx,
			event.ID,
			w.clock.Now().UTC(),
			"webhook: delivery attempts exhausted",
		); err != nil && ctx.Err() == nil {
			w.logStoreFailure("mark exhausted event", err)
		}
		return
	}
	if permanentError, ok := durablePermanentError(event.LastError); ok {
		if err := w.store.MarkOutboxFailed(ctx, event.ID, w.clock.Now().UTC(), permanentError); err != nil && ctx.Err() == nil {
			w.logStoreFailure("restore permanent failure", err)
		}
		return
	}

	deliveryCtx, cancel := context.WithTimeout(ctx, w.deliveryTimeout)
	result := invokeDeliverer(deliveryCtx, w.deliverer, event)
	cancel()
	if ctx.Err() != nil {
		return
	}
	completedAt := w.clock.Now().UTC()
	if result.Err == nil {
		if err := w.store.MarkOutboxDelivered(ctx, event.ID, completedAt); err != nil && ctx.Err() == nil {
			w.logStoreFailure("mark delivered", err)
		}
		return
	}

	lastError := stableDeliveryError(result)
	attempt := event.Attempts + 1
	nextAttemptAt := completedAt.Add(terminalStateRetryDelay)
	willRetry := result.Retryable && attempt < MaxOutboxDeliveryAttempts
	if willRetry {
		nextAttemptAt = completedAt.Add(retryBackoff[attempt-1])
	}
	if err := w.store.MarkOutboxAttempt(
		ctx,
		event.ID,
		completedAt,
		nextAttemptAt,
		lastError,
	); err != nil {
		if ctx.Err() == nil {
			w.logStoreFailure("mark attempt", err)
		}
		return
	}
	if willRetry || ctx.Err() != nil {
		return
	}
	if err := w.store.MarkOutboxFailed(ctx, event.ID, completedAt, lastError); err != nil && ctx.Err() == nil {
		w.logStoreFailure("mark permanent failure", err)
	}
}

func invokeDeliverer(ctx context.Context, deliverer Deliverer, event ledger.OutboxEvent) (result DeliveryResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = retryableDelivery(errDelivererPanic)
		}
	}()
	return deliverer.Deliver(ctx, event)
}

func stableDeliveryError(result DeliveryResult) string {
	switch value := result.Err.(type) {
	case httpStatusError:
		disposition := "permanent"
		if result.Retryable {
			disposition = "retryable"
		}
		return fmt.Sprintf("webhook: %s HTTP status %d", disposition, value.HTTPStatus())
	case safeDeliveryError:
		return value.Error()
	}
	if result.Retryable {
		return "webhook: retryable delivery failure"
	}
	return "webhook: permanent delivery failure"
}

func durablePermanentError(value string) (string, bool) {
	switch value {
	case errInvalidDelivery.Error(), "webhook: permanent delivery failure":
		return value, true
	}
	const prefix = "webhook: permanent HTTP status "
	if !strings.HasPrefix(value, prefix) {
		return "", false
	}
	status, err := strconv.Atoi(strings.TrimPrefix(value, prefix))
	if err != nil || status < 100 || status > 999 || retryableHTTPStatus(status) {
		return "", false
	}
	return fmt.Sprintf("%s%d", prefix, status), true
}

func (w *OutboxWorker) logStoreFailure(operation string, err error) {
	w.logger.Warn(
		"webhook outbox store write failed",
		"operation", operation,
		"failure", stableStoreFailure(err),
	)
}

func stableStoreFailure(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "context canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context deadline exceeded"
	case errors.Is(err, ledger.ErrClosed):
		return "store closed"
	case errors.Is(err, ledger.ErrOutboxNotFound):
		return "event not found"
	case errors.Is(err, ledger.ErrInvalidOutboxEvent):
		return "invalid outbox state"
	default:
		return "store write failure"
	}
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

type systemTimer struct{}

func (systemTimer) Wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
