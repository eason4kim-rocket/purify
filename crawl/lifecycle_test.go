package crawl

import (
	"context"
	"errors"
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

func TestCrawlMergesLinkGroupsBeforeExcludeScopeAndDedup(t *testing.T) {
	runner := runnerFunc(func(_ context.Context, request *models.ScrapeRequest, _ scrape.Observer) (*scrape.Result, error) {
		response := successResponse(request.URL)
		if request.URL == "https://www.example.co.uk/root" {
			response.Links.Internal = []models.Link{
				{Href: "/keep#one"},
				{Href: "/private/skip"},
				{Href: "https://evil.test/off-scope"},
			}
			response.Links.External = []models.Link{
				{Href: "https://api.example.co.uk/z"},
				{Href: "https://WWW.EXAMPLE.CO.UK:443/keep#duplicate"},
				{Href: "https://other.co.uk/off-scope"},
			}
		}
		return &scrape.Result{Response: response}, nil
	})
	service, executor := newTestService(t, runner, 2, 8, Config{IDGenerator: fixedID("crawl-link-pipeline")})
	defer executor.Close()
	defer service.Close()

	accepted, err := service.Submit(models.CrawlRequest{
		URL:             "https://www.example.co.uk/root",
		MaxDepth:        1,
		MaxPages:        20,
		Scope:           scopeSubdomain,
		ExcludePatterns: []string{"/private/*"},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertResultURLs(t, awaitTerminal(t, service, accepted.ID), []string{
		"https://www.example.co.uk/root",
		"https://api.example.co.uk/z",
		"https://www.example.co.uk/keep",
	})
}

func TestQueueFullSettlesSynchronouslyWithoutRetry(t *testing.T) {
	executor, err := jobs.NewExecutor(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		executor.Close()
	})
	started := make(chan struct{})
	if err := executor.Submit(context.Background(), func(context.Context) {
		close(started)
		<-release
	}); err != nil {
		t.Fatal(err)
	}
	receive(t, started, "executor blocker")
	if err := executor.Submit(context.Background(), func(context.Context) {}); err != nil {
		t.Fatalf("fill executor queue: %v", err)
	}

	var runnerCalls atomic.Int32
	runner := runnerFunc(func(context.Context, *models.ScrapeRequest, scrape.Observer) (*scrape.Result, error) {
		runnerCalls.Add(1)
		return nil, errors.New("runner must not execute")
	})
	service, err := NewService(runner, executor, Config{IDGenerator: fixedID("crawl-queue-full")})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	accepted, err := service.Submit(models.CrawlRequest{URL: "https://example.test", Scope: scopePage})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	status := awaitTerminal(t, service, accepted.ID)
	if status.Status != statusFailed || status.Completed != 1 || status.Total != 1 {
		t.Fatalf("status = %#v", status)
	}
	if got := status.Results[0]; got.Error == nil || got.Error.Code != models.ErrCodeRateLimited || !containsMessage(got, "queue is full") ||
		got.FinalURL != "https://example.test/" || got.Metadata.SourceURL != "https://example.test/" {
		t.Fatalf("queue failure = %#v", got)
	}
	if got := runnerCalls.Load(); got != 0 {
		t.Fatalf("runner calls = %d, want 0", got)
	}

	releaseOnce.Do(func() { close(release) })
}

func TestClosedExecutorSettlesAcceptedCrawlAsFailed(t *testing.T) {
	executor, err := jobs.NewExecutor(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	executor.Close()
	var runnerCalls atomic.Int32
	runner := runnerFunc(func(context.Context, *models.ScrapeRequest, scrape.Observer) (*scrape.Result, error) {
		runnerCalls.Add(1)
		return nil, errors.New("runner must not execute")
	})
	service, err := NewService(runner, executor, Config{IDGenerator: fixedID("crawl-executor-closed")})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	accepted, err := service.Submit(models.CrawlRequest{URL: "https://example.test", Scope: scopePage})
	if err != nil {
		t.Fatal(err)
	}
	status := awaitTerminal(t, service, accepted.ID)
	if status.Status != statusFailed || status.Results[0].Error == nil ||
		status.Results[0].Error.Code != models.ErrCodeInternal || !containsMessage(status.Results[0], "executor is closed") {
		t.Fatalf("status = %#v", status)
	}
	if runnerCalls.Load() != 0 {
		t.Fatalf("runner calls = %d, want 0", runnerCalls.Load())
	}
}

func TestExecutorRejectionErrorMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		err     error
		code    string
		message string
	}{
		{name: "queue full", err: jobs.ErrQueueFull, code: models.ErrCodeRateLimited, message: "queue is full"},
		{name: "submission canceled", err: jobs.ErrSubmitCanceled, code: models.ErrCodeTimeout, message: "canceled before execution"},
		{name: "executor closed", err: jobs.ErrClosed, code: models.ErrCodeInternal, message: "executor is closed"},
		{name: "unexpected rejection", err: jobs.ErrNilTask, code: models.ErrCodeInternal, message: "rejected scrape task"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := executorFailureResponse(test.err)
			if response.Success || response.Error == nil || response.Error.Code != test.code || !containsMessage(response, test.message) {
				t.Fatalf("executorFailureResponse(%v) = %#v", test.err, response)
			}
		})
	}
}

