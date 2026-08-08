package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/use-agent/purify/cleaner"
	"github.com/use-agent/purify/engine"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/scrape"
)

type scrapeRunnerFixture struct {
	run func(context.Context, *models.ScrapeRequest, scrape.Observer) (*scrape.Result, error)
}

func (fixture scrapeRunnerFixture) Run(ctx context.Context, request *models.ScrapeRequest, observe scrape.Observer) (*scrape.Result, error) {
	return fixture.run(ctx, request, observe)
}

func TestScrapeWithServiceReturnsCanonicalJSONResponse(t *testing.T) {
	want := &models.ScrapeResponse{
		Success: true, Content: "canonical content", StatusCode: http.StatusOK,
		FinalURL: "https://final.example/page", EngineUsed: "http",
		Quality: &models.QualityInfo{Score: 0.9, Status: models.QualityStatusGood, Warnings: []models.QualityReason{}},
	}
	runner := scrapeRunnerFixture{run: func(_ context.Context, request *models.ScrapeRequest, observe scrape.Observer) (*scrape.Result, error) {
		if request.URL != "https://example.test/page" {
			t.Fatalf("request URL = %q", request.URL)
		}
		observe(scrape.Event{Type: scrape.EventStarted, URL: request.URL})
		observe(scrape.Event{Type: scrape.EventCompleted, URL: request.URL, Response: want})
		return &scrape.Result{Response: want}, nil
	}}

	response := performScrapeRequest(t, ScrapeWithService(runner), "application/json")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	var got models.ScrapeResponse
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Success || got.Content != want.Content || got.FinalURL != want.FinalURL || got.Quality == nil {
		t.Fatalf("response = %#v", got)
	}
}

func TestScrapeWithServiceRunsProductionHTTPAdapterThroughCanonicalService(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "/final", http.StatusFound)
	})
	mux.HandleFunc("/final", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = writer.Write([]byte(`<html><head><title>Canonical fixture</title></head><body><article>` +
			strings.Repeat("useful canonical service content ", 30) + `</article></body></html>`))
	})
	mux.HandleFunc("/empty", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = writer.Write([]byte(`<html><head><title>Head only</title></head></html>`))
	})
	source := httptest.NewServer(mux)
	t.Cleanup(source.Close)

	fetcher, err := scrape.NewEngineFetcher(engine.NewHTTPEngine(""))
	if err != nil {
		t.Fatal(err)
	}
	service, err := scrape.NewService([]scrape.Fetcher{fetcher}, cleaner.NewCleaner(), nil, nil, scrape.Config{})
	if err != nil {
		t.Fatal(err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/scrape", Scrape(service))
	body, _ := json.Marshal(models.ScrapeRequest{URL: source.URL + "/start"})
	request := httptest.NewRequest(http.MethodPost, "/scrape", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	var got models.ScrapeResponse
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Success || got.FinalURL != source.URL+"/final" || got.EngineUsed != "http" ||
		got.Metadata.SourceURL != source.URL+"/final" || got.Metadata.FetchMethod != "http" || got.Quality == nil {
		t.Fatalf("canonical response = %#v", got)
	}

	emptyBody, _ := json.Marshal(models.ScrapeRequest{URL: source.URL + "/empty"})
	emptyRequest := httptest.NewRequest(http.MethodPost, "/scrape", bytes.NewReader(emptyBody))
	emptyRequest.Header.Set("Content-Type", "application/json")
	emptyResponse := httptest.NewRecorder()
	router.ServeHTTP(emptyResponse, emptyRequest)
	if emptyResponse.Code != http.StatusBadGateway {
		t.Fatalf("empty status = %d, want 502; body = %s", emptyResponse.Code, emptyResponse.Body)
	}
	var rejected models.ScrapeResponse
	if err := json.Unmarshal(emptyResponse.Body.Bytes(), &rejected); err != nil {
		t.Fatal(err)
	}
	if rejected.Success || rejected.Error == nil || rejected.Error.Code != models.ErrCodeContentUnusable ||
		rejected.Quality == nil || len(rejected.Quality.FetchAttempts) != 1 {
		t.Fatalf("empty response was not quality-rejected: %#v", rejected)
	}
}

func TestScrapeWithServiceMapsContentUnusableAndUsesServiceErrorResponse(t *testing.T) {
	scrapeErr := models.NewScrapeError(models.ErrCodeContentUnusable, "all candidates rejected", nil)
	want := &models.ScrapeResponse{
		Success: false,
		Error:   scrapeErr.ToDetail(),
		Quality: &models.QualityInfo{
			Status:   models.QualityStatusUnusable,
			Warnings: []models.QualityReason{models.QualityReasonChallengePage},
			FetchAttempts: []models.FetchAttempt{{
				Engine: "http", Outcome: models.FetchAttemptRejected, Reason: models.QualityReasonChallengePage,
			}},
		},
	}
	runner := scrapeRunnerFixture{run: func(_ context.Context, request *models.ScrapeRequest, observe scrape.Observer) (*scrape.Result, error) {
		observe(scrape.Event{Type: scrape.EventError, URL: request.URL, Response: want})
		return nil, scrapeErr
	}}

	response := performScrapeRequest(t, ScrapeWithService(runner), "application/json")
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", response.Code, response.Body)
	}
	var got models.ScrapeResponse
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Error == nil || got.Error.Code != models.ErrCodeContentUnusable || got.Quality == nil || len(got.Quality.FetchAttempts) != 1 {
		t.Fatalf("response = %#v", got)
	}
}

func TestScrapeWithServiceStreamsCanonicalEventSequence(t *testing.T) {
	completed := &models.ScrapeResponse{Success: true, Content: "streamed content", EngineUsed: "rod"}
	runner := scrapeRunnerFixture{run: func(_ context.Context, request *models.ScrapeRequest, observe scrape.Observer) (*scrape.Result, error) {
		observe(scrape.Event{Type: scrape.EventStarted, URL: request.URL})
		observe(scrape.Event{Type: scrape.EventAttempt, URL: request.URL, Attempt: &models.FetchAttempt{Engine: "http", Outcome: models.FetchAttemptRejected}})
		observe(scrape.Event{Type: scrape.EventNavigated, URL: request.URL, Navigation: &scrape.Navigation{StatusCode: 200, FinalURL: request.URL, EngineUsed: "rod", FetchMethod: "browser"}})
		observe(scrape.Event{Type: scrape.EventCompleted, URL: request.URL, Response: completed})
		return &scrape.Result{Response: completed}, nil
	}}

	response := performScrapeRequest(t, ScrapeWithService(runner), "application/json, text/event-stream; charset=utf-8")
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status/content-type = %d/%q", response.Code, response.Header().Get("Content-Type"))
	}
	body := response.Body.String()
	for _, event := range []string{"scrape.started", "scrape.attempt", "scrape.navigated", "scrape.completed"} {
		if strings.Count(body, "event: "+event+"\n") != 1 {
			t.Fatalf("event %q count != 1 in %s", event, body)
		}
	}
	if !strings.Contains(body, `"content":"streamed content"`) {
		t.Fatalf("completed payload missing canonical response: %s", body)
	}
}

