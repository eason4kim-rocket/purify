package cache

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/use-agent/purify/models"
)

func TestSetAndGetUseDeepIndependentCopies(t *testing.T) {
	cache := New(4)
	defer cache.Close()
	response := populatedResponse("original")
	cache.Set("key", response)

	// Mutating the caller-owned value after Set must not affect cache storage.
	response.Content = "mutated source"
	response.Links.Internal[0].Href = "https://mutated.invalid/internal"
	response.Links.External[0].Text = "mutated external"
	response.Images[0].Src = "https://mutated.invalid/image.png"
	response.Quality.Status = models.QualityStatusUnusable
	response.Quality.Warnings[0] = models.QualityReasonEmptyContent
	response.Quality.FetchAttempts[0].Engine = "mutated source"
	response.Error.Message = "mutated error"

	first, hit := cache.Get("key", 1_000)
	if !hit {
		t.Fatal("Get() missed stored response")
	}
	assertPopulatedResponse(t, first, "original")

	// Every Get must return another independent copy, including every nested
	// slice and pointer currently present on ScrapeResponse.
	first.Content = "mutated hit"
	first.Links.Internal[0].Href = "https://mutated.invalid/hit"
	first.Links.External[0].Text = "mutated hit"
	first.Images[0].Alt = "mutated hit"
	first.Quality.Status = models.QualityStatusUnusable
	first.Quality.Warnings[0] = models.QualityReasonChallengePage
	first.Quality.FetchAttempts[0].Outcome = models.FetchAttemptFailed
	first.Error.Code = "MUTATED"
	second, hit := cache.Get("key", 1_000)
	if !hit {
		t.Fatal("second Get() missed stored response")
	}
	assertPopulatedResponse(t, second, "original")
	if first == second || &first.Links.Internal[0] == &second.Links.Internal[0] ||
		&first.Links.External[0] == &second.Links.External[0] ||
		&first.Images[0] == &second.Images[0] || first.Quality == second.Quality ||
		&first.Quality.Warnings[0] == &second.Quality.Warnings[0] ||
		&first.Quality.FetchAttempts[0] == &second.Quality.FetchAttempts[0] ||
		first.Error == second.Error {
		t.Fatal("Get returned shared nested storage")
	}
	if response.Quality.Warnings[0] != models.QualityReasonEmptyContent ||
		response.Quality.FetchAttempts[0].Engine != "mutated source" {
		t.Fatal("mutating a cache hit changed the caller-owned response")
	}
}

func TestClonePreservesNilAndEmptySlices(t *testing.T) {
	cache := New(2)
	defer cache.Close()
	response := populatedResponse("shape")
	response.Links.Internal = nil
	response.Links.External = make([]models.Link, 0)
	response.Images = make([]models.Image, 0)
	response.Quality.Warnings = nil
	response.Quality.FetchAttempts = make([]models.FetchAttempt, 0)
	cache.Set("shape", response)
	got, hit := cache.Get("shape", 1_000)
	if !hit {
		t.Fatal("Get() missed")
	}
	if got.Links.Internal != nil {
		t.Fatal("nil internal links changed shape")
	}
	if got.Links.External == nil || got.Images == nil || got.Quality.FetchAttempts == nil {
		t.Fatal("non-nil empty slices changed shape")
	}
	if got.Quality.Warnings != nil {
		t.Fatal("nil quality warnings changed shape")
	}

	withoutPointers := populatedResponse("nil pointers")
	withoutPointers.Quality = nil
	withoutPointers.Error = nil
	cache.Set("nil-pointers", withoutPointers)
	got, hit = cache.Get("nil-pointers", 1_000)
	if !hit {
		t.Fatal("Get() missed response with nil pointers")
	}
	if got.Quality != nil || got.Error != nil {
		t.Fatal("nil response pointers changed shape")
	}
}

func TestCapacityEvictsOldestAndUpdateDoesNotEvict(t *testing.T) {
	current := time.Date(2026, time.August, 9, 0, 0, 0, 0, time.UTC)
	cache := newCache(Options{MaxEntries: 2, TTL: -1, CleanupInterval: time.Hour}, func() time.Time { return current })
	defer cache.Close()
	cache.Set("a", populatedResponse("a1"))
	current = current.Add(time.Millisecond)
	cache.Set("b", populatedResponse("b"))
	current = current.Add(time.Millisecond)
	cache.Set("a", populatedResponse("a2")) // refresh existing key
	if cache.Len() != 2 {
		t.Fatalf("Len() = %d after update, want 2", cache.Len())
	}
	current = current.Add(time.Millisecond)
	cache.Set("c", populatedResponse("c"))
	if cache.Len() != 2 {
		t.Fatalf("Len() = %d at capacity, want 2", cache.Len())
	}
	if _, hit := cache.Get("b", 10_000); hit {
		t.Fatal("oldest entry b was not evicted")
	}
	if got, hit := cache.Get("a", 10_000); !hit || got.Content != "a2" {
		t.Fatalf("updated a = %#v, hit=%v", got, hit)
	}
	if _, hit := cache.Get("c", 10_000); !hit {
		t.Fatal("new entry c was not retained")
	}
}

