package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/use-agent/purify/models"
)

const (
	DefaultTTL             = time.Hour
	DefaultCleanupInterval = 5 * time.Minute
	requestKeyVersion      = "purify-scrape-cache/v3"
)

// Options configures cache retention and its cleanup lifecycle. TTL is the
// maximum time an entry is retained; callers may request a shorter age in Get.
// A negative TTL disables retention expiry, while zero selects DefaultTTL.
type Options struct {
	MaxEntries      int
	TTL             time.Duration
	CleanupInterval time.Duration
}

// entry owns an immutable response snapshot.
type entry struct {
	response  *models.ScrapeResponse
	createdAt time.Time
}

// Cache is a bounded in-memory cache for immutable scrape response snapshots.
// It is safe for concurrent use. Close must be called to stop its cleanup
// goroutine when the cache's process lifetime ends before program exit.
type Cache struct {
	mu              sync.RWMutex
	store           map[string]*entry
	maxEntries      int
	ttl             time.Duration
	cleanupInterval time.Duration
	now             func() time.Time
	stop            chan struct{}
	done            chan struct{}
	closeOnce       sync.Once
	closed          bool
}

// New creates a cache with the historical one-hour retention and five-minute
// cleanup interval. The existing constructor remains source-compatible.
func New(maxEntries int) *Cache {
	return NewWithOptions(Options{MaxEntries: maxEntries})
}

// NewWithOptions creates a cache with explicit capacity and lifecycle options.
func NewWithOptions(options Options) *Cache {
	return newCache(options, time.Now)
}

func newCache(options Options, now func() time.Time) *Cache {
	ttl := options.TTL
	if ttl == 0 {
		ttl = DefaultTTL
	}
	cleanupInterval := options.CleanupInterval
	if cleanupInterval <= 0 {
		cleanupInterval = DefaultCleanupInterval
	}
	if now == nil {
		now = time.Now
	}
	c := &Cache{
		store:           make(map[string]*entry),
		maxEntries:      options.MaxEntries,
		ttl:             ttl,
		cleanupInterval: cleanupInterval,
		now:             now,
		stop:            make(chan struct{}),
		done:            make(chan struct{}),
	}
	go c.cleanupLoop()
	return c
}

// Key is the legacy cache key covering only URL, output format, and extraction
// mode. It remains for source compatibility; new call sites should use
// KeyForRequest so every output-affecting request option participates.
func Key(url, outputFormat, extractMode string) string {
	h := sha256.New()
	h.Write([]byte(url))
	h.Write([]byte("|"))
	h.Write([]byte(outputFormat))
	h.Write([]byte("|"))
	h.Write([]byte(extractMode))
	return hex.EncodeToString(h.Sum(nil))
}

type canonicalHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// canonicalRequest deliberately excludes MaxAge because it controls cache
// policy, not scrape output. OnlyMainContent is normalized into ExtractMode by
// ScrapeRequest.Defaults before this value is assembled.
type canonicalRequest struct {
	Version            string            `json:"version"`
	URL                string            `json:"url"`
	WaitForNetworkIdle bool              `json:"wait_for_network_idle"`
	Timeout            int               `json:"timeout"`
	Stealth            bool              `json:"stealth"`
	ProxyURL           string            `json:"proxy_url"`
	OutputFormat       string            `json:"output_format"`
	ExtractMode        string            `json:"extract_mode"`
	CSSSelector        string            `json:"css_selector"`
	Headers            []canonicalHeader `json:"headers,omitempty"`
	Cookies            []models.Cookie   `json:"cookies,omitempty"`
	Actions            []models.Action   `json:"actions,omitempty"`
	IncludeTags        []string          `json:"include_tags,omitempty"`
	ExcludeTags        []string          `json:"exclude_tags,omitempty"`
	RemoveOverlays     bool              `json:"remove_overlays"`
	BlockAds           bool              `json:"block_ads"`
	CDPURL             string            `json:"cdp_url"`
	MaximumBodyBytes   int64             `json:"maximum_body_bytes"`
}

