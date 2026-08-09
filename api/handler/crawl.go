package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	crawldomain "github.com/use-agent/purify/crawl"
	"github.com/use-agent/purify/jobs"
	"github.com/use-agent/purify/models"
)

// CrawlService is the transport-neutral subset of crawl.Service used by the
// HTTP adapter.
type CrawlService interface {
	Submit(models.CrawlRequest) (*models.CrawlResponse, error)
	Get(id string) (*models.CrawlStatusResponse, bool)
}

// PostCrawl returns a handler for POST /api/v1/crawl.
func PostCrawl(service CrawlService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request models.CrawlRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			// Preserve the legacy binding-failure response shape.
			c.JSON(http.StatusBadRequest, models.CrawlResponse{Status: "failed"})
			return
		}

		if service == nil {
			writeCrawlError(c, http.StatusInternalServerError, models.ErrCodeInternal, "crawl service is not configured")
			return
		}

		response, err := service.Submit(request)
		if err != nil {
			writeCrawlSubmitError(c, err)
			return
		}
		if response == nil {
			writeCrawlError(c, http.StatusInternalServerError, models.ErrCodeInternal, "crawl service returned an empty response")
			return
		}

		c.JSON(http.StatusOK, response)
	}
}

// GetCrawl returns a handler for GET /api/v1/crawl/:id.
func GetCrawl(service CrawlService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if service == nil {
			writeCrawlError(c, http.StatusInternalServerError, models.ErrCodeInternal, "crawl service is not configured")
			return
		}

		response, ok := service.Get(c.Param("id"))
		if !ok {
			writeCrawlError(c, http.StatusNotFound, models.ErrCodeInvalidInput, "crawl job not found")
			return
		}
		if response == nil {
			writeCrawlError(c, http.StatusInternalServerError, models.ErrCodeInternal, "crawl service returned an empty response")
			return
		}

		c.JSON(http.StatusOK, response)
	}
}

func writeCrawlSubmitError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, crawldomain.ErrInvalidURL):
		writeCrawlError(c, http.StatusBadRequest, models.ErrCodeInvalidInput, "crawl URL is invalid")
	case errors.Is(err, crawldomain.ErrInvalidMaxDepth):
		writeCrawlError(c, http.StatusBadRequest, models.ErrCodeInvalidInput, "max_depth must be between 1 and 10")
	case errors.Is(err, crawldomain.ErrInvalidMaxPages):
		writeCrawlError(c, http.StatusBadRequest, models.ErrCodeInvalidInput, "max_pages must be between 1 and 500")
	case errors.Is(err, crawldomain.ErrInvalidScope):
		writeCrawlError(c, http.StatusBadRequest, models.ErrCodeInvalidInput, "scope must be page, domain, or subdomain")
	case errors.Is(err, crawldomain.ErrInvalidExcludePattern):
		writeCrawlError(c, http.StatusBadRequest, models.ErrCodeInvalidInput, "exclude_patterns contains an invalid pattern")
	case errors.Is(err, jobs.ErrManagerFull):
		writeCrawlError(c, http.StatusTooManyRequests, models.ErrCodeRateLimited, "crawl job capacity is full")
	case errors.Is(err, jobs.ErrManagerClosed):
		writeCrawlError(c, http.StatusServiceUnavailable, models.ErrCodeInternal, "crawl service is unavailable")
	default:
		writeCrawlError(c, http.StatusInternalServerError, models.ErrCodeInternal, "failed to create crawl job")
	}
}

func writeCrawlError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{
		"error": models.ErrorDetail{
			Code:    code,
			Message: message,
		},
	})
}
