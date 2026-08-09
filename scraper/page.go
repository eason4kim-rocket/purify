package scraper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/go-rod/stealth"
	"github.com/use-agent/purify/engine"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/snapshot"
)

// DoScrape is the top-level orchestrator.
//
// If the multi-engine dispatcher is configured AND the request has no Actions
// AND no CDPURL, it delegates to the dispatcher for a faster path (HTTP-first
// with Rod fallback via engine racing). Otherwise it falls through to the
// direct Rod-based scraping path.
func (s *Scraper) DoScrape(ctx context.Context, req *models.ScrapeRequest) (*ScrapeResult, error) {
	timeout := s.scrapeTimeout(req)
	requestCtx, requestCancel := context.WithTimeout(ctx, timeout)
	defer requestCancel()

	// ── 0. Multi-engine dispatch ────────────────────────────────────
	// If the dispatcher is configured AND the request has no Actions AND
	// no CDPURL, delegate to the multi-engine dispatcher for a faster path.
	if s.dispatcher != nil && len(req.Actions) == 0 && req.CDPURL == "" {
		fetchReq := FetchRequestFromScrapeRequest(req, timeout)

		result, err := s.dispatcher.Dispatch(requestCtx, fetchReq)
		if err == nil {
			return s.finalizeScrape(req, &ScrapeResult{
				RawHTML:     result.HTML,
				Title:       result.Title,
				StatusCode:  result.StatusCode,
				FinalURL:    result.FinalURL,
				EngineUsed:  result.EngineName,
				FetchMethod: result.EngineName,
				ContentType: result.ContentType,
			})
		}
		// Dispatcher failed entirely — fall through to existing rod logic.
		slog.Warn("dispatcher failed, falling back to direct rod scrape",
			"url", req.URL, "error", err)
	}

	result, err := s.doScrapeRod(requestCtx, req)
	if err != nil {
		return nil, err
	}
	return s.finalizeScrape(req, result)
}

