// Package jobs provides process-wide primitives for bounded background work.
package jobs

import (
	"context"
	"errors"
	"sync"
)

var (
	// ErrClosed is returned when work is submitted after shutdown begins.
	ErrClosed = errors.New("jobs: executor is closed")
	// ErrQueueFull is returned when the bounded queue has no free slot.
	ErrQueueFull = errors.New("jobs: executor queue is full")
	// ErrSubmitCanceled is returned when the submission context is already done.
	ErrSubmitCanceled = errors.New("jobs: submission context is canceled")
	// ErrNilContext is returned when Submit is called with a nil context.
	ErrNilContext = errors.New("jobs: submission context is nil")
	// ErrNilTask is returned when Submit is called with a nil task.
	ErrNilTask = errors.New("jobs: task is nil")
	// ErrInvalidWorkerCount is returned when constructing an executor without workers.
	ErrInvalidWorkerCount = errors.New("jobs: worker count must be positive")
	// ErrInvalidQueueCapacity is returned when constructing an executor with a negative queue capacity.
	ErrInvalidQueueCapacity = errors.New("jobs: queue capacity must not be negative")
)

// Task is a unit of work accepted by an Executor.
//
// The supplied context inherits the context passed to Submit. It is also
// canceled when the executor closes. Every task accepted by Submit is invoked
// exactly once, including when its context is canceled while queued or the
// executor closes. Tasks must inspect the context before expensive work and
// return when it is canceled so they can settle their caller and Close can wait
// for all workers to exit.
type Task func(context.Context)

type submission struct {
	ctx  context.Context
	task Task
}

// Executor runs tasks with a fixed number of workers and one bounded queue.
// A single shared Executor therefore bounds concurrency and queued work across
// all of its callers, rather than creating a separate limit for each request.
type Executor struct {
	ctx    context.Context
	cancel context.CancelFunc
	queue  chan submission

	mu        sync.Mutex
	closed    bool
	closeOnce sync.Once
	workers   sync.WaitGroup
}

// NewExecutor starts workerCount workers. queueCapacity is the maximum number
// of tasks waiting for a worker; zero creates an unbuffered handoff queue.
func NewExecutor(workerCount, queueCapacity int) (*Executor, error) {
	if workerCount <= 0 {
		return nil, ErrInvalidWorkerCount
	}
	if queueCapacity < 0 {
		return nil, ErrInvalidQueueCapacity
	}

	ctx, cancel := context.WithCancel(context.Background())
	executor := &Executor{
		ctx:    ctx,
		cancel: cancel,
		queue:  make(chan submission, queueCapacity),
	}

	executor.workers.Add(workerCount)
	for range workerCount {
		go executor.work()
	}

	return executor, nil
}

// Submit adds task to the global queue without blocking. It returns a stable
// sentinel when shutdown has begun, the caller is already canceled, or the
// queue is full. A nil return transfers ownership of task to the executor: its
// closure will be invoked exactly once. A task accepted immediately before
// concurrent shutdown will be invoked with a context canceled by Close.
func (e *Executor) Submit(ctx context.Context, task Task) error {
	if e == nil {
		return ErrClosed
	}
	if ctx == nil {
		return ErrNilContext
	}
	if task == nil {
		return ErrNilTask
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.closed {
		return ErrClosed
	}
	if ctx.Err() != nil {
		return ErrSubmitCanceled
	}

	select {
	case e.queue <- submission{ctx: ctx, task: task}:
		return nil
	default:
		return ErrQueueFull
	}
}

// Close prevents new submissions, cancels queued and running work, invokes all
// accepted queued tasks with canceled contexts, and waits for every worker to
// exit. It is safe to call repeatedly or concurrently. Tasks must cooperate
// with context cancellation for Close to return.
func (e *Executor) Close() {
	if e == nil {
		return
	}

	e.closeOnce.Do(func() {
		e.mu.Lock()
		e.closed = true
		e.cancel()
		close(e.queue)
		e.mu.Unlock()
	})

	e.workers.Wait()
}

func (e *Executor) work() {
	defer e.workers.Done()

	for queued := range e.queue {
		e.run(queued)
	}
}

func (e *Executor) run(queued submission) {
	taskCtx, cancelTask := context.WithCancel(queued.ctx)
	shutdownRelayed := make(chan struct{})
	stopShutdownRelay := context.AfterFunc(e.ctx, func() {
		cancelTask()
		close(shutdownRelayed)
	})
	// AfterFunc schedules its callback asynchronously when e.ctx is already
	// canceled. Reflect that state before invoking a task drained during Close,
	// so settlement code observes cancellation on its first context check.
	if e.ctx.Err() != nil {
		cancelTask()
	}

	defer func() {
		if !stopShutdownRelay() {
			<-shutdownRelayed
		}
		cancelTask()
		// A task panic must not remove a worker from the fixed-size pool.
		_ = recover()
	}()

	queued.task(taskCtx)
}