// KeyForRequest returns a stable SHA-256 key for every ScrapeRequest option
// that can affect output. Header maps are sorted on a private copy. Cookies,
// browser actions, and include/exclude selectors retain caller order because
// duplicate cookies and structural CSS selectors can be order-sensitive.
// Semantic defaults are applied to a shallow request copy, so key generation
// never mutates req.
func KeyForRequest(req *models.ScrapeRequest) string {
	var normalized models.ScrapeRequest
	if req != nil {
		normalized = *req
	}
	normalized.Defaults()

	headers := make([]canonicalHeader, 0, len(normalized.Headers))
	for name, value := range normalized.Headers {
		headers = append(headers, canonicalHeader{Name: name, Value: value})
	}
	sort.Slice(headers, func(i, j int) bool {
		if headers[i].Name == headers[j].Name {
			return headers[i].Value < headers[j].Value
		}
		return headers[i].Name < headers[j].Name
	})
	if len(headers) == 0 {
		headers = nil
	}

	cookies := cloneSlice(normalized.Cookies)
	includeTags := cloneSlice(normalized.IncludeTags)
	excludeTags := cloneSlice(normalized.ExcludeTags)
	actions := cloneSlice(normalized.Actions)

	waitForNetworkIdle := false
	if normalized.WaitForNetworkIdle != nil {
		waitForNetworkIdle = *normalized.WaitForNetworkIdle
	}
	canonical := canonicalRequest{
		Version:            requestKeyVersion,
		URL:                normalized.URL,
		WaitForNetworkIdle: waitForNetworkIdle,
		Timeout:            normalized.Timeout,
		Stealth:            normalized.Stealth,
		ProxyURL:           normalized.ProxyURL,
		OutputFormat:       normalized.OutputFormat,
		ExtractMode:        normalized.ExtractMode,
		CSSSelector:        normalized.CSSSelector,
		Headers:            headers,
		Cookies:            cookies,
		Actions:            actions,
		IncludeTags:        includeTags,
		ExcludeTags:        excludeTags,
		RemoveOverlays:     normalized.RemoveOverlays,
		BlockAds:           normalized.BlockAds,
		CDPURL:             normalized.CDPURL,
		MaximumBodyBytes:   normalized.MaximumBodyBytes,
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		// canonicalRequest contains only JSON-safe concrete fields. Retain a
		// deterministic fallback rather than exposing an impossible error path
		// in every handler.
		encoded = []byte(requestKeyVersion + "|marshal-error")
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

// RequestKey is an alias for KeyForRequest for migration call sites that read
// more naturally with the request noun first.
func RequestKey(req *models.ScrapeRequest) string {
	return KeyForRequest(req)
}

// Get retrieves an independent response copy if key exists and is younger than
// both maxAgeMs and the cache retention TTL. maxAgeMs <= 0 disables lookup.
func (c *Cache) Get(key string, maxAgeMs int) (*models.ScrapeResponse, bool) {
	if c == nil || maxAgeMs <= 0 {
		return nil, false
	}
	requestedTTL := durationFromMilliseconds(maxAgeMs)

	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return nil, false
	}
	e, ok := c.store[key]
	if !ok {
		c.mu.RUnlock()
		return nil, false
	}
	expired := isExpired(e.createdAt, c.now(), shorterPositiveDuration(requestedTTL, c.ttl))
	if !expired {
		response := cloneResponse(e.response)
		c.mu.RUnlock()
		return response, true
	}
	c.mu.RUnlock()

	// Remove only the entry observed above; a concurrent Set may already have
	// replaced it with a fresh snapshot under the same key.
	c.mu.Lock()
	if current, exists := c.store[key]; exists && current == e {
		delete(c.store, key)
	}
	c.mu.Unlock()
	return nil, false
}

// Set stores an immutable deep copy of resp. At capacity, the oldest entry is
// evicted deterministically. A non-positive capacity or nil response disables
// storage without affecting callers.
func (c *Cache) Set(key string, resp *models.ScrapeResponse) {
	if c == nil || resp == nil {
		return
	}
	response := cloneResponse(resp)
	now := c.now()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.maxEntries <= 0 {
		return
	}
	c.removeExpiredLocked(now)
	if _, exists := c.store[key]; !exists && len(c.store) >= c.maxEntries {
		c.evictOldestLocked()
	}
	c.store[key] = &entry{response: response, createdAt: now}
}

// Len returns the number of physically retained entries. It is primarily
// useful for lifecycle and capacity observability.
func (c *Cache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.store)
}