func TestRunnerPanicsAndObservedFailuresSettleAsPartial(t *testing.T) {
	runner := runnerFunc(func(_ context.Context, request *models.ScrapeRequest, observe scrape.Observer) (*scrape.Result, error) {
		switch request.URL {
		case "https://example.test/root":
			response := successResponse(request.URL)
			response.Links.Internal = []models.Link{{Href: "/ok"}, {Href: "/panic"}, {Href: "/observed"}}
			return &scrape.Result{Response: response}, nil
		case "https://example.test/panic":
			panic("runner exploded")
		case "https://example.test/observed":
			failure := richFailureResponse("observed-original")
			observe(scrape.Event{Type: scrape.EventError, URL: request.URL, Response: failure})
			failure.Content = "mutated-after-observe"
			failure.Links.Internal[0].Href = "mutated-after-observe"
			failure.Error.Message = "mutated-after-observe"
			failure.Quality.Warnings[0] = models.QualityReasonErrorPage
			return nil, models.NewScrapeError(models.ErrCodeNavigation, "runner failed", nil)
		default:
			return &scrape.Result{Response: successResponse(request.URL)}, nil
		}
	})
	service, executor := newTestService(t, runner, 3, 8, Config{IDGenerator: fixedID("crawl-partial")})
	defer executor.Close()
	defer service.Close()

	accepted, err := service.Submit(models.CrawlRequest{
		URL: "https://example.test/root", MaxDepth: 1, MaxPages: 10, Scope: scopeDomain,
	})
	if err != nil {
		t.Fatal(err)
	}
	status := awaitTerminal(t, service, accepted.ID)
	if status.Status != statusPartial || status.Completed != 4 || status.Total != 4 {
		t.Fatalf("status = %#v", status)
	}
	if got := status.Results[1]; got == nil || got.Success || got.Error == nil || got.Error.Code != models.ErrCodeNavigation || got.Content != "observed-original" ||
		got.Links.Internal[0].Href != "https://observed.example/link" || got.Error.Message != "observed failure" ||
		got.Quality.Warnings[0] != models.QualityReasonChallengePage {
		t.Fatalf("observed failure result = %#v", got)
	}
	if got := status.Results[2]; got == nil || !got.Success || got.Content != "https://example.test/ok" {
		t.Fatalf("success result = %#v", got)
	}
	if got := status.Results[3]; got == nil || got.Success || got.Error == nil || got.Error.Code != models.ErrCodeInternal || !containsMessage(got, "panicked") {
		t.Fatalf("panic result = %#v", got)
	}
}

func TestCompletedJobsConsumeCapacityUntilTTLExpires(t *testing.T) {
	runner := runnerFunc(func(_ context.Context, request *models.ScrapeRequest, _ scrape.Observer) (*scrape.Result, error) {
		return &scrape.Result{Response: successResponse(request.URL)}, nil
	})
	service, executor := newTestService(t, runner, 1, 4, Config{
		Capacity:      1,
		TTL:           150 * time.Millisecond,
		SweepInterval: 2 * time.Millisecond,
		IDGenerator:   sequentialIDs("crawl-retained"),
	})
	defer executor.Close()
	defer service.Close()

	first, err := service.Submit(models.CrawlRequest{URL: "https://example.test/first", Scope: scopePage})
	if err != nil {
		t.Fatal(err)
	}
	awaitTerminal(t, service, first.ID)
	if response, err := service.Submit(models.CrawlRequest{URL: "https://example.test/second", Scope: scopePage}); !errors.Is(err, jobs.ErrManagerFull) || response != nil {
		t.Fatalf("Submit(while retained) = (%#v, %v), want manager full", response, err)
	}

	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, exists := service.Get(first.ID); !exists {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("completed job did not expire after TTL")
		case <-ticker.C:
		}
	}
	second, err := service.Submit(models.CrawlRequest{URL: "https://example.test/second", Scope: scopePage})
	if err != nil {
		t.Fatalf("Submit(after TTL) error = %v", err)
	}
	awaitTerminal(t, service, second.ID)
}

