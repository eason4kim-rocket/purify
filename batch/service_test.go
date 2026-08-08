package batch

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/use-agent/purify/jobs"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/scrape"
	"github.com/use-agent/purify/webhook"
)

type runnerFunc func(context.Context, *models.ScrapeRequest, scrape.Observer) (*scrape.Result, error)

func (run runnerFunc) Run(ctx context.Context, request *models.ScrapeRequest, observe scrape.Observer) (*scrape.Result, error) {
	return run(ctx, request, observe)
}

func TestNewServiceValidatesDependenciesAndManagerConfig(t *testing.T) {
	runner := runnerFunc(func(_ context.Context, request *models.ScrapeRequest, _ scrape.Observer) (*scrape.Result, error) {
		return &scrape.Result{Response: successResponse(request.URL)}, nil
	})
	executor, err := jobs.NewExecutor(1, 1)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	defer executor.Close()

	tests := []struct {
		name     string
		runner   Runner
		executor *jobs.Executor
		config   Config
		want     error
	}{
		{name: "nil runner", executor: executor},
		{name: "nil executor", runner: runner},
		{name: "negative capacity", runner: runner, executor: executor, config: Config{Capacity: -1}, want: jobs.ErrInvalidManagerConfig},
		{name: "negative ttl", runner: runner, executor: executor, config: Config{TTL: -time.Second}, want: jobs.ErrInvalidManagerConfig},
		{name: "negative sweep", runner: runner, executor: executor, config: Config{SweepInterval: -time.Second}, want: jobs.ErrInvalidManagerConfig},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, err := NewService(test.runner, test.executor, test.config)
			if err == nil {
				service.Close()
				t.Fatal("NewService() error = nil")
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("NewService() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestSubmitReportsIDGenerationFailures(t *testing.T) {
	runner := runnerFunc(func(_ context.Context, request *models.ScrapeRequest, _ scrape.Observer) (*scrape.Result, error) {
		return &scrape.Result{Response: successResponse(request.URL)}, nil
	})

	t.Run("generator error", func(t *testing.T) {
		sentinel := errors.New("id source unavailable")
		service, executor := newTestService(t, runner, 1, 1, Config{IDGenerator: func() (string, error) { return "", sentinel }})
		defer executor.Close()
		defer service.Close()
		if _, err := service.Submit(models.BatchRequest{URLs: []string{"one"}}); !errors.Is(err, sentinel) {
			t.Fatalf("Submit() error = %v, want %v", err, sentinel)
		}
	})

	t.Run("empty id", func(t *testing.T) {
		service, executor := newTestService(t, runner, 1, 1, Config{IDGenerator: fixedID("  ")})
		defer executor.Close()
		defer service.Close()
		if _, err := service.Submit(models.BatchRequest{URLs: []string{"one"}}); !errors.Is(err, ErrEmptyJobID) {
			t.Fatalf("Submit() error = %v, want %v", err, ErrEmptyJobID)
		}
	})

	t.Run("duplicate id", func(t *testing.T) {
		service, executor := newTestService(t, runner, 1, 2, Config{IDGenerator: fixedID("batch-duplicate")})
		defer executor.Close()
		defer service.Close()
		if _, err := service.Submit(models.BatchRequest{URLs: []string{"one"}}); err != nil {
			t.Fatalf("Submit(first) error = %v", err)
		}
		if _, err := service.Submit(models.BatchRequest{URLs: []string{"two"}}); !errors.Is(err, jobs.ErrDuplicateJob) {
			t.Fatalf("Submit(duplicate) error = %v, want %v", err, jobs.ErrDuplicateJob)
		}
	})
}

func TestNotifierPanicCannotEscapeSynchronousCompletion(t *testing.T) {
	executor, err := jobs.NewExecutor(1, 1)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	executor.Close()
	runner := runnerFunc(func(context.Context, *models.ScrapeRequest, scrape.Observer) (*scrape.Result, error) {
		return nil, errors.New("must not run")
	})
	service, err := NewService(runner, executor, Config{
		IDGenerator: fixedID("batch-notifier-panic"),
		Notifier: CompletionNotifierFunc(func(string, string, *webhook.Event) {
			panic("notifier exploded")
		}),
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	defer service.Close()

	accepted, err := service.Submit(models.BatchRequest{
		URLs:       []string{"one"},
		WebhookURL: "https://webhook.test/callback",
	})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	status := awaitTerminal(t, service, accepted.ID)
	assertRejectedBatch(t, status, models.ErrCodeInternal, "batch worker executor is closed")
}

func TestServicePreservesInputOrderAndPropagatesOptions(t *testing.T) {
	waitForNetwork := false
	urls := []string{"https://example.test/slow", "https://example.test/fast", "https://example.test/middle"}
	delays := map[string]time.Duration{
		urls[0]: 35 * time.Millisecond,
		urls[1]: 2 * time.Millisecond,
		urls[2]: 15 * time.Millisecond,
	}

	var requests sync.Map
	runner := runnerFunc(func(ctx context.Context, request *models.ScrapeRequest, _ scrape.Observer) (*scrape.Result, error) {
		if _, hasDeadline := ctx.Deadline(); hasDeadline {
			return nil, errors.New("batch job context unexpectedly inherited a deadline")
		}
		requestCopy := *request
		if request.WaitForNetworkIdle != nil {
			wait := *request.WaitForNetworkIdle
			requestCopy.WaitForNetworkIdle = &wait
		}
		requests.Store(request.URL, requestCopy)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delays[request.URL]):
		}
		return &scrape.Result{Response: successResponse(request.URL)}, nil
	})
	service, executor := newTestService(t, runner, 3, len(urls), Config{IDGenerator: fixedID("batch-order")})
	defer executor.Close()
	defer service.Close()

	request := models.BatchRequest{
		URLs: urls,
		Options: models.BatchOptions{
			OutputFormat:       "html",
			ExtractMode:        "raw",
			WaitForNetworkIdle: &waitForNetwork,
			Timeout:            17,
			Stealth:            true,
		},
	}
	accepted, err := service.Submit(request)
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if accepted.ID != "batch-order" || accepted.Status != statusProcessing || accepted.Total != len(urls) {
		t.Fatalf("Submit() = %#v", accepted)
	}

	// Mutating caller-owned input after Submit must not affect queued work.
	urls[0] = "https://mutated.invalid/"
	waitForNetwork = true

	status := awaitTerminal(t, service, accepted.ID)
	if status.Status != statusCompleted || status.Completed != 3 || status.Total != 3 {
		t.Fatalf("terminal status = %#v", status)
	}
	wantURLs := []string{"https://example.test/slow", "https://example.test/fast", "https://example.test/middle"}
	for index, wantURL := range wantURLs {
		if got := status.Results[index]; got == nil || got.Content != wantURL {
			t.Fatalf("result[%d] = %#v, want content %q", index, got, wantURL)
		}
		stored, ok := requests.Load(wantURL)
		if !ok {
			t.Fatalf("runner did not receive %q", wantURL)
		}
		got := stored.(models.ScrapeRequest)
		if got.OutputFormat != "html" || got.ExtractMode != "raw" || got.Timeout != 17 || !got.Stealth {
			t.Fatalf("request options for %q = %#v", wantURL, got)
		}
		if got.WaitForNetworkIdle == nil || *got.WaitForNetworkIdle {
			t.Fatalf("wait_for_network_idle for %q = %#v, want false", wantURL, got.WaitForNetworkIdle)
		}
	}
}

func TestServiceRetainsFailuresWithoutDroppingSuccesses(t *testing.T) {
	tests := []struct {
		name       string
		urls       []string
		wantStatus string
	}{
		{name: "partial", urls: []string{"ok-1", "fail", "ok-2"}, wantStatus: statusPartial},
		{name: "all failed", urls: []string{"fail-1", "fail-2"}, wantStatus: statusFailed},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := runnerFunc(func(_ context.Context, request *models.ScrapeRequest, observe scrape.Observer) (*scrape.Result, error) {
				if request.URL == "fail" || request.URL == "fail-1" || request.URL == "fail-2" {
					failure := richFailureResponse(request.URL)
					observe(scrape.Event{Type: scrape.EventError, URL: request.URL, Response: failure})
					// EventError is an ownership boundary: later runner mutation must
					// not corrupt the batch result.
					failure.Content = "mutated"
					failure.Links.Internal[0].Href = "mutated"
					failure.Images[0].Src = "mutated"
					failure.Error.Message = "mutated"
					failure.Quality.Warnings[0] = models.QualityReasonErrorPage
					failure.Quality.FetchAttempts[0].Engine = "mutated"
					return nil, models.NewScrapeError(models.ErrCodeNavigation, "failed", nil)
				}
				return &scrape.Result{Response: successResponse(request.URL)}, nil
			})
			service, executor := newTestService(t, runner, 3, len(test.urls), Config{IDGenerator: fixedID("batch-" + test.name)})
			defer executor.Close()
			defer service.Close()

			accepted, err := service.Submit(models.BatchRequest{URLs: test.urls})
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			status := awaitTerminal(t, service, accepted.ID)
			if status.Status != test.wantStatus || status.Completed != len(test.urls) || status.Total != len(test.urls) {
				t.Fatalf("terminal status = %#v", status)
			}
			for index, targetURL := range test.urls {
				response := status.Results[index]
				if response == nil {
					t.Fatalf("result[%d] is nil", index)
				}
				if targetURL == "fail" || targetURL == "fail-1" || targetURL == "fail-2" {
					assertRichFailure(t, response, targetURL)
				} else if !response.Success || response.Content != targetURL {
					t.Fatalf("success result[%d] = %#v", index, response)
				}
			}
		})
	}
}

func TestServicesShareExecutorConcurrencyLimit(t *testing.T) {
	const workerCount = 2
	executor, err := jobs.NewExecutor(workerCount, 8)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	defer executor.Close()

	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	started := make(chan struct{}, 8)
	var active atomic.Int32
	var maximum atomic.Int32
	runner := runnerFunc(func(ctx context.Context, request *models.ScrapeRequest, _ scrape.Observer) (*scrape.Result, error) {
		current := active.Add(1)
		updateMaximum(&maximum, current)
		started <- struct{}{}
		defer active.Add(-1)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return &scrape.Result{Response: successResponse(request.URL)}, nil
		}
	})

	ids := sequenceIDs("batch-a", "batch-b")
	serviceA, err := NewService(runner, executor, Config{IDGenerator: ids})
	if err != nil {
		t.Fatalf("NewService(A) error = %v", err)
	}
	defer serviceA.Close()
	serviceB, err := NewService(runner, executor, Config{IDGenerator: ids})
	if err != nil {
		t.Fatalf("NewService(B) error = %v", err)
	}
	defer serviceB.Close()

	batchA, err := serviceA.Submit(models.BatchRequest{URLs: []string{"a1", "a2", "a3", "a4"}})
	if err != nil {
		t.Fatalf("Submit(A) error = %v", err)
	}
	batchB, err := serviceB.Submit(models.BatchRequest{URLs: []string{"b1", "b2", "b3", "b4"}})
	if err != nil {
		t.Fatalf("Submit(B) error = %v", err)
	}

	for range workerCount {
		receive(t, started, "worker start")
	}
	select {
	case <-started:
		t.Fatal("shared executor started work above its global limit")
	case <-time.After(20 * time.Millisecond):
	}
	if got := maximum.Load(); got != workerCount {
		t.Fatalf("maximum concurrency = %d, want %d", got, workerCount)
	}

	releaseOnce.Do(func() { close(release) })
	if status := awaitTerminal(t, serviceA, batchA.ID); status.Status != statusCompleted {
		t.Fatalf("batch A status = %#v", status)
	}
	if status := awaitTerminal(t, serviceB, batchB.ID); status.Status != statusCompleted {
		t.Fatalf("batch B status = %#v", status)
	}
}

