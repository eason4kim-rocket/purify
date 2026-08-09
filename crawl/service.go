package crawl

import (
	"container/heap"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
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

	scopePage      = "page"
	scopeDomain    = "domain"
	scopeSubdomain = "subdomain"

	defaultMaxDepth      = 3
	maximumMaxDepth      = 10
	defaultMaxPages      = 100
	maximumMaxPages      = 500
	defaultCapacity      = 1_000
	defaultTTL           = time.Hour
	defaultSweepInterval = 5 * time.Minute
	maximumExcludeCount  = 100
	maximumExcludeLength = 512
)

var (
	// ErrInvalidMaxDepth is returned when max_depth is outside the public API
	// range after applying its default.
	ErrInvalidMaxDepth = errors.New("crawl: max_depth must be between 1 and 10")
	// ErrInvalidMaxPages is returned when max_pages is outside the public API
	// range after applying its default.
	ErrInvalidMaxPages = errors.New("crawl: max_pages must be between 1 and 500")
	// ErrInvalidScope is returned for a scope other than page, domain, or
	// subdomain.
	ErrInvalidScope = errors.New("crawl: scope must be page, domain, or subdomain")
	// ErrEmptyJobID is returned when an injected generator yields no ID.
	ErrEmptyJobID = errors.New("crawl: id generator returned an empty id")
)

type crawlState struct {
	ID            string
	Status        string
	Completed     int
	Total         int
	Failed        int
	Results       []*models.ScrapeResponse
	WebhookURL    string
	WebhookSecret string
}

type crawlPlan struct {
	root          crawlItem
	maxDepth      int
	maxPages      int
	scope         scopeRule
	exclude       []string
	options       models.ScrapeOptions
	webhookURL    string
	webhookSecret string
}

type crawlItem struct {
	canonical string
	parsed    *url.URL
	depth     int
	slot      int
}

type pageOutcome struct {
	item     crawlItem
	response *models.ScrapeResponse
	base     *url.URL
}

// Service owns crawl state and coordinators while borrowing one process-wide
// bounded executor for page work.
type Service struct {
	runner   Runner
	executor *jobs.Executor
	manager  *jobs.Manager[crawlState]
	ids      IDGenerator
	notifier Notifier
	now      func() time.Time
	ctx      context.Context
	cancel   context.CancelFunc

	notifications chan notification
	notifyStop    chan struct{}
	notifyDone    chan struct{}
	notifyMu      sync.Mutex

	lifecycleMu  sync.Mutex
	closed       bool
	coordinators sync.WaitGroup
	closeOnce    sync.Once
}

// NewService constructs a bounded in-memory crawl service. The executor is
// borrowed: Close cancels crawl jobs and waits for coordinators, but never
// closes the shared executor.
func NewService(runner Runner, executor *jobs.Executor, config Config) (*Service, error) {
	if runner == nil {
		return nil, fmt.Errorf("crawl: runner is required")
	}
	if executor == nil {
		return nil, fmt.Errorf("crawl: executor is required")
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
	manager, err := jobs.NewManager(capacity, ttl, sweepInterval, cloneCrawlState)
	if err != nil {
		return nil, fmt.Errorf("crawl: create manager: %w", err)
	}

	ids := config.IDGenerator
	if ids == nil {
		ids = randomCrawlID
	}
	notifier := config.Notifier
	if notifier == nil {
		notifier = NotifierFunc(func(targetURL, secret string, event *webhook.Event) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = webhook.Deliver(ctx, targetURL, secret, event)
		})
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}

	serviceContext, cancel := context.WithCancel(context.Background())
	service := &Service{
		runner:   runner,
		executor: executor,
		manager:  manager,
		ids:      ids,
		notifier: notifier,
		now:      now,
		ctx:      serviceContext,
		cancel:   cancel,
	}
	service.startNotifier(maximumMaxPages + capacity)
	return service, nil
}

