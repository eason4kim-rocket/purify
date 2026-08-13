// Package scrape owns the canonical scrape orchestration shared by HTTP, SSE,
// batch, crawl, extract, and agent-facing adapters.
package scrape

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/use-agent/purify/cache"
	"github.com/use-agent/purify/cleaner"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/quality"
	"github.com/use-agent/purify/scraper"
)

const defaultMaximumTimeout = 120 * time.Second

// Fetcher returns one raw candidate. Supports must be side-effect free and
// prevents an engine from silently ignoring options it cannot honor.
type Fetcher interface {
	Name() string
	Supports(*models.ScrapeRequest) bool
	Fetch(context.Context, *models.ScrapeRequest) (*scraper.ScrapeResult, error)
}

// ContentCleaner is the stable subset of cleaner.Cleaner used by the service.
type ContentCleaner interface {
	Clean(rawHTML, sourceURL, format, extractMode string, opts ...cleaner.CleanOptions) (*models.ScrapeResponse, error)
}

// ResponseCache stores only public response values. It deliberately cannot
// manufacture raw source or snapshots on a cache hit.
type ResponseCache interface {
	Get(key string, maxAgeMs int) (*models.ScrapeResponse, bool)
	Set(key string, response *models.ScrapeResponse)
}

// SourceFinalizer persists or enriches only the candidate ultimately selected
// by the quality gate. Rejected candidates must never reach this interface.
type SourceFinalizer interface {
	FinalizeSelected(*models.ScrapeRequest, *scraper.ScrapeResult) (*scraper.ScrapeResult, error)
}

// Result includes the public response and its selected raw source. Source is
// nil for cache hits because the response cache intentionally stores no HTML.
type Result struct {
	Response *models.ScrapeResponse
	Source   *scraper.ScrapeResult
	CacheHit bool
}

// Config controls service-level invariants.
type Config struct {
	MaximumTimeout time.Duration
	Now            func() time.Time
}

// Service executes the canonical ordered scrape pipeline.
type Service struct {
	fetchers       []Fetcher
	cleaner        ContentCleaner
	cache          ResponseCache
	finalizer      SourceFinalizer
	maximumTimeout time.Duration
	now            func() time.Time
}

// NewService constructs a canonical scrape service. Fetchers retain caller
// order, which must be HTTP, ordinary browser, then stealth browser in the
// production composition root.
func NewService(fetchers []Fetcher, contentCleaner ContentCleaner, responseCache ResponseCache, finalizer SourceFinalizer, cfg Config) (*Service, error) {
	if len(fetchers) == 0 {
		return nil, fmt.Errorf("scrape: at least one fetcher is required")
	}
	for index, fetcher := range fetchers {
		if fetcher == nil {
			return nil, fmt.Errorf("scrape: fetcher %d is nil", index)
		}
	}
	if contentCleaner == nil {
		return nil, fmt.Errorf("scrape: content cleaner is required")
	}
	maximumTimeout := cfg.MaximumTimeout
	if maximumTimeout <= 0 {
		maximumTimeout = defaultMaximumTimeout
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		fetchers:       slices.Clone(fetchers),
		cleaner:        contentCleaner,
		cache:          responseCache,
		finalizer:      finalizer,
		maximumTimeout: maximumTimeout,
		now:            now,
	}, nil
}