func TestQueueAndClosedExecutorRejectionsBecomeStableResults(t *testing.T) {
	t.Run("queue full", func(t *testing.T) {
		executor, err := jobs.NewExecutor(1, 1)
		if err != nil {
			t.Fatalf("NewExecutor() error = %v", err)
		}
		defer executor.Close()
		release := make(chan struct{})
		var releaseOnce sync.Once
		defer releaseOnce.Do(func() { close(release) })
		started := make(chan struct{})
		if err := executor.Submit(context.Background(), func(context.Context) {
			close(started)
			<-release
		}); err != nil {
			t.Fatalf("Submit(blocker) error = %v", err)
		}
		receive(t, started, "queue blocker")
		if err := executor.Submit(context.Background(), func(context.Context) {}); err != nil {
			t.Fatalf("Submit(queued marker) error = %v", err)
		}

		var runnerCalls atomic.Int32
		runner := runnerFunc(func(context.Context, *models.ScrapeRequest, scrape.Observer) (*scrape.Result, error) {
			runnerCalls.Add(1)
			return nil, errors.New("must not run")
		})
		service, err := NewService(runner, executor, Config{IDGenerator: fixedID("batch-queue-full")})
		if err != nil {
			t.Fatalf("NewService() error = %v", err)
		}
		defer service.Close()
		accepted, err := service.Submit(models.BatchRequest{URLs: []string{"one", "two"}})
		if err != nil {
			t.Fatalf("Submit() error = %v", err)
		}
		status := awaitTerminal(t, service, accepted.ID)
		assertRejectedBatch(t, status, models.ErrCodeRateLimited, "batch worker queue is full")
		if got := runnerCalls.Load(); got != 0 {
			t.Fatalf("runner calls = %d, want 0", got)
		}
		releaseOnce.Do(func() { close(release) })
	})

	t.Run("executor closed", func(t *testing.T) {
		executor, err := jobs.NewExecutor(1, 1)
		if err != nil {
			t.Fatalf("NewExecutor() error = %v", err)
		}
		executor.Close()
		runner := runnerFunc(func(context.Context, *models.ScrapeRequest, scrape.Observer) (*scrape.Result, error) {
			return nil, errors.New("must not run")
		})
		service, err := NewService(runner, executor, Config{IDGenerator: fixedID("batch-closed-executor")})
		if err != nil {
			t.Fatalf("NewService() error = %v", err)
		}
		defer service.Close()
		accepted, err := service.Submit(models.BatchRequest{URLs: []string{"one"}})
		if err != nil {
			t.Fatalf("Submit() error = %v", err)
		}
		status := awaitTerminal(t, service, accepted.ID)
		assertRejectedBatch(t, status, models.ErrCodeInternal, "batch worker executor is closed")
	})

	t.Run("submission canceled", func(t *testing.T) {
		response := executorFailureResponse(jobs.ErrSubmitCanceled)
		if response.Success || response.Error == nil || response.Error.Code != models.ErrCodeTimeout || response.Error.Message != "batch job was canceled before execution" {
			t.Fatalf("canceled response = %#v", response)
		}
	})
}

