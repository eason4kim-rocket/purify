package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/scrape"
)

// ScrapeRunner is the transport-neutral scrape service contract used by the
// HTTP adapter. The concrete implementation is *scrape.Service.
type ScrapeRunner interface {
	Run(context.Context, *models.ScrapeRequest, scrape.Observer) (*scrape.Result, error)
}

// ScrapeWithService serves JSON and SSE through the same canonical scrape
// pipeline. The transport layer owns only binding, status mapping, and event
// framing; fetch, quality, cleaning, cache, and response assembly stay in the
// service.
func ScrapeWithService(runner ScrapeRunner) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request models.ScrapeRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, failedResponse(
				models.NewScrapeError(models.ErrCodeInvalidInput, err.Error(), err),
			))
			return
		}

		if runner == nil {
			c.JSON(http.StatusInternalServerError, failedResponse(
				models.NewScrapeError(models.ErrCodeInternal, "scrape service is not configured", nil),
			))
			return
		}

		if acceptsEventStream(c.GetHeader("Accept")) {
			runScrapeSSE(c, runner, &request)
			return
		}

		var failure *models.ScrapeResponse
		result, err := runner.Run(c.Request.Context(), &request, func(event scrape.Event) {
			if event.Type == scrape.EventError {
				failure = event.Response
			}
		})
		if err != nil {
			if failure == nil {
				failure = failedResponse(asHandlerScrapeError(err))
			}
			c.JSON(statusForScrapeError(err), failure)
			return
		}
		if result == nil || result.Response == nil {
			internal := models.NewScrapeError(models.ErrCodeInternal, "scrape service returned an empty result", nil)
			c.JSON(http.StatusInternalServerError, failedResponse(internal))
			return
		}

		c.JSON(http.StatusOK, result.Response)
	}
}

func runScrapeSSE(c *gin.Context, runner ScrapeRunner, request *models.ScrapeRequest) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Status(http.StatusOK)

	_, err := runner.Run(c.Request.Context(), request, func(event scrape.Event) {
		writeSSE(c, string(event.Type), scrapeEventPayload(event))
	})
	if err != nil {
		// Service.Run emits exactly one terminal error event. Writing another
		// transport-local event here would make JSON and SSE disagree.
		return
	}
}

func scrapeEventPayload(event scrape.Event) any {
	switch event.Type {
	case scrape.EventStarted:
		return struct {
			URL string `json:"url"`
		}{URL: event.URL}
	case scrape.EventAttempt:
		return event.Attempt
	case scrape.EventNavigated:
		return event.Navigation
	case scrape.EventCompleted, scrape.EventError:
		return event.Response
	default:
		return event
	}
}

func acceptsEventStream(accept string) bool {
	for _, mediaRange := range strings.Split(accept, ",") {
		mediaType := strings.TrimSpace(strings.SplitN(mediaRange, ";", 2)[0])
		if strings.EqualFold(mediaType, "text/event-stream") {
			return true
		}
	}
	return false
}

func statusForScrapeError(err error) int {
	return mapErrorToStatus(asHandlerScrapeError(err))
}

func asHandlerScrapeError(err error) *models.ScrapeError {
	var scrapeErr *models.ScrapeError
	if errors.As(err, &scrapeErr) {
		return scrapeErr
	}
	return models.NewScrapeError(models.ErrCodeInternal, err.Error(), err)
}

func failedResponse(err *models.ScrapeError) *models.ScrapeResponse {
	return &models.ScrapeResponse{Success: false, Error: err.ToDetail()}
}
