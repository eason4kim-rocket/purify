package models

import "slices"

// ScrapeRequest is the payload for POST /api/v1/scrape.
type ScrapeRequest struct {
	// URL is the target page to scrape. Required.
	URL string `json:"url" binding:"required,url"`

	// WaitForNetworkIdle instructs the scraper to wait until the page
	// has no more than 2 in-flight network requests for 500ms.
	// Useful for SPAs that load data asynchronously.
	// Default: true.
	WaitForNetworkIdle *bool `json:"wait_for_network_idle,omitempty"`

	// Timeout is the maximum duration in seconds for the entire
	// scrape operation (navigation + rendering + extraction).
	// Default: 30. Max: 120.
	Timeout int `json:"timeout,omitempty" binding:"omitempty,min=1,max=120"`

	// Stealth enables anti-bot-detection evasions (e.g. navigator.webdriver masking).
	// Default: false.
	Stealth bool `json:"stealth,omitempty"`

	// ProxyURL overrides the default proxy for this request.
	// Format: "http://user:pass@host:port" or "socks5://host:port".
	ProxyURL string `json:"proxy_url,omitempty" binding:"omitempty,url"`

	// OutputFormat controls the response body format.
	// Allowed: "markdown" (default), "html", "text".
	OutputFormat string `json:"output_format,omitempty" binding:"omitempty,oneof=markdown html text markdown_citations"`

	// ExtractMode controls the content extraction strategy.
	// "readability" (default): two-stage pipeline, readability extracts main body → format conversion.
	// "raw": skip readability, pass full rendered HTML directly to format conversion.
	ExtractMode string `json:"extract_mode,omitempty" binding:"omitempty,oneof=readability raw pruning auto"`

	// CSSSelector is an optional CSS selector to filter HTML before cleaning.
	// When set, only the matched elements' outer HTML is passed to the pipeline.
	CSSSelector string `json:"css_selector,omitempty"`

	// Headers sets custom HTTP headers for the request.
	// Example: {"Authorization": "Bearer xxx", "Accept-Language": "en-US"}
	Headers map[string]string `json:"headers,omitempty"`

	// Cookies sets cookies on the browser before navigation.
	Cookies []Cookie `json:"cookies,omitempty"`

	// Actions is an ordered list of browser interactions to perform after
	// the page loads and before extracting content. Max 50 actions.
	Actions []Action `json:"actions,omitempty" binding:"omitempty,max=50,dive"`

	// IncludeTags is a list of CSS selectors. When non-empty, only elements
	// matching these selectors are kept in the output.
	IncludeTags []string `json:"include_tags,omitempty"`

	// ExcludeTags is a list of CSS selectors. Matching elements are removed
	// from the DOM before content extraction.
	ExcludeTags []string `json:"exclude_tags,omitempty"`

	// OnlyMainContent is a Firecrawl-compatible alias. When explicitly set
	// to false, it sets ExtractMode to "raw".
	OnlyMainContent *bool `json:"only_main_content,omitempty"`

	// RemoveOverlays removes fixed/sticky overlays (cookie banners, popups)
	// by injecting JS after page load.
	RemoveOverlays bool `json:"remove_overlays,omitempty"`

	// BlockAds blocks requests to known ad/tracking domains.
	BlockAds bool `json:"block_ads,omitempty"`

	// CDPURL connects to a user-provided Chrome DevTools Protocol endpoint
	// instead of using the shared browser pool.
	CDPURL string `json:"cdp_url,omitempty"`

	// MaxAge is the cache max age in milliseconds. If > 0, the response
	// may be served from cache if a cached entry exists within this age.
	// Default: 0 (no caching).
	MaxAge int `json:"max_age,omitempty" binding:"omitempty,min=0"`
}

// ScrapeOptions is the URL-independent option set shared by single-page,
// batch, and crawl scraping. ScrapeRequest intentionally keeps these fields
// directly addressable so existing Go callers can continue to use composite
// literals such as ScrapeRequest{Timeout: 30} while batch and crawl keep their
// existing nested "options" JSON object.
//
// Keep this field set, its types, and its tags in sync with every ScrapeRequest
// field except URL. The contract is guarded by reflection tests.
type ScrapeOptions struct {
	WaitForNetworkIdle *bool             `json:"wait_for_network_idle,omitempty"`
	Timeout            int               `json:"timeout,omitempty" binding:"omitempty,min=1,max=120"`
	Stealth            bool              `json:"stealth,omitempty"`
	ProxyURL           string            `json:"proxy_url,omitempty" binding:"omitempty,url"`
	OutputFormat       string            `json:"output_format,omitempty" binding:"omitempty,oneof=markdown html text markdown_citations"`
	ExtractMode        string            `json:"extract_mode,omitempty" binding:"omitempty,oneof=readability raw pruning auto"`
	CSSSelector        string            `json:"css_selector,omitempty"`
	Headers            map[string]string `json:"headers,omitempty"`
	Cookies            []Cookie          `json:"cookies,omitempty"`
	Actions            []Action          `json:"actions,omitempty" binding:"omitempty,max=50,dive"`
	IncludeTags        []string          `json:"include_tags,omitempty"`
	ExcludeTags        []string          `json:"exclude_tags,omitempty"`
	OnlyMainContent    *bool             `json:"only_main_content,omitempty"`
	RemoveOverlays     bool              `json:"remove_overlays,omitempty"`
	BlockAds           bool              `json:"block_ads,omitempty"`
	CDPURL             string            `json:"cdp_url,omitempty"`
	MaxAge             int               `json:"max_age,omitempty" binding:"omitempty,min=0"`
}