// Submit validates and normalizes a crawl, reserves its root page, and starts
// one coordinator goroutine. Coordinators do not occupy executor workers, so a
// one-worker executor can make progress through arbitrarily many BFS levels.
func (service *Service) Submit(request models.CrawlRequest) (*models.CrawlResponse, error) {
	if service == nil || service.manager == nil {
		return nil, jobs.ErrManagerClosed
	}

	plan, err := normalizeRequest(request)
	if err != nil {
		return nil, err
	}
	id, err := service.ids()
	if err != nil {
		return nil, fmt.Errorf("crawl: generate id: %w", err)
	}
	if strings.TrimSpace(id) == "" {
		return nil, ErrEmptyJobID
	}

	initial := crawlState{
		ID:            id,
		Status:        statusProcessing,
		Total:         1,
		Results:       make([]*models.ScrapeResponse, 1),
		WebhookURL:    plan.webhookURL,
		WebhookSecret: plan.webhookSecret,
	}

	service.lifecycleMu.Lock()
	defer service.lifecycleMu.Unlock()
	if service.closed {
		return nil, jobs.ErrManagerClosed
	}
	jobContext, err := service.manager.Create(id, initial)
	if err != nil {
		return nil, fmt.Errorf("crawl: create job: %w", err)
	}
	coordinateContext, cancelCoordinate := context.WithCancel(jobContext)
	stopCloseRelay := context.AfterFunc(service.ctx, cancelCoordinate)
	service.coordinators.Add(1)
	go func() {
		defer cancelCoordinate()
		defer stopCloseRelay()
		service.coordinate(coordinateContext, id, plan)
	}()

	return &models.CrawlResponse{ID: id, Status: statusProcessing}, nil
}