func TestNonPositiveCapacityAndNilResponseDoNotStore(t *testing.T) {
	cache := New(0)
	defer cache.Close()
	cache.Set("key", populatedResponse("value"))
	cache.Set("nil", nil)
	if cache.Len() != 0 {
		t.Fatalf("Len() = %d, want disabled cache", cache.Len())
	}
	if _, hit := cache.Get("key", 1_000); hit {
		t.Fatal("disabled cache returned hit")
	}
}

func TestCapacityHandlesEmptyLegacyKey(t *testing.T) {
	cache := NewWithOptions(Options{MaxEntries: 1, TTL: -1})
	defer cache.Close()
	cache.Set("", populatedResponse("empty"))
	cache.Set("replacement", populatedResponse("replacement"))
	if cache.Len() != 1 {
		t.Fatalf("Len() = %d, want strict capacity 1", cache.Len())
	}
	if _, hit := cache.Get("", 1_000); hit {
		t.Fatal("empty legacy key was not evicted")
	}
	if _, hit := cache.Get("replacement", 1_000); !hit {
		t.Fatal("replacement entry missing")
	}
}

func TestGetHonorsRequestedAndRetentionTTL(t *testing.T) {
	current := time.Date(2026, time.August, 9, 0, 0, 0, 0, time.UTC)
	cache := newCache(Options{MaxEntries: 4, TTL: 100 * time.Millisecond, CleanupInterval: time.Hour}, func() time.Time { return current })
	defer cache.Close()

	cache.Set("requested", populatedResponse("requested"))
	current = current.Add(49 * time.Millisecond)
	if _, hit := cache.Get("requested", 50); !hit {
		t.Fatal("entry expired before requested max age")
	}
	current = current.Add(time.Millisecond)
	if _, hit := cache.Get("requested", 50); hit {
		t.Fatal("entry survived requested max age boundary")
	}

	cache.Set("retention", populatedResponse("retention"))
	current = current.Add(99 * time.Millisecond)
	if _, hit := cache.Get("retention", 1_000); !hit {
		t.Fatal("entry expired before retention TTL")
	}
	current = current.Add(time.Millisecond)
	if _, hit := cache.Get("retention", 1_000); hit {
		t.Fatal("entry survived retention TTL boundary")
	}
	if _, hit := cache.Get("missing", 0); hit {
		t.Fatal("non-positive max age performed lookup")
	}
}

func TestSetPurgesExpiredEntriesBeforeCapacityEviction(t *testing.T) {
	current := time.Date(2026, time.August, 9, 0, 0, 0, 0, time.UTC)
	cache := newCache(Options{MaxEntries: 2, TTL: 10 * time.Millisecond, CleanupInterval: time.Hour}, func() time.Time { return current })
	defer cache.Close()
	cache.Set("old-a", populatedResponse("a"))
	cache.Set("old-b", populatedResponse("b"))
	current = current.Add(10 * time.Millisecond)
	cache.Set("fresh", populatedResponse("fresh"))
	if cache.Len() != 1 {
		t.Fatalf("Len() = %d, want only fresh entry", cache.Len())
	}
	if _, hit := cache.Get("fresh", 1_000); !hit {
		t.Fatal("fresh entry missing")
	}
}

func TestBackgroundCleanupAndCloseLifecycle(t *testing.T) {
	cache := NewWithOptions(Options{MaxEntries: 2, TTL: 2 * time.Millisecond, CleanupInterval: time.Millisecond})
	cache.Set("old", populatedResponse("old"))
	deadline := time.Now().Add(time.Second)
	for cache.Len() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if cache.Len() != 0 {
		cache.Close()
		t.Fatal("background cleanup did not remove expired entry")
	}

	const closers = 16
	var wait sync.WaitGroup
	wait.Add(closers)
	for range closers {
		go func() {
			defer wait.Done()
			cache.Close()
		}()
	}
	wait.Wait()
	select {
	case <-cache.done:
	default:
		t.Fatal("Close returned before cleanup goroutine stopped")
	}
	cache.Set("after-close", populatedResponse("value"))
	if cache.Len() != 0 {
		t.Fatal("Set retained data after Close")
	}
	if _, hit := cache.Get("after-close", 1_000); hit {
		t.Fatal("Get returned hit after Close")
	}
}

