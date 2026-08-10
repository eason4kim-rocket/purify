package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/use-agent/purify/models"
)

// Auth returns API-key authentication middleware.
//
// Supports two header styles:
//
//	X-API-Key: <key>
//	Authorization: Bearer <key>
//
// If apiKeys is empty, the middleware is a no-op (open access).
func Auth(apiKeys []string) gin.HandlerFunc {
	if len(apiKeys) == 0 {
		return func(c *gin.Context) { c.Next() }
	}

	keySet := make(map[string]struct{}, len(apiKeys))
	for _, k := range apiKeys {
		if k != "" {
			keySet[k] = struct{}{}
		}
	}

	return func(c *gin.Context) {
		key := extractAPIKey(c)
		if key == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, models.ScrapeResponse{
				Success: false,
				Error: &models.ErrorDetail{
					Code:    models.ErrCodeUnauthorized,
					Message: "missing API key: provide X-API-Key header or Authorization: Bearer <key>",
				},
			})
			return
		}

		if _, valid := keySet[key]; !valid {
			c.AbortWithStatusJSON(http.StatusUnauthorized, models.ScrapeResponse{
				Success: false,
				Error: &models.ErrorDetail{
					Code:    models.ErrCodeUnauthorized,
					Message: "invalid API key",
				},
			})
			return
		}

		c.Set("api_key", key)
		c.Next()
	}
}

// SearchAuth authenticates the Search route while preserving its
// provider-neutral response envelope. An empty effective key set is a no-op;
// the router independently disables the Search capability in that state so
// the permanently registered route returns a stable 503.
func SearchAuth(apiKeys []string) gin.HandlerFunc {
	keySet := make(map[string]struct{}, len(apiKeys))
	for _, key := range apiKeys {
		if strings.TrimSpace(key) != "" {
			keySet[key] = struct{}{}
		}
	}
	if len(keySet) == 0 {
		return func(c *gin.Context) { c.Next() }
	}

	return func(c *gin.Context) {
		key := extractAPIKey(c)
		if key == "" {
			abortSearchUnauthorized(c, "missing API key: provide X-API-Key header or Authorization: Bearer <key>")
			return
		}
		if _, valid := keySet[key]; !valid {
			abortSearchUnauthorized(c, "invalid API key")
			return
		}
		c.Set("api_key", key)
		c.Next()
	}
}

// AnswerAuth authenticates the permanently registered Answer route while
// preserving its error-only non-2xx envelope. An empty effective key set is a
// no-op; the router independently disables Answer in that state.
func AnswerAuth(apiKeys []string) gin.HandlerFunc {
	keySet := make(map[string]struct{}, len(apiKeys))
	for _, key := range apiKeys {
		if strings.TrimSpace(key) != "" {
			keySet[key] = struct{}{}
		}
	}
	if len(keySet) == 0 {
		return func(c *gin.Context) { c.Next() }
	}

	return func(c *gin.Context) {
		key := extractAPIKey(c)
		if key == "" {
			abortAnswerUnauthorized(c, "missing API key: provide X-API-Key header or Authorization: Bearer <key>")
			return
		}
		if _, valid := keySet[key]; !valid {
			abortAnswerUnauthorized(c, "invalid API key")
			return
		}
		c.Set("api_key", key)
		c.Next()
	}
}

// WatchAuth authenticates all permanently registered Watch and Facts routes
// while preserving their error-only non-2xx envelope. An empty effective key
// set is inert; the router's independent capability latch then returns 503.
func WatchAuth(apiKeys []string) gin.HandlerFunc {
	keySet := make(map[string]struct{}, len(apiKeys))
	for _, key := range apiKeys {
		if strings.TrimSpace(key) != "" {
			keySet[key] = struct{}{}
		}
	}
	if len(keySet) == 0 {
		return func(c *gin.Context) { c.Next() }
	}

	return func(c *gin.Context) {
		key := extractAPIKey(c)
		if key == "" {
			abortWatchUnauthorized(c, "missing API key: provide X-API-Key header or Authorization: Bearer <key>")
			return
		}
		if _, valid := keySet[key]; !valid {
			abortWatchUnauthorized(c, "invalid API key")
			return
		}
		c.Set("api_key", key)
		c.Next()
	}
}

func abortSearchUnauthorized(c *gin.Context, message string) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, models.SearchResponse{
		Success: false,
		Results: []models.SearchResult{},
		Error: &models.ErrorDetail{
			Code:    models.ErrCodeUnauthorized,
			Message: message,
		},
	})
}

func abortAnswerUnauthorized(c *gin.Context, message string) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, models.AnswerErrorResponse{
		Error: &models.ErrorDetail{
			Code:    models.ErrCodeUnauthorized,
			Message: message,
		},
	})
}

func abortWatchUnauthorized(c *gin.Context, message string) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, models.WatchErrorResponse{
		Error: &models.ErrorDetail{
			Code:    models.ErrCodeUnauthorized,
			Message: message,
		},
	})
}

// extractAPIKey tries X-API-Key first, then Authorization: Bearer.
func extractAPIKey(c *gin.Context) string {
	if key := c.GetHeader("X-API-Key"); key != "" {
		return key
	}
	if auth := c.GetHeader("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}
