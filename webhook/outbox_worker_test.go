package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/use-agent/purify/ledger"
)

func TestNewOutboxWorkerValidatesBounds(t *testing.T) {
	store := newMemoryOutboxStore()
	deliverer := delivererFunc(func(context.Context, ledger.OutboxEvent) DeliveryResult { return DeliveryResult{} })
	tests := []struct {
		name      string
		parent    context.Context
		store     OutboxStore
		deliverer Deliverer
		options   OutboxWorkerOptions
	}{
		{name: "nil context", store: store, deliverer: deliverer},
		{name: "nil store", parent: context.Background(), deliverer: deliverer},
		{name: "nil deliverer", parent: context.Background(), store: store},
		{name: "negative poll", parent: context.Background(), store: store, deliverer: deliverer, options: OutboxWorkerOptions{PollInterval: -1}},
		{name: "negative timeout", parent: context.Background(), store: store, deliverer: deliverer, options: OutboxWorkerOptions{DeliveryTimeout: -1}},
		{name: "large batch", parent: context.Background(), store: store, deliverer: deliverer, options: OutboxWorkerOptions{BatchSize: ledger.MaxPendingOutbox + 1}},
		{name: "small bytes", parent: context.Background(), store: store, deliverer: deliverer, options: OutboxWorkerOptions{MaximumBytes: ledger.MaxOutboxPayloadBytes - 1}},
		{name: "large bytes", parent: context.Background(), store: store, deliverer: deliverer, options: OutboxWorkerOptions{MaximumBytes: ledger.MaxPendingOutboxBytes + 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewOutboxWorker(test.parent, test.store, test.deliverer, test.options); !errors.Is(err, ErrInvalidOutboxConfig) {
				t.Fatalf("NewOutboxWorker() error = %v", err)
			}
		})
	}
}

func TestOutboxWorkerPollsDueEventsOneBoundedPageAtATime(t *testing.T) {
	now := outboxWorkerTestTime()
	clock := newTestClock(now)
	timer := newManualTimer()
	store := newMemoryOutboxStore(
		workerEvent("event-1", now),
		workerEvent("event-2", now),
		workerEvent("event-3", now),
		workerEvent("event-future", now.Add(time.Hour)),
	)
	var callsMu sync.Mutex
	var calls []string
	deliverer := delivererFunc(func(_ context.Context, event ledger.OutboxEvent) DeliveryResult {
		callsMu.Lock()
		calls = append(calls, event.ID)
		callsMu.Unlock()
		return DeliveryResult{}
	})
	worker := newTestWorker(t, store, deliverer, clock, timer, OutboxWorkerOptions{
		BatchSize:    2,
		MaximumBytes: ledger.MaxOutboxPayloadBytes,
	})

	awaitWorkerWait(t, timer, time.Second)
	assertDelivered(t, store, "event-1", true)
	assertDelivered(t, store, "event-2", true)
	assertDelivered(t, store, "event-3", false)
	queries := store.pendingCallsSnapshot()
	if len(queries) != 1 || !queries[0].dueAt.Equal(now) || queries[0].limit != 2 ||
		queries[0].maximumBytes != ledger.MaxOutboxPayloadBytes {
		t.Fatalf("first pending calls = %#v", queries)
	}

	releaseWorkerWait(timer)
	awaitWorkerWait(t, timer, time.Second)
	assertDelivered(t, store, "event-3", true)
	assertDelivered(t, store, "event-future", false)

	clock.Set(now.Add(time.Hour))
	releaseWorkerWait(timer)
	awaitWorkerWait(t, timer, time.Second)
	assertDelivered(t, store, "event-future", true)
	if err := worker.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	callsMu.Lock()
	defer callsMu.Unlock()
	want := []string{"event-1", "event-2", "event-3", "event-future"}
	if fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Fatalf("delivery calls = %v, want %v", calls, want)
	}
}

