package rerank

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type blockingRuntimeScorer struct {
	started atomic.Int64
	release chan struct{}
	panic   bool
}

func (scorer *blockingRuntimeScorer) Score(ctx context.Context, request ScoreRequest) ([]ScoreResult, error) {
	scorer.started.Add(1)
	if scorer.panic {
		panic("private panic detail")
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-scorer.release:
	}
	results := make([]ScoreResult, len(request.Candidates))
	for index, candidate := range request.Candidates {
		results[index] = ScoreResult{StableID: candidate.StableID, RelevanceScore: 1}
	}
	return results, nil
}

func TestRuntimeSharesFourProcessSlotsAcrossInstances(t *testing.T) {
	scorer := &blockingRuntimeScorer{release: make(chan struct{})}
	first, err := NewRuntime(scorer, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := NewRuntime(scorer, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	request := validAdapterRequest(t)

	var wait sync.WaitGroup
	errorsOut := make(chan error, MaxConcurrentScores)
	for index := 0; index < MaxConcurrentScores; index++ {
		wait.Add(1)
		runtime := first
		if index%2 == 1 {
			runtime = second
		}
		go func() {
			defer wait.Done()
			_, scoreErr := runtime.Score(context.Background(), request)
			errorsOut <- scoreErr
		}()
	}
	waitForAtomicValue(t, &scorer.started, MaxConcurrentScores)

	waitingContext, cancel := context.WithCancel(context.Background())
	fifthDone := make(chan error, 1)
	go func() {
		_, scoreErr := second.Score(waitingContext, request)
		fifthDone <- scoreErr
	}()
	cancel()
	if err := <-fifthDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("fifth Score() error = %v, want context.Canceled", err)
	}
	if calls := scorer.started.Load(); calls != MaxConcurrentScores {
		t.Fatalf("backend starts = %d, want %d", calls, MaxConcurrentScores)
	}
	queuedRuntime, err := NewRuntime(scorer, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if results, err := queuedRuntime.Score(context.Background(), request); !errors.Is(err, ErrScoringFailed) || results != nil {
		t.Fatalf("queued timeout Score() = %#v, %v; want nil ErrScoringFailed", results, err)
	}
	queuedRuntime.Close()
	if calls := scorer.started.Load(); calls != MaxConcurrentScores {
		t.Fatalf("queued timeout entered backend: starts = %d", calls)
	}

	close(scorer.release)
	wait.Wait()
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatalf("released Score() error = %v", err)
		}
	}
	if results, err := first.Score(context.Background(), request); err != nil || len(results) != len(request.Candidates) {
		t.Fatalf("follow-up Score() = %#v, %v", results, err)
	}
}

func TestRuntimeTimeoutParentCancellationPanicAndLateResult(t *testing.T) {
	request := validAdapterRequest(t)
	t.Run("child timeout", func(t *testing.T) {
		scorer := ScorerFunc(func(ctx context.Context, request ScoreRequest) ([]ScoreResult, error) {
			<-ctx.Done()
			results := make([]ScoreResult, len(request.Candidates))
			for index, candidate := range request.Candidates {
				results[index] = ScoreResult{StableID: candidate.StableID, RelevanceScore: 1}
			}
			return results, nil
		})
		runtime, err := NewRuntime(scorer, 20*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		defer runtime.Close()
		results, err := runtime.Score(context.Background(), request)
		if !errors.Is(err, ErrScoringFailed) || results != nil {
			t.Fatalf("Score() = %#v, %v; want nil ErrScoringFailed", results, err)
		}
	})

	t.Run("parent shorter", func(t *testing.T) {
		scorer := ScorerFunc(func(ctx context.Context, _ ScoreRequest) ([]ScoreResult, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
		runtime, err := NewRuntime(scorer, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer runtime.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		results, err := runtime.Score(ctx, request)
		if !errors.Is(err, context.DeadlineExceeded) || results != nil {
			t.Fatalf("Score() = %#v, %v; want parent deadline", results, err)
		}
	})

	t.Run("panic releases slot", func(t *testing.T) {
		panicking := &blockingRuntimeScorer{release: make(chan struct{}), panic: true}
		runtime, err := NewRuntime(panicking, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer runtime.Close()
		if results, err := runtime.Score(context.Background(), request); !errors.Is(err, ErrScoringFailed) || results != nil {
			t.Fatalf("panic Score() = %#v, %v", results, err)
		}
		healthy := &blockingRuntimeScorer{release: make(chan struct{})}
		close(healthy.release)
		healthyRuntime, err := NewRuntime(healthy, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer healthyRuntime.Close()
		if results, err := healthyRuntime.Score(context.Background(), request); err != nil || len(results) != 2 {
			t.Fatalf("post-panic Score() = %#v, %v", results, err)
		}
	})
}

type closeAwareScorer struct {
	started chan struct{}
	closed  atomic.Int64
	once    sync.Once
}

func (scorer *closeAwareScorer) Score(ctx context.Context, _ ScoreRequest) ([]ScoreResult, error) {
	scorer.once.Do(func() { close(scorer.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func (scorer *closeAwareScorer) CloseIdleConnections() {
	scorer.closed.Add(1)
}

func TestRuntimeCloseCancelsWaitsAndClosesExactlyOnce(t *testing.T) {
	scorer := &closeAwareScorer{started: make(chan struct{})}
	runtime, err := NewRuntime(scorer, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	request := validAdapterRequest(t)
	done := make(chan error, 1)
	go func() {
		_, scoreErr := runtime.Score(context.Background(), request)
		done <- scoreErr
	}()
	<-scorer.started

	closed := make(chan struct{})
	go func() {
		runtime.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close() did not wait for canceled scorer")
	}
	if err := <-done; !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("active Score() after Close = %v, want ErrRuntimeClosed", err)
	}
	runtime.Close()
	if calls := scorer.closed.Load(); calls != 1 {
		t.Fatalf("CloseIdleConnections calls = %d, want 1", calls)
	}
	if results, err := runtime.Score(context.Background(), request); !errors.Is(err, ErrRuntimeClosed) || results != nil {
		t.Fatalf("post-close Score() = %#v, %v", results, err)
	}
}

func TestNewRuntimeValidatesConfiguration(t *testing.T) {
	for _, test := range []struct {
		name    string
		scorer  Scorer
		timeout time.Duration
	}{
		{name: "nil scorer", timeout: time.Second},
		{name: "zero timeout", scorer: ScorerFunc(func(context.Context, ScoreRequest) ([]ScoreResult, error) { return nil, nil })},
		{name: "over maximum", scorer: ScorerFunc(func(context.Context, ScoreRequest) ([]ScoreResult, error) { return nil, nil }), timeout: MaximumScorerTimeout + time.Nanosecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			if runtime, err := NewRuntime(test.scorer, test.timeout); !errors.Is(err, ErrNotConfigured) || runtime != nil {
				t.Fatalf("NewRuntime() = %#v, %v", runtime, err)
			}
		})
	}
}

func waitForAtomicValue(t *testing.T, value *atomic.Int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if value.Load() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("value = %d, want %d", value.Load(), want)
}
