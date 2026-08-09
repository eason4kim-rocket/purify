package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/use-agent/purify/models"
)

// ExtractService is the transport-neutral structured extraction boundary.
type ExtractService interface {
	Extract(context.Context, *models.ExtractRequest) (*models.ExtractResponse, error)
}

// Extract returns the thin HTTP adapter for POST /api/v1/extract.
func Extract(service ExtractService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if service == nil {
			respondExtractError(c, models.NewScrapeError(models.ErrCodeInternal, "extract service is unavailable", nil), models.ExtractTimingInfo{})
			return
		}

		var request models.ExtractRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			respondExtractError(c, models.NewScrapeError(models.ErrCodeInvalidInput, err.Error(), err), models.ExtractTimingInfo{})
			return
		}

		response, err := service.Extract(c.Request.Context(), &request)
		if err != nil {
			var timed interface {
				ExtractTiming() models.ExtractTimingInfo
			}
			timing := models.ExtractTimingInfo{}
			if errors.As(err, &timed) {
				timing = timed.ExtractTiming()
			}
			respondExtractError(c, err, timing)
			return
		}
		if response == nil {
			respondExtractError(c, models.NewScrapeError(models.ErrCodeInternal, "extract service returned an empty response", nil), models.ExtractTimingInfo{})
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

// respondExtractError maps a ScrapeError to the correct HTTP status and writes
// a structured JSON error response for the extract endpoint.
func respondExtractError(c *gin.Context, err error, timing models.ExtractTimingInfo) {
	var scrapeErr *models.ScrapeError
	if !errors.As(err, &scrapeErr) {
		message := "unknown extraction failure"
		if err != nil {
			message = err.Error()
		}
		scrapeErr = models.NewScrapeError(models.ErrCodeInternal, message, err)
	}

	c.JSON(mapExtractErrorToStatus(scrapeErr), models.ExtractResponse{
		Success: false,
		Error:   scrapeErr.ToDetail(),
		Timing:  timing,
	})
}

// mapExtractErrorToStatus translates error codes to HTTP status codes,
// including LLM-specific codes.
func mapExtractErrorToStatus(e *models.ScrapeError) int {
	if e == nil {
		return http.StatusInternalServerError
	}
	switch e.Code {
	case models.ErrCodeTimeout:
		return http.StatusGatewayTimeout
	case models.ErrCodeNavigation:
		return http.StatusBadGateway
	case models.ErrCodeInvalidInput:
		return http.StatusBadRequest
	case models.ErrCodeRateLimited, models.ErrCodeLLMRateLimited:
		return http.StatusTooManyRequests
	case models.ErrCodeUnauthorized, models.ErrCodeLLMAuthFailure:
		return http.StatusUnauthorized
	case models.ErrCodeLLMFailure:
		return http.StatusBadGateway
	case models.ErrCodeEvidenceUnavailable:
		return http.StatusServiceUnavailable
	case models.ErrCodeExtractorUnavailable:
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}