// Run applies defaults and validation, checks the immutable response cache,
// then evaluates fetchers in order under one total request deadline.
func (s *Service) Run(ctx context.Context, request *models.ScrapeRequest, observe Observer) (*Result, error) {
	startedAt := s.now()
	req, err := cloneAndValidateRequest(request, s.maximumTimeout)
	if err != nil {
		s.emitError(observe, requestURL(request), startedAt, err)
		return nil, err
	}
	emit(observe, Event{Type: EventStarted, URL: req.URL})

	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(req.Timeout)*time.Second)
	defer cancel()

	cacheKey := cache.KeyForRequest(req)
	if s.cache != nil && req.MaxAge > 0 {
		if cached, hit := s.cache.Get(cacheKey, req.MaxAge); hit {
			cached.CacheStatus = "hit"
			cached.Timing = models.TimingInfo{TotalMs: elapsedMilliseconds(startedAt, s.now())}
			result := &Result{Response: cached, CacheHit: true}
			emit(observe, Event{Type: EventCompleted, URL: req.URL, Response: cached})
			return result, nil
		}
	}

	var (
		attempts       []models.FetchAttempt
		navigationTime time.Duration
		cleaningTime   time.Duration
		lastErr        error
		lastRejected   *quality.Assessment
		supported      int
	)

	for _, fetcher := range s.fetchers {
		if !fetcher.Supports(req) {
			continue
		}
		supported++
		fetchStarted := s.now()
		source, fetchErr := fetcher.Fetch(requestCtx, cloneRequest(req))
		fetchDuration := nonNegativeDuration(fetchStarted, s.now())
		navigationTime += fetchDuration
		if fetchErr != nil {
			// The HTTP response deliberately carries only the error category, so
			// this is the one place the underlying fetch error stays diagnosable.
			slog.Warn("fetch attempt failed",
				"engine", fetcher.Name(), "url", req.URL,
				"duration_ms", fetchDuration.Milliseconds(), "error", fetchErr)
			attempt := models.FetchAttempt{
				Engine:     fetcher.Name(),
				Outcome:    models.FetchAttemptFailed,
				DurationMs: fetchDuration.Milliseconds(),
			}
			attempts = append(attempts, attempt)
			emitAttempt(observe, req.URL, attempt)
			lastErr = fetchErr
			if requestCtx.Err() != nil {
				break
			}
			continue
		}
		if source == nil {
			lastErr = fmt.Errorf("scrape: fetcher %s returned a nil result", fetcher.Name())
			attempt := models.FetchAttempt{Engine: fetcher.Name(), Outcome: models.FetchAttemptFailed, DurationMs: fetchDuration.Milliseconds()}
			attempts = append(attempts, attempt)
			emitAttempt(observe, req.URL, attempt)
			continue
		}
		normalizeSource(source, req.URL, fetcher.Name())

		modeUsed := req.ExtractMode
		if rawReason := quality.EvaluateRawDocument(source.RawHTML); rawReason != "" {
			assessment := quality.EvaluateCandidate(quality.Candidate{
				RawHTML:     source.RawHTML,
				ExtractMode: modeUsed,
			})
			attempt := assessment.AsFetchAttempt(fetcher.Name(), fetchDuration)
			attempts = append(attempts, attempt)
			emitAttempt(observe, req.URL, attempt)
			lastRejected = &assessment
			continue
		}

		cleanStarted := s.now()
		response, cleanErr := s.cleaner.Clean(
			source.RawHTML,
			source.FinalURL,
			req.OutputFormat,
			req.ExtractMode,
			cleaner.CleanOptions{
				IncludeTags: req.IncludeTags,
				ExcludeTags: req.ExcludeTags,
				CSSSelector: req.CSSSelector,
			},
		)
		cleanDuration := nonNegativeDuration(cleanStarted, s.now())
		cleaningTime += cleanDuration
		if cleanErr != nil {
			s.emitError(observe, req.URL, startedAt, cleanErr)
			return nil, cleanErr
		}
		if response == nil {
			err = models.NewScrapeError(models.ErrCodeInternal, "content cleaner returned a nil response", nil)
			s.emitError(observe, req.URL, startedAt, err)
			return nil, err
		}
		if response.Quality != nil && response.Quality.ExtractModeUsed != "" {
			modeUsed = response.Quality.ExtractModeUsed
		}
		assessment := quality.EvaluateCandidate(quality.Candidate{
			RawHTML:        source.RawHTML,
			CleanedContent: response.Content,
			ExtractMode:    modeUsed,
		})
		attempt := assessment.AsFetchAttempt(fetcher.Name(), fetchDuration)
		attempts = append(attempts, attempt)
		emitAttempt(observe, req.URL, attempt)
		if !assessment.Usable() {
			lastRejected = &assessment
			continue
		}

		selected, finalizeErr := s.finalize(req, source)
		if finalizeErr != nil {
			s.emitError(observe, req.URL, startedAt, finalizeErr)
			return nil, finalizeErr
		}
		if selected == nil {
			err = models.NewScrapeError(models.ErrCodeInternal, "source finalizer returned a nil result", nil)
			s.emitError(observe, req.URL, startedAt, err)
			return nil, err
		}
		qualityInfo := assessment.Info
		qualityInfo.FetchAttempts = slices.Clone(attempts)
		response.Quality = &qualityInfo
		assembleResponse(response, selected, startedAt, s.now(), navigationTime, cleaningTime)
		if s.cache != nil && req.MaxAge > 0 {
			response.CacheStatus = "miss"
			s.cache.Set(cacheKey, response)
		}

		emit(observe, Event{
			Type: EventNavigated,
			URL:  req.URL,
			Navigation: &Navigation{
				StatusCode:   selected.StatusCode,
				FinalURL:     selected.FinalURL,
				EngineUsed:   selected.EngineUsed,
				FetchMethod:  selected.FetchMethod,
				NavigationMs: navigationTime.Milliseconds(),
			},
		})
		result := &Result{Response: response, Source: selected}
		emit(observe, Event{Type: EventCompleted, URL: req.URL, Response: response})
		return result, nil
	}

	terminalErr := terminalError(requestCtx, supported, lastErr, lastRejected)
	s.emitErrorWithQuality(observe, req.URL, startedAt, terminalErr, terminalQualityInfo(attempts, lastRejected))
	return nil, terminalErr
}

