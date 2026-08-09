package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/use-agent/purify/models"
)

const maximumExtractRequestBytes = 1 << 20

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

		request, err := decodeExtractRequest(c)
		if err != nil {
			var maximumBytesError *http.MaxBytesError
			if errors.As(err, &maximumBytesError) {
				respondExtractInputError(c, http.StatusRequestEntityTooLarge, "extract request is too large")
				return
			}
			respondExtractInputError(c, http.StatusBadRequest, "invalid extract request")
			return
		}

		response, err := service.Extract(c.Request.Context(), request)
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

func decodeExtractRequest(c *gin.Context) (*models.ExtractRequest, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maximumExtractRequestBytes)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(body) {
		return nil, errors.New("extract request is not valid UTF-8")
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var request *models.ExtractRequest
	if err := decoder.Decode(&request); err != nil {
		return nil, err
	}
	if request == nil {
		return nil, errors.New("extract request must be a JSON object")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("extract request contains multiple JSON values")
		}
		return nil, err
	}
	if err := binding.Validator.ValidateStruct(request); err != nil {
		return nil, err
	}
	return request, nil
}

func respondExtractInputError(c *gin.Context, status int, message string) {
	c.JSON(status, models.ExtractResponse{
		Success: false,
		Error: &models.ErrorDetail{
			Code:    models.ErrCodeInvalidInput,
			Message: message,
		},
	})
}

// respondExtractError maps a ScrapeError to the correct HTTP status and writes
// a structured JSON error response for the extract endpoint.
func respondExtractError(c *gin.Context, err error, timing models.ExtractTimingInfo) {
	var scrapeErr *models.ScrapeError
	if !errors.As(err, &scrapeErr) {
		scrapeErr = models.NewScrapeError(models.ErrCodeInternal, "internal extraction failure", nil)
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