func TestSnapshotsAndWebhookAreIsolatedAndConsistent(t *testing.T) {
	type delivered struct {
		url      string
		secret   string
		event    webhook.Event
		snapshot models.BatchStatusResponse
	}
	deliveries := make(chan delivered, 2)
	var calls atomic.Int32
	notifier := CompletionNotifierFunc(func(url, secret string, event *webhook.Event) {
		calls.Add(1)
		snapshot := event.Data.(models.BatchStatusResponse)
		deliveries <- delivered{url: url, secret: secret, event: *event, snapshot: cloneStatus(snapshot)}

		// A notifier is allowed to retain and mutate its payload; manager state
		// must remain unchanged.
		snapshot.Results[0].Content = "notifier mutation"
		snapshot.Results[0].Links.Internal[0].Href = "notifier mutation"
		snapshot.Results[0].Images[0].Src = "notifier mutation"
		snapshot.Results[0].Error.Message = "notifier mutation"
		snapshot.Results[0].Quality.Warnings[0] = models.QualityReasonErrorPage
		snapshot.Results[0].Quality.FetchAttempts[0].Engine = "notifier mutation"
		event.Data = snapshot
	})

	runner := runnerFunc(func(_ context.Context, request *models.ScrapeRequest, _ scrape.Observer) (*scrape.Result, error) {
		return &scrape.Result{Response: richSuccessResponse(request.URL)}, nil
	})
	fixedNow := time.Unix(1_800_000_000, 0)
	service, executor := newTestService(t, runner, 1, 1, Config{
		IDGenerator: fixedID("batch-webhook"),
		Notifier:    notifier,
		Now:         func() time.Time { return fixedNow },
	})
	defer executor.Close()
	defer service.Close()

	accepted, err := service.Submit(models.BatchRequest{
		URLs:          []string{"source"},
		WebhookURL:    "https://webhook.test/callback",
		WebhookSecret: "secret",
	})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	terminal := awaitTerminal(t, service, accepted.ID)
	delivery := receive(t, deliveries, "completion webhook")
	if delivery.url != "https://webhook.test/callback" || delivery.secret != "secret" {
		t.Fatalf("delivery target = %#v", delivery)
	}
	if delivery.event.Type != "batch.completed" || delivery.event.JobID != accepted.ID || delivery.event.Timestamp != fixedNow.Unix() {
		t.Fatalf("event envelope = %#v", delivery.event)
	}
	if !reflect.DeepEqual(delivery.snapshot, *terminal) {
		t.Fatalf("webhook snapshot = %#v, Get = %#v", delivery.snapshot, *terminal)
	}
	select {
	case extra := <-deliveries:
		t.Fatalf("unexpected second delivery: %#v", extra)
	case <-time.After(20 * time.Millisecond):
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("notifier calls = %d, want 1", got)
	}

	terminal.Results[0].Content = "caller mutation"
	terminal.Results[0].Links.Internal[0].Href = "caller mutation"
	terminal.Results[0].Links.External[0].Href = "caller mutation"
	terminal.Results[0].Images[0].Src = "caller mutation"
	terminal.Results[0].Error.Message = "caller mutation"
	terminal.Results[0].Quality.Warnings[0] = models.QualityReasonErrorPage
	terminal.Results[0].Quality.FetchAttempts[0].Engine = "caller mutation"

	again, ok := service.Get(accepted.ID)
	if !ok {
		t.Fatal("Get() lost retained job")
	}
	assertRichSuccess(t, again.Results[0], "source")
}

