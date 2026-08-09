package handler

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	compilerdomain "github.com/use-agent/purify/compiler"
	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/models"
)

// ExtractorHealService is the narrow durable scheduling boundary used by the
// HTTP adapter.
type ExtractorHealService interface {
	ScheduleExtractor(context.Context, string) (compilerdomain.HealSchedule, error)
}

var _ ExtractorHealService = (*compilerdomain.HealWorker)(nil)

// PostExtractorHeal schedules the exact durable candidate associated with an
// extractor revision. The endpoint intentionally accepts no request body.
func PostExtractorHeal(service ExtractorHealService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !emptyExtractorHealBody(c) {
			respondExtractorHealError(c, http.StatusBadRequest, models.ErrCodeInvalidInput,
				"extractor heal request body must be empty")
			return
		}
		if service == nil {
			respondExtractorHealError(c, http.StatusServiceUnavailable, models.ErrCodeExtractorUnavailable,
				"extractor healing is unavailable")
			return
		}

		extractorID := c.Param("id")
		schedule, err := service.ScheduleExtractor(c.Request.Context(), extractorID)
		if err != nil {
			status, code, message := mapExtractorHealError(err)
			respondExtractorHealError(c, status, code, message)
			return
		}
		if schedule.ExtractorID != extractorID || !validExtractorHealUUID(schedule.ExtractorID) ||
			!validExtractorHealUUID(schedule.HealRunID) {
			respondExtractorHealError(c, http.StatusInternalServerError, models.ErrCodeInternal,
				"extractor healing could not be scheduled")
			return
		}
		c.JSON(http.StatusAccepted, models.ExtractorHealResponse{
			ExtractorID: schedule.ExtractorID,
			HealRunID:   schedule.HealRunID,
			Status:      models.ExtractorHealStatusAccepted,
		})
	}
}

func emptyExtractorHealBody(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 0)
	_, err := io.ReadAll(c.Request.Body)
	return err == nil
}

func mapExtractorHealError(err error) (int, string, string) {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, models.ErrCodeTimeout, "extractor healing timed out"
	case errors.Is(err, compilerdomain.ErrInvalidHealSchedule):
		return http.StatusBadRequest, models.ErrCodeInvalidInput, "extractor ID is invalid"
	case errors.Is(err, compilerdomain.ErrHealExtractorNotFound):
		return http.StatusNotFound, models.ErrCodeExtractorUnavailable, "extractor was not found"
	case errors.Is(err, compilerdomain.ErrHealScheduleNotReady):
		return http.StatusConflict, models.ErrCodeExtractorUnavailable, "extractor has no ready heal candidate"
	case errors.Is(err, compilerdomain.ErrHealScheduleAmbiguous):
		return http.StatusConflict, models.ErrCodeExtractorUnavailable, "extractor heal candidate is ambiguous"
	case errors.Is(err, compilerdomain.ErrHealWorkerClosed),
		errors.Is(err, compilerdomain.ErrInvalidHealWorker), errors.Is(err, ledger.ErrClosed):
		return http.StatusServiceUnavailable, models.ErrCodeExtractorUnavailable, "extractor healing is unavailable"
	default:
		return http.StatusInternalServerError, models.ErrCodeInternal, "extractor healing could not be scheduled"
	}
}

func respondExtractorHealError(c *gin.Context, status int, code, message string) {
	c.JSON(status, models.ExtractorHealErrorResponse{
		Error: &models.ErrorDetail{Code: code, Message: message},
	})
}

func validExtractorHealUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for index, current := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if !(current >= '0' && current <= '9' || current >= 'a' && current <= 'f') {
			return false
		}
	}
	return true
}
