package search

import (
	"sync"
	"testing"
	"time"
	"unsafe"
)

func TestBaselineCacheDeepCopiesSetAndGet(t *testing.T) {
	clock := &searchTestClock{now: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)}
	cache := newBaselineCache(time.Minute, 4, 1<<20, clock.Now)
	key := testBaselineKey(1)
	score := 0.5
	publishedAt := clock.Now()
	wantPublishedAt := publishedAt
	source := []baselineResult{{
		score:       &score,
		title:       "title",
		url:         "https://example.com/",
		snippet:     "snippet",
		publishedAt: &publishedAt,
	}}
	cache.set(key, source)
	*source[0].score = 0.9
	*source[0].publishedAt = publishedAt.Add(time.Hour)
	source[0].title = "caller mutation"

	first, ok := cache.get(key)
	if !ok || len(first) != 1 || first[0].score == nil || *first[0].score != 0.5 || first[0].title != "title" || !first[0].publishedAt.Equal(wantPublishedAt) {
		t.Fatalf("first get = (%#v, %v)", first, ok)
	}
	*first[0].score = 0.1
	first[0].publishedAt = nil
	first[0].title = "reader mutation"

	second, ok := cache.get(key)
	if !ok || len(second) != 1 || second[0].score == nil || *second[0].score != 0.5 || second[0].publishedAt == nil || second[0].title != "title" {
		t.Fatalf("second get = (%#v, %v)", second, ok)
	}
}

func TestBaselineCacheDetachesRetainedStringBackings(t *testing.T) {
	clock := &searchTestClock{now: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)}
	cache := newBaselineCache(time.Minute, 4, 1<<20, clock.Now)
	key := testBaselineKey(1)
	title := suffixOfLargeBacking("title")
	url := suffixOfLargeBacking("https://example.com/")
	snippet := suffixOfLargeBacking("snippet")
	cache.set(key, []baselineResult{{title: title, url: url, snippet: snippet}})

	cache.mu.Lock()
	stored := cache.entries[key].results[0]
	cache.mu.Unlock()
	for _, check := range []struct {
		name   string
		stored string
		source string
	}{
		{name: "title", stored: stored.title, source: title},
		{name: "url", stored: stored.url, source: url},
		{name: "snippet", stored: stored.snippet, source: snippet},
	} {
		if check.stored != check.source || unsafe.StringData(check.stored) == unsafe.StringData(check.source) {
			t.Fatalf("cached %s retained source backing", check.name)
		}
	}

	read, ok := cache.get(key)
	if !ok || len(read) != 1 {
		t.Fatalf("get() = (%#v, %v)", read, ok)
	}
	if unsafe.StringData(read[0].title) == unsafe.StringData(stored.title) ||
		unsafe.StringData(read[0].url) == unsafe.StringData(stored.url) ||
		unsafe.StringData(read[0].snippet) == unsafe.StringData(stored.snippet) {
		t.Fatal("cache get retained internal string backing")
	}
}

func TestBaselineCacheExpiresAtTTLBoundary(t *testing.T) {
	clock := &searchTestClock{now: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)}
	cache := newBaselineCache(time.Minute, 4, 1<<20, clock.Now)
	key := testBaselineKey(1)
	cache.set(key, []baselineResult{})
	clock.Advance(time.Minute - time.Nanosecond)
	if _, ok := cache.get(key); !ok {
		t.Fatal("cache entry expired before TTL")
	}
	clock.Advance(time.Nanosecond)
	if _, ok := cache.get(key); ok {
		t.Fatal("cache entry survived at TTL boundary")
	}
}