func (s *Service) finalize(req *models.ScrapeRequest, source *scraper.ScrapeResult) (*scraper.ScrapeResult, error) {
	if s.finalizer == nil {
		return source, nil
	}
	return s.finalizer.FinalizeSelected(req, source)
}

func (s *Service) emitError(observe Observer, target string, startedAt time.Time, err error) {
	s.emitErrorWithQuality(observe, target, startedAt, err, nil)
}

func (s *Service) emitErrorWithQuality(observe Observer, target string, startedAt time.Time, err error, qualityInfo *models.QualityInfo) {
	scrapeErr := asScrapeError(err)
	response := &models.ScrapeResponse{
		Success: false,
		Error:   scrapeErr.ToDetail(),
		Quality: qualityInfo,
		Timing:  models.TimingInfo{TotalMs: elapsedMilliseconds(startedAt, s.now())},
	}
	emit(observe, Event{Type: EventError, URL: target, Response: response})
}

func terminalQualityInfo(attempts []models.FetchAttempt, lastRejected *quality.Assessment) *models.QualityInfo {
	if len(attempts) == 0 {
		return nil
	}
	info := models.QualityInfo{
		Status:        models.QualityStatusUnusable,
		Warnings:      []models.QualityReason{},
		FetchAttempts: slices.Clone(attempts),
	}
	if lastRejected != nil {
		info = lastRejected.Info
		info.Warnings = slices.Clone(lastRejected.Info.Warnings)
		info.FetchAttempts = slices.Clone(attempts)
	}
	return &info
}

func emit(observer Observer, event Event) {
	if observer != nil {
		observer(event)
	}
}

func emitAttempt(observer Observer, target string, attempt models.FetchAttempt) {
	attemptCopy := attempt
	emit(observer, Event{Type: EventAttempt, URL: target, Attempt: &attemptCopy})
}

func terminalError(ctx context.Context, supported int, lastErr error, lastRejected *quality.Assessment) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return models.NewScrapeError(models.ErrCodeTimeout, "scrape request deadline exceeded", ctxErr)
	}
	if lastRejected != nil {
		return quality.NewContentUnusableError(lastRejected.RejectionReason())
	}
	if lastErr != nil {
		return asScrapeError(lastErr)
	}
	if supported == 0 {
		return models.NewScrapeError(models.ErrCodeInvalidInput, "no fetch engine supports the requested options", nil)
	}
	return quality.NewContentUnusableError(models.QualityReasonLowContentQuality)
}

func asScrapeError(err error) *models.ScrapeError {
	if err == nil {
		return models.NewScrapeError(models.ErrCodeInternal, "unknown scrape failure", nil)
	}
	var scrapeErr *models.ScrapeError
	if errors.As(err, &scrapeErr) {
		return scrapeErr
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return models.NewScrapeError(models.ErrCodeTimeout, "scrape request deadline exceeded", err)
	}
	return models.NewScrapeError(models.ErrCodeNavigation, err.Error(), err)
}

func assembleResponse(response *models.ScrapeResponse, source *scraper.ScrapeResult, startedAt, finishedAt time.Time, navigationTime, cleaningTime time.Duration) {
	response.Success = true
	response.StatusCode = source.StatusCode
	response.FinalURL = source.FinalURL
	response.EngineUsed = source.EngineUsed
	if response.Metadata.Title == "" {
		response.Metadata.Title = source.Title
	}
	response.Metadata.SourceURL = source.FinalURL
	response.Metadata.FetchMethod = source.FetchMethod
	response.Timing = models.TimingInfo{
		TotalMs:      elapsedMilliseconds(startedAt, finishedAt),
		NavigationMs: navigationTime.Milliseconds(),
		CleaningMs:   cleaningTime.Milliseconds(),
	}
}