func TestCloseCancelsCoordinatorWithoutClosingOrWaitingForBorrowedExecutor(t *testing.T) {
	runnerStarted := make(chan struct{})
	releaseRunner := make(chan struct{})
	var releaseOnce sync.Once
	runner := runnerFunc(func(_ context.Context, request *models.ScrapeRequest, _ scrape.Observer) (*scrape.Result, error) {
		close(runnerStarted)
		<-releaseRunner // Deliberately ignores cancellation.
		return &scrape.Result{Response: successResponse(request.URL)}, nil
	})
	executor, err := jobs.NewExecutor(1, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseRunner) })
		executor.Close()
	})
	service, err := NewService(runner, executor, Config{IDGenerator: fixedID("crawl-close")})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := service.Submit(models.CrawlRequest{URL: "https://example.test", Scope: scopePage})
	if err != nil {
		t.Fatal(err)
	}
	receive(t, runnerStarted, "uncancelable runner start")

	closed := make(chan struct{})
	go func() {
		service.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Service.Close waited for borrowed executor task")
	}
	if response, err := service.Submit(models.CrawlRequest{URL: "https://example.test/new", Scope: scopePage}); !errors.Is(err, jobs.ErrManagerClosed) || response != nil {
		t.Fatalf("Submit(after Close) = (%#v, %v)", response, err)
	}
	if status, ok := service.Get(accepted.ID); !ok || status.Status != statusFailed || status.Completed != 1 || status.Total != 1 ||
		len(status.Results) != 1 || status.Results[0] == nil || status.Results[0].FinalURL != "https://example.test/" ||
		status.Results[0].Error == nil || status.Results[0].Error.Code != models.ErrCodeTimeout {
		t.Fatalf("Get(after Close) = (%#v, %v)", status, ok)
	}

	releaseOnce.Do(func() { close(releaseRunner) })
	marker := make(chan struct{})
	if err := executor.Submit(context.Background(), func(context.Context) { close(marker) }); err != nil {
		t.Fatalf("borrowed executor rejected work after crawl Close: %v", err)
	}
	receive(t, marker, "borrowed executor marker")
	service.Close()
}

func TestBlockingNotifierCannotBlockCrawlCompletionOrClose(t *testing.T) {
	notifierStarted := make(chan struct{})
	releaseNotifier := make(chan struct{})
	var startedOnce sync.Once
	var releaseOnce sync.Once
	notifier := NotifierFunc(func(string, string, *webhook.Event) {
		startedOnce.Do(func() { close(notifierStarted) })
		<-releaseNotifier
	})
	runner := runnerFunc(func(_ context.Context, request *models.ScrapeRequest, _ scrape.Observer) (*scrape.Result, error) {
		return &scrape.Result{Response: successResponse(request.URL)}, nil
	})
	service, executor := newTestService(t, runner, 1, 2, Config{
		IDGenerator: fixedID("crawl-blocking-notifier"),
		Notifier:    notifier,
	})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseNotifier) })
		service.Close()
		executor.Close()
	})

	accepted, err := service.Submit(models.CrawlRequest{
		URL: "https://example.test", Scope: scopePage, WebhookURL: "https://hook.example/crawl",
	})
	if err != nil {
		t.Fatal(err)
	}
	if status := awaitTerminal(t, service, accepted.ID); status.Status != statusCompleted {
		t.Fatalf("status = %#v", status)
	}
	receive(t, notifierStarted, "blocking notifier start")

	closed := make(chan struct{})
	go func() {
		service.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Service.Close waited indefinitely for a custom notifier")
	}
	releaseOnce.Do(func() { close(releaseNotifier) })
}