func TestScrapeWithServiceDoesNotDuplicateTerminalSSEError(t *testing.T) {
	scrapeErr := models.NewScrapeError(models.ErrCodeTimeout, "deadline exceeded", context.DeadlineExceeded)
	runner := scrapeRunnerFixture{run: func(_ context.Context, request *models.ScrapeRequest, observe scrape.Observer) (*scrape.Result, error) {
		observe(scrape.Event{Type: scrape.EventStarted, URL: request.URL})
		observe(scrape.Event{Type: scrape.EventError, URL: request.URL, Response: failedResponse(scrapeErr)})
		return nil, scrapeErr
	}}

	response := performScrapeRequest(t, ScrapeWithService(runner), "text/event-stream")
	if strings.Count(response.Body.String(), "event: scrape.error\n") != 1 {
		t.Fatalf("terminal error was duplicated: %s", response.Body)
	}
}

func TestScrapeWithServiceValidatesBindingAndConfiguration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name   string
		body   string
		runner ScrapeRunner
		status int
	}{
		{name: "invalid JSON", body: `{`, runner: scrapeRunnerFixture{}, status: http.StatusBadRequest},
		{name: "missing service", body: `{"url":"https://example.test"}`, status: http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			router := gin.New()
			router.POST("/scrape", ScrapeWithService(test.runner))
			request := httptest.NewRequest(http.MethodPost, "/scrape", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.status, response.Body)
			}
		})
	}
}

func performScrapeRequest(t *testing.T, handler gin.HandlerFunc, accept string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/scrape", handler)
	body, err := json.Marshal(models.ScrapeRequest{URL: "https://example.test/page"})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/scrape", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", accept)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestAsHandlerScrapeErrorPreservesWrappedError(t *testing.T) {
	want := models.NewScrapeError(models.ErrCodeRateLimited, "limited", nil)
	if got := asHandlerScrapeError(errors.Join(errors.New("outer"), want)); got != want {
		t.Fatalf("asHandlerScrapeError() = %#v, want original error", got)
	}
}
