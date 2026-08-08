package jobs

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestNewManagerValidatesConfiguration(t *testing.T) {
	t.Parallel()

	clone := CloneFunc[int](func(value int) int { return value })
	clock := newManualManagerClock(time.Unix(1_000, 0))
	tests := []struct {
		name     string
		capacity int
		ttl      time.Duration
		interval time.Duration
		clone    CloneFunc[int]
		clock    managerClock
	}{
		{name: "zero capacity", capacity: 0, ttl: time.Minute, interval: time.Minute, clone: clone, clock: clock},
		{name: "negative capacity", capacity: -1, ttl: time.Minute, interval: time.Minute, clone: clone, clock: clock},
		{name: "zero ttl", capacity: 1, ttl: 0, interval: time.Minute, clone: clone, clock: clock},
		{name: "negative ttl", capacity: 1, ttl: -1, interval: time.Minute, clone: clone, clock: clock},
		{name: "zero interval", capacity: 1, ttl: time.Minute, interval: 0, clone: clone, clock: clock},
		{name: "negative interval", capacity: 1, ttl: time.Minute, interval: -1, clone: clone, clock: clock},
		{name: "nil clone", capacity: 1, ttl: time.Minute, interval: time.Minute, clone: nil, clock: clock},
		{name: "nil clock", capacity: 1, ttl: time.Minute, interval: time.Minute, clone: clone, clock: nil},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			manager, err := newManagerWithClock(
				test.capacity,
				test.ttl,
				test.interval,
				test.clone,
				test.clock,
			)
			if err != ErrInvalidManagerConfig {
				t.Fatalf("newManagerWithClock() error = %v, want exact sentinel %v", err, ErrInvalidManagerConfig)
			}
			if manager != nil {
				t.Fatalf("newManagerWithClock() manager = %#v, want nil", manager)
			}
		})
	}
}

func TestManagerCapacityDuplicateAndCompletedTTL(t *testing.T) {
	const retention = 10 * time.Minute
	clock := newManualManagerClock(time.Unix(2_000, 0))
	manager := newTestManager(t, 2, retention, clock, func(value int) int { return value })

	firstCtx, err := manager.Create("first", 1)
	if err != nil {
		t.Fatalf("Create(first) error = %v", err)
	}
	secondCtx, err := manager.Create("second", 2)
	if err != nil {
		t.Fatalf("Create(second) error = %v", err)
	}
	assertContextActive(t, firstCtx, "first job")
	assertContextActive(t, secondCtx, "second job")
	if got := manager.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2", got)
	}

	if duplicateCtx, err := manager.Create("first", 100); err != ErrDuplicateJob || duplicateCtx != nil {
		t.Fatalf("Create(duplicate) = (%v, %v), want (nil, %v)", duplicateCtx, err, ErrDuplicateJob)
	}
	if fullCtx, err := manager.Create("third", 3); err != ErrManagerFull || fullCtx != nil {
		t.Fatalf("Create(full) = (%v, %v), want (nil, %v)", fullCtx, err, ErrManagerFull)
	}
	if invalidCtx, err := manager.Create("", 3); err != ErrInvalidJobID || invalidCtx != nil {
		t.Fatalf("Create(empty ID) = (%v, %v), want (nil, %v)", invalidCtx, err, ErrInvalidJobID)
	}

	if err := manager.Complete("first", func(value *int) { *value = 10 }); err != nil {
		t.Fatalf("Complete(first) error = %v", err)
	}
	assertContextCanceled(t, firstCtx, "completed first job")
	if fullCtx, err := manager.Create("third", 3); err != ErrManagerFull || fullCtx != nil {
		t.Fatalf("Create(while completed retained) = (%v, %v), want (nil, %v)", fullCtx, err, ErrManagerFull)
	}

	clock.Advance(retention - time.Nanosecond)
	manager.sweepExpired(clock.Now())
	if _, exists := manager.Snapshot("first"); !exists {
		t.Fatal("completed job expired before its TTL")
	}
	if got := manager.Len(); got != 2 {
		t.Fatalf("Len(before TTL) = %d, want 2", got)
	}

	clock.Advance(time.Nanosecond)
	manager.sweepExpired(clock.Now())
	if _, exists := manager.Snapshot("first"); exists {
		t.Fatal("completed job remains at its TTL boundary")
	}
	if got := manager.Len(); got != 1 {
		t.Fatalf("Len(after TTL) = %d, want 1", got)
	}
	if _, exists := manager.Snapshot("second"); !exists {
		t.Fatal("running job was removed by completed-job TTL cleanup")
	}
	assertContextActive(t, secondCtx, "running second job after sweep")

	thirdCtx, err := manager.Create("third", 3)
	if err != nil {
		t.Fatalf("Create(after TTL cleanup) error = %v", err)
	}
	assertContextActive(t, thirdCtx, "third job")
}