func TestConcurrentGetSetReturnsIsolatedResponses(t *testing.T) {
	cache := NewWithOptions(Options{MaxEntries: 512, TTL: -1})
	defer cache.Close()
	cache.Set("shared", populatedResponse("shared"))

	const workers = 32
	const iterations = 100
	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(workers)
	for worker := 0; worker < workers; worker++ {
		worker := worker
		go func() {
			defer wait.Done()
			<-start
			for iteration := 0; iteration < iterations; iteration++ {
				key := fmt.Sprintf("worker-%d-%d", worker, iteration%8)
				cache.Set(key, populatedResponse(key))
				if got, hit := cache.Get(key, 60_000); hit {
					mutateResponse(got, fmt.Sprintf("mutated-%d", worker))
				}
				if got, hit := cache.Get("shared", 60_000); hit {
					mutateResponse(got, fmt.Sprintf("shared-mutation-%d", worker))
				}
			}
		}()
	}
	close(start)
	wait.Wait()
	shared, hit := cache.Get("shared", 60_000)
	if !hit {
		t.Fatal("shared entry was unexpectedly evicted")
	}
	assertPopulatedResponse(t, shared, "shared")
}

func TestScrapeResponseCloneClassificationStaysCurrent(t *testing.T) {
	assertFields(t, reflect.TypeOf(models.ScrapeResponse{}), []string{
		"Success", "StatusCode", "FinalURL", "Content", "Metadata", "Links",
		"Images", "OGMetadata", "Tokens", "Timing", "Quality", "CacheStatus", "EngineUsed", "Error",
	})
	assertFields(t, reflect.TypeOf(models.LinksResult{}), []string{"Internal", "External"})
	assertFields(t, reflect.TypeOf(models.QualityInfo{}), []string{
		"Score", "Status", "Warnings", "ExtractModeUsed", "FetchAttempts",
	})
	assertFields(t, reflect.TypeOf(models.FetchAttempt{}), []string{
		"Engine", "Outcome", "Reason", "DurationMs",
	})
	assertFields(t, reflect.TypeOf(models.ErrorDetail{}), []string{"Code", "Message"})
}

func populatedResponse(label string) *models.ScrapeResponse {
	return &models.ScrapeResponse{
		Success:    true,
		StatusCode: 200,
		FinalURL:   "https://example.com/final",
		Content:    label,
		Metadata: models.Metadata{
			Title:       "title",
			Description: "description",
			SiteName:    "site",
			Author:      "author",
			Language:    "en",
			SourceURL:   "https://example.com/source",
			FetchMethod: "http",
		},
		Links: models.LinksResult{
			Internal: []models.Link{{Href: "https://example.com/internal", Text: "internal"}},
			External: []models.Link{{Href: "https://other.example/external", Text: "external"}},
		},
		Images:     []models.Image{{Src: "https://example.com/image.png", Alt: "image"}},
		OGMetadata: models.OGMetadata{Title: "og", Description: "og description", Image: "og.png", Type: "article"},
		Tokens:     models.TokenInfo{OriginalEstimate: 100, CleanedEstimate: 25, SavingsPercent: 75},
		Timing:     models.TimingInfo{TotalMs: 30, NavigationMs: 20, CleaningMs: 10},
		Quality: &models.QualityInfo{
			Score:           0.95,
			Status:          models.QualityStatusGood,
			Warnings:        []models.QualityReason{models.QualityReasonLowContentQuality},
			ExtractModeUsed: "readability",
			FetchAttempts: []models.FetchAttempt{{
				Engine:     "http",
				Outcome:    models.FetchAttemptSelected,
				DurationMs: 12,
			}},
		},
		CacheStatus: "miss",
		EngineUsed:  "http",
		Error:       &models.ErrorDetail{Code: "TEST", Message: "original error"},
	}
}

func assertPopulatedResponse(t *testing.T, response *models.ScrapeResponse, label string) {
	t.Helper()
	if response == nil || response.Content != label ||
		response.Links.Internal[0].Href != "https://example.com/internal" ||
		response.Links.External[0].Text != "external" ||
		response.Images[0].Src != "https://example.com/image.png" ||
		response.Quality == nil || response.Quality.Score != 0.95 ||
		response.Quality.Status != models.QualityStatusGood ||
		response.Quality.Warnings[0] != models.QualityReasonLowContentQuality ||
		response.Quality.FetchAttempts[0].Engine != "http" ||
		response.Quality.FetchAttempts[0].Outcome != models.FetchAttemptSelected ||
		response.Error.Code != "TEST" || response.Error.Message != "original error" {
		t.Fatalf("response was not isolated: %#v", response)
	}
}

func mutateResponse(response *models.ScrapeResponse, value string) {
	response.Content = value
	response.Links.Internal[0].Href = value
	response.Links.External[0].Text = value
	response.Images[0].Alt = value
	response.Quality.Warnings[0] = models.QualityReasonChallengePage
	response.Quality.FetchAttempts[0].Engine = value
	response.Error.Message = value
}
