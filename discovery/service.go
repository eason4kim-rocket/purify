// Package discovery provides bounded, deterministic URL discovery for the Map
// API. Sitemap, robots.txt, and homepage sources share one injected HTTP
// transport and one total deadline.
package discovery

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/use-agent/purify/cleaner"
)

const (
	defaultTimeout         = 15 * time.Second
	defaultMaxSitemapDepth = 3
	defaultMaxSitemapFiles = 32
	defaultMaxSitemapBytes = 4 << 20
	defaultMaxRobotsBytes  = 512 << 10
	defaultMaxHomeBytes    = 4 << 20
	defaultMaxURLs         = 10_000
	defaultMaxWarnings     = 128
	defaultMaxRedirects    = 10
	defaultUserAgent       = "Purify-Map/1.0"

	hardMaxTimeout            = 2 * time.Minute
	hardMaxSitemapDepth       = 10
	hardMaxSitemapFiles       = 512
	hardMaxBodyBytes    int64 = 64 << 20
	hardMaxURLs               = 100_000
	hardMaxWarnings           = 1_024
	hardMaxRedirects          = 20
)

var (
	ErrInvalidConfig    = errors.New("discovery: invalid configuration")
	ErrInvalidRootURL   = errors.New("discovery: invalid root URL")
	ErrAllSourcesFailed = errors.New("discovery: all sources failed")
)

// httpDoer is an internal test seam. Production construction always owns the
// safe transport so callers cannot bypass DNS pinning or redirect policy.
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Config bounds all network and parsing work. Zero values select defaults.
type Config struct {
	Timeout              time.Duration
	MaxSitemapDepth      int
	MaxSitemapFiles      int
	MaxSitemapBytes      int64
	MaxRobotsBytes       int64
	MaxHomeBytes         int64
	MaxURLs              int
	MaxWarnings          int
	MaxRedirects         int
	UserAgent            string
	AllowPrivateNetworks bool
	Resolver             IPResolver
	DialContext          DialContextFunc
}