func TestOutboxWorkerPersistsExactAttemptsBackoffAndTerminalFailure(t *testing.T) {
	now := outboxWorkerTestTime()
	clock := newTestClock(now)
	timer := newManualTimer()
	store := newMemoryOutboxStore(workerEvent("event-retry", now))
	var deliveries atomic.Int32
	deliverer := delivererFunc(func(context.Context, ledger.OutboxEvent) DeliveryResult {
		deliveries.Add(1)
		return DeliveryResult{
			Err:       errors.New("https://private.example/hook?secret=must-not-persist"),
			Retryable: true,
		}
	})
	worker := newTestWorker(t, store, deliverer, clock, timer, OutboxWorkerOptions{})

	awaitWorkerWait(t, timer, time.Second)
	clock.Set(now.Add(time.Second))
	releaseWorkerWait(timer)
	awaitWorkerWait(t, timer, time.Second)
	clock.Set(now.Add(6 * time.Second))
	releaseWorkerWait(timer)
	awaitWorkerWait(t, timer, time.Second)
	clock.Set(now.Add(36 * time.Second))
	releaseWorkerWait(timer)
	awaitWorkerWait(t, timer, time.Second)
	if err := worker.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if got := deliveries.Load(); got != MaxOutboxDeliveryAttempts {
		t.Fatalf("deliveries = %d, want %d", got, MaxOutboxDeliveryAttempts)
	}
	attempts := store.attemptCallsSnapshot()
	if len(attempts) != MaxOutboxDeliveryAttempts {
		t.Fatalf("attempt calls = %#v", attempts)
	}
	wantAttemptedAt := []time.Time{now, now.Add(time.Second), now.Add(6 * time.Second), now.Add(36 * time.Second)}
	wantNextAttemptAt := []time.Time{now.Add(time.Second), now.Add(6 * time.Second), now.Add(36 * time.Second), now.Add(36*time.Second + time.Nanosecond)}
	for index := range attempts {
		if !attempts[index].attemptedAt.Equal(wantAttemptedAt[index]) ||
			!attempts[index].nextAttemptAt.Equal(wantNextAttemptAt[index]) {
			t.Fatalf("attempt[%d] = %#v, want attempted=%v next=%v", index, attempts[index], wantAttemptedAt[index], wantNextAttemptAt[index])
		}
		if attempts[index].lastError != "webhook: retryable delivery failure" {
			t.Fatalf("attempt[%d] last_error = %q", index, attempts[index].lastError)
		}
	}
	event := store.eventSnapshot(t, "event-retry")
	if event.Attempts != MaxOutboxDeliveryAttempts || event.FailedAt == nil || !event.FailedAt.Equal(now.Add(36*time.Second)) ||
		event.DeliveredAt != nil || strings.Contains(event.LastError, "secret=") {
		t.Fatalf("terminal event = %#v", event)
	}
}

func TestOutboxWorkerPermanentFailureIsOneAttemptAndRedacted(t *testing.T) {
	now := outboxWorkerTestTime()
	clock := newTestClock(now)
	timer := newManualTimer()
	store := newMemoryOutboxStore(workerEvent("event-permanent", now))
	var logs bytes.Buffer
	deliverer := delivererFunc(func(context.Context, ledger.OutboxEvent) DeliveryResult {
		return DeliveryResult{Err: errors.New("secret-value https://host/hook?token=private")}
	})
	worker := newTestWorker(t, store, deliverer, clock, timer, OutboxWorkerOptions{Logger: testLogger(&logs)})
	awaitWorkerWait(t, timer, time.Second)
	if err := worker.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	event := store.eventSnapshot(t, "event-permanent")
	if event.Attempts != 1 || event.FailedAt == nil || event.LastError != "webhook: permanent delivery failure" {
		t.Fatalf("permanent event = %#v", event)
	}
	if strings.Contains(logs.String(), "secret-value") || strings.Contains(logs.String(), "token=private") {
		t.Fatalf("logs leaked delivery detail: %s", logs.String())
	}
}

