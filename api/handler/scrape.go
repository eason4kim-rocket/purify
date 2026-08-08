package handler

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/use-agent/purify/models"
)

// Scrape returns a handler for POST /api/v1/scrape.
// JSON and SSE both delegate to the same canonical scrape service.
func Scrape(runner ScrapeRunner) gin.HandlerFunc {
	return ScrapeWithService(runner)
}

// mapErrorToStatus translates error codes to HTTP status codes.
func mapErrorToStatus(e *models.ScrapeError) int {
	switch e.Code {
	case models.ErrCodeTimeout:
		return http.StatusGatewayTimeout // 504
	case models.ErrCodeNavigation:
		return http.StatusBadGateway // 502
	case models.ErrCodeContentUnusable:
		return http.StatusBadGateway // 502
	case models.ErrCodeInvalidInput:
		return http.StatusBadRequest // 400
	case models.ErrCodeRateLimited:
		return http.StatusTooManyRequests // 429
	case models.ErrCodeUnauthorized:
		return http.StatusUnauthorized // 401
	default:
		return http.StatusInternalServerError // 500
	}
}

// writeSSE writes a single SSE event to the response.
func writeSSE(c *gin.Context, event string, data interface{}) {
	jsonData, _ := json.Marshal(data)
	fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, jsonData)
	c.Writer.Flush()
}