func TestRunnerContractFailuresStillConverge(t *testing.T) {
	tests := []struct {
		name   string
		runner Runner
	}{
		{
			name: "panic",
			runner: runnerFunc(func(context.Context, *models.ScrapeRequest, scrape.Observer) (*scrape.Result, error) {
				panic("runner exploded")
			}),
		},
		{
			name: "empty result",
			runner: runnerFunc(func(context.Context, *models.ScrapeRequest, scrape.Observer) (*scrape.Result, error) {
				return nil, nil
			}),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, executor := newTestService(t, test.runner, 1, 1, Config{IDGenerator: fixedID("batch-contract-" + test.name)})
			defer executor.Close()
			defer service.Close()
			accepted, err := service.Submit(models.BatchRequest{URLs: []string{"one"}})
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			status := awaitTerminal(t, service, accepted.ID)
			if status.Status != statusFailed || status.Completed != 1 || status.Results[0] == nil || status.Results[0].Success || status.Results[0].Error == nil || status.Results[0].Error.Code != models.ErrCodeInternal {
				t.Fatalf("terminal status = %#v", status)
			}
		})
	}
}

func TestTTLExpiresCompletedJobs(t *testing.T) {
	runner := runnerFunc(func(_ context.Context, request *models.ScrapeRequest, _ scrape.Observer) (*scrape.Result, error) {
		return &scrape.Result{Response: successResponse(request.URL)}, nil
	})
	service, executor := newTestService(t, runner, 1, 1, Config{
		IDGenerator:   fixedID("batch-ttl"),
		TTL:           25 * time.Millisecond,
		SweepInterval: 5 * time.Millisecond,
	})
	defer executor.Close()
	defer service.Close()

	accepted, err := service.Submit(models.BatchRequest{URLs: []string{"one"}})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if status := awaitTerminal(t, service, accepted.ID); status.Status != statusCompleted {
		t.Fatalf("terminal status = %#v", status)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := service.Get(accepted.ID); !ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("completed batch was not evicted after TTL")
}

func TestCloseCancelsJobsButLeavesSharedExecutorOpen(t *testing.T) {
	executor, err := jobs.NewExecutor(1, 2)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	defer executor.Close()
	started := make(chan struct{})
	canceled := make(chan error, 1)
	runner := runnerFunc(func(ctx context.Context, _ *models.ScrapeRequest, observe scrape.Observer) (*scrape.Result, error) {
		close(started)
		<-ctx.Done()
		canceled <- ctx.Err()
		failure := failureResponse(ctx.Err())
		observe(scrape.Event{Type: scrape.EventError, Response: failure})
		return nil, ctx.Err()
	})
	service, err := NewService(runner, executor, Config{IDGenerator: fixedID("batch-close")})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}

	accepted, err := service.Submit(models.BatchRequest{URLs: []string{"one"}})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	receive(t, started, "runner start")
	service.Close()
	if got := receive(t, canceled, "runner cancellation"); !errors.Is(got, context.Canceled) {
		t.Fatalf("runner context error = %v, want %v", got, context.Canceled)
	}
	// Close is idempotent and retained snapshots remain readable by manager
	// contract, even though the running job can no longer be mutated.
	service.Close()
	status, ok := service.Get(accepted.ID)
	if !ok || status.Status != statusProcessing || status.Completed != 0 {
		t.Fatalf("snapshot after Close = %#v, ok=%v", status, ok)
	}

	executorStillWorks := make(chan struct{})
	if err := executor.Submit(context.Background(), func(context.Context) { close(executorStillWorks) }); err != nil {
		t.Fatalf("shared Executor.Submit() after Service.Close error = %v", err)
	}
	receive(t, executorStillWorks, "shared executor task")
	if _, err := service.Submit(models.BatchRequest{URLs: []string{"two"}}); !errors.Is(err, jobs.ErrManagerClosed) {
		t.Fatalf("Submit() after Close error = %v, want %v", err, jobs.ErrManagerClosed)
	}
}

