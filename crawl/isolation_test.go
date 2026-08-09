package crawl

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/scrape"
	"github.com/use-agent/purify/webhook"
)

func TestGetDeepCloneIsolatedFromRunnerAndCallerMutation(t *testing.T) {
	original := richSuccessResponse("https://example.test/root")
	runner := runnerFunc(func(_ context.Context, _ *models.ScrapeRequest, _ scrape.Observer) (*scrape.Result, error) {
		return &scrape.Result{Response: original}, nil
	})
	service, executor := newTestService(t, runner, 1, 2, Config{IDGenerator: fixedID("crawl-clone")})
	defer executor.Close()
	defer service.Close()

	accepted, err := service.Submit(models.CrawlRequest{URL: "https://example.test/root", Scope: scopePage})
	if err != nil {
		t.Fatal(err)
	}
	want := cloneScrapeResponse(original)
	awaitTerminal(t, service, accepted.ID)

	mutateRichResponse(original, "runner-mutated")
	first, ok := service.Get(accepted.ID)
	if !ok || len(first.Results) != 1 || !reflect.DeepEqual(first.Results[0], want) {
		t.Fatalf("Get() after runner mutation = (%#v, %v), want %#v", first, ok, want)
	}

	mutateRichResponse(first.Results[0], "caller-mutated")
	first.Results = append(first.Results, nil)
	first.Status = "caller-mutated"
	second, ok := service.Get(accepted.ID)
	if !ok || second.Status != statusCompleted || len(second.Results) != 1 || !reflect.DeepEqual(second.Results[0], want) {
		t.Fatalf("second Get() = (%#v, %v), want detached %#v", second, ok, want)
	}
}

