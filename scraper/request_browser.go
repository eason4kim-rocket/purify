package scraper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/proto"
	"github.com/go-rod/stealth"
	"github.com/use-agent/purify/engine"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/proxy"
)

const (
	browserContextCleanupTimeout      = 250 * time.Millisecond
	browserContextAsyncCleanupTimeout = 5 * time.Second
)

// doScrapeInRequestContext uses an isolated Chromium BrowserContext whenever a
// request needs its own proxy or cookie jar. The shared Chrome process remains
// pooled, while context disposal guarantees request state cannot leak into a
// later page.
func (s *Scraper) doScrapeInRequestContext(ctx context.Context, req *models.ScrapeRequest, maximumBodyBytes int64) (*ScrapeResult, error, <-chan struct{}) {
	browser, cleanup, err := newIsolatedBrowserContext(ctx, s.browser, req.ProxyURL)
	if err != nil {
		return nil, models.NewScrapeError(models.ErrCodeNavigation, "failed to create isolated browser context", err), nil
	}
	result, scrapeErr := s.scrapeStandalonePage(ctx, browser, req, rodEngineName(req.Stealth), maximumBodyBytes)
	return result, scrapeErr, cleanup()
}

func newIsolatedBrowserContext(ctx context.Context, base *rod.Browser, rawProxyURL string) (*rod.Browser, func() <-chan struct{}, error) {
	completed := func() <-chan struct{} {
		done := make(chan struct{})
		close(done)
		return done
	}
	proxyServer, relay, err := browserProxy(rawProxyURL)
	if err != nil {
		return nil, completed, err
	}

	created, err := (proto.TargetCreateBrowserContext{
		DisposeOnDetach: true,
		ProxyServer:     proxyServer,
		// Chromium implicitly bypasses loopback hosts. Removing that implicit
		// rule makes an explicit request proxy authoritative for every target.
		ProxyBypassList: "<-loopback>",
	}).Call(base.Context(ctx))
	if err != nil {
		if relay != nil {
			_ = relay.Close()
		}
		return nil, completed, err
	}

	isolated := *base.Context(ctx)
	isolated.BrowserContextID = created.BrowserContextID
	var cleanupOnce sync.Once
	cleanupDone := make(chan struct{})
	cleanup := func() <-chan struct{} {
		cleanupOnce.Do(func() {
			dispose := func(cleanupCtx context.Context) {
				defer close(cleanupDone)
				disposeBrowserContext(cleanupCtx, base, created.BrowserContextID)
				if relay != nil {
					_ = relay.Close()
				}
			}
			if ctx.Err() != nil {
				// The request budget is already exhausted. Do not extend response
				// latency. The caller transfers its global browser slot to this
				// tracked cleanup until the BrowserContext is disposed.
				go func() {
					cleanupCtx, cancel := context.WithTimeout(context.Background(), browserContextAsyncCleanupTimeout)
					defer cancel()
					dispose(cleanupCtx)
				}()
				return
			}
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), browserContextCleanupTimeout)
			defer cancel()
			dispose(cleanupCtx)
		})
		return cleanupDone
	}
	return &isolated, cleanup, nil
}