func TestManagerBackgroundSweeperUsesInjectedClock(t *testing.T) {
	const retention = 30 * time.Minute
	clock := newManualManagerClock(time.Unix(2_500, 0))
	manager, err := newManagerWithClock(
		2,
		retention,
		time.Millisecond,
		CloneFunc[int](func(value int) int { return value }),
		clock,
	)
	if err != nil {
		t.Fatalf("newManagerWithClock() error = %v", err)
	}
	t.Cleanup(manager.Close)

	if _, err := manager.Create("completed", 1); err != nil {
		t.Fatalf("Create(completed) error = %v", err)
	}
	if _, err := manager.Create("running", 2); err != nil {
		t.Fatalf("Create(running) error = %v", err)
	}
	if err := manager.Complete("completed", nil); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	clock.Advance(retention)

	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		if _, exists := manager.Snapshot("completed"); !exists {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("background sweeper did not expire completed job")
		default:
			runtime.Gosched()
		}
	}
	if value, exists := manager.Snapshot("running"); !exists || value != 2 {
		t.Fatalf("Snapshot(running) = (%d, %t), want (2, true)", value, exists)
	}
}

func TestManagerCloneBoundariesAndImmutableCompletion(t *testing.T) {
	clock := newManualManagerClock(time.Unix(3_000, 0))
	manager := newTestManager(t, 1, time.Hour, clock, cloneManagerState)
	initialNumber := 7
	initial := managerState{
		Values: []int{1, 2},
		Labels: map[string]string{"phase": "initial"},
		Nested: &managerNested{Number: &initialNumber},
	}
	jobCtx, err := manager.Create("job", initial)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	initial.Values[0] = 100
	initial.Labels["phase"] = "caller-mutated"
	*initial.Nested.Number = 100
	assertManagerState(t, manager, "job", []int{1, 2}, "initial", 7)

	snapshot, exists := manager.Snapshot("job")
	if !exists {
		t.Fatal("Snapshot() missing created job")
	}
	snapshot.Values[0] = 200
	snapshot.Labels["phase"] = "snapshot-mutated"
	*snapshot.Nested.Number = 200
	assertManagerState(t, manager, "job", []int{1, 2}, "initial", 7)

	var retained *managerState
	if err := manager.Update("job", func(value *managerState) {
		value.Values = append(value.Values, 3)
		value.Labels["phase"] = "running"
		*value.Nested.Number = 8
		retained = value
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	retained.Values[0] = 300
	retained.Labels["phase"] = "retained-update-mutated"
	*retained.Nested.Number = 300
	assertManagerState(t, manager, "job", []int{1, 2, 3}, "running", 8)

	var retainedFinal *managerState
	if err := manager.Complete("job", func(value *managerState) {
		value.Values = append(value.Values, 4)
		value.Labels["phase"] = "completed"
		*value.Nested.Number = 9
		retainedFinal = value
	}); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	assertContextCanceled(t, jobCtx, "completed cloned job")
	retainedFinal.Values[0] = 400
	retainedFinal.Labels["phase"] = "retained-complete-mutated"
	*retainedFinal.Nested.Number = 400
	assertManagerState(t, manager, "job", []int{1, 2, 3, 4}, "completed", 9)

	if err := manager.Update("job", nil); err != ErrJobCompleted {
		t.Fatalf("Update(completed) error = %v, want %v", err, ErrJobCompleted)
	}
	if err := manager.Complete("job", nil); err != ErrJobCompleted {
		t.Fatalf("Complete(completed) error = %v, want %v", err, ErrJobCompleted)
	}
	if err := manager.Update("missing", nil); err != ErrJobNotFound {
		t.Fatalf("Update(missing) error = %v, want %v", err, ErrJobNotFound)
	}
	if err := manager.Complete("missing", nil); err != ErrJobNotFound {
		t.Fatalf("Complete(missing) error = %v, want %v", err, ErrJobNotFound)
	}
}

func TestManagerCompleteMutationPanicDoesNotFinalize(t *testing.T) {
	clock := newManualManagerClock(time.Unix(4_000, 0))
	manager := newTestManager(t, 1, time.Hour, clock, func(value int) int { return value })
	jobCtx, err := manager.Create("job", 1)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("Complete() mutation did not propagate panic")
			}
		}()
		_ = manager.Complete("job", func(*int) { panic("mutation panic") })
	}()

	assertContextActive(t, jobCtx, "job after panicking completion")
	if err := manager.Update("job", func(value *int) { *value = 2 }); err != nil {
		t.Fatalf("Update(after panicking completion) error = %v", err)
	}
	if err := manager.Complete("job", nil); err != nil {
		t.Fatalf("Complete(after panicking completion) error = %v", err)
	}
	assertContextCanceled(t, jobCtx, "job after successful completion")
}

