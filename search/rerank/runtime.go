package rerank

import (
	"context"
	"errors"
	"sync"
	"time"
)

const (
	DefaultScorerTimeout = 5 * time.Second
	MaximumScorerTimeout = 10 * time.Second
	MaxConcurrentScores  = 4
)

var (
	ErrRuntimeClosed  = errors.New("rerank: runtime is closed")
	processScoreSlots = make(chan struct{}, MaxConcurrentScores)
)

type idleConnectionCloser interface {
	CloseIdleConnections()
}

// Runtime applies the process-wide scorer concurrency gate and one bounded
// child deadline around a transport-neutral Scorer. It never detaches backend
// work: a scorer must honor context cancellation before a slot is released.
type Runtime struct {
	scorer  Scorer
	timeout time.Duration
	slots   chan struct{}

	lifecycle context.Context
	cancel    context.CancelFunc

	mu        sync.Mutex
	closed    bool
	active    sync.WaitGroup
	closeOnce sync.Once
}

func NewRuntime(scorer Scorer, timeout time.Duration) (*Runtime, error) {
	return newRuntime(scorer, timeout, processScoreSlots)
}

func newRuntime(scorer Scorer, timeout time.Duration, slots chan struct{}) (*Runtime, error) {
	if isNilScorer(scorer) || timeout <= 0 || timeout > MaximumScorerTimeout || slots == nil || cap(slots) != MaxConcurrentScores {
		return nil, ErrNotConfigured
	}
	lifecycle, cancel := context.WithCancel(context.Background())
	return &Runtime{scorer: scorer, timeout: timeout, slots: slots, lifecycle: lifecycle, cancel: cancel}, nil
}

func (runtime *Runtime) Score(ctx context.Context, request ScoreRequest) ([]ScoreResult, error) {
	if runtime == nil {
		return nil, ErrNotConfigured
	}
	if ctx == nil {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !runtime.admit() {
		return nil, ErrRuntimeClosed
	}
	defer runtime.active.Done()

	callContext, cancel := context.WithTimeout(ctx, runtime.timeout)
	stopLifecycle := context.AfterFunc(runtime.lifecycle, cancel)
	defer func() {
		stopLifecycle()
		cancel()
	}()

	if err := runtime.contextError(ctx, callContext); err != nil {
		return nil, err
	}
	select {
	case runtime.slots <- struct{}{}:
		if err := runtime.contextError(ctx, callContext); err != nil {
			<-runtime.slots
			return nil, err
		}
	case <-callContext.Done():
		return nil, runtime.contextError(ctx, callContext)
	}
	defer func() { <-runtime.slots }()

	results, scoreErr := invokeScorer(callContext, runtime.scorer, cloneScoreRequest(request))
	if err := runtime.contextError(ctx, callContext); err != nil {
		return nil, err
	}
	if scoreErr != nil {
		return nil, ErrScoringFailed
	}
	cloned := make([]ScoreResult, len(results))
	for index, result := range results {
		cloned[index] = ScoreResult{StableID: result.StableID, RelevanceScore: result.RelevanceScore}
	}
	return cloned, nil
}

func (runtime *Runtime) Close() {
	if runtime == nil {
		return
	}
	runtime.closeOnce.Do(func() {
		runtime.mu.Lock()
		runtime.closed = true
		runtime.cancel()
		runtime.mu.Unlock()
		runtime.active.Wait()
		if closer, ok := runtime.scorer.(idleConnectionCloser); ok {
			closer.CloseIdleConnections()
		}
	})
}

func (runtime *Runtime) admit() bool {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.closed {
		return false
	}
	runtime.active.Add(1)
	return true
}

func (runtime *Runtime) contextError(parent, child context.Context) error {
	if err := parent.Err(); err != nil {
		return err
	}
	runtime.mu.Lock()
	closed := runtime.closed
	runtime.mu.Unlock()
	if closed || runtime.lifecycle.Err() != nil {
		return ErrRuntimeClosed
	}
	if child.Err() != nil {
		return ErrScoringFailed
	}
	return nil
}