func TestCapacityAndValidation(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	runner := runnerFunc(func(ctx context.Context, request *models.ScrapeRequest, _ scrape.Observer) (*scrape.Result, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return &scrape.Result{Response: successResponse(request.URL)}, nil
		}
	})
	executor, err := jobs.NewExecutor(1, 2)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	defer executor.Close()
	service, err := NewService(runner, executor, Config{
		Capacity:    1,
		IDGenerator: sequenceIDs("batch-one", "batch-two"),
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	defer service.Close()

	if _, err := service.Submit(models.BatchRequest{}); !errors.Is(err, ErrInvalidBatchSize) {
		t.Fatalf("Submit(empty) error = %v, want %v", err, ErrInvalidBatchSize)
	}
	tooMany := make([]string, maximumBatchSize+1)
	if _, err := service.Submit(models.BatchRequest{URLs: tooMany}); !errors.Is(err, ErrInvalidBatchSize) {
		t.Fatalf("Submit(too many) error = %v, want %v", err, ErrInvalidBatchSize)
	}
	if _, err := service.Submit(models.BatchRequest{URLs: []string{"one"}}); err != nil {
		t.Fatalf("Submit(first) error = %v", err)
	}
	if _, err := service.Submit(models.BatchRequest{URLs: []string{"two"}}); !errors.Is(err, jobs.ErrManagerFull) {
		t.Fatalf("Submit(over capacity) error = %v, want %v", err, jobs.ErrManagerFull)
	}
	releaseOnce.Do(func() { close(release) })
}

func newTestService(t *testing.T, runner Runner, workers, queue int, config Config) (*Service, *jobs.Executor) {
	t.Helper()
	executor, err := jobs.NewExecutor(workers, queue)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	service, err := NewService(runner, executor, config)
	if err != nil {
		executor.Close()
		t.Fatalf("NewService() error = %v", err)
	}
	return service, executor
}

func awaitTerminal(t *testing.T, service *Service, id string) *models.BatchStatusResponse {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status, ok := service.Get(id)
		if !ok {
			t.Fatalf("Get(%q) did not find job", id)
		}
		if status.Status != statusProcessing {
			return status
		}
		time.Sleep(time.Millisecond)
	}
	status, _ := service.Get(id)
	t.Fatalf("batch %q did not finish: %#v", id, status)
	return nil
}

