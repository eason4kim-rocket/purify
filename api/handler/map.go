package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/use-agent/purify/discovery"
	"github.com/use-agent/purify/models"
)

// MapService is the transport-neutral subset of discovery.Service used by the
// HTTP adapter.
type MapService interface {
	Discover(context.Context, string) (*discovery.Result, error)
}

var _ MapService = (*discovery.Service)(nil)

// PostMap returns a handler for POST /api/v1/map.
func PostMap(service MapService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request models.MapRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			writeMapError(c, http.StatusBadRequest, nil, models.ErrCodeInvalidInput, "map URL is invalid")
			return
		}

		if service == nil {
			writeMapError(c, http.StatusInternalServerError, nil, models.ErrCodeInternal, "map service is not configured")
			return
		}

		result, err := service.Discover(c.Request.Context(), request.URL)
		if err != nil {
			writeMapServiceError(c, result, err)
			return
		}
		if result == nil {
			writeMapError(c, http.StatusInternalServerError, nil, models.ErrCodeInternal, "map service returned an empty response")
			return
		}

		response := mapResponse(result)
		response.Success = true
		c.JSON(http.StatusOK, response)
	}
}

func writeMapServiceError(c *gin.Context, result *discovery.Result, err error) {
	switch {
	case errors.Is(err, discovery.ErrInvalidRootURL):
		writeMapError(c, http.StatusBadRequest, result, models.ErrCodeInvalidInput, "map URL is invalid")
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		writeMapError(c, http.StatusGatewayTimeout, result, models.ErrCodeTimeout, "map discovery timed out")
	case errors.Is(err, discovery.ErrAllSourcesFailed):
		writeMapError(c, http.StatusBadGateway, result, models.ErrCodeNavigation, "map discovery sources are unavailable")
	default:
		writeMapError(c, http.StatusInternalServerError, result, models.ErrCodeInternal, "map discovery failed")
	}
}

func writeMapError(c *gin.Context, status int, result *discovery.Result, code, message string) {
	response := mapResponse(result)
	response.Error = &models.ErrorDetail{Code: code, Message: message}
	c.JSON(status, response)
}

func mapResponse(result *discovery.Result) models.MapResponse {
	if result == nil {
		return models.MapResponse{}
	}

	response := models.MapResponse{
		URLs:  make([]string, len(result.URLs)),
		Total: len(result.URLs),
		Sources: &models.MapSourceStats{
			SitemapFiles:   result.Sources.SitemapFiles,
			SitemapURLs:    result.Sources.SitemapURLs,
			RobotsSitemaps: result.Sources.RobotsSitemaps,
			HomeLinks:      result.Sources.HomeLinks,
			Truncated:      result.Sources.Truncated,
		},
	}
	copy(response.URLs, result.URLs)
	if len(result.Warnings) == 0 {
		return response
	}

	response.Warnings = make([]models.MapWarning, len(result.Warnings))
	for index, warning := range result.Warnings {
		response.Warnings[index] = models.MapWarning{
			Source:  warning.Source,
			URL:     warning.URL,
			Code:    warning.Code,
			Message: warning.Message,
		}
	}
	return response
}
