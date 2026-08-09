package middleware

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/use-agent/purify/config"
	"github.com/use-agent/purify/models"
	"golang.org/x/time/rate"
)

func TestRateLimitPreservesOneTokenMiddlewareContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/", RateLimit(config.RateLimitConfig{RequestsPerSecond: 0, Burst: 1}), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	first := httptest.NewRecorder()
	router.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/", nil))
	if first.Code != http.StatusNoContent {
		t.Fatalf("first status = %d, body=%s", first.Code, first.Body)
	}

	second := httptest.NewRecorder()
	router.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/", nil))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, body=%s", second.Code, second.Body)
	}
	var response models.ScrapeResponse
	if err := json.Unmarshal(second.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Success || response.Error == nil || response.Error.Code != models.ErrCodeRateLimited {
		t.Fatalf("rate-limit response = %#v", response)
	}
}

func TestLimiterSharesFixedAndWeightedChargesByAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	limiter := NewLimiter(config.RateLimitConfig{RequestsPerSecond: 0, Burst: 3})
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if key := c.GetHeader("X-Test-Key"); key != "" {
			c.Set("api_key", key)
		}
		c.Next()
	})
	router.GET("/fixed", limiter.MiddlewareFixed(1), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	router.GET("/weighted", func(c *gin.Context) {
		if !limiter.Allow(c, 2) {
			c.Status(http.StatusTooManyRequests)
			return
		}
		c.Status(http.StatusNoContent)
	})

	request := func(path, key string) int {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Test-Key", key)
		router.ServeHTTP(recorder, req)
		return recorder.Code
	}
	if got := request("/fixed", "alpha"); got != http.StatusNoContent {
		t.Fatalf("fixed alpha = %d", got)
	}
	if got := request("/weighted", "alpha"); got != http.StatusNoContent {
		t.Fatalf("weighted alpha = %d", got)
	}
	if got := request("/fixed", "alpha"); got != http.StatusTooManyRequests {
		t.Fatalf("exhausted alpha = %d", got)
	}
	if got := request("/weighted", "beta"); got != http.StatusNoContent {
		t.Fatalf("independent beta = %d", got)
	}
}

func TestLimiterRejectsNonPositiveCostAndHandlesConcurrentIdentities(t *testing.T) {
	gin.SetMode(gin.TestMode)
	limiter := NewLimiter(config.RateLimitConfig{RequestsPerSecond: 0, Burst: 1})
	contextFor := func(key string) *gin.Context {
		context, _ := gin.CreateTestContext(httptest.NewRecorder())
		context.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		context.Set("api_key", key)
		return context
	}
	if limiter.Allow(contextFor("invalid"), 0) || limiter.Allow(contextFor("invalid"), -1) {
		t.Fatal("non-positive token cost was accepted")
	}

	const identities = 64
	var wait sync.WaitGroup
	wait.Add(identities)
	for index := 0; index < identities; index++ {
		go func(index int) {
			defer wait.Done()
			if !limiter.Allow(contextFor(string(rune('a'+index))), 1) {
				t.Errorf("identity %d unexpectedly denied", index)
			}
		}(index)
	}
	wait.Wait()
}

func TestLimiterEvictsStaleEntriesDuringTraffic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	limiter := NewLimiter(config.RateLimitConfig{RequestsPerSecond: 1, Burst: 1})
	now := time.Now()
	limiter.mu.Lock()
	limiter.limiters["api_key:old"] = &limiterEntry{
		limiter:  rate.NewLimiter(1, 1),
		lastSeen: now.Add(-2 * time.Hour),
	}
	limiter.lastSweep = now.Add(-6 * time.Minute)
	limiter.mu.Unlock()

	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	context.Set("api_key", "new")
	if !limiter.Allow(context, 1) {
		t.Fatal("new identity was unexpectedly denied")
	}
	limiter.mu.Lock()
	_, oldExists := limiter.limiters["api_key:old"]
	_, newExists := limiter.limiters["api_key:new"]
	limiter.mu.Unlock()
	if oldExists || !newExists {
		t.Fatalf("entries after sweep: old=%t new=%t", oldExists, newExists)
	}
}

func TestLimiterIdentityCapacityExactBoundaryUsesSharedOverflow(t *testing.T) {
	limiter := NewLimiter(config.RateLimitConfig{RequestsPerSecond: 0, Burst: 1})
	now := time.Now()
	for index := 0; index < maxRateLimitIdentities; index++ {
		bucket := limiter.bucket(fmt.Sprintf("identity:%05d", index), now)
		if bucket == limiter.overflow {
			t.Fatalf("identity %d used overflow before exact capacity", index)
		}
	}
	limiter.mu.Lock()
	countAtLimit := len(limiter.limiters)
	limiter.mu.Unlock()
	if countAtLimit != maxRateLimitIdentities {
		t.Fatalf("identity count at N = %d, want %d", countAtLimit, maxRateLimitIdentities)
	}

	firstOverflow := limiter.bucket("identity:overflow-a", now)
	secondOverflow := limiter.bucket("identity:overflow-b", now)
	if firstOverflow != limiter.overflow || secondOverflow != limiter.overflow || firstOverflow != secondOverflow {
		t.Fatal("N+1 identities did not use the one shared overflow bucket")
	}
	limiter.mu.Lock()
	countAfterOverflow := len(limiter.limiters)
	_, retainedA := limiter.limiters["identity:overflow-a"]
	_, retainedB := limiter.limiters["identity:overflow-b"]
	limiter.mu.Unlock()
	if countAfterOverflow != maxRateLimitIdentities || retainedA || retainedB {
		t.Fatalf("overflow changed identity map: count=%d retained=%t/%t", countAfterOverflow, retainedA, retainedB)
	}
}

func TestLimiterConcurrentOverflowCannotResetRateLimitOrGrowMap(t *testing.T) {
	gin.SetMode(gin.TestMode)
	limiter := NewLimiter(config.RateLimitConfig{RequestsPerSecond: 0, Burst: 1})
	now := time.Now()
	for index := 0; index < maxRateLimitIdentities; index++ {
		limiter.bucket(fmt.Sprintf("identity:%05d", index), now)
	}

	const overflowAttempts = 128
	var allowed atomic.Int64
	var wait sync.WaitGroup
	wait.Add(overflowAttempts)
	for index := 0; index < overflowAttempts; index++ {
		go func(index int) {
			defer wait.Done()
			context, _ := gin.CreateTestContext(httptest.NewRecorder())
			context.Request = httptest.NewRequest(http.MethodGet, "/", nil)
			context.Set("api_key", fmt.Sprintf("overflow-key-%03d", index))
			if limiter.Allow(context, 1) {
				allowed.Add(1)
			}
		}(index)
	}
	wait.Wait()
	if got := allowed.Load(); got != 1 {
		t.Fatalf("shared overflow allowed %d requests, want exactly initial burst 1", got)
	}
	limiter.mu.Lock()
	count := len(limiter.limiters)
	limiter.mu.Unlock()
	if count != maxRateLimitIdentities {
		t.Fatalf("identity map grew to %d, want hard cap %d", count, maxRateLimitIdentities)
	}
}