func fixedID(id string) IDGenerator {
	return func() (string, error) { return id, nil }
}

func sequenceIDs(ids ...string) IDGenerator {
	var index atomic.Int32
	return func() (string, error) {
		position := int(index.Add(1)) - 1
		if position >= len(ids) {
			return "", fmt.Errorf("id sequence exhausted")
		}
		return ids[position], nil
	}
}

func successResponse(content string) *models.ScrapeResponse {
	return &models.ScrapeResponse{Success: true, Content: content, FinalURL: content}
}

func richSuccessResponse(content string) *models.ScrapeResponse {
	return &models.ScrapeResponse{
		Success: true,
		Content: content,
		Links: models.LinksResult{
			Internal: []models.Link{{Href: "internal"}},
			External: []models.Link{{Href: "external"}},
		},
		Images: []models.Image{{Src: "image"}},
		Error:  &models.ErrorDetail{Code: "diagnostic", Message: "original error detail"},
		Quality: &models.QualityInfo{
			Status:        models.QualityStatusGood,
			Warnings:      []models.QualityReason{models.QualityReasonLowContentQuality},
			FetchAttempts: []models.FetchAttempt{{Engine: "http", Outcome: models.FetchAttemptSelected}},
		},
	}
}

func richFailureResponse(targetURL string) *models.ScrapeResponse {
	return &models.ScrapeResponse{
		Success: false,
		Content: "failure:" + targetURL,
		Links: models.LinksResult{
			Internal: []models.Link{{Href: "failure-internal"}},
			External: []models.Link{{Href: "failure-external"}},
		},
		Images: []models.Image{{Src: "failure-image"}},
		Error:  &models.ErrorDetail{Code: models.ErrCodeNavigation, Message: "original failure"},
		Quality: &models.QualityInfo{
			Status:        models.QualityStatusUnusable,
			Warnings:      []models.QualityReason{models.QualityReasonChallengePage},
			FetchAttempts: []models.FetchAttempt{{Engine: "rod", Outcome: models.FetchAttemptRejected}},
		},
	}
}

