package scraper

import (
	"context"
	"encoding/json"
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
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/proxy"
)

const browserContextCleanupTimeout = 250 * time.Millisecond

// doScrapeInRequestContext uses an isolated Chromium BrowserContext whenever a
// request needs its own proxy or cookie jar. The shared Chrome process remains
// pooled, while context disposal guarantees request state cannot leak into a
// later page.
func (s *Scraper) doScrapeInRequestContext(ctx context.Context, req *models.ScrapeRequest) (*ScrapeResult, error) {
	browser, cleanup, err := newIsolatedBrowserContext(ctx, s.browser, req.ProxyURL)
	if err != nil {
		return nil, models.NewScrapeError(models.ErrCodeNavigation, "failed to create isolated browser context", err)
	}
	defer cleanup()
	return s.scrapeStandalonePage(ctx, browser, req, rodEngineName(req.Stealth))
}

func newIsolatedBrowserContext(ctx context.Context, base *rod.Browser, rawProxyURL string) (*rod.Browser, func(), error) {
	proxyServer, relay, err := browserProxy(rawProxyURL)
	if err != nil {
		return nil, func() {}, err
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
		return nil, func() {}, err
	}

	isolated := *base.Context(ctx)
	isolated.BrowserContextID = created.BrowserContextID
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			dispose := func(cleanupCtx context.Context) {
				_ = (proto.TargetDisposeBrowserContext{BrowserContextID: created.BrowserContextID}).Call(base.Context(cleanupCtx))
				if relay != nil {
					_ = relay.Close()
				}
			}
			if ctx.Err() != nil {
				// The request budget is already exhausted. Do not extend response
				// latency; dispose on the still-live shared CDP connection instead.
				go func() {
					cleanupCtx, cancel := context.WithTimeout(context.Background(), browserContextCleanupTimeout)
					defer cancel()
					dispose(cleanupCtx)
				}()
				return
			}
			cleanupCtx, cancel := context.WithTimeout(ctx, browserContextCleanupTimeout)
			defer cancel()
			dispose(cleanupCtx)
		})
	}
	return &isolated, cleanup, nil
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
func (s *Scraper) scrapeStandalonePage(ctx context.Context, browser *rod.Browser, req *models.ScrapeRequest, engineName string) (*ScrapeResult, error) {
	s.activePages.Add(1)
	defer s.activePages.Add(-1)

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

	router := setupHijack(p, s.scraperCfg.BlockedResourceTypes, req.BlockAds)
	if router != nil {
		defer func() { _ = router.Stop() }()
	}
	if networkIdleRequested(req) {
		removeTracker, err := installNetworkTracker(p)
		if err != nil {
			return nil, categorizeError(err, "failed to install network idle tracker")
		}
		defer func() { _ = removeTracker() }()
	}

	if err := p.Navigate(req.URL); err != nil {
		return nil, categorizeError(err, "navigation to target URL failed")
	}
	if err := waitForDocument(p, networkIdleRequested(req)); err != nil {
		return nil, categorizeError(err, "document did not become ready")
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
			return nil, categorizeError(err, "document did not stabilize after actions")
		}
	}

	rawHTML, err := p.HTML()
	if err != nil {
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
			headers["Referer"] = "https://www.google.com/search?q=" + url.QueryEscape(parsed.Hostname())
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