func TestOutboxWorkerDoesNotRedeliverExhaustedEventAfterRestart(t *testing.T) {
	now := outboxWorkerTestTime()
	event := workerEvent("event-exhausted", now)
	event.Attempts = MaxOutboxDeliveryAttempts
	lastAttempt := now.Add(-time.Second)
	event.LastAttemptAt = &lastAttempt
	event.LastError = "https://private.example/hook?secret=must-not-persist"
	store := newMemoryOutboxStore(event)
	clock := newTestClock(now)
	timer := newManualTimer()
	var deliveries atomic.Int32
	worker := newTestWorker(t, store, delivererFunc(func(context.Context, ledger.OutboxEvent) DeliveryResult {
		deliveries.Add(1)
		return DeliveryResult{}
	}), clock, timer, OutboxWorkerOptions{})
	awaitWorkerWait(t, timer, time.Second)
	if err := worker.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if deliveries.Load() != 0 {
		t.Fatalf("exhausted event deliveries = %d, want 0", deliveries.Load())
	}
	stored := store.eventSnapshot(t, event.ID)
	if stored.FailedAt == nil || stored.Attempts != MaxOutboxDeliveryAttempts ||
		stored.LastError != "webhook: delivery attempts exhausted" {
		t.Fatalf("exhausted event = %#v", stored)
	}
}

func TestOutboxWorkerRecoversDeliveryPanicAndContinues(t *testing.T) {
	now := outboxWorkerTestTime()
	clock := newTestClock(now)
	timer := newManualTimer()
	store := newMemoryOutboxStore(workerEvent("event-panic", now), workerEvent("event-after-panic", now))
	var logs bytes.Buffer
	var callsMu sync.Mutex
	var calls []string
	deliverer := delivererFunc(func(_ context.Context, event ledger.OutboxEvent) DeliveryResult {
		callsMu.Lock()
		calls = append(calls, event.ID)
		currentCalls := len(calls)
		callsMu.Unlock()
		if event.ID == "event-panic" && currentCalls == 1 {
			panic("secret-value https://host/hook?token=private")
		}
		return DeliveryResult{}
	})
	worker := newTestWorker(t, store, deliverer, clock, timer, OutboxWorkerOptions{Logger: testLogger(&logs)})
	awaitWorkerWait(t, timer, time.Second)
	panicEvent := store.eventSnapshot(t, "event-panic")
	if panicEvent.Attempts != 1 || panicEvent.LastError != "webhook: deliverer panic" {
		t.Fatalf("panic event after first poll = %#v", panicEvent)
	}
	assertDelivered(t, store, "event-after-panic", true)
	clock.Set(now.Add(time.Second))
	releaseWorkerWait(timer)
	awaitWorkerWait(t, timer, time.Second)
	assertDelivered(t, store, "event-panic", true)
	if err := worker.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if strings.Contains(logs.String(), "secret-value") || strings.Contains(logs.String(), "token=private") {
		t.Fatalf("logs leaked panic detail: %s", logs.String())
	}
}