func assertRichFailure(t *testing.T, response *models.ScrapeResponse, targetURL string) {
	t.Helper()
	if response.Success || response.Content != "failure:"+targetURL || response.Error == nil || response.Error.Message != "original failure" {
		t.Fatalf("failure response = %#v", response)
	}
	if response.Links.Internal[0].Href != "failure-internal" || response.Links.External[0].Href != "failure-external" || response.Images[0].Src != "failure-image" {
		t.Fatalf("failure references = %#v", response)
	}
	if response.Quality == nil || response.Quality.Warnings[0] != models.QualityReasonChallengePage || response.Quality.FetchAttempts[0].Engine != "rod" {
		t.Fatalf("failure quality = %#v", response.Quality)
	}
}

func assertRichSuccess(t *testing.T, response *models.ScrapeResponse, content string) {
	t.Helper()
	if response == nil || !response.Success || response.Content != content {
		t.Fatalf("success response = %#v", response)
	}
	if response.Links.Internal[0].Href != "internal" || response.Links.External[0].Href != "external" || response.Images[0].Src != "image" {
		t.Fatalf("success references = %#v", response)
	}
	if response.Error == nil || response.Error.Message != "original error detail" {
		t.Fatalf("success error reference = %#v", response.Error)
	}
	if response.Quality == nil || response.Quality.Warnings[0] != models.QualityReasonLowContentQuality || response.Quality.FetchAttempts[0].Engine != "http" {
		t.Fatalf("success quality = %#v", response.Quality)
	}
}

func assertRejectedBatch(t *testing.T, status *models.BatchStatusResponse, code, message string) {
	t.Helper()
	if status.Status != statusFailed || status.Completed != status.Total {
		t.Fatalf("rejected batch status = %#v", status)
	}
	for index, response := range status.Results {
		if response == nil || response.Success || response.Error == nil || response.Error.Code != code || response.Error.Message != message {
			t.Fatalf("rejected result[%d] = %#v", index, response)
		}
	}
}

func updateMaximum(maximum *atomic.Int32, value int32) {
	for {
		current := maximum.Load()
		if value <= current || maximum.CompareAndSwap(current, value) {
			return
		}
	}
}

func receive[T any](t *testing.T, channel <-chan T, label string) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
		var zero T
		return zero
	}
}