// Close stops the background cleanup goroutine, clears retained responses, and
// makes subsequent Get/Set calls harmless misses/no-ops. It is idempotent and
// safe to call concurrently.
func (c *Cache) Close() {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.store = nil
		c.mu.Unlock()
		close(c.stop)
		<-c.done
	})
}

func (c *Cache) cleanupLoop() {
	ticker := time.NewTicker(c.cleanupInterval)
	defer ticker.Stop()
	defer close(c.done)
	for {
		select {
		case now := <-ticker.C:
			c.mu.Lock()
			if !c.closed {
				c.removeExpiredLocked(now)
			}
			c.mu.Unlock()
		case <-c.stop:
			return
		}
	}
}

func (c *Cache) removeExpiredLocked(now time.Time) {
	if c.ttl <= 0 {
		return
	}
	for key, candidate := range c.store {
		if isExpired(candidate.createdAt, now, c.ttl) {
			delete(c.store, key)
		}
	}
}

func (c *Cache) evictOldestLocked() {
	var oldestKey string
	var oldestTime time.Time
	found := false
	for key, candidate := range c.store {
		if !found || candidate.createdAt.Before(oldestTime) ||
			(candidate.createdAt.Equal(oldestTime) && key < oldestKey) {
			oldestKey = key
			oldestTime = candidate.createdAt
			found = true
		}
	}
	if found {
		delete(c.store, oldestKey)
	}
}

func isExpired(createdAt, now time.Time, ttl time.Duration) bool {
	if ttl <= 0 {
		return false
	}
	age := now.Sub(createdAt)
	return age >= ttl
}

func shorterPositiveDuration(left, right time.Duration) time.Duration {
	if left <= 0 {
		return right
	}
	if right <= 0 || left < right {
		return left
	}
	return right
}

func durationFromMilliseconds(milliseconds int) time.Duration {
	const maxDuration = time.Duration(1<<63 - 1)
	maxMilliseconds := int64(maxDuration / time.Millisecond)
	if int64(milliseconds) > maxMilliseconds {
		return maxDuration
	}
	return time.Duration(milliseconds) * time.Millisecond
}

func cloneResponse(source *models.ScrapeResponse) *models.ScrapeResponse {
	if source == nil {
		return nil
	}
	cloned := *source
	cloned.Links.Internal = cloneSlice(source.Links.Internal)
	cloned.Links.External = cloneSlice(source.Links.External)
	cloned.Images = cloneSlice(source.Images)
	if source.Quality != nil {
		qualityCopy := *source.Quality
		qualityCopy.Warnings = cloneSlice(source.Quality.Warnings)
		qualityCopy.FetchAttempts = cloneSlice(source.Quality.FetchAttempts)
		cloned.Quality = &qualityCopy
	}
	if source.Error != nil {
		errorCopy := *source.Error
		cloned.Error = &errorCopy
	}
	return &cloned
}

func cloneSlice[T any](source []T) []T {
	if source == nil {
		return nil
	}
	cloned := make([]T, len(source))
	copy(cloned, source)
	return cloned
}