func TestOutboxWorkerStoreFailuresLeaveEventsDurableAndContinue(t *testing.T) {
	t.Run("pending", func(t *testing.T) {
		now := outboxWorkerTestTime()
		clock := newTestClock(now)
		timer := newManualTimer()
		store := newMemoryOutboxStore(workerEvent("event-pending-recovery", now))
		store.pendingErrors = []error{errors.New("store https://host?token=private")}
		var deliveries atomic.Int32
		var logs bytes.Buffer
		worker := newTestWorker(t, store, delivererFunc(func(context.Context, ledger.OutboxEvent) DeliveryResult {
			deliveries.Add(1)
			return DeliveryResult{}
		}), clock, timer, OutboxWorkerOptions{Logger: testLogger(&logs)})
		awaitWorkerWait(t, timer, time.Second)
		if deliveries.Load() != 0 {
			t.Fatalf("deliveries after failed poll = %d", deliveries.Load())
		}
		releaseWorkerWait(timer)
		awaitWorkerWait(t, timer, time.Second)
		assertDelivered(t, store, "event-pending-recovery", true)
		_ = worker.Close()
		assertLogRedacted(t, logs.String())
	})

	t.Run("attempt", func(t *testing.T) {
		now := outboxWorkerTestTime()
		clock := newTestClock(now)
		timer := newManualTimer()
		store := newMemoryOutboxStore(workerEvent("event-attempt-mark-error", now), workerEvent("event-after-error", now))
		store.attemptErrors["event-attempt-mark-error"] = []error{errors.New("store https://host?token=private")}
		var logs bytes.Buffer
		worker := newTestWorker(t, store, delivererFunc(func(_ context.Context, event ledger.OutboxEvent) DeliveryResult {
			if event.ID == "event-attempt-mark-error" {
				return retryableDelivery(errors.New("private transport detail"))
			}
			return DeliveryResult{}
		}), clock, timer, OutboxWorkerOptions{Logger: testLogger(&logs)})
		awaitWorkerWait(t, timer, time.Second)
		failedMark := store.eventSnapshot(t, "event-attempt-mark-error")
		if failedMark.Attempts != 0 || failedMark.DeliveredAt != nil || failedMark.FailedAt != nil {
			t.Fatalf("event with failed mark was lost: %#v", failedMark)
		}
		assertDelivered(t, store, "event-after-error", true)
		_ = worker.Close()
		assertLogRedacted(t, logs.String())
	})

	t.Run("terminal", func(t *testing.T) {
		now := outboxWorkerTestTime()
		clock := newTestClock(now)
		timer := newManualTimer()
		store := newMemoryOutboxStore(workerEvent("event-terminal-mark-error", now), workerEvent("event-after-terminal-error", now))
		store.failedErrors["event-terminal-mark-error"] = []error{errors.New("store https://host?token=private")}
		var logs bytes.Buffer
		var terminalDeliveries atomic.Int32
		worker := newTestWorker(t, store, delivererFunc(func(_ context.Context, event ledger.OutboxEvent) DeliveryResult {
			if event.ID == "event-terminal-mark-error" {
				terminalDeliveries.Add(1)
				return permanentDelivery(httpStatusError(400))
			}
			return DeliveryResult{}
		}), clock, timer, OutboxWorkerOptions{Logger: testLogger(&logs)})
		awaitWorkerWait(t, timer, time.Second)
		failedMark := store.eventSnapshot(t, "event-terminal-mark-error")
		if failedMark.Attempts != 1 || failedMark.FailedAt != nil || failedMark.LastError != "webhook: permanent HTTP status 400" {
			t.Fatalf("terminal mark failure event = %#v", failedMark)
		}
		assertDelivered(t, store, "event-after-terminal-error", true)
		clock.Set(now.Add(time.Nanosecond))
		releaseWorkerWait(timer)
		awaitWorkerWait(t, timer, time.Second)
		failedMark = store.eventSnapshot(t, "event-terminal-mark-error")
		if failedMark.FailedAt == nil || terminalDeliveries.Load() != 1 {
			t.Fatalf("permanent recovery redelivered: event=%#v deliveries=%d", failedMark, terminalDeliveries.Load())
		}
		_ = worker.Close()
		assertLogRedacted(t, logs.String())
	})
}