// Warning reports one failed or truncated discovery source without discarding
// URLs recovered from other sources.
type Warning struct {
	Source  string `json:"source"`
	URL     string `json:"url,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// SourceStats makes partial behavior auditable without exposing transport
// internals.
type SourceStats struct {
	SitemapFiles   int  `json:"sitemap_files"`
	SitemapURLs    int  `json:"sitemap_urls"`
	RobotsSitemaps int  `json:"robots_sitemaps"`
	HomeLinks      int  `json:"home_links"`
	Truncated      bool `json:"truncated"`
}

// Result is stable: URLs are canonical, unique, and sorted; warnings have a
// deterministic sort order.
type Result struct {
	URLs     []string    `json:"urls"`
	Warnings []Warning   `json:"warnings,omitempty"`
	Sources  SourceStats `json:"sources"`
}

// Service performs bounded URL discovery.
type Service struct {
	client httpDoer
	cfg    Config
}

// NewService validates and defaults discovery bounds.
func NewService(cfg Config) (*Service, error) {
	return newServiceWithClient(nil, cfg)
}

func newServiceWithClient(client httpDoer, cfg Config) (*Service, error) {
	if cfg.Timeout < 0 || cfg.MaxSitemapDepth < 0 || cfg.MaxSitemapFiles < 0 ||
		cfg.MaxSitemapBytes < 0 || cfg.MaxRobotsBytes < 0 || cfg.MaxHomeBytes < 0 || cfg.MaxURLs < 0 ||
		cfg.MaxWarnings < 0 || cfg.MaxRedirects < 0 {
		return nil, fmt.Errorf("%w: limits cannot be negative", ErrInvalidConfig)
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.MaxSitemapDepth == 0 {
		cfg.MaxSitemapDepth = defaultMaxSitemapDepth
	}
	if cfg.MaxSitemapFiles == 0 {
		cfg.MaxSitemapFiles = defaultMaxSitemapFiles
	}
	if cfg.MaxSitemapBytes == 0 {
		cfg.MaxSitemapBytes = defaultMaxSitemapBytes
	}
	if cfg.MaxRobotsBytes == 0 {
		cfg.MaxRobotsBytes = defaultMaxRobotsBytes
	}
	if cfg.MaxHomeBytes == 0 {
		cfg.MaxHomeBytes = defaultMaxHomeBytes
	}
	if cfg.MaxURLs == 0 {
		cfg.MaxURLs = defaultMaxURLs
	}
	if cfg.MaxWarnings == 0 {
		cfg.MaxWarnings = defaultMaxWarnings
	}
	if cfg.MaxRedirects == 0 {
		cfg.MaxRedirects = defaultMaxRedirects
	}
	if strings.TrimSpace(cfg.UserAgent) == "" {
		cfg.UserAgent = defaultUserAgent
	}
	if cfg.Timeout > hardMaxTimeout || cfg.MaxSitemapDepth > hardMaxSitemapDepth ||
		cfg.MaxSitemapFiles > hardMaxSitemapFiles || cfg.MaxSitemapBytes > hardMaxBodyBytes ||
		cfg.MaxRobotsBytes > hardMaxBodyBytes || cfg.MaxHomeBytes > hardMaxBodyBytes ||
		cfg.MaxURLs > hardMaxURLs || cfg.MaxWarnings > hardMaxWarnings || cfg.MaxRedirects > hardMaxRedirects {
		return nil, fmt.Errorf("%w: configured limit exceeds the safety ceiling", ErrInvalidConfig)
	}
	if client == nil {
		client = newSafeHTTPClient(cfg)
	}
	return &Service{client: client, cfg: cfg}, nil
}

type sitemapDocument struct {
	XMLName   xml.Name       `xml:""`
	Sitemaps  []sitemapEntry `xml:"sitemap"`
	URLs      []sitemapEntry `xml:"url"`
	Truncated bool           `xml:"-"`
}

type sitemapEntry struct {
	Loc string `xml:"loc"`
}

type sitemapWork struct {
	URL   string
	Depth int
}

// Discover queries the conventional sitemap, robots.txt directives, and the
// homepage. It returns partial results with warnings when at least one source
// succeeds. If every source fails, Result still carries diagnostics alongside
// ErrAllSourcesFailed.
func (s *Service) Discover(ctx context.Context, rawRoot string) (*Result, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is nil", ErrInvalidConfig)
	}
	rootCanonical, root, err := normalizeURL(rawRoot, nil, s.cfg.AllowPrivateNetworks)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRootURL, err)
	}

	requestCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	state := discoveryState{
		service:   s,
		ctx:       requestCtx,
		root:      root,
		collector: newURLCollector(root, s.cfg.MaxURLs, s.cfg.MaxWarnings, s.cfg.AllowPrivateNetworks),
		seenMaps:  make(map[string]struct{}),
		warnings:  newWarningCollector(s.cfg.MaxWarnings),
	}
	state.collector.addCanonical(rootCanonical, "root")

	origin := &url.URL{Scheme: root.Scheme, Host: root.Host, Path: "/"}
	defaultSitemap, _, _ := normalizeURL("/sitemap.xml", origin, s.cfg.AllowPrivateNetworks)
	robotsURL, _, _ := normalizeURL("/robots.txt", origin, s.cfg.AllowPrivateNetworks)

	type robotsOutcome struct {
		sitemaps []sitemapWork
		success  bool
	}
	robotsResults := make(chan robotsOutcome, 1)
	homeResults := make(chan bool, 1)
	go func() {
		robotsSitemaps, robotsOK := state.fetchRobots(robotsURL)
		robotsResults <- robotsOutcome{sitemaps: robotsSitemaps, success: robotsOK}
	}()
	go func() {
		homeResults <- state.fetchHomepage(rootCanonical)
	}()

	succeeded := 0
	// Sitemap work has a single deterministic scheduler. The default sitemap
	// tree has fixed priority over robots-discovered trees, while robots.txt
	// and the homepage are fetched concurrently so a slow sitemap cannot starve
	// those independent sources.
	if state.processSitemaps([]sitemapWork{{URL: defaultSitemap, Depth: 0}}) {
		succeeded++
	}
	robots := <-robotsResults
	if robots.success {
		succeeded++
	}
	if len(robots.sitemaps) > 0 && state.processSitemaps(robots.sitemaps) {
		succeeded++
	}
	if <-homeResults {
		succeeded++
	}

	warnings := state.warnings.list()
	warnings = append(warnings, state.collector.warningList()...)
	stats := state.snapshotStats()
	result := &Result{
		URLs:     state.collector.sorted(),
		Warnings: limitWarnings(warnings, s.cfg.MaxWarnings),
		Sources:  stats,
	}
	if err := requestCtx.Err(); err != nil {
		return result, err
	}
	if succeeded == 0 {
		return result, ErrAllSourcesFailed
	}
	return result, nil
}

type discoveryState struct {
	mu        sync.Mutex
	service   *Service
	ctx       context.Context
	root      *url.URL
	collector *urlCollector
	seenMaps  map[string]struct{}
	warnings  *warningCollector
	stats     SourceStats
}

func (state *discoveryState) processSitemaps(initial []sitemapWork) bool {
	queue := append([]sitemapWork(nil), initial...)
	anySuccess := false
	for len(queue) > 0 {
		if state.ctx.Err() != nil {
			state.warn("sitemap", "", "deadline", state.ctx.Err().Error())
			break
		}
		work := queue[0]
		queue = queue[1:]
		canonical, parsed, err := normalizeURL(work.URL, nil, state.service.cfg.AllowPrivateNetworks)
		if err != nil {
			state.warn("sitemap", work.URL, "invalid_url", err.Error())
			continue
		}
		state.mu.Lock()
		_, seen := state.seenMaps[canonical]
		if seen {
			state.mu.Unlock()
			continue
		}
		if len(state.seenMaps) >= state.service.cfg.MaxSitemapFiles {
			state.stats.Truncated = true
			state.mu.Unlock()
			state.warn("sitemap", canonical, "file_limit", fmt.Sprintf("sitemap file limit %d reached", state.service.cfg.MaxSitemapFiles))
			break
		}
		state.seenMaps[canonical] = struct{}{}
		state.stats.SitemapFiles++
		state.mu.Unlock()

		body, finalURL, err := state.service.fetch(state.ctx, canonical, state.service.cfg.MaxSitemapBytes, sitemapContentType)
		if err != nil {
			state.warn("sitemap", canonical, warningCode(err), err.Error())
			continue
		}
		document, err := parseSitemap(body, state.service.cfg.MaxSitemapFiles, state.service.cfg.MaxURLs)
		if err != nil {
			state.warn("sitemap", canonical, "invalid_xml", err.Error())
			continue
		}
		if document.Truncated {
			state.setTruncated()
			state.warn("sitemap", canonical, "entry_limit", "sitemap entries exceeded the configured in-memory limit")
		}
		base := parsed
		if finalURL != nil {
			base = finalURL
		}
		switch strings.ToLower(document.XMLName.Local) {
		case "sitemapindex":
			anySuccess = true
			if work.Depth >= state.service.cfg.MaxSitemapDepth {
				if len(document.Sitemaps) > 0 {
					state.setTruncated()
					state.warn("sitemap", canonical, "depth_limit", fmt.Sprintf("sitemap depth limit %d reached", state.service.cfg.MaxSitemapDepth))
				}
				continue
			}
			children := make([]sitemapWork, 0, len(document.Sitemaps))
			for _, entry := range document.Sitemaps {
				child, _, normalizeErr := normalizeURL(entry.Loc, base, state.service.cfg.AllowPrivateNetworks)
				if normalizeErr != nil {
					state.warn("sitemap", strings.TrimSpace(entry.Loc), "invalid_url", normalizeErr.Error())
					continue
				}
				children = append(children, sitemapWork{URL: child, Depth: work.Depth + 1})
			}
			sort.Slice(children, func(i, j int) bool { return children[i].URL < children[j].URL })
			queue = append(queue, children...)
		case "urlset":
			anySuccess = true
			for _, entry := range document.URLs {
				if state.collector.add(entry.Loc, base, "sitemap") {
					state.mu.Lock()
					state.stats.SitemapURLs++
					state.mu.Unlock()
				}
			}
		default:
			state.warn("sitemap", canonical, "invalid_xml", "XML root must be sitemapindex or urlset")
		}
	}
	return anySuccess
}

func (state *discoveryState) fetchRobots(rawURL string) ([]sitemapWork, bool) {
	body, finalURL, err := state.service.fetch(state.ctx, rawURL, state.service.cfg.MaxRobotsBytes, robotsContentType)
	if err != nil {
		state.warn("robots", rawURL, warningCode(err), err.Error())
		return nil, false
	}
	base, _ := url.Parse(rawURL)
	if finalURL != nil {
		base = finalURL
	}
	works := make([]sitemapWork, 0)
	seen := make(map[string]struct{})
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		colon := strings.IndexByte(line, ':')
		if colon < 0 || !strings.EqualFold(strings.TrimSpace(line[:colon]), "sitemap") {
			continue
		}
		value := strings.TrimSpace(line[colon+1:])
		canonical, _, normalizeErr := normalizeURL(value, base, state.service.cfg.AllowPrivateNetworks)
		if normalizeErr != nil {
			state.warn("robots", value, "invalid_url", normalizeErr.Error())
			continue
		}
		if _, duplicate := seen[canonical]; duplicate {
			continue
		}
		seen[canonical] = struct{}{}
		works = append(works, sitemapWork{URL: canonical, Depth: 0})
	}
	sort.Slice(works, func(i, j int) bool { return works[i].URL < works[j].URL })
	state.mu.Lock()
	state.stats.RobotsSitemaps += len(works)
	state.mu.Unlock()
	return works, true
}

func (state *discoveryState) fetchHomepage(rawURL string) bool {
	body, finalURL, err := state.service.fetch(state.ctx, rawURL, state.service.cfg.MaxHomeBytes, htmlContentType)
	if err != nil {
		state.warn("homepage", rawURL, warningCode(err), err.Error())
		return false
	}
	base := rawURL
	if finalURL != nil {
		base = finalURL.String()
	}
	links := cleaner.ExtractLinks(string(body), base)
	parsedBase, _ := url.Parse(base)
	for _, link := range links.Internal {
		if state.collector.add(link.Href, parsedBase, "homepage") {
			state.mu.Lock()
			state.stats.HomeLinks++
			state.mu.Unlock()
		}
	}
	for _, link := range links.External {
		if state.collector.add(link.Href, parsedBase, "homepage") {
			state.mu.Lock()
			state.stats.HomeLinks++
			state.mu.Unlock()
		}
	}
	return true
}

func (state *discoveryState) warn(source, rawURL, code, message string) {
	state.warnings.add(Warning{Source: source, URL: rawURL, Code: code, Message: message})
}

func (state *discoveryState) setTruncated() {
	state.mu.Lock()
	state.stats.Truncated = true
	state.mu.Unlock()
}

func (state *discoveryState) snapshotStats() SourceStats {
	state.mu.Lock()
	defer state.mu.Unlock()
	stats := state.stats
	stats.Truncated = stats.Truncated || state.collector.wasTruncated()
	return stats
}

type fetchError struct {
	code    string
	message string
}

func (e *fetchError) Error() string { return e.message }

func warningCode(err error) string {
	var fetchErr *fetchError
	if errors.As(err, &fetchErr) {
		return fetchErr.code
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "deadline"
	}
	return "fetch_failed"
}

type contentTypeValidator func(string) bool

func (s *Service) fetch(ctx context.Context, rawURL string, maximumBytes int64, validateContentType contentTypeValidator) ([]byte, *url.URL, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, &fetchError{code: "invalid_url", message: err.Error()}
	}
	request.Header.Set("Accept", "application/xml,text/xml,text/plain,text/html,application/gzip;q=0.9,*/*;q=0.1")
	request.Header.Set("User-Agent", s.cfg.UserAgent)
	response, err := s.client.Do(request)
	if err != nil {
		return nil, nil, err
	}
	if response == nil {
		return nil, nil, &fetchError{code: "fetch_failed", message: "HTTP client returned a nil response"}
	}
	if response.Body == nil {
		return nil, nil, &fetchError{code: "fetch_failed", message: "HTTP response body is nil"}
	}
	defer response.Body.Close()
	finalURL := responseURL(response, request)
	if finalURL != nil {
		_, normalizedFinal, normalizeErr := normalizeURL(finalURL.String(), nil, s.cfg.AllowPrivateNetworks)
		if normalizeErr != nil {
			return nil, finalURL, &fetchError{code: "redirect_policy", message: fmt.Sprintf("reject final URL: %v", normalizeErr)}
		}
		finalURL = normalizedFinal
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, finalURL, &fetchError{
			code:    "http_status",
			message: fmt.Sprintf("HTTP status %d", response.StatusCode),
		}
	}
	contentType := response.Header.Get("Content-Type")
	if validateContentType != nil && !validateContentType(contentType) {
		return nil, finalURL, &fetchError{
			code:    "content_type",
			message: fmt.Sprintf("unsupported Content-Type %q", contentType),
		}
	}
	body, tooLarge, err := readBoundedBody(response.Body, maximumBytes)
	if err != nil {
		return nil, finalURL, &fetchError{code: "read_failed", message: err.Error()}
	}
	if tooLarge {
		return nil, finalURL, &fetchError{
			code:    "body_limit",
			message: fmt.Sprintf("response exceeds %d-byte limit", maximumBytes),
		}
	}
	return body, finalURL, nil
}

func responseURL(response *http.Response, fallback *http.Request) *url.URL {
	if response != nil && response.Request != nil && response.Request.URL != nil {
		copy := *response.Request.URL
		return &copy
	}
	if fallback == nil || fallback.URL == nil {
		return nil
	}
	copy := *fallback.URL
	return &copy
}

func readBoundedBody(body io.Reader, maximumBytes int64) ([]byte, bool, error) {
	compressed := &io.LimitedReader{R: body, N: maximumBytes + 1}
	buffered := bufio.NewReader(compressed)
	reader := io.Reader(buffered)
	if signature, _ := buffered.Peek(2); len(signature) == 2 && signature[0] == 0x1f && signature[1] == 0x8b {
		gzipReader, err := gzip.NewReader(buffered)
		if err != nil {
			return nil, false, fmt.Errorf("open gzip response: %w", err)
		}
		defer gzipReader.Close()
		reader = gzipReader
	}
	decoded := &io.LimitedReader{R: reader, N: maximumBytes + 1}
	data, err := io.ReadAll(decoded)
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > maximumBytes || compressed.N == 0 {
		return nil, true, nil
	}
	return data, false, nil
}

func parseSitemap(body []byte, maximumSitemaps, maximumURLs int) (sitemapDocument, error) {
	var document sitemapDocument
	decoder := xml.NewDecoder(bytes.NewReader(body))
	decoder.Strict = true
	depth := 0
	rootClosed := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return sitemapDocument{}, fmt.Errorf("parse sitemap XML: %w", err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			if rootClosed {
				return sitemapDocument{}, errors.New("parse sitemap XML: multiple root elements")
			}
			if depth == 0 {
				document.XMLName = value.Name
				switch strings.ToLower(value.Name.Local) {
				case "sitemapindex", "urlset":
				default:
					return sitemapDocument{}, errors.New("parse sitemap XML: root must be sitemapindex or urlset")
				}
				depth = 1
				continue
			}
			rootKind := strings.ToLower(document.XMLName.Local)
			entryKind := strings.ToLower(value.Name.Local)
			if depth == 1 && (rootKind == "sitemapindex" && entryKind == "sitemap" || rootKind == "urlset" && entryKind == "url") {
				var entry sitemapEntry
				if err := decoder.DecodeElement(&entry, &value); err != nil {
					return sitemapDocument{}, fmt.Errorf("parse sitemap XML entry: %w", err)
				}
				if rootKind == "sitemapindex" {
					if len(document.Sitemaps) < maximumSitemaps {
						document.Sitemaps = append(document.Sitemaps, entry)
					} else {
						document.Truncated = true
					}
				} else if len(document.URLs) < maximumURLs {
					document.URLs = append(document.URLs, entry)
				} else {
					document.Truncated = true
				}
				continue
			}
			depth++
		case xml.EndElement:
			if depth == 0 {
				return sitemapDocument{}, errors.New("parse sitemap XML: unexpected closing element")
			}
			depth--
			if depth == 0 {
				rootClosed = true
			}
		case xml.CharData:
			if (depth == 0 || rootClosed) && strings.TrimSpace(string(value)) != "" {
				return sitemapDocument{}, errors.New("parse sitemap XML: non-whitespace data outside root element")
			}
		}
	}
	if document.XMLName.Local == "" || !rootClosed {
		return sitemapDocument{}, errors.New("parse sitemap XML: missing or unclosed root element")
	}
	return document, nil
}

func sitemapContentType(value string) bool {
	mediaType := normalizedMediaType(value)
	switch mediaType {
	case "", "application/xml", "text/xml", "text/plain", "application/gzip", "application/x-gzip", "application/octet-stream":
		return true
	default:
		return strings.HasSuffix(mediaType, "+xml")
	}
}

func robotsContentType(value string) bool {
	mediaType := normalizedMediaType(value)
	return mediaType == "" || mediaType == "text/plain" || mediaType == "text/robots" || mediaType == "application/octet-stream"
}

func htmlContentType(value string) bool {
	mediaType := normalizedMediaType(value)
	return mediaType == "" || mediaType == "text/html" || mediaType == "application/xhtml+xml"
}

func normalizedMediaType(value string) string {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return "!invalid"
	}
	return strings.ToLower(mediaType)
}

func stableWarnings(warnings []Warning) []Warning {
	cloned := append([]Warning(nil), warnings...)
	sort.SliceStable(cloned, func(i, j int) bool { return warningLess(cloned[i], cloned[j]) })
	return cloned
}

func warningLess(left, right Warning) bool {
	if left.Source != right.Source {
		return left.Source < right.Source
	}
	if left.URL != right.URL {
		return left.URL < right.URL
	}
	if left.Code != right.Code {
		return left.Code < right.Code
	}
	return left.Message < right.Message
}