func TestPageAndTerminalWebhooksReceiveDetachedStableSnapshots(t *testing.T) {
	original := richSuccessResponse("https://example.test/root")
	terminalNotified := make(chan struct{})
	var eventsMu sync.Mutex
	var eventTypes []string
	const timestamp = int64(1_789_000_000)
	notifier := NotifierFunc(func(targetURL, secret string, event *webhook.Event) {
		if targetURL != "https://hook.example/crawl" || secret != "secret" {
			t.Errorf("Notify target = (%q, %q)", targetURL, secret)
		}
		if event.JobID != "crawl-webhook" || event.Timestamp != timestamp {
			t.Errorf("event identity = %#v", event)
		}
		eventsMu.Lock()
		eventTypes = append(eventTypes, event.Type)
		eventsMu.Unlock()
		switch event.Type {
		case "crawl.page":
			page, ok := event.Data.(*models.ScrapeResponse)
			if !ok || page.Content != "original" || page.Links.Internal[0].Href != "https://example.test/link" {
				t.Errorf("page event data = %#v", event.Data)
				return
			}
			mutateRichResponse(page, "page-notifier-mutated")
		case "crawl.completed":
			status, ok := event.Data.(models.CrawlStatusResponse)
			if !ok || len(status.Results) != 1 || status.Results[0].Content != "original" ||
				status.Results[0].Links.Internal[0].Href != "https://example.test/link" {
				t.Errorf("terminal event data = %#v", event.Data)
				close(terminalNotified)
				return
			}
			mutateRichResponse(status.Results[0], "terminal-notifier-mutated")
			status.Results = append(status.Results, nil)
			close(terminalNotified)
		default:
			t.Errorf("unexpected event type %q", event.Type)
		}
	})
	runner := runnerFunc(func(_ context.Context, _ *models.ScrapeRequest, _ scrape.Observer) (*scrape.Result, error) {
		return &scrape.Result{Response: original}, nil
	})
	service, executor := newTestService(t, runner, 1, 2, Config{
		IDGenerator: fixedID("crawl-webhook"),
		Notifier:    notifier,
		Now:         func() time.Time { return time.Unix(timestamp, 0) },
	})
	defer executor.Close()
	defer service.Close()

	accepted, err := service.Submit(models.CrawlRequest{
		URL:           "https://example.test/root",
		Scope:         scopePage,
		WebhookURL:    "https://hook.example/crawl",
		WebhookSecret: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := cloneScrapeResponse(original)
	awaitTerminal(t, service, accepted.ID)
	receive(t, terminalNotified, "terminal webhook")

	status, ok := service.Get(accepted.ID)
	if !ok || len(status.Results) != 1 || !reflect.DeepEqual(status.Results[0], want) {
		t.Fatalf("state after notifier mutation = (%#v, %v), want %#v", status, ok, want)
	}
	eventsMu.Lock()
	defer eventsMu.Unlock()
	if !reflect.DeepEqual(eventTypes, []string{"crawl.page", "crawl.completed"}) {
		t.Fatalf("event types = %v", eventTypes)
	}
}

func TestNotifierPanicsCannotPreventTerminalCompletion(t *testing.T) {
	var calls atomic.Int32
	notifier := NotifierFunc(func(string, string, *webhook.Event) {
		calls.Add(1)
		panic("notifier exploded")
	})
	runner := runnerFunc(func(_ context.Context, request *models.ScrapeRequest, _ scrape.Observer) (*scrape.Result, error) {
		return &scrape.Result{Response: successResponse(request.URL)}, nil
	})
	service, executor := newTestService(t, runner, 1, 2, Config{
		IDGenerator: fixedID("crawl-notifier-panic"),
		Notifier:    notifier,
	})
	defer executor.Close()
	defer service.Close()

	accepted, err := service.Submit(models.CrawlRequest{
		URL: "https://example.test", Scope: scopePage, WebhookURL: "https://hook.example/crawl",
	})
	if err != nil {
		t.Fatal(err)
	}
	status := awaitTerminal(t, service, accepted.ID)
	if status.Status != statusCompleted {
		t.Fatalf("status = %#v", status)
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for calls.Load() != 2 {
		select {
		case <-deadline.C:
			t.Fatalf("notifier calls = %d, want 2", calls.Load())
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestSubmitClonesExcludePatternsBeforeCoordinatorUsesThem(t *testing.T) {
	rootStarted := make(chan struct{})
	releaseRoot := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseRoot) }) })
	runner := runnerFunc(func(ctx context.Context, request *models.ScrapeRequest, _ scrape.Observer) (*scrape.Result, error) {
		response := successResponse(request.URL)
		if request.URL == "https://example.test/root" {
			close(rootStarted)
			select {
			case <-releaseRoot:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			response.Links.Internal = []models.Link{{Href: "/blocked/page"}, {Href: "/kept"}}
		}
		return &scrape.Result{Response: response}, nil
	})
	service, executor := newTestService(t, runner, 1, 4, Config{IDGenerator: fixedID("crawl-exclude-clone")})
	defer executor.Close()
	defer service.Close()

	patterns := []string{"/blocked/*"}
	accepted, err := service.Submit(models.CrawlRequest{
		URL: "https://example.test/root", MaxDepth: 1, MaxPages: 10, Scope: scopeDomain, ExcludePatterns: patterns,
	})
	if err != nil {
		t.Fatal(err)
	}
	receive(t, rootStarted, "root runner")
	patterns[0] = "/nothing/*"
	releaseOnce.Do(func() { close(releaseRoot) })
	assertResultURLs(t, awaitTerminal(t, service, accepted.ID), []string{
		"https://example.test/root",
		"https://example.test/kept",
	})
}

func richSuccessResponse(targetURL string) *models.ScrapeResponse {
	return &models.ScrapeResponse{
		Success:    true,
		StatusCode: 200,
		FinalURL:   targetURL,
		Content:    "original",
		Metadata: models.Metadata{
			Title: "original", SourceURL: targetURL, FetchMethod: "http",
		},
		Links: models.LinksResult{
			Internal: []models.Link{{Href: "https://example.test/link", Text: "original"}},
			External: []models.Link{{Href: "https://external.test/link", Text: "original"}},
		},
		Images: []models.Image{{Src: "https://example.test/image.png", Alt: "original"}},
		Error:  &models.ErrorDetail{Code: "TEST", Message: "original"},
		Quality: &models.QualityInfo{
			Score:           0.95,
			Status:          models.QualityStatusGood,
			Warnings:        []models.QualityReason{models.QualityReasonLowContentQuality},
			ExtractModeUsed: "readability",
			FetchAttempts: []models.FetchAttempt{{
				Engine: "http", Outcome: models.FetchAttemptSelected, DurationMs: 1,
			}},
		},
		CacheStatus: "miss",
		EngineUsed:  "http",
	}
}

func mutateRichResponse(response *models.ScrapeResponse, marker string) {
	response.Content = marker
	response.Metadata.Title = marker
	response.Links.Internal[0].Href = marker
	response.Links.External[0].Href = marker
	response.Images[0].Src = marker
	response.Error.Message = marker
	response.Quality.Warnings[0] = models.QualityReasonErrorPage
	response.Quality.FetchAttempts[0].Engine = marker
}
