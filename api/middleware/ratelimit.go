package middleware

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/use-agent/purify/config"
	"github.com/use-agent/purify/models"
	"golang.org/x/time/rate"
)

type limiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

const maxRateLimitIdentities = 10_000

// Limiter owns the per-identity token buckets shared by all protected routes.
// A caller may combine fixed-cost middleware with direct weighted Allow calls
// without splitting one identity across independent buckets.
type Limiter struct {
	cfg config.RateLimitConfig

	mu        sync.Mutex
	limiters  map[string]*limiterEntry
	overflow  *rate.Limiter
	lastSweep time.Time
}

// RateLimit returns per-identity (API key or IP) token-bucket rate limiting
// middleware powered by golang.org/x/time/rate.
//
// It preserves the historical one-token middleware API. Routers that also
// have weighted routes should create one NewLimiter and share it instead.
func RateLimit(cfg config.RateLimitConfig) gin.HandlerFunc {
	return NewLimiter(cfg).MiddlewareFixed(1)
}

// NewLimiter creates a reusable per-identity limiter. Stale entries are
// evicted opportunistically during traffic, avoiding a permanent cleanup
// goroutine per router while retaining the one-hour identity lifetime.
func NewLimiter(cfg config.RateLimitConfig) *Limiter {
	if cfg.Burst < 0 {
		cfg.Burst = 0
	}
	now := time.Now()
	return &Limiter{
		cfg:       cfg,
		limiters:  make(map[string]*limiterEntry),
		overflow:  rate.NewLimiter(rate.Limit(cfg.RequestsPerSecond), cfg.Burst),
		lastSweep: now,
	}
}

// Allow atomically consumes tokens from the request identity's shared bucket.
// The API key installed by Auth takes precedence; unauthenticated deployments
// fall back to the client IP. A non-positive cost is rejected rather than
// becoming a rate-limit bypass.
func (limiter *Limiter) Allow(c *gin.Context, cost int) bool {
	if limiter == nil || c == nil || cost <= 0 {
		return false
	}
	identity := rateLimitIdentity(c)
	now := time.Now()
	bucket := limiter.bucket(identity, now)
	return bucket.AllowN(now, cost)
}

// MiddlewareFixed charges the same positive token cost for every request and
// retains the existing ScrapeResponse-shaped 429 used by legacy endpoints.
func (limiter *Limiter) MiddlewareFixed(cost int) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !limiter.Allow(c, cost) {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, models.ScrapeResponse{
				Success: false,
				Error: &models.ErrorDetail{
					Code:    models.ErrCodeRateLimited,
					Message: "rate limit exceeded, please slow down",
				},
			})
			return
		}
		c.Next()
	}
}

func (limiter *Limiter) bucket(identity string, now time.Time) *rate.Limiter {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if now.Sub(limiter.lastSweep) >= 5*time.Minute {
		cutoff := now.Add(-1 * time.Hour)
		for id, entry := range limiter.limiters {
			if entry.lastSeen.Before(cutoff) {
				delete(limiter.limiters, id)
			}
		}
		limiter.lastSweep = now
	}
	if entry, ok := limiter.limiters[identity]; ok {
		entry.lastSeen = now
		return entry.limiter
	}
	if len(limiter.limiters) >= maxRateLimitIdentities {
		// Never evict a live identity to admit an attacker-controlled new key or
		// IP: that would reset the evicted bucket. All overflow identities share
		// one conservative bucket and are intentionally not retained in the map.
		return limiter.overflow
	}
	entry := &limiterEntry{
		limiter:  rate.NewLimiter(rate.Limit(limiter.cfg.RequestsPerSecond), limiter.cfg.Burst),
		lastSeen: now,
	}
	limiter.limiters[identity] = entry
	return entry.limiter
}

func rateLimitIdentity(c *gin.Context) string {
	if value, exists := c.Get("api_key"); exists {
		if apiKey, ok := value.(string); ok && apiKey != "" {
			return "api_key:" + apiKey
		}
	}
	return "ip:" + c.ClientIP()
}