func TestConcurrentSubmitAndCloseUsesSafeWaitGroupGate(t *testing.T) {
	runner := runnerFunc(func(ctx context.Context, request *models.ScrapeRequest, _ scrape.Observer) (*scrape.Result, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			return &scrape.Result{Response: successResponse(request.URL)}, nil
		}
	})
	service, executor := newTestService(t, runner, 4, 512, Config{
		Capacity:    512,
		IDGenerator: sequentialIDs("crawl-close-race"),
	})
	defer executor.Close()

	const submitters = 200
	start := make(chan struct{})
	errorsSeen := make(chan error, submitters)
	var submitWG sync.WaitGroup
	for index := range submitters {
		submitWG.Add(1)
		go func(index int) {
			defer submitWG.Done()
			<-start
			response, err := service.Submit(models.CrawlRequest{
				URL: "https://example.test/" + string(rune('a'+index%26)), Scope: scopePage,
			})
			if err == nil {
				if response == nil {
					errorsSeen <- errors.New("accepted submission returned nil response")
				}
				return
			}
			if !errors.Is(err, jobs.ErrManagerClosed) {
				errorsSeen <- err
			}
		}(index)
	}

	close(start)
	var closeWG sync.WaitGroup
	for range 16 {
		closeWG.Add(1)
		go func() {
			defer closeWG.Done()
			service.Close()
		}()
	}
	submitWG.Wait()
	closeWG.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatalf("concurrent lifecycle error = %v", err)
	}
	if response, err := service.Submit(models.CrawlRequest{URL: "https://example.test/after", Scope: scopePage}); !errors.Is(err, jobs.ErrManagerClosed) || response != nil {
		t.Fatalf("Submit(after concurrent Close) = (%#v, %v)", response, err)
	}
}

func TestNilServiceIsSafe(t *testing.T) {
	var service *Service
	if response, err := service.Submit(models.CrawlRequest{URL: "https://example.test"}); !errors.Is(err, jobs.ErrManagerClosed) || response != nil {
		t.Fatalf("nil Submit() = (%#v, %v)", response, err)
	}
	if response, ok := service.Get("missing"); ok || response != nil {
		t.Fatalf("nil Get() = (%#v, %v)", response, ok)
	}
	service.Close()
}

func richFailureResponse(content string) *models.ScrapeResponse {
	return &models.ScrapeResponse{
		Success: false,
		Content: content,
		Links: models.LinksResult{
			Internal: []models.Link{{Href: "https://observed.example/link", Text: "observed"}},
		},
		Images: []models.Image{{Src: "https://observed.example/image.png", Alt: "observed"}},
		Error:  &models.ErrorDetail{Code: models.ErrCodeNavigation, Message: "observed failure"},
		Quality: &models.QualityInfo{
			Status:        models.QualityStatusDegraded,
			Warnings:      []models.QualityReason{models.QualityReasonChallengePage},
			FetchAttempts: []models.FetchAttempt{{Engine: "http", Outcome: models.FetchAttemptFailed}},
		},
	}
}

func TestTerminalStatusMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		failed int
		total  int
		want   string
	}{
		{failed: 0, total: 3, want: statusCompleted},
		{failed: 1, total: 3, want: statusPartial},
		{failed: 3, total: 3, want: statusFailed},
	}
	for _, test := range tests {
		if got := terminalStatus(test.failed, test.total); got != test.want {
			t.Fatalf("terminalStatus(%d, %d) = %q, want %q", test.failed, test.total, got, test.want)
		}
	}
}

func TestDiscoverNextKeepsStableCanonicalSlots(t *testing.T) {
	t.Parallel()
	root := mustNormalizedURL(t, "https://example.test/root")
	plan := crawlPlan{
		maxDepth: 1,
		maxPages: 3,
		scope:    newScopeRule(scopeDomain, root),
	}
	outcomes := []pageOutcome{{
		item: crawlItem{canonical: root.String(), parsed: root, depth: 0, slot: 0},
		base: root,
		response: &models.ScrapeResponse{Success: true, Links: models.LinksResult{
			Internal: []models.Link{{Href: "/z"}, {Href: "/a"}, {Href: "/m"}},
		}},
	}}
	visited := map[string]struct{}{root.String(): {}}
	next := discoverNext(outcomes, plan, visited, 1)
	got := make([]string, len(next))
	gotSlots := make([]int, len(next))
	for index, item := range next {
		got[index] = item.canonical
		gotSlots[index] = item.slot
	}
	if !reflect.DeepEqual(got, []string{"https://example.test/a", "https://example.test/m"}) || !reflect.DeepEqual(gotSlots, []int{1, 2}) {
		t.Fatalf("discoverNext() URLs=%v slots=%v", got, gotSlots)
	}
}