// ScrapeOptionsFromRequest returns a detached copy of every URL-independent
// option in request. URL is deliberately not part of ScrapeOptions.
func ScrapeOptionsFromRequest(request *ScrapeRequest) ScrapeOptions {
	if request == nil {
		return ScrapeOptions{}
	}
	return CloneScrapeOptions(ScrapeOptions{
		WaitForNetworkIdle: request.WaitForNetworkIdle,
		Timeout:            request.Timeout,
		Stealth:            request.Stealth,
		ProxyURL:           request.ProxyURL,
		OutputFormat:       request.OutputFormat,
		ExtractMode:        request.ExtractMode,
		CSSSelector:        request.CSSSelector,
		Headers:            request.Headers,
		Cookies:            request.Cookies,
		Actions:            request.Actions,
		IncludeTags:        request.IncludeTags,
		ExcludeTags:        request.ExcludeTags,
		OnlyMainContent:    request.OnlyMainContent,
		RemoveOverlays:     request.RemoveOverlays,
		BlockAds:           request.BlockAds,
		CDPURL:             request.CDPURL,
		MaxAge:             request.MaxAge,
	})
}

// ApplyScrapeOptions replaces every URL-independent option on request with a
// detached copy of options. It leaves request.URL unchanged.
func ApplyScrapeOptions(request *ScrapeRequest, options ScrapeOptions) {
	if request == nil {
		return
	}
	options = CloneScrapeOptions(options)
	request.WaitForNetworkIdle = options.WaitForNetworkIdle
	request.Timeout = options.Timeout
	request.Stealth = options.Stealth
	request.ProxyURL = options.ProxyURL
	request.OutputFormat = options.OutputFormat
	request.ExtractMode = options.ExtractMode
	request.CSSSelector = options.CSSSelector
	request.Headers = options.Headers
	request.Cookies = options.Cookies
	request.Actions = options.Actions
	request.IncludeTags = options.IncludeTags
	request.ExcludeTags = options.ExcludeTags
	request.OnlyMainContent = options.OnlyMainContent
	request.RemoveOverlays = options.RemoveOverlays
	request.BlockAds = options.BlockAds
	request.CDPURL = options.CDPURL
	request.MaxAge = options.MaxAge
}

// CloneScrapeOptions returns a deep copy suitable for crossing asynchronous
// job and transport ownership boundaries. Ordered collections retain order.
func CloneScrapeOptions(options ScrapeOptions) ScrapeOptions {
	if options.WaitForNetworkIdle != nil {
		value := *options.WaitForNetworkIdle
		options.WaitForNetworkIdle = &value
	}
	if options.OnlyMainContent != nil {
		value := *options.OnlyMainContent
		options.OnlyMainContent = &value
	}
	if options.Headers != nil {
		headers := make(map[string]string, len(options.Headers))
		for key, value := range options.Headers {
			headers[key] = value
		}
		options.Headers = headers
	}
	options.Cookies = slices.Clone(options.Cookies)
	options.Actions = slices.Clone(options.Actions)
	options.IncludeTags = slices.Clone(options.IncludeTags)
	options.ExcludeTags = slices.Clone(options.ExcludeTags)
	return options
}

// Action represents a single browser interaction in the actions pipeline.
type Action struct {
	// Type is the action kind: "wait", "click", "scroll", "execute_js", "scrape".
	Type string `json:"type" binding:"required,oneof=wait click scroll execute_js scrape"`

	// Selector is a CSS selector (used by "wait" and "click").
	Selector string `json:"selector,omitempty"`

	// Milliseconds is the wait duration (used by "wait" when Selector is empty).
	Milliseconds int `json:"milliseconds,omitempty"`

	// Direction is the scroll direction: "up" or "down" (used by "scroll").
	Direction string `json:"direction,omitempty" binding:"omitempty,oneof=up down"`

	// Amount is the number of viewport-heights to scroll (used by "scroll").
	Amount int `json:"amount,omitempty"`

	// Code is the JavaScript to execute (used by "execute_js").
	Code string `json:"code,omitempty"`
}

// Cookie represents a browser cookie to set before scraping.
type Cookie struct {
	Name   string `json:"name" binding:"required"`
	Value  string `json:"value" binding:"required"`
	Domain string `json:"domain,omitempty"`
	Path   string `json:"path,omitempty"`
}

// Defaults applies default values to unset fields.
func (r *ScrapeRequest) Defaults() {
	if r.WaitForNetworkIdle == nil {
		t := true
		r.WaitForNetworkIdle = &t
	}
	if r.Timeout == 0 {
		r.Timeout = 30
	}
	if r.OutputFormat == "" {
		r.OutputFormat = "markdown"
	}
	if r.ExtractMode == "" {
		r.ExtractMode = "readability"
	}
	// OnlyMainContent is a Firecrawl-compatible alias: when explicitly
	// set to false, override ExtractMode to "raw".
	if r.OnlyMainContent != nil && !*r.OnlyMainContent {
		r.ExtractMode = "raw"
	}
}