func TestManagerCloseCancelsRunningJobsAndStopsSweeper(t *testing.T) {
	clock := newManualManagerClock(time.Unix(5_000, 0))
	manager := newTestManager(t, 3, time.Hour, clock, func(value int) int { return value })
	runningCtx, err := manager.Create("running", 1)
	if err != nil {
		t.Fatalf("Create(running) error = %v", err)
	}
	completedCtx, err := manager.Create("completed", 2)
	if err != nil {
		t.Fatalf("Create(completed) error = %v", err)
	}
	if err := manager.Complete("completed", nil); err != nil {
		t.Fatalf("Complete(completed) error = %v", err)
	}
	assertContextCanceled(t, completedCtx, "completed job before manager close")

	var closers sync.WaitGroup
	for range 32 {
		closers.Add(1)
		go func() {
			defer closers.Done()
			manager.Close()
		}()
	}
	closers.Wait()

	assertContextCanceled(t, runningCtx, "running job after manager close")
	select {
	case <-manager.done:
	default:
		t.Fatal("Close returned before the sweeper goroutine exited")
	}
	if createdCtx, err := manager.Create("new", 3); err != ErrManagerClosed || createdCtx != nil {
		t.Fatalf("Create(after Close) = (%v, %v), want (nil, %v)", createdCtx, err, ErrManagerClosed)
	}
	if err := manager.Update("running", nil); err != ErrManagerClosed {
		t.Fatalf("Update(after Close) error = %v, want %v", err, ErrManagerClosed)
	}
	if err := manager.Complete("running", nil); err != ErrManagerClosed {
		t.Fatalf("Complete(after Close) error = %v, want %v", err, ErrManagerClosed)
	}
	if value, exists := manager.Snapshot("running"); !exists || value != 1 {
		t.Fatalf("Snapshot(after Close) = (%d, %t), want (1, true)", value, exists)
	}
	if got := manager.Len(); got != 2 {
		t.Fatalf("Len(after Close) = %d, want 2", got)
	}
	manager.Close()
}

func TestNilManagerIsSafe(t *testing.T) {
	var manager *Manager[int]
	if ctx, err := manager.Create("job", 1); err != ErrManagerClosed || ctx != nil {
		t.Fatalf("nil Create() = (%v, %v), want (nil, %v)", ctx, err, ErrManagerClosed)
	}
	if err := manager.Update("job", nil); err != ErrManagerClosed {
		t.Fatalf("nil Update() error = %v, want %v", err, ErrManagerClosed)
	}
	if err := manager.Complete("job", nil); err != ErrManagerClosed {
		t.Fatalf("nil Complete() error = %v, want %v", err, ErrManagerClosed)
	}
	if value, exists := manager.Snapshot("job"); exists || value != 0 {
		t.Fatalf("nil Snapshot() = (%d, %t), want (0, false)", value, exists)
	}
	if got := manager.Len(); got != 0 {
		t.Fatalf("nil Len() = %d, want 0", got)
	}
	manager.Close()
}