func TestOutboxWorkerRestartRecoversLedgerAttempt(t *testing.T) {
	now := outboxWorkerTestTime()
	directory := t.TempDir()
	store, err := ledger.Open(directory)
	if err != nil {
		t.Fatalf("ledger.Open() error = %v", err)
	}
	row := ledger.Verification{
		ID:             "row-restart",
		VerificationID: "verification-restart",
		ClaimIndex:     0,
		URL:            "https://example.com/item",
		Path:           "/price",
		OldValue:       json.RawMessage(`"old"`),
		NewValue:       json.RawMessage(`"old"`),
		Outcome:        ledger.OutcomeConfirmed,
		OldSnapshotID:  "sha256:old",
		VerifiedAt:     now,
	}
	event := workerEvent("event-restart", now)
	event.VerificationID = row.VerificationID
	if err := store.RecordVerificationBatch(context.Background(), []ledger.Verification{row}, &event); err != nil {
		t.Fatalf("RecordVerificationBatch() error = %v", err)
	}

	clock := newTestClock(now)
	firstTimer := newManualTimer()
	first := newTestWorker(t, store, delivererFunc(func(context.Context, ledger.OutboxEvent) DeliveryResult {
		return retryableDelivery(errors.New("network detail"))
	}), clock, firstTimer, OutboxWorkerOptions{})
	awaitWorkerWait(t, firstTimer, time.Second)
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("first ledger Close() error = %v", err)
	}

	store, err = ledger.Open(directory)
	if err != nil {
		t.Fatalf("ledger.Open(restart) error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	clock.Set(now.Add(time.Second))
	secondTimer := newManualTimer()
	var delivered atomic.Int32
	second := newTestWorker(t, store, delivererFunc(func(context.Context, ledger.OutboxEvent) DeliveryResult {
		delivered.Add(1)
		return DeliveryResult{}
	}), clock, secondTimer, OutboxWorkerOptions{})
	awaitWorkerWait(t, secondTimer, time.Second)
	if err := second.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if delivered.Load() != 1 {
		t.Fatalf("deliveries after restart = %d, want 1", delivered.Load())
	}
	pending, err := store.PendingOutbox(context.Background(), now.Add(24*time.Hour), ledger.MaxPendingOutbox, ledger.MaxOutboxPayloadBytes)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after restart delivery = %#v, err=%v", pending, err)
	}
}

func TestOutboxWorkerCloseCancelsInflightWithoutRecordingShutdown(t *testing.T) {
	now := outboxWorkerTestTime()
	store := newMemoryOutboxStore(workerEvent("event-inflight", now))
	started := make(chan struct{})
	exited := make(chan struct{})
	deliverer := delivererFunc(func(ctx context.Context, _ ledger.OutboxEvent) DeliveryResult {
		close(started)
		<-ctx.Done()
		close(exited)
		return retryableDelivery(errors.New("context canceled at https://host?secret=private"))
	})
	worker := newTestWorker(t, store, deliverer, newTestClock(now), newManualTimer(), OutboxWorkerOptions{})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("delivery did not start")
	}

	const closers = 16
	errorsSeen := make(chan error, closers)
	var group sync.WaitGroup
	for index := 0; index < closers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			errorsSeen <- worker.Close()
		}()
	}
	done := make(chan struct{})
	go func() {
		group.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Close calls did not return")
	}
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}
	select {
	case <-exited:
	default:
		t.Fatal("in-flight deliverer goroutine did not exit")
	}
	event := store.eventSnapshot(t, "event-inflight")
	if event.Attempts != 0 || event.DeliveredAt != nil || event.FailedAt != nil {
		t.Fatalf("shutdown was persisted as delivery failure: %#v", event)
	}
}

