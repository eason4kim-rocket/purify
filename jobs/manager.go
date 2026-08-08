package jobs

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	// ErrManagerClosed is returned when a mutating operation begins after Close.
	ErrManagerClosed = errors.New("jobs: manager is closed")
	// ErrManagerFull is returned when running and retained completed jobs fill capacity.
	ErrManagerFull = errors.New("jobs: manager is full")
	// ErrDuplicateJob is returned when Create receives an ID already in the manager.
	ErrDuplicateJob = errors.New("jobs: duplicate job")
	// ErrJobNotFound is returned when a requested job is not retained by the manager.
	ErrJobNotFound = errors.New("jobs: job not found")
	// ErrJobCompleted is returned when a final job is updated or completed again.
	ErrJobCompleted = errors.New("jobs: job is already completed")
	// ErrInvalidJobID is returned when Create receives an empty job ID.
	ErrInvalidJobID = errors.New("jobs: job ID is empty")
	// ErrInvalidManagerConfig is returned when manager bounds or cloning are invalid.
	ErrInvalidManagerConfig = errors.New("jobs: invalid manager configuration")
)

// CloneFunc returns a copy whose reference-backed fields do not alias value.
// Manager calls it at every ownership boundary so snapshots and mutation
// callbacks cannot retain access to internal slices, maps, or pointers.
type CloneFunc[T any] func(value T) T

type managedJob[T any] struct {
	value       T
	cancel      context.CancelFunc
	completed   bool
	completedAt time.Time
}

type managerClock interface {
	Now() time.Time
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// Manager is a bounded, in-memory owner of running and recently completed
// jobs. Capacity includes both states. Completed jobs remain available until a
// periodic sweep observes that their TTL has elapsed.
type Manager[T any] struct {
	mu       sync.RWMutex
	jobs     map[string]*managedJob[T]
	capacity int
	ttl      time.Duration
	clone    CloneFunc[T]
	clock    managerClock
	closed   bool

	ctx    context.Context
	cancel context.CancelFunc
	ticker *time.Ticker
	stop   chan struct{}
	done   chan struct{}

	closeOnce sync.Once
}

// NewManager starts a manager with a fixed total capacity. completedTTL and
// sweepInterval must be positive, and clone must deeply copy reference-backed
// fields in T.
func NewManager[T any](
	capacity int,
	completedTTL time.Duration,
	sweepInterval time.Duration,
	clone CloneFunc[T],
) (*Manager[T], error) {
	return newManagerWithClock(capacity, completedTTL, sweepInterval, clone, wallClock{})
}

func newManagerWithClock[T any](
	capacity int,
	completedTTL time.Duration,
	sweepInterval time.Duration,
	clone CloneFunc[T],
	clock managerClock,
) (*Manager[T], error) {
	if capacity <= 0 || completedTTL <= 0 || sweepInterval <= 0 || clone == nil || clock == nil {
		return nil, ErrInvalidManagerConfig
	}

	ctx, cancel := context.WithCancel(context.Background())
	manager := &Manager[T]{
		jobs:     make(map[string]*managedJob[T], capacity),
		capacity: capacity,
		ttl:      completedTTL,
		clone:    clone,
		clock:    clock,
		ctx:      ctx,
		cancel:   cancel,
		ticker:   time.NewTicker(sweepInterval),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go manager.sweepLoop()

	return manager, nil
}

// Create stores an isolated copy of initial and returns a background job
// context. The context is canceled by Complete or Close. Create rejects a
// duplicate ID before checking capacity so callers receive a stable diagnosis.
func (m *Manager[T]) Create(id string, initial T) (context.Context, error) {
	if m == nil {
		return nil, ErrManagerClosed
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil, ErrManagerClosed
	}
	if id == "" {
		return nil, ErrInvalidJobID
	}
	if _, exists := m.jobs[id]; exists {
		return nil, ErrDuplicateJob
	}
	if len(m.jobs) >= m.capacity {
		return nil, ErrManagerFull
	}

	stored := m.clone(initial)
	jobCtx, cancel := context.WithCancel(m.ctx)
	m.jobs[id] = &managedJob[T]{
		value:  stored,
		cancel: cancel,
	}
	return jobCtx, nil
}

// Update atomically mutates a private copy of a running job. A second clone is
// stored after mutate returns, so a callback that retains its argument cannot
// subsequently change manager state. A nil mutate function is a no-op.
func (m *Manager[T]) Update(id string, mutate func(*T)) error {
	if m == nil {
		return ErrManagerClosed
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return ErrManagerClosed
	}
	job, exists := m.jobs[id]
	if !exists {
		return ErrJobNotFound
	}
	if job.completed {
		return ErrJobCompleted
	}
	m.mutateLocked(job, mutate)
	return nil
}

// Complete atomically applies the final mutation, makes the value immutable,
// starts its retention TTL, and cancels the context returned by Create. A nil
// mutate function is permitted.
func (m *Manager[T]) Complete(id string, mutate func(*T)) error {
	if m == nil {
		return ErrManagerClosed
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return ErrManagerClosed
	}
	job, exists := m.jobs[id]
	if !exists {
		return ErrJobNotFound
	}
	if job.completed {
		return ErrJobCompleted
	}

	m.mutateLocked(job, mutate)
	job.completed = true
	job.completedAt = m.clock.Now()
	job.cancel()
	job.cancel = nil
	return nil
}

// Snapshot returns a detached copy of a running or retained completed job.
// Reads remain available after Close, but no further mutations are accepted.
func (m *Manager[T]) Snapshot(id string) (T, bool) {
	var zero T
	if m == nil {
		return zero, false
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	job, exists := m.jobs[id]
	if !exists {
		return zero, false
	}
	return m.clone(job.value), true
}

// Len returns the number of running and retained completed jobs.
func (m *Manager[T]) Len() int {
	if m == nil {
		return 0
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.jobs)
}

// Close stops cleanup, cancels every running job context, and rejects future
// mutations. It waits for the sweeper goroutine and is safe to call repeatedly
// or concurrently.
func (m *Manager[T]) Close() {
	if m == nil {
		return
	}

	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		for _, job := range m.jobs {
			if job.cancel != nil {
				job.cancel()
				job.cancel = nil
			}
		}
		m.cancel()
		m.mu.Unlock()

		m.ticker.Stop()
		close(m.stop)
		<-m.done
	})
}

func (m *Manager[T]) mutateLocked(job *managedJob[T], mutate func(*T)) {
	working := m.clone(job.value)
	if mutate != nil {
		mutate(&working)
	}
	job.value = m.clone(working)
}

func (m *Manager[T]) sweepLoop() {
	defer close(m.done)

	for {
		select {
		case <-m.stop:
			return
		case <-m.ticker.C:
			m.sweepExpired(m.clock.Now())
		}
	}
}

func (m *Manager[T]) sweepExpired(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return
	}
	for id, job := range m.jobs {
		if job.completed && !now.Before(job.completedAt.Add(m.ttl)) {
			delete(m.jobs, id)
		}
	}
}
