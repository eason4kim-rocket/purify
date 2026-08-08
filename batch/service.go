package batch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/use-agent/purify/jobs"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/scrape"
	"github.com/use-agent/purify/webhook"
)

const (
	statusProcessing = "processing"
	statusCompleted  = "completed"
	statusPartial    = "partial"
	statusFailed     = "failed"

	maximumBatchSize     = 100
	defaultCapacity      = 1_000
	defaultTTL           = time.Hour
	defaultSweepInterval = 5 * time.Minute
)

var (
	// ErrInvalidBatchSize is returned when a batch has no URLs or exceeds the
	// public API limit.
	ErrInvalidBatchSize = errors.New("batch: urls must contain between 1 and 100 entries")
	// ErrEmptyJobID is returned when an injected generator yields no ID.
	ErrEmptyJobID = errors.New("batch: id generator returned an empty id")
)

type batchState struct {
	ID            string
	Status        string
	Completed     int
	Total         int
	Failed        int
	Results       []*models.ScrapeResponse
	WebhookURL    string
	WebhookSecret string
}

// Service owns batch state while delegating all work to the canonical scrape
// runner and one process-wide bounded executor.
type Service struct {
	runner   Runner
	executor *jobs.Executor
	manager  *jobs.Manager[batchState]
	ids      IDGenerator
	notifier CompletionNotifier
	now      func() time.Time
}

// NewService constructs a bounded in-memory batch service. The executor is
// borrowed: Close cancels and closes only this service's manager.
func NewService(runner Runner, executor *jobs.Executor, config Config) (*Service, error) {
	if runner == nil {
		return nil, fmt.Errorf("batch: runner is required")
	}
	if executor == nil {
		return nil, fmt.Errorf("batch: executor is required")
	}

	capacity := config.Capacity
	if capacity == 0 {
		capacity = defaultCapacity
	}
	ttl := config.TTL
	if ttl == 0 {
		ttl = defaultTTL
	}
	sweepInterval := config.SweepInterval
	if sweepInterval == 0 {
		sweepInterval = defaultSweepInterval
	}
	manager, err := jobs.NewManager(capacity, ttl, sweepInterval, cloneBatchState)
	if err != nil {
		return nil, fmt.Errorf("batch: create manager: %w", err)
	}

	ids := config.IDGenerator
	if ids == nil {
		ids = randomBatchID
	}
	notifier := config.Notifier
	if notifier == nil {
		notifier = CompletionNotifierFunc(webhook.DeliverAsync)
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}

	return &Service{
		runner:   runner,
		executor: executor,
		manager:  manager,
		ids:      ids,
		notifier: notifier,
		now:      now,
	}, nil
}

// Submit creates a job under an independent manager context and enqueues one
// canonical scrape per URL. Queue rejection is recorded in the corresponding
// result slot, so an accepted batch always reaches a terminal state unless the
// service itself is closed.
func (service *Service) Submit(request models.BatchRequest) (*models.BatchResponse, error) {
	if service == nil || service.manager == nil {
		return nil, jobs.ErrManagerClosed
	}
	if len(request.URLs) < 1 || len(request.URLs) > maximumBatchSize {
		return nil, ErrInvalidBatchSize
	}

	id, err := service.ids()
	if err != nil {
		return nil, fmt.Errorf("batch: generate id: %w", err)
	}
	if strings.TrimSpace(id) == "" {
		return nil, ErrEmptyJobID
	}

	urls := append([]string(nil), request.URLs...)
	options := cloneBatchOptions(request.Options)
	initial := batchState{
		ID:            id,
		Status:        statusProcessing,
		Total:         len(urls),
		Results:       make([]*models.ScrapeResponse, len(urls)),
		WebhookURL:    request.WebhookURL,
		WebhookSecret: request.WebhookSecret,
	}
	jobContext, err := service.manager.Create(id, initial)
	if err != nil {
		return nil, fmt.Errorf("batch: create job: %w", err)
	}

	for index, targetURL := range urls {
		index := index
		targetURL := targetURL
		err = service.executor.Submit(jobContext, func(ctx context.Context) {
			response := service.runOneSafely(ctx, targetURL, options)
			service.finishItem(id, index, response)
		})
		if err != nil {
			service.finishItem(id, index, executorFailureResponse(err))
		}
	}

	return &models.BatchResponse{
		ID:     id,
		Status: statusProcessing,
		Total:  len(urls),
	}, nil
}

// Get returns a detached point-in-time snapshot. Neither the caller nor a
// notifier can mutate values retained by the service.
func (service *Service) Get(id string) (*models.BatchStatusResponse, bool) {
	if service == nil || service.manager == nil {
		return nil, false
	}
	state, ok := service.manager.Snapshot(id)
	if !ok {
		return nil, false
	}
	response := statusResponse(state)
	return &response, true
}

// Close cancels this service's running job contexts and stops its TTL sweeper.
// The shared executor remains open for other domains and service instances.
func (service *Service) Close() {
	if service == nil {
		return
	}
	service.manager.Close()
}

func (service *Service) runOneSafely(ctx context.Context, targetURL string, options models.BatchOptions) (response *models.ScrapeResponse) {
	defer func() {
		if recovered := recover(); recovered != nil {
			response = failureResponse(models.NewScrapeError(
				models.ErrCodeInternal,
				fmt.Sprintf("batch scrape panicked: %v", recovered),
				nil,
			))
		}
	}()
	return service.runOne(ctx, targetURL, options)
}

