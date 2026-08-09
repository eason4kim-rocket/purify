package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	batchdomain "github.com/use-agent/purify/batch"
	"github.com/use-agent/purify/jobs"
	"github.com/use-agent/purify/models"
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