func newTestWorker(
	t *testing.T,
	store OutboxStore,
	deliverer Deliverer,
	clock Clock,
	timer Timer,
	overrides OutboxWorkerOptions,
) *OutboxWorker {
	t.Helper()
	options := overrides
	options.Clock = clock
	options.Timer = timer
	if options.PollInterval == 0 {
		options.PollInterval = time.Second
	}
	if options.BatchSize == 0 {
		options.BatchSize = ledger.MaxPendingOutbox
	}
	if options.MaximumBytes == 0 {
		options.MaximumBytes = ledger.MaxOutboxPayloadBytes
	}
	if options.DeliveryTimeout == 0 {
		options.DeliveryTimeout = time.Minute
	}
	if options.Logger == nil {
		options.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	worker, err := NewOutboxWorker(context.Background(), store, deliverer, options)
	if err != nil {
		t.Fatalf("NewOutboxWorker() error = %v", err)
	}
	t.Cleanup(func() { _ = worker.Close() })
	return worker
}

func testLogger(destination io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(destination, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func assertLogRedacted(t *testing.T, logs string) {
	t.Helper()
	if strings.Contains(logs, "token=private") || strings.Contains(logs, "https://") {
		t.Fatalf("logs leaked sensitive delivery detail: %s", logs)
	}
}

func outboxWorkerTestTime() time.Time {
	return time.Date(2026, 8, 9, 14, 15, 16, 123456789, time.UTC)
}

func workerEvent(id string, nextAttemptAt time.Time) ledger.OutboxEvent {
	return ledger.OutboxEvent{
		ID:             id,
		VerificationID: "verification-" + id,
		Type:           "fact.changed",
		URL:            "https://hooks.example/events",
		Secret:         "secret",
		Payload:        json.RawMessage(`{"type":"fact.changed"}`),
		CreatedAt:      outboxWorkerTestTime(),
		NextAttemptAt:  nextAttemptAt,
	}
}

type delivererFunc func(context.Context, ledger.OutboxEvent) DeliveryResult

func (function delivererFunc) Deliver(ctx context.Context, event ledger.OutboxEvent) DeliveryResult {
	return function(ctx, event)
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock(now time.Time) *testClock { return &testClock{now: now} }

func (clock *testClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *testClock) Set(now time.Time) {
	clock.mu.Lock()
	clock.now = now
	clock.mu.Unlock()
}

type manualTimer struct {
	waits    chan time.Duration
	releases chan struct{}
}

func newManualTimer() *manualTimer {
	return &manualTimer{
		waits:    make(chan time.Duration, 32),
		releases: make(chan struct{}, 32),
	}
}

func (timer *manualTimer) Wait(ctx context.Context, delay time.Duration) error {
	select {
	case timer.waits <- delay:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-timer.releases:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func awaitWorkerWait(t *testing.T, timer *manualTimer, want time.Duration) {
	t.Helper()
	select {
	case got := <-timer.waits:
		if got != want {
			t.Fatalf("worker wait = %v, want %v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not finish polling")
	}
}

func releaseWorkerWait(timer *manualTimer) { timer.releases <- struct{}{} }

type pendingCall struct {
	dueAt        time.Time
	limit        int
	maximumBytes int
}

type attemptCall struct {
	id            string
	attemptedAt   time.Time
	nextAttemptAt time.Time
	lastError     string
}

type memoryOutboxStore struct {
	mu sync.Mutex

	events map[string]ledger.OutboxEvent
	order  []string

	pendingErrors   []error
	attemptErrors   map[string][]error
	deliveredErrors map[string][]error
	failedErrors    map[string][]error

	pendingCalls []pendingCall
	attemptCalls []attemptCall
}

func newMemoryOutboxStore(events ...ledger.OutboxEvent) *memoryOutboxStore {
	store := &memoryOutboxStore{
		events:          make(map[string]ledger.OutboxEvent, len(events)),
		attemptErrors:   make(map[string][]error),
		deliveredErrors: make(map[string][]error),
		failedErrors:    make(map[string][]error),
	}
	for _, event := range events {
		store.events[event.ID] = cloneOutboxEvent(event)
		store.order = append(store.order, event.ID)
	}
	return store
}

func (store *memoryOutboxStore) PendingOutbox(
	ctx context.Context,
	dueAt time.Time,
	limit int,
	maximumBytes int,
) ([]ledger.OutboxEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.pendingCalls = append(store.pendingCalls, pendingCall{dueAt: dueAt, limit: limit, maximumBytes: maximumBytes})
	if len(store.pendingErrors) > 0 {
		err := store.pendingErrors[0]
		store.pendingErrors = store.pendingErrors[1:]
		return nil, err
	}
	events := make([]ledger.OutboxEvent, 0, limit)
	totalBytes := 0
	for _, id := range store.order {
		event := store.events[id]
		if event.DeliveredAt != nil || event.FailedAt != nil || event.NextAttemptAt.After(dueAt) {
			continue
		}
		if len(events) == limit || len(event.Payload) > maximumBytes-totalBytes {
			break
		}
		events = append(events, cloneOutboxEvent(event))
		totalBytes += len(event.Payload)
	}
	return events, nil
}

func (store *memoryOutboxStore) MarkOutboxAttempt(
	ctx context.Context,
	id string,
	attemptedAt time.Time,
	nextAttemptAt time.Time,
	lastError string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.attemptCalls = append(store.attemptCalls, attemptCall{id: id, attemptedAt: attemptedAt, nextAttemptAt: nextAttemptAt, lastError: lastError})
	if err := popStoreError(store.attemptErrors, id); err != nil {
		return err
	}
	event, exists := store.events[id]
	if !exists {
		return ledger.ErrOutboxNotFound
	}
	event.Attempts++
	attemptedAtCopy := attemptedAt
	event.LastAttemptAt = &attemptedAtCopy
	event.LastError = lastError
	event.NextAttemptAt = nextAttemptAt
	store.events[id] = event
	return nil
}

func (store *memoryOutboxStore) MarkOutboxDelivered(ctx context.Context, id string, deliveredAt time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := popStoreError(store.deliveredErrors, id); err != nil {
		return err
	}
	event, exists := store.events[id]
	if !exists {
		return ledger.ErrOutboxNotFound
	}
	deliveredAtCopy := deliveredAt
	event.DeliveredAt = &deliveredAtCopy
	store.events[id] = event
	return nil
}

func (store *memoryOutboxStore) MarkOutboxFailed(ctx context.Context, id string, failedAt time.Time, lastError string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := popStoreError(store.failedErrors, id); err != nil {
		return err
	}
	event, exists := store.events[id]
	if !exists {
		return ledger.ErrOutboxNotFound
	}
	if event.Attempts == 0 {
		event.Attempts = 1
		failedAttempt := failedAt
		event.LastAttemptAt = &failedAttempt
	}
	event.LastError = lastError
	failedAtCopy := failedAt
	event.FailedAt = &failedAtCopy
	store.events[id] = event
	return nil
}

func popStoreError(source map[string][]error, id string) error {
	errorsForID := source[id]
	if len(errorsForID) == 0 {
		return nil
	}
	err := errorsForID[0]
	source[id] = errorsForID[1:]
	return err
}

func (store *memoryOutboxStore) eventSnapshot(t *testing.T, id string) ledger.OutboxEvent {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	event, exists := store.events[id]
	if !exists {
		t.Fatalf("event %q does not exist", id)
	}
	return cloneOutboxEvent(event)
}

func (store *memoryOutboxStore) pendingCallsSnapshot() []pendingCall {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]pendingCall(nil), store.pendingCalls...)
}

func (store *memoryOutboxStore) attemptCallsSnapshot() []attemptCall {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]attemptCall(nil), store.attemptCalls...)
}

func cloneOutboxEvent(event ledger.OutboxEvent) ledger.OutboxEvent {
	event.Payload = append(json.RawMessage(nil), event.Payload...)
	if event.LastAttemptAt != nil {
		value := *event.LastAttemptAt
		event.LastAttemptAt = &value
	}
	if event.DeliveredAt != nil {
		value := *event.DeliveredAt
		event.DeliveredAt = &value
	}
	if event.FailedAt != nil {
		value := *event.FailedAt
		event.FailedAt = &value
	}
	return event
}

func assertDelivered(t *testing.T, store *memoryOutboxStore, id string, want bool) {
	t.Helper()
	event := store.eventSnapshot(t, id)
	if (event.DeliveredAt != nil) != want {
		t.Fatalf("event %q delivered=%v, want %v; event=%#v", id, event.DeliveredAt != nil, want, event)
	}
}