func TestBaselineCacheUsesLRUEntryBound(t *testing.T) {
	clock := &searchTestClock{now: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)}
	cache := newBaselineCache(-1, 2, 1<<20, clock.Now)
	first := testBaselineKey(1)
	second := testBaselineKey(2)
	third := testBaselineKey(3)
	cache.set(first, []baselineResult{{title: "first"}})
	clock.Advance(time.Nanosecond)
	cache.set(second, []baselineResult{{title: "second"}})
	if _, ok := cache.get(first); !ok {
		t.Fatal("first entry was unexpectedly absent")
	}
	clock.Advance(time.Nanosecond)
	cache.set(third, []baselineResult{{title: "third"}})
	if _, ok := cache.get(second); ok {
		t.Fatal("least-recently-used entry was not evicted")
	}
	if _, ok := cache.get(first); !ok {
		t.Fatal("recently read entry was evicted")
	}
	if _, ok := cache.get(third); !ok {
		t.Fatal("new entry was not retained")
	}
}

func TestBaselineCacheUsesByteBoundAndRejectsOversizedEntry(t *testing.T) {
	results := []baselineResult{{title: "title", url: "https://example.com/", snippet: "snippet"}}
	entryBytes, ok := baselineResultsSize(results, 1<<20)
	if !ok {
		t.Fatal("test entry unexpectedly exceeded generous bound")
	}

	clock := &searchTestClock{now: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)}
	cache := newBaselineCache(-1, 10, entryBytes*2-1, clock.Now)
	first := testBaselineKey(1)
	second := testBaselineKey(2)
	cache.set(first, results)
	cache.set(second, results)
	if _, ok := cache.get(first); ok {
		t.Fatal("old entry was retained past byte bound")
	}
	if _, ok := cache.get(second); !ok {
		t.Fatal("new entry was not retained under byte bound")
	}
	assertBaselineCacheBounds(t, cache)

	oversized := newBaselineCache(-1, 10, entryBytes-1, clock.Now)
	oversized.set(first, results)
	if _, ok := oversized.get(first); ok {
		t.Fatal("oversized entry was cached")
	}
	assertBaselineCacheBounds(t, oversized)
}

func TestBaselineCacheReplacementKeepsAccountingExact(t *testing.T) {
	clock := &searchTestClock{now: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)}
	cache := newBaselineCache(-1, 2, 1<<20, clock.Now)
	key := testBaselineKey(1)
	cache.set(key, []baselineResult{{title: "short"}})
	cache.set(key, []baselineResult{{title: stringsOfLength(1 << 10)}})
	cache.mu.Lock()
	entries := len(cache.entries)
	bytes := cache.bytes
	entry := cache.entries[key]
	cache.mu.Unlock()
	if entries != 1 || entry == nil || bytes != entry.byteCount {
		t.Fatalf("replacement accounting entries=%d bytes=%d entry=%#v", entries, bytes, entry)
	}
	assertBaselineCacheBounds(t, cache)
}

func TestBaselineCacheConcurrentAccessStaysBounded(t *testing.T) {
	cache := newBaselineCache(time.Minute, 32, 64<<10, time.Now)
	var group sync.WaitGroup
	for worker := range 16 {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			for iteration := range 250 {
				key := testBaselineKey(byte((worker*17 + iteration) % 64))
				cache.set(key, []baselineResult{{
					title:   stringsOfLength((iteration % 32) + 1),
					url:     "https://example.com/",
					snippet: "copy",
				}})
				results, ok := cache.get(key)
				if ok && len(results) != 1 {
					t.Errorf("get(%x) returned %d results", key[0], len(results))
					return
				}
			}
		}(worker)
	}
	group.Wait()
	assertBaselineCacheBounds(t, cache)
}

func assertBaselineCacheBounds(t *testing.T, cache *baselineCache) {
	t.Helper()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.entries) > cache.maxEntries {
		t.Fatalf("entries = %d, max = %d", len(cache.entries), cache.maxEntries)
	}
	if cache.bytes < 0 || cache.bytes > cache.maxBytes {
		t.Fatalf("bytes = %d, max = %d", cache.bytes, cache.maxBytes)
	}
	total := 0
	for _, entry := range cache.entries {
		total += entry.byteCount
	}
	if total != cache.bytes {
		t.Fatalf("accounted bytes = %d, entries total = %d", cache.bytes, total)
	}
}

func testBaselineKey(value byte) [32]byte {
	var key [32]byte
	key[0] = value
	return key
}

func stringsOfLength(length int) string {
	return string(make([]byte, length))
}