func normalizeSource(source *scraper.ScrapeResult, requestedURL, fetcherName string) {
	if source.FinalURL == "" {
		source.FinalURL = requestedURL
	}
	if source.EngineUsed == "" {
		source.EngineUsed = fetcherName
	}
	if source.FetchMethod == "" {
		source.FetchMethod = fetcherName
	}
	if source.ContentType == "" {
		source.ContentType = "text/html"
	}
}

func cloneAndValidateRequest(request *models.ScrapeRequest, maximumTimeout time.Duration) (*models.ScrapeRequest, error) {
	if request == nil {
		return nil, models.NewScrapeError(models.ErrCodeInvalidInput, "scrape request is required", nil)
	}
	req := cloneRequest(request)
	req.Defaults()

	parsed, err := url.ParseRequestURI(req.URL)
	if err != nil || parsed.Host == "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, models.NewScrapeError(models.ErrCodeInvalidInput, "url must be an absolute http or https URL", err)
	}
	if req.Timeout < 1 {
		return nil, models.NewScrapeError(models.ErrCodeInvalidInput, "timeout must be at least one second", nil)
	}
	maximumSeconds := int(maximumTimeout / time.Second)
	if maximumSeconds < 1 {
		return nil, models.NewScrapeError(models.ErrCodeInvalidInput, "maximum timeout must be at least one second", nil)
	}
	if req.Timeout > maximumSeconds {
		req.Timeout = maximumSeconds
	}
	if !oneOf(req.OutputFormat, "markdown", "html", "text", "markdown_citations") {
		return nil, models.NewScrapeError(models.ErrCodeInvalidInput, "unsupported output_format", nil)
	}
	if !oneOf(req.ExtractMode, "readability", "raw", "pruning", "auto") {
		return nil, models.NewScrapeError(models.ErrCodeInvalidInput, "unsupported extract_mode", nil)
	}
	if len(req.Actions) > 50 {
		return nil, models.NewScrapeError(models.ErrCodeInvalidInput, "actions cannot exceed 50 entries", nil)
	}
	for index, action := range req.Actions {
		if !oneOf(action.Type, "wait", "click", "scroll", "execute_js", "scrape") {
			return nil, models.NewScrapeError(models.ErrCodeInvalidInput, fmt.Sprintf("action %d has an unsupported type", index), nil)
		}
		if action.Direction != "" && !oneOf(action.Direction, "up", "down") {
			return nil, models.NewScrapeError(models.ErrCodeInvalidInput, fmt.Sprintf("action %d has an unsupported direction", index), nil)
		}
	}
	for index, cookie := range req.Cookies {
		if cookie.Name == "" || cookie.Value == "" {
			return nil, models.NewScrapeError(models.ErrCodeInvalidInput, fmt.Sprintf("cookie %d requires name and value", index), nil)
		}
	}
	if err := validateOptionalURL(req.ProxyURL, "proxy_url", "http", "https", "socks5", "socks5h"); err != nil {
		return nil, err
	}
	if err := validateOptionalURL(req.CDPURL, "cdp_url", "http", "https", "ws", "wss"); err != nil {
		return nil, err
	}
	if req.MaxAge < 0 {
		return nil, models.NewScrapeError(models.ErrCodeInvalidInput, "max_age cannot be negative", nil)
	}
	return req, nil
}

func validateOptionalURL(rawURL, field string, schemes ...string) error {
	if rawURL == "" {
		return nil
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || !slices.Contains(schemes, parsed.Scheme) {
		return models.NewScrapeError(models.ErrCodeInvalidInput, field+" is not a supported URL", err)
	}
	return nil
}

func cloneRequest(source *models.ScrapeRequest) *models.ScrapeRequest {
	if source == nil {
		return nil
	}
	cloned := *source
	models.ApplyScrapeOptions(&cloned, models.ScrapeOptionsFromRequest(source))
	return &cloned
}

func oneOf(value string, allowed ...string) bool {
	return slices.Contains(allowed, value)
}

func requestURL(request *models.ScrapeRequest) string {
	if request == nil {
		return ""
	}
	return strings.TrimSpace(request.URL)
}

func nonNegativeDuration(startedAt, finishedAt time.Time) time.Duration {
	if finishedAt.Before(startedAt) {
		return 0
	}
	return finishedAt.Sub(startedAt)
}

func elapsedMilliseconds(startedAt, finishedAt time.Time) int64 {
	return nonNegativeDuration(startedAt, finishedAt).Milliseconds()
}