func TestManagerConcurrentLifecycle(t *testing.T) {
	const (
		iterations = 10
		jobCount   = 48
	)

	for iteration := range iterations {
		clock := newManualManagerClock(time.Unix(int64(10_000+iteration), 0))
		manager, err := newManagerWithClock(
			2*jobCount,
			time.Second,
			time.Hour,
			CloneFunc[int](func(value int) int { return value }),
			clock,
		)
		if err != nil {
			t.Fatalf("iteration %d: newManagerWithClock() error = %v", iteration, err)
		}
		for index := range jobCount {
			if _, err := manager.Create(fmt.Sprintf("existing-%d", index), index); err != nil {
				t.Fatalf("iteration %d: Create(existing %d) error = %v", iteration, index, err)
			}
		}

		start := make(chan struct{})
		unexpected := make(chan error, 4*jobCount)
		var operations sync.WaitGroup
		for index := range jobCount {
			id := fmt.Sprintf("existing-%d", index)
			operations.Add(3)
			go func() {
				defer operations.Done()
				<-start
				if err := manager.Update(id, func(value *int) { *value++ }); !isAllowedLifecycleError(err) {
					unexpected <- fmt.Errorf("Update(%s): %w", id, err)
				}
			}()
			go func() {
				defer operations.Done()
				<-start
				if err := manager.Complete(id, nil); !isAllowedLifecycleError(err) {
					unexpected <- fmt.Errorf("Complete(%s): %w", id, err)
				}
			}()
			go func() {
				defer operations.Done()
				<-start
				for range 4 {
					_, _ = manager.Snapshot(id)
					runtime.Gosched()
				}
			}()
		}

		for index := range jobCount {
			id := fmt.Sprintf("new-%d", index)
			operations.Add(1)
			go func() {
				defer operations.Done()
				<-start
				if _, err := manager.Create(id, index); err != nil && err != ErrManagerClosed && err != ErrManagerFull {
					unexpected <- fmt.Errorf("Create(%s): %w", id, err)
				}
			}()
		}

		operations.Add(3)
		go func() {
			defer operations.Done()
			<-start
			for range 20 {
				clock.Advance(time.Second)
				manager.sweepExpired(clock.Now())
				runtime.Gosched()
			}
		}()
		for range 2 {
			go func() {
				defer operations.Done()
				<-start
				manager.Close()
			}()
		}

		close(start)
		operations.Wait()
		manager.Close()
		close(unexpected)
		for err := range unexpected {
			t.Errorf("iteration %d: unexpected lifecycle error: %v", iteration, err)
		}
	}
}

type managerNested struct {
	Number *int
}

type managerState struct {
	Values []int
	Labels map[string]string
	Nested *managerNested
}

func cloneManagerState(value managerState) managerState {
	clone := managerState{
		Values: append([]int(nil), value.Values...),
		Labels: make(map[string]string, len(value.Labels)),
	}
	for key, label := range value.Labels {
		clone.Labels[key] = label
	}
	if value.Nested != nil {
		clone.Nested = &managerNested{}
		if value.Nested.Number != nil {
			number := *value.Nested.Number
			clone.Nested.Number = &number
		}
	}
	return clone
}

func assertManagerState(
	t *testing.T,
	manager *Manager[managerState],
	id string,
	wantValues []int,
	wantPhase string,
	wantNumber int,
) {
	t.Helper()

	state, exists := manager.Snapshot(id)
	if !exists {
		t.Fatalf("Snapshot(%q) does not exist", id)
	}
	if len(state.Values) != len(wantValues) {
		t.Fatalf("Snapshot(%q).Values = %v, want %v", id, state.Values, wantValues)
	}
	for index := range wantValues {
		if state.Values[index] != wantValues[index] {
			t.Fatalf("Snapshot(%q).Values = %v, want %v", id, state.Values, wantValues)
		}
	}
	if got := state.Labels["phase"]; got != wantPhase {
		t.Fatalf("Snapshot(%q).Labels[phase] = %q, want %q", id, got, wantPhase)
	}
	if state.Nested == nil || state.Nested.Number == nil || *state.Nested.Number != wantNumber {
		t.Fatalf("Snapshot(%q).Nested.Number = %#v, want %d", id, state.Nested, wantNumber)
	}
}

type manualManagerClock struct {
	mu  sync.Mutex
	now time.Time
}

func newManualManagerClock(now time.Time) *manualManagerClock {
	return &manualManagerClock{now: now}
}

func (c *manualManagerClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualManagerClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

func newTestManager[T any](
	t *testing.T,
	capacity int,
	ttl time.Duration,
	clock managerClock,
	clone CloneFunc[T],
) *Manager[T] {
	t.Helper()

	manager, err := newManagerWithClock(capacity, ttl, time.Hour, clone, clock)
	if err != nil {
		t.Fatalf("newManagerWithClock() error = %v", err)
	}
	t.Cleanup(manager.Close)
	return manager
}

func assertContextActive(t *testing.T, ctx context.Context, description string) {
	t.Helper()
	select {
	case <-ctx.Done():
		t.Fatalf("%s context is unexpectedly canceled: %v", description, ctx.Err())
	default:
	}
}

func assertContextCanceled(t *testing.T, ctx context.Context, description string) {
	t.Helper()
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("%s context error = %v, want %v", description, ctx.Err(), context.Canceled)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s context cancellation", description)
	}
}

func isAllowedLifecycleError(err error) bool {
	return err == nil || err == ErrManagerClosed || err == ErrJobCompleted || err == ErrJobNotFound
}
