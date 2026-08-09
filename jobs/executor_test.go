package jobs

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewExecutorValidatesBounds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		workers  int
		capacity int
		want     error
	}{
		{name: "no workers", workers: 0, capacity: 1, want: ErrInvalidWorkerCount},
		{name: "negative workers", workers: -1, capacity: 1, want: ErrInvalidWorkerCount},
		{name: "negative capacity", workers: 1, capacity: -1, want: ErrInvalidQueueCapacity},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			executor, err := NewExecutor(test.workers, test.capacity)
			if !errors.Is(err, test.want) {
				t.Fatalf("NewExecutor() error = %v, want %v", err, test.want)
			}
			if executor != nil {
				t.Fatalf("NewExecutor() executor = %#v, want nil", executor)
			}
		})
	}
}

func TestExecutorFixedConcurrency(t *testing.T) {
	const (
		workerCount = 3
		taskCount   = 12
	)

	executor := newTestExecutor(t, workerCount, taskCount)
	release := make(chan struct{})
	var closeRelease sync.Once
	t.Cleanup(func() { closeRelease.Do(func() { close(release) }) })

	started := make(chan struct{}, taskCount)
	finished := make(chan struct{}, taskCount)
	var active atomic.Int32
	var maximum atomic.Int32

	for range taskCount {
		err := executor.Submit(context.Background(), func(context.Context) {
			current := active.Add(1)
			updateMaximum(&maximum, current)
			started <- struct{}{}
			<-release
			active.Add(-1)
			finished <- struct{}{}
		})
		if err != nil {
			t.Fatalf("Submit() error = %v", err)
		}
	}

	for range workerCount {
		receive(t, started, "task start")
	}
	if got := maximum.Load(); got != workerCount {
		t.Fatalf("maximum concurrency = %d, want %d", got, workerCount)
	}
	select {
	case <-started:
		t.Fatal("a task started above the fixed worker limit")
	default:
	}

	closeRelease.Do(func() { close(release) })
	for range taskCount {
		receive(t, finished, "task completion")
	}
	if got := maximum.Load(); got != workerCount {
		t.Fatalf("maximum concurrency after completion = %d, want %d", got, workerCount)
	}
}

func TestExecutorSharesLimitAcrossCallers(t *testing.T) {
	const (
		workerCount    = 2
		tasksPerCaller = 5
	)

	executor := newTestExecutor(t, workerCount, 2*tasksPerCaller)
	release := make(chan struct{})
	var closeRelease sync.Once
	t.Cleanup(func() { closeRelease.Do(func() { close(release) }) })

	started := make(chan struct{}, 2*tasksPerCaller)
	finished := make(chan struct{}, 2*tasksPerCaller)
	var active atomic.Int32
	var maximum atomic.Int32
	var submitters sync.WaitGroup
	submitErrors := make(chan error, 2)

	for range 2 {
		submitters.Add(1)
		go func() {
			defer submitters.Done()
			for range tasksPerCaller {
				err := executor.Submit(context.Background(), func(context.Context) {
					current := active.Add(1)
					updateMaximum(&maximum, current)
					started <- struct{}{}
					<-release
					active.Add(-1)
					finished <- struct{}{}
				})
				if err != nil {
					submitErrors <- err
					return
				}
			}
		}()
	}

	submitters.Wait()
	close(submitErrors)
	for err := range submitErrors {
		t.Fatalf("Submit() error = %v", err)
	}
	for range workerCount {
		receive(t, started, "shared task start")
	}
	if got := maximum.Load(); got != workerCount {
		t.Fatalf("shared maximum concurrency = %d, want %d", got, workerCount)
	}
	select {
	case <-started:
		t.Fatal("separate callers exceeded the shared worker limit")
	default:
	}

	closeRelease.Do(func() { close(release) })
	for range 2 * tasksPerCaller {
		receive(t, finished, "shared task completion")
	}
}

func TestExecutorQueueFull(t *testing.T) {
	executor := newTestExecutor(t, 1, 2)
	release := make(chan struct{})
	var closeRelease sync.Once
	t.Cleanup(func() { closeRelease.Do(func() { close(release) }) })

	started := make(chan struct{}, 1)
	if err := executor.Submit(context.Background(), func(context.Context) {
		started <- struct{}{}
		<-release
	}); err != nil {
		t.Fatalf("Submit(running task) error = %v", err)
	}
	receive(t, started, "blocking task start")

	for index := range 2 {
		if err := executor.Submit(context.Background(), func(context.Context) {}); err != nil {
			t.Fatalf("Submit(queued task %d) error = %v", index, err)
		}
	}
	if err := executor.Submit(context.Background(), func(context.Context) {}); err != ErrQueueFull {
		t.Fatalf("Submit(full queue) error = %v, want exact sentinel %v", err, ErrQueueFull)
	}

	closeRelease.Do(func() { close(release) })
}