// Get returns a detached, point-in-time crawl snapshot.
func (service *Service) Get(id string) (*models.CrawlStatusResponse, bool) {
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

// Close gates new submissions, cancels every crawl job through the manager,
// and waits for all coordinator goroutines. It leaves the borrowed executor
// open for other services.
func (service *Service) Close() {
	if service == nil {
		return
	}
	service.closeOnce.Do(func() {
		service.lifecycleMu.Lock()
		service.closed = true
		service.lifecycleMu.Unlock()

		service.cancel()
		service.coordinators.Wait()
		service.manager.Close()
		service.stopNotifier()
	})
}

func normalizeRequest(request models.CrawlRequest) (crawlPlan, error) {
	root, parsedRoot, err := normalizeRootURL(request.URL)
	if err != nil {
		return crawlPlan{}, err
	}
	maxDepth := request.MaxDepth
	if maxDepth == 0 {
		maxDepth = defaultMaxDepth
	}
	if maxDepth < 1 || maxDepth > maximumMaxDepth {
		return crawlPlan{}, ErrInvalidMaxDepth
	}
	maxPages := request.MaxPages
	if maxPages == 0 {
		maxPages = defaultMaxPages
	}
	if maxPages < 1 || maxPages > maximumMaxPages {
		return crawlPlan{}, ErrInvalidMaxPages
	}
	scope := request.Scope
	if scope == "" {
		scope = scopeSubdomain
	}
	if scope != scopePage && scope != scopeDomain && scope != scopeSubdomain {
		return crawlPlan{}, ErrInvalidScope
	}
	exclude := slices.Clone(request.ExcludePatterns)
	if err := validateExcludePatterns(exclude); err != nil {
		return crawlPlan{}, err
	}

	return crawlPlan{
		root: crawlItem{
			canonical: root,
			parsed:    parsedRoot,
			depth:     0,
			slot:      0,
		},
		maxDepth:      maxDepth,
		maxPages:      maxPages,
		scope:         newScopeRule(scope, parsedRoot),
		exclude:       exclude,
		options:       models.CloneScrapeOptions(request.Options),
		webhookURL:    request.WebhookURL,
		webhookSecret: request.WebhookSecret,
	}, nil
}

func (service *Service) coordinate(ctx context.Context, id string, plan crawlPlan) {
	defer service.coordinators.Done()

	visited := map[string]struct{}{plan.root.canonical: {}}
	level := []crawlItem{plan.root}
	totalReserved := 1

	for len(level) > 0 {
		outcomes := service.runLevel(ctx, level, plan.options)
		for index := range outcomes {
			outcome := &outcomes[index]
			outcome.base = outcome.item.parsed
			if outcome.response == nil || outcome.response.FinalURL == "" {
				continue
			}
			finalURL, parsedFinal, err := normalizeURL(outcome.response.FinalURL, outcome.item.parsed)
			if err != nil {
				continue
			}
			visited[finalURL] = struct{}{}
			outcome.base = parsedFinal
		}

		next := discoverNext(outcomes, plan, visited, totalReserved)
		willComplete := len(next) == 0
		failedThisLevel := countFailures(outcomes)

		var terminal crawlState
		mutate := func(state *crawlState) {
			for _, outcome := range outcomes {
				if outcome.item.slot >= 0 && outcome.item.slot < len(state.Results) {
					state.Results[outcome.item.slot] = cloneScrapeResponse(outcome.response)
				}
			}
			state.Completed += len(outcomes)
			state.Failed += failedThisLevel
			if willComplete {
				state.Status = terminalStatus(state.Failed, state.Total)
				terminal = cloneCrawlState(*state)
				return
			}
			state.Results = append(state.Results, make([]*models.ScrapeResponse, len(next))...)
			state.Total += len(next)
		}

		var err error
		if willComplete {
			err = service.manager.Complete(id, mutate)
		} else {
			err = service.manager.Update(id, mutate)
		}
		if err != nil {
			return
		}

		for _, outcome := range outcomes {
			service.notifyPage(plan, id, outcome.response)
		}
		if willComplete {
			service.notifyTerminal(terminal)
			return
		}

		totalReserved += len(next)
		level = next
	}
}

func (service *Service) runLevel(ctx context.Context, level []crawlItem, options models.ScrapeOptions) []pageOutcome {
	results := make(chan pageOutcome, len(level))
	for _, item := range level {
		item := item
		err := service.executor.Submit(ctx, func(taskContext context.Context) {
			service.executePage(taskContext, item, options, results)
		})
		if err != nil {
			results <- pageOutcome{item: item, response: identifyResponse(item.canonical, executorFailureResponse(err))}
		}
	}

	outcomes := make(map[int]pageOutcome, len(level))
	for len(outcomes) < len(level) {
		select {
		case outcome := <-results:
			outcomes[outcome.item.slot] = outcome
		case <-ctx.Done():
			for _, item := range level {
				if _, settled := outcomes[item.slot]; settled {
					continue
				}
				outcomes[item.slot] = pageOutcome{
					item:     item,
					response: identifyResponse(item.canonical, failureResponse(ctx.Err())),
				}
			}
		}
	}
	ordered := make([]pageOutcome, 0, len(outcomes))
	for _, outcome := range outcomes {
		ordered = append(ordered, outcome)
	}
	sort.Slice(ordered, func(left, right int) bool {
		return ordered[left].item.slot < ordered[right].item.slot
	})
	return ordered
}

func (service *Service) executePage(ctx context.Context, item crawlItem, options models.ScrapeOptions, results chan<- pageOutcome) {
	outcome := pageOutcome{item: item}
	defer func() {
		if recovered := recover(); recovered != nil {
			outcome.response = failureResponse(models.NewScrapeError(
				models.ErrCodeInternal,
				fmt.Sprintf("crawl scrape panicked: %v", recovered),
				nil,
			))
		}
		if outcome.response == nil {
			outcome.response = failureResponse(models.NewScrapeError(
				models.ErrCodeInternal,
				"crawl page returned an empty response",
				nil,
			))
		}
		outcome.response = identifyResponse(item.canonical, outcome.response)
		results <- outcome
	}()

	if err := ctx.Err(); err != nil {
		outcome.response = failureResponse(err)
		return
	}
	outcome.response = service.runOne(ctx, item.canonical, options)
}

func (service *Service) runOne(ctx context.Context, targetURL string, options models.ScrapeOptions) *models.ScrapeResponse {
	request := &models.ScrapeRequest{URL: targetURL}
	models.ApplyScrapeOptions(request, options)

	var observedMu sync.Mutex
	var observedFailure *models.ScrapeResponse
	result, err := service.runner.Run(ctx, request, func(event scrape.Event) {
		if event.Type == scrape.EventError && event.Response != nil {
			observedMu.Lock()
			observedFailure = cloneScrapeResponse(event.Response)
			observedMu.Unlock()
		}
	})
	if err != nil {
		observedMu.Lock()
		failure := observedFailure
		observedMu.Unlock()
		if failure != nil {
			return failure
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

func discoverNext(outcomes []pageOutcome, plan crawlPlan, visited map[string]struct{}, totalReserved int) []crawlItem {
	if len(outcomes) == 0 || outcomes[0].item.depth >= plan.maxDepth || plan.scope.mode == scopePage || totalReserved >= plan.maxPages {
		return nil
	}

	remaining := plan.maxPages - totalReserved
	candidates := make(map[string]*url.URL, remaining)
	orderedCandidates := &urlCandidateHeap{}
	heap.Init(orderedCandidates)
	for _, outcome := range outcomes {
		if outcome.response == nil || !outcome.response.Success {
			continue
		}
		for _, links := range [][]models.Link{outcome.response.Links.Internal, outcome.response.Links.External} {
			for _, link := range links {
				canonical, parsed, err := normalizeURL(link.Href, outcome.base)
				if err != nil {
					continue
				}
				if isExcluded(canonical, parsed, plan.exclude) {
					continue
				}
				if !plan.scope.allows(parsed) {
					continue
				}
				if _, exists := visited[canonical]; exists {
					continue
				}
				if _, exists := candidates[canonical]; exists {
					continue
				}
				candidate := urlCandidate{canonical: canonical}
				if len(candidates) < remaining {
					candidates[canonical] = parsed
					heap.Push(orderedCandidates, candidate)
					continue
				}
				worst := (*orderedCandidates)[0]
				if canonical >= worst.canonical {
					continue
				}
				heap.Pop(orderedCandidates)
				delete(candidates, worst.canonical)
				candidates[canonical] = parsed
				heap.Push(orderedCandidates, candidate)
			}
		}
	}

	canonicalURLs := make([]string, 0, len(candidates))
	for canonical := range candidates {
		canonicalURLs = append(canonicalURLs, canonical)
	}
	sort.Strings(canonicalURLs)

	nextDepth := outcomes[0].item.depth + 1
	next := make([]crawlItem, len(canonicalURLs))
	for index, canonical := range canonicalURLs {
		visited[canonical] = struct{}{}
		next[index] = crawlItem{
			canonical: canonical,
			parsed:    candidates[canonical],
			depth:     nextDepth,
			slot:      totalReserved + index,
		}
	}
	return next
}

type urlCandidate struct {
	canonical string
}

type urlCandidateHeap []urlCandidate

func (values urlCandidateHeap) Len() int { return len(values) }
func (values urlCandidateHeap) Less(left, right int) bool {
	return values[left].canonical > values[right].canonical
}
func (values urlCandidateHeap) Swap(left, right int) {
	values[left], values[right] = values[right], values[left]
}
func (values *urlCandidateHeap) Push(value any) {
	*values = append(*values, value.(urlCandidate))
}
func (values *urlCandidateHeap) Pop() any {
	old := *values
	last := old[len(old)-1]
	*values = old[:len(old)-1]
	return last
}

func countFailures(outcomes []pageOutcome) int {
	failed := 0
	for _, outcome := range outcomes {
		if outcome.response == nil || !outcome.response.Success {
			failed++
		}
	}
	return failed
}

func (service *Service) notifyPage(plan crawlPlan, id string, response *models.ScrapeResponse) {
	if plan.webhookURL == "" || service.notifier == nil {
		return
	}
	event := &webhook.Event{
		Type:      "crawl.page",
		JobID:     id,
		Timestamp: service.now().Unix(),
		Data:      cloneScrapeResponse(response),
	}
	service.enqueueNotification(notification{targetURL: plan.webhookURL, secret: plan.webhookSecret, event: event})
}

func (service *Service) notifyTerminal(state crawlState) {
	if state.WebhookURL == "" || service.notifier == nil {
		return
	}
	snapshot := statusResponse(state)
	event := &webhook.Event{
		Type:      "crawl." + snapshot.Status,
		JobID:     snapshot.ID,
		Timestamp: service.now().Unix(),
		Data:      cloneStatus(snapshot),
	}
	service.enqueueNotification(notification{targetURL: state.WebhookURL, secret: state.WebhookSecret, event: event})
}

func executorFailureResponse(err error) *models.ScrapeResponse {
	switch {
	case errors.Is(err, jobs.ErrQueueFull):
		return failureResponse(models.NewScrapeError(models.ErrCodeRateLimited, "crawl worker queue is full", err))
	case errors.Is(err, jobs.ErrSubmitCanceled):
		return failureResponse(models.NewScrapeError(models.ErrCodeTimeout, "crawl job was canceled before execution", err))
	case errors.Is(err, jobs.ErrClosed):
		return failureResponse(models.NewScrapeError(models.ErrCodeInternal, "crawl worker executor is closed", err))
	default:
		return failureResponse(models.NewScrapeError(models.ErrCodeInternal, "crawl worker rejected scrape task", err))
	}
}

func failureResponse(err error) *models.ScrapeResponse {
	var scrapeError *models.ScrapeError
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		scrapeError = models.NewScrapeError(models.ErrCodeTimeout, "scrape request deadline exceeded", err)
	case errors.As(err, &scrapeError):
	default:
		if err == nil {
			err = errors.New("unknown crawl error")
		}
		scrapeError = models.NewScrapeError(models.ErrCodeInternal, err.Error(), err)
	}
	return &models.ScrapeResponse{Success: false, Error: scrapeError.ToDetail()}
}

func identifyResponse(targetURL string, response *models.ScrapeResponse) *models.ScrapeResponse {
	if response == nil {
		response = failureResponse(errors.New("crawl page returned an empty response"))
	}
	if response.FinalURL == "" {
		response.FinalURL = targetURL
	}
	if response.Metadata.SourceURL == "" {
		response.Metadata.SourceURL = targetURL
	}
	return response
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

func statusResponse(state crawlState) models.CrawlStatusResponse {
	return models.CrawlStatusResponse{
		ID:        state.ID,
		Status:    state.Status,
		Completed: state.Completed,
		Total:     state.Total,
		Results:   cloneResults(state.Results),
	}
}

func randomCrawlID() (string, error) {
	bytes := make([]byte, 8)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return "crawl-" + hex.EncodeToString(bytes), nil
}