func disposeBrowserContext(ctx context.Context, base *rod.Browser, browserContextID proto.BrowserBrowserContextID) {
	for {
		cleanupBrowser := base.Context(ctx)
		_ = (proto.TargetDisposeBrowserContext{BrowserContextID: browserContextID}).Call(cleanupBrowser)
		contexts, err := (proto.TargetGetBrowserContexts{}).Call(cleanupBrowser)
		if err == nil {
			found := false
			for _, activeID := range contexts.BrowserContextIDs {
				if activeID == browserContextID {
					found = true
					break
				}
			}
			if !found {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// browserProxy converts authenticated proxies to the local unauthenticated
// relay Chromium requires. Unauthenticated proxies can be assigned directly
// to a request BrowserContext.
func browserProxy(rawProxyURL string) (string, *proxy.Relay, error) {
	if rawProxyURL == "" {
		return "", nil, nil
	}
	parsed, err := url.Parse(rawProxyURL)
	if err != nil {
		return "", nil, fmt.Errorf("parse request proxy URL: %w", err)
	}
	if parsed.Host == "" {
		return "", nil, fmt.Errorf("request proxy URL has no host")
	}
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", nil, fmt.Errorf("unsupported request proxy scheme: %s", parsed.Scheme)
	}
	if parsed.User == nil {
		return rawProxyURL, nil, nil
	}
	relay, err := proxy.StartRelay(rawProxyURL)
	if err != nil {
		return "", nil, fmt.Errorf("start authenticated request proxy relay: %w", err)
	}
	return "socks5://127.0.0.1:" + addrPort(relay.Addr()), relay, nil
}

// scrapeStandalonePage applies the same browser fetch options as the pooled
// Rod path to a page owned by an isolated or external browser context.
func (s *Scraper) scrapeStandalonePage(ctx context.Context, browser *rod.Browser, req *models.ScrapeRequest, engineName string, maximumBodyBytes int64) (*ScrapeResult, error) {
	page, err := browser.Context(ctx).Page(proto.TargetCreateTarget{})
	if err != nil {
		return nil, models.NewScrapeError(models.ErrCodeBrowserCrash, "failed to create request page", err)
	}
	defer func() { _ = page.Close() }()
	p := page.Context(ctx)

	if req.Stealth {
		if _, err := p.EvalOnNewDocument(stealth.JS); err != nil {
			return nil, categorizeError(err, "failed to install stealth script")
		}
	}

	extraHeaders := browserHeaders(req)
	if len(extraHeaders) > 0 {
		restoreHeaders, err := setBrowserHeaders(p, extraHeaders)
		if err != nil {
			return nil, categorizeError(err, "failed to set request headers")
		}
		defer restoreHeaders()
	}
	for _, cookie := range req.Cookies {
		domain := cookie.Domain
		if domain == "" {
			if parsed, parseErr := url.Parse(req.URL); parseErr == nil {
				domain = parsed.Hostname()
			}
		}
		path := cookie.Path
		if path == "" {
			path = "/"
		}
		if _, err := (proto.NetworkSetCookie{
			Name: cookie.Name, Value: cookie.Value, Domain: domain, Path: path,
		}).Call(p); err != nil {
			return nil, categorizeError(err, "failed to set request cookie")
		}
	}

	interception, err := setupPageInterception(p, s.scraperCfg.BlockedResourceTypes, req.BlockAds, maximumBodyBytes)
	if err != nil {
		return nil, categorizeError(err, "failed to install browser response bounds")
	}
	defer interception.stop()
	if networkIdleRequested(req) {
		removeTracker, err := installNetworkTracker(p)
		if err != nil {
			return nil, categorizeError(err, "failed to install network idle tracker")
		}
		defer func() { _ = removeTracker() }()
	}

	if err := p.Navigate(req.URL); err != nil {
		if terminalErr := interception.err(); terminalErr != nil {
			return nil, terminalErr
		}
		return nil, categorizeError(err, "navigation to target URL failed")
	}
	if terminalErr := interception.err(); terminalErr != nil {
		return nil, terminalErr
	}
	if err := waitForDocument(p, networkIdleRequested(req)); err != nil {
		if terminalErr := interception.err(); terminalErr != nil {
			return nil, terminalErr
		}
		return nil, categorizeError(err, "document did not become ready")
	}
	if terminalErr := interception.err(); terminalErr != nil {
		return nil, terminalErr
	}

	statusCode := evalIntOrZero(p, `() => {
		try {
			const entries = performance.getEntriesByType("navigation");
			return entries.length > 0 ? (entries[0].responseStatus || 0) : 0;
		} catch(e) { return 0; }
	}`)
	if req.RemoveOverlays {
		removeOverlays(p)
	}
	if len(req.Actions) > 0 {
		if err := executeActions(ctx, page, req.Actions); err != nil {
			return nil, err
		}
		if err := waitForPostActionStability(p, networkIdleRequested(req)); err != nil {
			if terminalErr := interception.err(); terminalErr != nil {
				return nil, terminalErr
			}
			return nil, categorizeError(err, "document did not stabilize after actions")
		}
		if terminalErr := interception.err(); terminalErr != nil {
			return nil, terminalErr
		}
	}

	rawHTML, err := extractBoundedHTML(p, maximumBodyBytes)
	if err != nil {
		if errors.Is(err, engine.ErrResponseBodyTooLarge) {
			return nil, err
		}
		return nil, categorizeError(err, "failed to extract page HTML")
	}
	finalURL := evalStringOrEmpty(p, `() => window.location.href`)
	if finalURL == "" {
		finalURL = req.URL
	}
	return &ScrapeResult{
		RawHTML:     rawHTML,
		Title:       evalStringOrEmpty(p, `() => document.title`),
		StatusCode:  statusCode,
		FinalURL:    finalURL,
		EngineUsed:  engineName,
		FetchMethod: "browser",
		ContentType: evalStringOrEmpty(p, `() => document.contentType`),
	}, nil
}

func browserHeaders(req *models.ScrapeRequest) map[string]string {
	headers := make(map[string]string, len(req.Headers)+1)
	if _, hasReferer := req.Headers["Referer"]; !hasReferer {
		if parsed, err := url.Parse(req.URL); err == nil && parsed.Scheme == "https" {
			// Real Google traffic sends only the origin as the referer (Google's
			// referrer policy strips the /search?q=... path), so the origin is
			// both more authentic and safe. The full search URL was an anti-bot
			// tell that also trips Chromium 151 into net::ERR_BLOCKED_BY_CLIENT,
			// failing every browser navigation.
			headers["Referer"] = "https://www.google.com/"
		}
	}
	for key, value := range req.Headers {
		headers[key] = value
	}
	return headers
}

func setBrowserHeaders(page *rod.Page, headers map[string]string) (func(), error) {
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values := make([]string, 0, len(keys)*2)
	for _, key := range keys {
		values = append(values, key, headers[key])
	}
	return page.SetExtraHeaders(values)
}

func connectCDP(ctx context.Context, rawURL string) (*rod.Browser, func(), error) {
	websocketURL, err := resolveCDPURL(ctx, rawURL)
	if err != nil {
		return nil, func() {}, err
	}
	websocket := &cdp.WebSocket{}
	if err := websocket.Connect(ctx, websocketURL, nil); err != nil {
		return nil, func() {}, err
	}
	client := cdp.New().Start(websocket)
	browser := rod.New().Client(client).Context(ctx)
	if err := browser.Connect(); err != nil {
		_ = websocket.Close()
		return nil, func() {}, err
	}
	return browser, func() { _ = websocket.Close() }, nil
}

func resolveCDPURL(ctx context.Context, rawURL string) (string, error) {
	trimmed := strings.TrimSpace(rawURL)
	if strings.HasPrefix(trimmed, "ws://") || strings.HasPrefix(trimmed, "wss://") {
		return trimmed, nil
	}
	if strings.HasPrefix(trimmed, ":") {
		trimmed = "127.0.0.1" + trimmed
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "http://" + trimmed
	}
	endpoint, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("parse CDP URL: %w", err)
	}
	endpoint.Path = "/json/version"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", fmt.Errorf("build CDP discovery request: %w", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("discover CDP websocket: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("discover CDP websocket: HTTP %d", response.StatusCode)
	}
	var version struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(response.Body).Decode(&version); err != nil {
		return "", fmt.Errorf("decode CDP discovery response: %w", err)
	}
	discovered, err := url.Parse(version.WebSocketDebuggerURL)
	if err != nil || discovered.Host == "" {
		return "", fmt.Errorf("CDP discovery returned invalid websocket URL")
	}
	discovered.Host = endpoint.Host
	return discovered.String(), nil
}