func (service *Service) runOne(ctx context.Context, targetURL string, options models.BatchOptions) *models.ScrapeResponse {
	request := requestForURL(targetURL, options)
	var observedFailure *models.ScrapeResponse
	result, err := service.runner.Run(ctx, request, func(event scrape.Event) {
		if event.Type == scrape.EventError && event.Response != nil {
			observedFailure = cloneScrapeResponse(event.Response)
		}
	})
	if err != nil {
		if observedFailure != nil {
			return observedFailure
		}
		return failureResponse(err)
	}
	if result == nil || result.Response == nil {
		return failureResponse(models.NewScrapeError(
			models.ErrCodeInternal,
			"scrape service returned an empty result",
			nil,
		))
	}

	response := cloneScrapeResponse(result.Response)
	if !response.Success && response.Error == nil {
		response.Error = (&models.ScrapeError{
			Code:    models.ErrCodeInternal,
			Message: "scrape service returned an unsuccessful response without an error",
		}).ToDetail()
	}
	return response
}

func (service *Service) finishItem(id string, index int, response *models.ScrapeResponse) {
	if response == nil {
		response = failureResponse(models.NewScrapeError(models.ErrCodeInternal, "batch item returned an empty response", nil))
	}

	shouldComplete := false
	err := service.manager.Update(id, func(state *batchState) {
		if index < 0 || index >= len(state.Results) || state.Results[index] != nil {
			return
		}
		state.Results[index] = cloneScrapeResponse(response)
		state.Completed++
		if !response.Success {
			state.Failed++
		}
		shouldComplete = state.Completed == state.Total
		if shouldComplete {
			state.Status = terminalStatus(state.Failed, state.Total)
		}
	})
	if err != nil || !shouldComplete {
		return
	}

	var terminal batchState
	err = service.manager.Complete(id, func(state *batchState) {
		terminal = cloneBatchState(*state)
	})
	if err != nil {
		return
	}
	service.notifyCompletion(terminal)
}

func (service *Service) notifyCompletion(state batchState) {
	if state.WebhookURL == "" || service.notifier == nil {
		return
	}
	snapshot := statusResponse(state)
	event := &webhook.Event{
		Type:      "batch." + snapshot.Status,
		JobID:     snapshot.ID,
		Timestamp: service.now().Unix(),
		Data:      cloneStatus(snapshot),
	}
	func() {
		// Completion notification is an external extension point. A broken
		// injected implementation must not escape Submit on synchronous queue
		// rejection or remove useful capacity from the shared executor.
		defer func() { _ = recover() }()
		service.notifier.Notify(state.WebhookURL, state.WebhookSecret, event)
	}()
}

func requestForURL(targetURL string, options models.BatchOptions) *models.ScrapeRequest {
	request := &models.ScrapeRequest{
		URL:          targetURL,
		OutputFormat: options.OutputFormat,
		ExtractMode:  options.ExtractMode,
		Timeout:      options.Timeout,
		Stealth:      options.Stealth,
	}
	if options.WaitForNetworkIdle != nil {
		wait := *options.WaitForNetworkIdle
		request.WaitForNetworkIdle = &wait
	}
	return request
}

func cloneBatchOptions(source models.BatchOptions) models.BatchOptions {
	if source.WaitForNetworkIdle != nil {
		wait := *source.WaitForNetworkIdle
		source.WaitForNetworkIdle = &wait
	}
	return source
}

func executorFailureResponse(err error) *models.ScrapeResponse {
	switch {
	case errors.Is(err, jobs.ErrQueueFull):
		return failureResponse(models.NewScrapeError(models.ErrCodeRateLimited, "batch worker queue is full", err))
	case errors.Is(err, jobs.ErrSubmitCanceled):
		return failureResponse(models.NewScrapeError(models.ErrCodeTimeout, "batch job was canceled before execution", err))
	case errors.Is(err, jobs.ErrClosed):
		return failureResponse(models.NewScrapeError(models.ErrCodeInternal, "batch worker executor is closed", err))
	default:
		return failureResponse(models.NewScrapeError(models.ErrCodeInternal, "batch worker rejected scrape task", err))
	}
}

func failureResponse(err error) *models.ScrapeResponse {
	var scrapeError *models.ScrapeError
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		scrapeError = models.NewScrapeError(models.ErrCodeTimeout, "scrape request deadline exceeded", err)
	} else if !errors.As(err, &scrapeError) {
		scrapeError = models.NewScrapeError(models.ErrCodeInternal, err.Error(), err)
	}
	return &models.ScrapeResponse{Success: false, Error: scrapeError.ToDetail()}
}

func terminalStatus(failed, total int) string {
	switch {
	case failed == 0:
		return statusCompleted
	case failed == total:
		return statusFailed
	default:
		return statusPartial
	}
}

func statusResponse(state batchState) models.BatchStatusResponse {
	return models.BatchStatusResponse{
		ID:        state.ID,
		Status:    state.Status,
		Completed: state.Completed,
		Total:     state.Total,
		Results:   cloneResults(state.Results),
	}
}

func cloneBatchState(source batchState) batchState {
	source.Results = cloneResults(source.Results)
	return source
}

func randomBatchID() (string, error) {
	bytes := make([]byte, 8)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return "batch-" + hex.EncodeToString(bytes), nil
}
