package search

import (
	"container/list"
	"strings"
	"sync"
	"time"
)

const (
	baselineCacheTTL        = time.Minute
	baselineCacheMaxEntries = 256
	baselineCacheMaxBytes   = 16 << 20
	baselineCacheEntryCost  = 96
	baselineResultCost      = 72
)

// baselineResult is the complete cache value. It deliberately cannot retain
// request credentials, schemas, fetched artifacts, cleaned content, or
// enrichment output.
type baselineResult struct {
	providerRank int
	score        *float64
	title        string
	url          string
	snippet      string
	publishedAt  *time.Time
}

type baselineCacheEntry struct {
	key       [32]byte
	results   []baselineResult
	storedAt  time.Time
	byteCount int
	element   *list.Element
}

// baselineCache is a lazy-expiring LRU bounded by entry count and retained
// value bytes. It has no background goroutine and therefore needs no lifecycle
// hook in the production binary.
type baselineCache struct {
	mu         sync.Mutex
	entries    map[[32]byte]*baselineCacheEntry
	recency    list.List
	ttl        time.Duration
	maxEntries int
	maxBytes   int
	bytes      int
	now        func() time.Time
}

func newBaselineCache(ttl time.Duration, maxEntries, maxBytes int, now func() time.Time) *baselineCache {
	if now == nil {
		now = time.Now
	}
	return &baselineCache{
		entries:    make(map[[32]byte]*baselineCacheEntry),
		ttl:        ttl,
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
		now:        now,
	}
}

func (cache *baselineCache) get(key [32]byte) ([]baselineResult, bool) {
	if cache == nil {
		return nil, false
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()

	entry, ok := cache.entries[key]
	if !ok {
		return nil, false
	}
	if cache.expired(entry, cache.now()) {
		cache.remove(entry)
		return nil, false
	}
	cache.recency.MoveToFront(entry.element)
	return cloneBaselineResults(entry.results), true
}

func (cache *baselineCache) set(key [32]byte, results []baselineResult) {
	if cache == nil || cache.maxEntries <= 0 || cache.maxBytes <= 0 {
		return
	}
	byteCount, ok := baselineResultsSize(results, cache.maxBytes)
	if !ok {
		return
	}
	cloned := cloneBaselineResults(results)
	now := cache.now()

	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.removeExpired(now)
	if existing, found := cache.entries[key]; found {
		cache.remove(existing)
	}
	for len(cache.entries) >= cache.maxEntries || cache.bytes > cache.maxBytes-byteCount {
		oldest := cache.recency.Back()
		if oldest == nil {
			break
		}
		cache.remove(oldest.Value.(*baselineCacheEntry))
	}
	if len(cache.entries) >= cache.maxEntries || cache.bytes > cache.maxBytes-byteCount {
		return
	}
	entry := &baselineCacheEntry{
		key:       key,
		results:   cloned,
		storedAt:  now,
		byteCount: byteCount,
	}
	entry.element = cache.recency.PushFront(entry)
	cache.entries[key] = entry
	cache.bytes += byteCount
}

func (cache *baselineCache) removeExpired(now time.Time) {
	if cache.ttl <= 0 {
		return
	}
	for element := cache.recency.Back(); element != nil; {
		previous := element.Prev()
		entry := element.Value.(*baselineCacheEntry)
		if cache.expired(entry, now) {
			cache.remove(entry)
		}
		element = previous
	}
}

func (cache *baselineCache) expired(entry *baselineCacheEntry, now time.Time) bool {
	return cache.ttl > 0 && now.Sub(entry.storedAt) >= cache.ttl
}

func (cache *baselineCache) remove(entry *baselineCacheEntry) {
	if entry == nil {
		return
	}
	current, ok := cache.entries[entry.key]
	if !ok || current != entry {
		return
	}
	delete(cache.entries, entry.key)
	cache.recency.Remove(entry.element)
	cache.bytes -= entry.byteCount
	if cache.bytes < 0 {
		cache.bytes = 0
	}
}

func baselineResultsSize(results []baselineResult, maximum int) (int, bool) {
	if maximum < baselineCacheEntryCost {
		return 0, false
	}
	total := baselineCacheEntryCost
	for _, result := range results {
		if total > maximum-baselineResultCost {
			return 0, false
		}
		total += baselineResultCost
		for _, value := range [...]string{result.title, result.url, result.snippet} {
			if len(value) > maximum-total {
				return 0, false
			}
			total += len(value)
		}
	}
	return total, true
}

func cloneBaselineResults(source []baselineResult) []baselineResult {
	if source == nil {
		return nil
	}
	cloned := make([]baselineResult, len(source))
	for index := range source {
		cloned[index] = cloneBaselineResult(source[index])
	}
	return cloned
}

func cloneBaselineResult(source baselineResult) baselineResult {
	cloned := source
	cloned.title = strings.Clone(source.title)
	cloned.url = strings.Clone(source.url)
	cloned.snippet = strings.Clone(source.snippet)
	if source.score != nil {
		value := *source.score
		cloned.score = &value
	}
	if source.publishedAt != nil {
		value := *source.publishedAt
		cloned.publishedAt = &value
	}
	return cloned
}
