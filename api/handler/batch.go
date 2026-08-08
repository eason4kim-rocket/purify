package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	batchdomain "github.com/use-agent/purify/batch"
	"github.com/use-agent/purify/cleaner"
	"github.com/use-agent/purify/jobs"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/scraper"
)

// BatchService is the transport-neutral subset of batch.Service used by the
// HTTP adapter.
type BatchService interface {
	Submit(models.BatchRequest) (*models.BatchResponse, error)
	Get(id string) (*models.BatchStatusResponse, bool)
}

// PostBatch returns a handler for POST /api/v1/batch/scrape.
func PostBatch(service BatchService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request models.BatchRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, models.BatchResponse{
				Status: "failed",
			})
			return
		}

		if service == nil {
			writeBatchError(c, http.StatusInternalServerError, models.ErrCodeInternal, "batch service is not configured")
			return
		}

		response, err := service.Submit(request)
		if err != nil {
			writeBatchSubmitError(c, err)
			return
		}
		if response == nil {
			writeBatchError(c, http.StatusInternalServerError, models.ErrCodeInternal, "batch service returned an empty response")
			return
		}

		c.JSON(http.StatusOK, response)
	}
}

// GetBatch returns a handler for GET /api/v1/batch/:id.
func GetBatch(service BatchService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if service == nil {
			writeBatchError(c, http.StatusInternalServerError, models.ErrCodeInternal, "batch service is not configured")
			return
		}

		jobID := c.Param("id")
		response, ok := service.Get(jobID)
		if !ok {
			writeBatchError(c, http.StatusNotFound, models.ErrCodeInvalidInput, "batch job not found")
			return
		}
		if response == nil {
			writeBatchError(c, http.StatusInternalServerError, models.ErrCodeInternal, "batch service returned an empty response")
			return
		}

		c.JSON(http.StatusOK, response)
	}
}

func writeBatchSubmitError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, batchdomain.ErrInvalidBatchSize):
		writeBatchError(c, http.StatusBadRequest, models.ErrCodeInvalidInput, "batch must contain between 1 and 100 URLs")
	case errors.Is(err, jobs.ErrManagerFull):
		writeBatchError(c, http.StatusTooManyRequests, models.ErrCodeRateLimited, "batch job capacity is full")
	case errors.Is(err, jobs.ErrManagerClosed):
		writeBatchError(c, http.StatusServiceUnavailable, models.ErrCodeInternal, "batch service is unavailable")
	default:
		writeBatchError(c, http.StatusInternalServerError, models.ErrCodeInternal, "failed to create batch job")
	}
}

func writeBatchError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{
		"error": models.ErrorDetail{
			Code:    code,
			Message: message,
		},
	})
}

// scrapeOne is retained only until Crawl migrates to its own canonical scrape
// service path. Batch HTTP and domain execution no longer use this helper.
func scrapeOne(sc *scraper.Scraper, cl *cleaner.Cleaner, targetURL string, opts models.BatchOptions) *models.ScrapeResponse {
	totalStart := time.Now()

	// Build a ScrapeRequest from shared options.
	sreq := &models.ScrapeRequest{
		URL:                targetURL,
		OutputFormat:       opts.OutputFormat,
		ExtractMode:        opts.ExtractMode,
		WaitForNetworkIdle: opts.WaitForNetworkIdle,
		Timeout:            opts.Timeout,
		Stealth:            opts.Stealth,
	}
	sreq.Defaults()

	// Scrape.
	navStart := time.Now()
	result, err := sc.DoScrape(context.Background(), sreq)
	navigationMs := time.Since(navStart).Milliseconds()

	if err != nil {
		scrapeErr, ok := err.(*models.ScrapeError)
		if !ok {
			scrapeErr = models.NewScrapeError(models.ErrCodeInternal, err.Error(), err)
		}
		return &models.ScrapeResponse{
			Success: false,
			Error:   scrapeErr.ToDetail(),
			Timing: models.TimingInfo{
				TotalMs:      time.Since(totalStart).Milliseconds(),
				NavigationMs: navigationMs,
			},
		}
	}

	// Clean.
	cleanStart := time.Now()
	resp, err := cl.Clean(result.RawHTML, sreq.URL, sreq.OutputFormat, sreq.ExtractMode)
	cleaningMs := time.Since(cleanStart).Milliseconds()

	if err != nil {
		scrapeErr, ok := err.(*models.ScrapeError)
		if !ok {
			scrapeErr = models.NewScrapeError(models.ErrCodeInternal, err.Error(), err)
		}
		return &models.ScrapeResponse{
			Success: false,
			Error:   scrapeErr.ToDetail(),
			Timing: models.TimingInfo{
				TotalMs:      time.Since(totalStart).Milliseconds(),
				NavigationMs: navigationMs,
				CleaningMs:   cleaningMs,
			},
		}
	}

	// Title fallback.
	if resp.Metadata.Title == "" {
		resp.Metadata.Title = result.Title
	}

	resp.StatusCode = result.StatusCode
	resp.FinalURL = result.FinalURL
	resp.EngineUsed = result.EngineUsed
	resp.Timing = models.TimingInfo{
		TotalMs:      time.Since(totalStart).Milliseconds(),
		NavigationMs: navigationMs,
		CleaningMs:   cleaningMs,
	}

	return resp
}

// randomID is retained only until Crawl migrates to its domain job manager.
func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