func TestExecutorRejectsInvalidAndCanceledSubmissions(t *testing.T) {
	executor := newTestExecutor(t, 1, 1)
	task := Task(func(context.Context) {})

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name string
		ctx  context.Context
		task Task
		want error
	}{
		{name: "nil context", ctx: nil, task: task, want: ErrNilContext},
		{name: "nil task", ctx: context.Background(), task: nil, want: ErrNilTask},
		{name: "canceled context", ctx: canceled, task: task, want: ErrSubmitCanceled},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := executor.Submit(test.ctx, test.task); err != test.want {
				t.Fatalf("Submit() error = %v, want exact sentinel %v", err, test.want)
			}
		})
	}
}

func TestExecutorPropagatesCallerCancellation(t *testing.T) {
	executor := newTestExecutor(t, 1, 1)
	type contextKey string
	const key contextKey = "request"

	parent, cancel := context.WithCancel(context.WithValue(context.Background(), key, "batch-a"))
	started := make(chan struct{})
	result := make(chan error, 1)
	if err := executor.Submit(parent, func(ctx context.Context) {
		if got := ctx.Value(key); got != "batch-a" {
			result <- errors.New("task context did not preserve caller value")
			return
		}
		close(started)
		<-ctx.Done()
		result <- ctx.Err()
	}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	receive(t, started, "cancelable task start")

	cancel()
	if err := receive(t, result, "task cancellation"); !errors.Is(err, context.Canceled) {
		t.Fatalf("task context error = %v, want %v", err, context.Canceled)
	}
}

func TestExecutorInvokesCanceledQueuedTaskExactlyOnce(t *testing.T) {
	executor := newTestExecutor(t, 1, 2)
	release := make(chan struct{})
	var closeRelease sync.Once
	t.Cleanup(func() { closeRelease.Do(func() { close(release) }) })

	started := make(chan struct{})
	if err := executor.Submit(context.Background(), func(context.Context) {
		close(started)
		<-release
	}); err != nil {
		t.Fatalf("Submit(blocker) error = %v", err)
	}
	receive(t, started, "queue blocker start")

	queuedCtx, cancelQueued := context.WithCancel(context.Background())
	canceledTaskDone := make(chan error, 1)
	var canceledTaskCalls atomic.Int32
	if err := executor.Submit(queuedCtx, func(ctx context.Context) {
		canceledTaskCalls.Add(1)
		canceledTaskDone <- ctx.Err()
	}); err != nil {
		t.Fatalf("Submit(canceled queued task) error = %v", err)
	}
	markerDone := make(chan struct{})
	if err := executor.Submit(context.Background(), func(context.Context) {
		close(markerDone)
	}); err != nil {
		t.Fatalf("Submit(marker) error = %v", err)
	}

	cancelQueued()
	closeRelease.Do(func() { close(release) })
	if err := receive(t, canceledTaskDone, "canceled queued task settlement"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled queued task context error = %v, want %v", err, context.Canceled)
	}
	receive(t, markerDone, "marker completion")
	if got := canceledTaskCalls.Load(); got != 1 {
		t.Fatalf("canceled queued task calls = %d, want 1", got)
	}
}

func TestExecutorCloseCancelsRunningAndQueuedWork(t *testing.T) {
	executor := newTestExecutor(t, 2, 2)
	started := make(chan struct{}, 2)
	canceled := make(chan error, 2)

	for range 2 {
		if err := executor.Submit(context.Background(), func(ctx context.Context) {
			started <- struct{}{}
			<-ctx.Done()
			canceled <- ctx.Err()
		}); err != nil {
			t.Fatalf("Submit(running task) error = %v", err)
		}
	}
	for range 2 {
		receive(t, started, "running task start")
	}

	queuedSettled := make(chan error, 2)
	var queuedCalls atomic.Int32
	for range 2 {
		if err := executor.Submit(context.Background(), func(ctx context.Context) {
			queuedCalls.Add(1)
			queuedSettled <- ctx.Err()
		}); err != nil {
			t.Fatalf("Submit(queued task) error = %v", err)
		}
	}

	executor.Close()
	for range 2 {
		if err := receive(t, canceled, "shutdown cancellation"); !errors.Is(err, context.Canceled) {
			t.Fatalf("shutdown task error = %v, want %v", err, context.Canceled)
		}
	}
	for range 2 {
		if err := receive(t, queuedSettled, "queued shutdown settlement"); !errors.Is(err, context.Canceled) {
			t.Fatalf("queued shutdown context error = %v, want %v", err, context.Canceled)
		}
	}
	if got := queuedCalls.Load(); got != 2 {
		t.Fatalf("queued shutdown task calls = %d, want 2", got)
	}
	if err := executor.Submit(context.Background(), func(context.Context) {}); err != ErrClosed {
		t.Fatalf("Submit(after Close) error = %v, want exact sentinel %v", err, ErrClosed)
	}
	// A second close is a no-op and must not panic.
	executor.Close()
}

func TestExecutorCloseWaitsForRunningTaskCleanup(t *testing.T) {
	executor := newTestExecutor(t, 1, 1)
	started := make(chan struct{})
	canceled := make(chan struct{})
	allowReturn := make(chan struct{})

	if err := executor.Submit(context.Background(), func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-allowReturn
	}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	receive(t, started, "cleanup task start")

	closed := make(chan struct{})
	go func() {
		executor.Close()
		close(closed)
	}()
	receive(t, canceled, "cleanup task cancellation")
	select {
	case <-closed:
		t.Fatal("Close returned before the running task finished cleanup")
	default:
	}

	close(allowReturn)
	receive(t, closed, "Close completion")
}

func TestExecutorConcurrentCloseAndSubmit(t *testing.T) {
	const (
		iterations = 20
		submitters = 64
	)

	for iteration := range iterations {
		executor, err := NewExecutor(4, 16)
		if err != nil {
			t.Fatalf("iteration %d: NewExecutor() error = %v", iteration, err)
		}

		start := make(chan struct{})
		type submitResult struct {
			index int
			err   error
		}
		results := make(chan submitResult, submitters)
		invocations := make([]atomic.Int32, submitters)
		var calls sync.WaitGroup
		for index := range submitters {
			calls.Add(1)
			go func() {
				defer calls.Done()
				<-start
				err := executor.Submit(context.Background(), func(ctx context.Context) {
					invocations[index].Add(1)
					<-ctx.Done()
				})
				results <- submitResult{index: index, err: err}
			}()
		}

		closed := make(chan struct{})
		go func() {
			<-start
			executor.Close()
			close(closed)
		}()
		close(start)
		calls.Wait()
		receive(t, closed, "concurrent Close")
		close(results)

		for result := range results {
			if result.err != nil && result.err != ErrClosed && result.err != ErrQueueFull {
				t.Fatalf("iteration %d: Submit() error = %v", iteration, result.err)
			}
			wantCalls := int32(0)
			if result.err == nil {
				wantCalls = 1
			}
			if got := invocations[result.index].Load(); got != wantCalls {
				t.Fatalf(
					"iteration %d: submission %d calls = %d, want %d for error %v",
					iteration,
					result.index,
					got,
					wantCalls,
					result.err,
				)
			}
		}
		if err := executor.Submit(context.Background(), func(context.Context) {}); err != ErrClosed {
			t.Fatalf("iteration %d: Submit(after race) error = %v, want %v", iteration, err, ErrClosed)
		}
	}
}

func TestExecutorConcurrentCloseIsIdempotent(t *testing.T) {
	executor := newTestExecutor(t, 4, 4)
	var callers sync.WaitGroup

	for range 32 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			executor.Close()
		}()
	}
	callers.Wait()
	if err := executor.Submit(context.Background(), func(context.Context) {}); err != ErrClosed {
		t.Fatalf("Submit(after concurrent Close) error = %v, want %v", err, ErrClosed)
	}
}

func TestExecutorRecoversTaskPanic(t *testing.T) {
	executor := newTestExecutor(t, 1, 2)
	if err := executor.Submit(context.Background(), func(context.Context) {
		panic("test panic")
	}); err != nil {
		t.Fatalf("Submit(panicking task) error = %v", err)
	}

	finished := make(chan struct{})
	if err := executor.Submit(context.Background(), func(context.Context) {
		close(finished)
	}); err != nil {
		t.Fatalf("Submit(follow-up task) error = %v", err)
	}
	receive(t, finished, "follow-up task completion")
}

func newTestExecutor(t *testing.T, workers, capacity int) *Executor {
	t.Helper()

	executor, err := NewExecutor(workers, capacity)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	t.Cleanup(executor.Close)
	return executor
}

func updateMaximum(maximum *atomic.Int32, current int32) {
	for {
		previous := maximum.Load()
		if current <= previous || maximum.CompareAndSwap(previous, current) {
			return
		}
	}
}

func receive[T any](t *testing.T, channel <-chan T, description string) T {
	t.Helper()

	select {
	case value := <-channel:
		return value
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
		var zero T
		return zero
	}
}