func (s *Scraper) scrapeTimeout(req *models.ScrapeRequest) time.Duration {
	timeout := s.scraperCfg.DefaultTimeout
	if req != nil && req.Timeout > 0 {
		timeout = time.Duration(req.Timeout) * time.Second
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if s.scraperCfg.MaxTimeout > 0 && timeout > s.scraperCfg.MaxTimeout {
		timeout = s.scraperCfg.MaxTimeout
	}
	return timeout
}

func (s *Scraper) finalizeScrape(req *models.ScrapeRequest, result *ScrapeResult) (*ScrapeResult, error) {
	result.FetchedAt = time.Now().UTC()
	if result.FinalURL == "" {
		result.FinalURL = req.URL
	}
	if result.EngineUsed == "" {
		result.EngineUsed = result.FetchMethod
	}
	if result.ContentType == "" {
		result.ContentType = "text/html"
	}
	if s.snapshots == nil {
		return result, nil
	}

	id, err := s.snapshots.Put([]byte(result.RawHTML), snapshot.Meta{
		URL:         result.FinalURL,
		FetchedAt:   result.FetchedAt,
		Engine:      result.EngineUsed,
		StatusCode:  result.StatusCode,
		ContentType: result.ContentType,
	})
	if err != nil {
		return nil, models.NewScrapeError(models.ErrCodeInternal, "failed to persist page snapshot", err)
	}
	result.SnapshotID = id
	return result, nil
}

// DoScrapeRod is the direct rod-based scraping path. It is exported so
// that the engine.RodEngine callback in main.go can call it without
// triggering the dispatcher (avoiding infinite recursion).
func (s *Scraper) DoScrapeRod(ctx context.Context, req *models.ScrapeRequest) (*ScrapeResult, error) {
	return s.doScrapeRodBounded(ctx, req, 0)
}

// DoScrapeRodBounded runs the direct browser path while bounding the main
// document, all decoded HTTP(S) response bodies in the page session, and the
// rendered HTML returned over CDP. A zero limit preserves the legacy browser
// behavior used by the public scrape API; verification callers pass their
// explicit observation bound.
func (s *Scraper) DoScrapeRodBounded(ctx context.Context, req *models.ScrapeRequest, maximumBodyBytes int64) (*ScrapeResult, error) {
	if maximumBodyBytes < 0 {
		return nil, fmt.Errorf("%w: maximum must not be negative", engine.ErrResponseBodyTooLarge)
	}
	return s.doScrapeRodBounded(ctx, req, maximumBodyBytes)
}

// doScrapeRod contains the full rod-based scraping logic (timeout, pool,
// stealth, navigation, extraction). This is the original DoScrape path.
//
// Lifecycle (numbered steps match the inline comments):
//
//  1. Timeout guard          – hard deadline on the entire operation
//  2. Acquire page           – borrow a tab from the pool (or create one)
//  3. DEFER: cleanup         – about:blank + return to pool (leak prevention)
//  4. Stealth injection      – mask navigator.webdriver etc. (before navigation!)
//  5. Hijack mount           – block images/CSS/fonts/media (before navigation!)
//  6. Context binding        – propagate timeout to all Rod operations
//  7. Activity tracker setup – MUST be registered before Navigate to capture fetch/XHR
//  8. Navigate               – triggers page load
//  9. Wait                   – network idle or DOM stable
//  10. Extract               – page.HTML() + document.title
//
// Why this order matters:
//   - Steps 4-5 MUST happen before step 8: stealth JS and resource blocking only
//     take effect for navigations that happen after they are installed.
//   - Step 7 MUST happen before step 8: the in-page network tracker wraps
//     fetch/XHR in every new document, so installing it after Navigate would
//     miss early requests and could report a false idle.
//   - Step 3's about:blank uses the ORIGINAL page reference (without request
//     context), so cleanup succeeds even if the request context has expired.
func (s *Scraper) doScrapeRod(ctx context.Context, req *models.ScrapeRequest) (*ScrapeResult, error) {
	return s.doScrapeRodBounded(ctx, req, 0)
}

func (s *Scraper) doScrapeRodBounded(ctx context.Context, req *models.ScrapeRequest, maximumBodyBytes int64) (*ScrapeResult, error) {
	// ── 1. Timeout guard ──────────────────────────────────────────────
	timeout := s.scrapeTimeout(req)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// One context-aware gate covers every Chromium route. The Rod page pool
	// alone cannot bound request-isolated BrowserContexts or external CDP pages.
	releaseSlot, slotErr := s.browserSlots.acquire(ctx)
	if slotErr != nil {
		if errors.Is(slotErr, context.Canceled) || errors.Is(slotErr, context.DeadlineExceeded) {
			return nil, categorizeError(slotErr, "waiting for browser capacity")
		}
		return nil, models.NewScrapeError(models.ErrCodeBrowserCrash, "browser is unavailable", slotErr)
	}
	s.activePages.Add(1)
	releaseSlotHere := true
	releaseBrowserSlot := func() {
		s.activePages.Add(-1)
		releaseSlot()
	}
	defer func() {
		if releaseSlotHere {
			releaseBrowserSlot()
		}
	}()
	transferCleanup := func(cleanupDone <-chan struct{}) {
		if releaseBrowserSlotAfter(cleanupDone, releaseBrowserSlot) {
			releaseSlotHere = false
		}
	}

	// ── 1b. Per-request CDP URL: connect to user's own Chrome ────────
	if req.CDPURL != "" {
		result, scrapeErr, cleanupDone := s.doScrapeWithCDP(ctx, req, maximumBodyBytes)
		transferCleanup(cleanupDone)
		return result, scrapeErr
	}
	// A request proxy must never mutate the shared Chrome process. Cookies are
	// isolated for the same reason: Chromium stores them at browser-context
	// scope, not page scope.
	if req.ProxyURL != "" || len(req.Cookies) > 0 {
		result, scrapeErr, cleanupDone := s.doScrapeInRequestContext(ctx, req, maximumBodyBytes)
		transferCleanup(cleanupDone)
		return result, scrapeErr
	}

	// ── 2. Acquire page from pool ─────────────────────────────────────
	page, acquireErr := s.pagePool.Get(func() (*rod.Page, error) {
		return s.browser.Page(proto.TargetCreateTarget{})
	})
	if acquireErr != nil {
		return nil, models.NewScrapeError(
			models.ErrCodeBrowserCrash,
			"failed to acquire page from pool",
			acquireErr,
		)
	}
	s.trackPage(page)

	// ── 3. CRITICAL DEFER: prevent DOM memory leak + guarantee pool return
	pageSucceeded := false
	defer func() {
		cleanupErr := page.Navigate("about:blank")
		if cleanupErr != nil {
			slog.Warn("cleanup: failed to navigate to about:blank",
				"error", cleanupErr,
			)
		}
		if s.observePageUse(page, pageSucceeded && cleanupErr == nil) || cleanupErr != nil {
			s.retirePage(page)
			return
		}
		s.pagePool.Put(page)
	}()
	p := page.Context(ctx)

	// ── 4. Stealth injection ──────────────────────────────────────────
	if req.Stealth {
		if _, evalErr := p.EvalOnNewDocument(stealth.JS); evalErr != nil {
			slog.Warn("stealth injection failed, proceeding without stealth",
				"error", evalErr,
			)
		}
	}

	// ── 4b. Build extra headers (custom + Google Referer) ────────────
	extraHeaders := browserHeaders(req)
	if len(extraHeaders) > 0 {
		restoreHeaders, headerErr := setBrowserHeaders(p, extraHeaders)
		if headerErr != nil {
			return nil, categorizeError(headerErr, "failed to set request headers")
		}
		defer restoreHeaders()
	}

	// ── 4c. Custom cookies ──────────────────────────────────────────
	for _, cookie := range req.Cookies {
		domain := cookie.Domain
		if domain == "" {
			if u, parseErr := url.Parse(req.URL); parseErr == nil {
				domain = u.Hostname()
			}
		}
		path := cookie.Path
		if path == "" {
			path = "/"
		}
		_, cookieErr := (proto.NetworkSetCookie{
			Name:   cookie.Name,
			Value:  cookie.Value,
			Domain: domain,
			Path:   path,
		}).Call(p)
		if cookieErr != nil {
			return nil, categorizeError(cookieErr, "failed to set request cookie")
		}
	}

	// ── 5. Mount hijack router (blocks Image/Stylesheet/Font/Media + ads) ──
	interception, interceptErr := setupPageInterception(p, s.scraperCfg.BlockedResourceTypes, req.BlockAds, maximumBodyBytes)
	if interceptErr != nil {
		return nil, categorizeError(interceptErr, "failed to install browser response bounds")
	}
	defer interception.stop()

	// ── 7. Install network activity tracking BEFORE navigation ────────
	// CDP WaitRequestIdle conflicts with Fetch-domain request hijacking on
	// recent Chromium. The in-page tracker observes fetch/XHR without enabling
	// a second interception domain and lets us honor wait_for_network_idle.
	if networkIdleRequested(req) {
		removeTracker, trackerErr := installNetworkTracker(p)
		if trackerErr != nil {
			return nil, categorizeError(trackerErr, "failed to install network idle tracker")
		}
		defer func() { _ = removeTracker() }()
	}

	// ── 7b. Status code capture ──────────────────────────────────────
	// NOTE: page.EachEvent(NetworkResponseReceived) causes ERR_BLOCKED_BY_CLIENT
	// on Chromium 145+ because it internally enables Network domain interception
	// which conflicts with the Fetch domain used by HijackRequests/WaitRequestIdle.
	// Instead, we capture the status code AFTER navigation from the page's
	// NavigationHistory, which is always available without any event listeners.
	var statusCode int

	// ── 8. Navigate ───────────────────────────────────────────────────
	var navErr error
	if navErr = p.Navigate(req.URL); navErr != nil {
		if terminalErr := interception.err(); terminalErr != nil {
			return nil, terminalErr
		}
		return nil, categorizeError(navErr, "navigation to target URL failed")
	}
	if terminalErr := interception.err(); terminalErr != nil {
		return nil, terminalErr
	}

	// ── 9. Wait for a complete, usable document ──────────────────────
	if waitErr := waitForDocument(p, networkIdleRequested(req)); waitErr != nil {
		if terminalErr := interception.err(); terminalErr != nil {
			return nil, terminalErr
		}
		return nil, categorizeError(waitErr, "document did not become ready")
	}
	if terminalErr := interception.err(); terminalErr != nil {
		return nil, terminalErr
	}

	// ── 9b. Collect status code via JS (best-effort) ────────────────
	// Use performance.getEntriesByType("navigation") to get the HTTP status
	// code without needing CDP event listeners.
	if res, err := p.Eval(`() => {
		try {
			const entries = performance.getEntriesByType("navigation");
			if (entries.length > 0) return entries[0].responseStatus || 0;
		} catch(e) {}
		return 0;
	}`); err == nil {
		statusCode = res.Value.Int()
	}

	// ── 9c. Remove overlays (cookie banners, popups) ────────────────
	if req.RemoveOverlays {
		removeOverlays(p)
	}

	// ── 9d. Execute browser actions ─────────────────────────────────
	if len(req.Actions) > 0 {
		if err := executeActions(ctx, page, req.Actions); err != nil {
			return nil, err
		}
		if waitErr := waitForPostActionStability(p, networkIdleRequested(req)); waitErr != nil {
			if terminalErr := interception.err(); terminalErr != nil {
				return nil, terminalErr
			}
			return nil, categorizeError(waitErr, "document did not stabilize after actions")
		}
		if terminalErr := interception.err(); terminalErr != nil {
			return nil, terminalErr
		}
	}

	// ── 10. Extract rendered HTML ─────────────────────────────────────
	rawHTML, htmlErr := extractBoundedHTML(p, maximumBodyBytes)
	if htmlErr != nil {
		if errors.Is(htmlErr, engine.ErrResponseBodyTooLarge) {
			return nil, htmlErr
		}
		return nil, categorizeError(htmlErr, "failed to extract page HTML")
	}

	// ── 11. Extract title and final URL (best-effort) ────────────────
	title := evalStringOrEmpty(p, `() => document.title`)
	finalURL := evalStringOrEmpty(p, `() => window.location.href`)
	contentType := evalStringOrEmpty(p, `() => document.contentType`)
	if finalURL == "" {
		finalURL = req.URL
	}

	result := &ScrapeResult{
		RawHTML:     rawHTML,
		Title:       title,
		StatusCode:  statusCode,
		FinalURL:    finalURL,
		EngineUsed:  rodEngineName(req.Stealth),
		FetchMethod: "browser",
		ContentType: contentType,
	}
	pageSucceeded = true
	return result, nil
}

func rodEngineName(stealthEnabled bool) string {
	if stealthEnabled {
		return "rod-stealth"
	}
	return "rod"
}

func networkIdleRequested(req *models.ScrapeRequest) bool {
	return req != nil && req.WaitForNetworkIdle != nil && *req.WaitForNetworkIdle
}

const networkTrackerScript = `(() => {
	if (globalThis.__purifyNetworkTrackerInstalled) return;
	globalThis.__purifyNetworkTrackerInstalled = true;
	let pending = 0;
	Object.defineProperty(globalThis, '__purifyPendingRequests', {
		configurable: true,
		get: () => pending
	});
	if (typeof globalThis.fetch === 'function') {
		const originalFetch = globalThis.fetch;
		globalThis.fetch = function(...args) {
			pending++;
			try {
				return Promise.resolve(originalFetch.apply(this, args)).finally(() => { pending = Math.max(0, pending - 1); });
			} catch (error) {
				pending = Math.max(0, pending - 1);
				throw error;
			}
		};
	}
	if (typeof globalThis.XMLHttpRequest === 'function') {
		const originalSend = globalThis.XMLHttpRequest.prototype.send;
		globalThis.XMLHttpRequest.prototype.send = function(...args) {
			pending++;
			this.addEventListener('loadend', () => { pending = Math.max(0, pending - 1); }, { once: true });
			try {
				return originalSend.apply(this, args);
			} catch (error) {
				pending = Math.max(0, pending - 1);
				throw error;
			}
		};
	}
})();`

func installNetworkTracker(page *rod.Page) (func() error, error) {
	return page.EvalOnNewDocument(networkTrackerScript)
}

func waitForDocument(page *rod.Page, networkIdle bool) error {
	if _, err := page.Element("body"); err != nil {
		return err
	}
	if err := page.WaitLoad(); err != nil {
		return err
	}
	return waitForPostActionStability(page, networkIdle)
}

func waitForPostActionStability(page *rod.Page, networkIdle bool) error {
	if networkIdle {
		_, err := page.Eval(`() => new Promise(resolve => {
			const quietForMs = 300;
			let quietSince = 0;
			const poll = () => {
				const pending = Number(globalThis.__purifyPendingRequests || 0);
				const bodyText = document.body ? (document.body.innerText || '').trim() : '';
				if (document.body && document.readyState === 'complete' && pending === 0 && bodyText.length > 0) {
					if (quietSince === 0) quietSince = Date.now();
					if (Date.now() - quietSince >= quietForMs) return resolve(true);
				} else {
					quietSince = 0;
				}
				setTimeout(poll, 50);
			};
			poll();
		})`)
		return err
	}
	return page.WaitDOMStable(300*time.Millisecond, 0.1)
}

// evalStringOrEmpty evaluates a JS expression and returns the string result,
// swallowing any errors (useful for optional metadata extraction).
func evalStringOrEmpty(page *rod.Page, js string) string {
	res, err := page.Eval(js)
	if err != nil {
		return ""
	}
	return res.Value.Str()
}

func evalIntOrZero(page *rod.Page, js string) int {
	res, err := page.Eval(js)
	if err != nil {
		return 0
	}
	return res.Value.Int()
}

// doScrapeWithCDP connects to a user-provided CDP endpoint, creates a
// temporary page, scrapes it, and disconnects (without killing the browser).
func (s *Scraper) doScrapeWithCDP(ctx context.Context, req *models.ScrapeRequest, maximumBodyBytes int64) (*ScrapeResult, error, <-chan struct{}) {
	browser, disconnect, err := connectCDP(ctx, req.CDPURL)
	if err != nil {
		return nil, models.NewScrapeError(
			models.ErrCodeBrowserCrash,
			"failed to connect to CDP URL",
			err,
		), nil
	}

	isolated, cleanup, err := newIsolatedBrowserContext(ctx, browser, req.ProxyURL)
	if err != nil {
		disconnect()
		return nil, models.NewScrapeError(
			models.ErrCodeBrowserCrash,
			"failed to create isolated CDP browser context",
			err,
		), nil
	}
	result, scrapeErr := s.scrapeStandalonePage(ctx, isolated, req, "cdp", maximumBodyBytes)
	cleanupDone := cleanup()
	select {
	case <-cleanupDone:
		disconnect()
		return result, scrapeErr, cleanupDone
	default:
	}
	disconnected := make(chan struct{})
	go func() {
		<-cleanupDone
		disconnect()
		close(disconnected)
	}()
	return result, scrapeErr, disconnected
}

// removeOverlays injects JS to remove fixed/sticky positioned elements with
// high z-index, which are typically cookie consent banners and popup overlays.
func removeOverlays(p *rod.Page) {
	const js = `() => {
		const els = document.querySelectorAll('*');
		for (const el of els) {
			const style = window.getComputedStyle(el);
			const pos = style.position;
			if (pos === 'fixed' || pos === 'sticky') {
				const z = parseInt(style.zIndex, 10);
				if (z >= 900 || style.zIndex === 'auto') {
					el.remove();
				}
			}
		}
		// Also remove common overlay class patterns.
		const selectors = [
			'[class*="cookie"]', '[class*="consent"]', '[class*="overlay"]',
			'[id*="cookie"]', '[id*="consent"]', '[id*="overlay"]',
			'[class*="popup"]', '[id*="popup"]',
			'[class*="gdpr"]', '[id*="gdpr"]',
		];
		for (const sel of selectors) {
			document.querySelectorAll(sel).forEach(el => {
				const style = window.getComputedStyle(el);
				if (style.position === 'fixed' || style.position === 'sticky' || style.position === 'absolute') {
					el.remove();
				}
			});
		}
		// Remove any overflow:hidden on body/html (often set by modals).
		document.documentElement.style.overflow = '';
		document.body.style.overflow = '';
	}`
	_, _ = p.Eval(js)
}

// categorizeError wraps raw errors into typed ScrapeErrors so the API layer
// can map them to appropriate HTTP status codes.
func categorizeError(err error, msg string) *models.ScrapeError {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return models.NewScrapeError(models.ErrCodeTimeout, msg, err)
	case errors.Is(err, context.Canceled):
		return models.NewScrapeError(models.ErrCodeTimeout, "request canceled", err)
	default:
		return models.NewScrapeError(models.ErrCodeNavigation, msg, err)
	}
}
