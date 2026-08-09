package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
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

// MultiExtractService is the optional transport-neutral multi-source boundary.
// Extract keeps its legacy single-page contract when a service does not expose
// this capability.
type MultiExtractService interface {
	ExtractMulti(context.Context, *models.ExtractRequest) (*models.MultiExtractResponse, error)
}

// MultiExtractResponseEncoder lets the production service share its global
// source/CPU slots with the final bounded JSON encoding stage.
type MultiExtractResponseEncoder interface {
	EncodeMultiResponse(context.Context, *models.MultiExtractResponse) ([]byte, error)
}

var fallbackMultiExtractEncodingSlots = make(chan struct{}, 4)
var fallbackMultiExtractTimeoutEncodingSlots = make(chan struct{}, 4)

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
		if request.Sources != nil {
			handleMultiExtract(c, service, request)
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
	if err := validateExtractTargetSelection(request); err != nil {
		return nil, err
	}
	return request, nil
}

func validateExtractTargetSelection(request *models.ExtractRequest) error {
	if request == nil {
		return errors.New("extract request is required")
	}
	hasURL := request.URL != ""
	hasSources := request.Sources != nil
	if hasURL == hasSources {
		return errors.New("exactly one of url and sources is required")
	}
	if !hasSources {
		return nil
	}
	if len(request.Sources) < 1 || len(request.Sources) > models.MaxExtractSources {
		return errors.New("sources must contain one to eight URLs")
	}
	used := 0
	for _, source := range request.Sources {
		if len(source) == 0 || len(source) > models.MaxExtractSourceURLBytes ||
			len(source) > models.MaxExtractSourcesURLBytes-used {
			return errors.New("source URLs exceed the request budget")
		}
		used += len(source)
	}
	return nil
}

func handleMultiExtract(c *gin.Context, service ExtractService, request *models.ExtractRequest) {
	multi, ok := service.(MultiExtractService)
	if !ok {
		respondMultiExtractError(c, models.NewScrapeError(
			models.ErrCodeMultiSourceUnavailable,
			"multi-source extraction is unavailable",
			nil,
		))
		return
	}
	timeoutSeconds := request.Timeout
	if timeoutSeconds == 0 {
		timeoutSeconds = 30
	}
	taskContext, cancel := context.WithTimeout(c.Request.Context(), time.Duration(timeoutSeconds)*time.Second)
	defer cancel()
	response, err := multi.ExtractMulti(taskContext, request)
	if err != nil {
		var scrapeError *models.ScrapeError
		if response != nil && errors.As(err, &scrapeError) && isStableMultiAggregateError(scrapeError.Code) {
			response.Success = false
			response.Status = ""
			response.Data = nil
			response.Consensus = nil
			response.Violations = nil
			status := http.StatusServiceUnavailable
			switch scrapeError.Code {
			case models.ErrCodeNoValidSource:
				response.Error = &models.ErrorDetail{
					Code:    models.ErrCodeNoValidSource,
					Message: "no valid extraction source",
				}
				status = http.StatusBadGateway
			case models.ErrCodeMultiSourceUnavailable:
				response.Error = &models.ErrorDetail{
					Code:    models.ErrCodeMultiSourceUnavailable,
					Message: "multi-source extraction is unavailable",
				}
			case models.ErrCodeTimeout:
				response.Error = &models.ErrorDetail{
					Code:    models.ErrCodeTimeout,
					Message: "multi-source extraction timed out",
				}
				status = http.StatusGatewayTimeout
			case models.ErrCodeLLMAuthFailure, models.ErrCodeUnauthorized:
				response.Error = &models.ErrorDetail{Code: models.ErrCodeLLMAuthFailure, Message: "multi-source authentication failed"}
				status = http.StatusUnauthorized
			case models.ErrCodeLLMRateLimited, models.ErrCodeRateLimited:
				response.Error = &models.ErrorDetail{Code: models.ErrCodeLLMRateLimited, Message: "multi-source rate limit exceeded"}
				status = http.StatusTooManyRequests
			case models.ErrCodeExtractorUnavailable:
				response.Error = &models.ErrorDetail{Code: scrapeError.Code, Message: "multi-source extractor is unavailable"}
				status = http.StatusConflict
			case models.ErrCodeInternal:
				response.Error = &models.ErrorDetail{Code: scrapeError.Code, Message: "internal multi-source extraction failure"}
				status = http.StatusInternalServerError
			}
			writeMultiExtractResponse(c, taskContext, service, status, response)
			return
		}
		respondMultiExtractError(c, err)
		return
	}
	if response == nil {
		respondMultiExtractError(c, models.NewScrapeError(models.ErrCodeInternal, "multi-source extraction returned an empty response", nil))
		return
	}
	writeMultiExtractResponse(c, taskContext, service, http.StatusOK, response)
}

func isStableMultiAggregateError(code string) bool {
	switch code {
	case models.ErrCodeNoValidSource, models.ErrCodeMultiSourceUnavailable, models.ErrCodeTimeout,
		models.ErrCodeLLMAuthFailure, models.ErrCodeUnauthorized,
		models.ErrCodeLLMRateLimited, models.ErrCodeRateLimited,
		models.ErrCodeExtractorUnavailable, models.ErrCodeInternal:
		return true
	default:
		return false
	}
}

func writeMultiExtractResponse(
	c *gin.Context,
	ctx context.Context,
	service ExtractService,
	status int,
	response *models.MultiExtractResponse,
) {
	encoded, err := encodeMultiExtractResponse(ctx, service, response)
	if err != nil {
		if multiExtractEncodingTimedOut(err) && response != nil {
			timeoutResponse := multiExtractTimeoutFailure(response)
			encoded, fallbackErr := encodeMultiExtractResponseFallback(
				context.Background(),
				timeoutResponse,
				fallbackMultiExtractTimeoutEncodingSlots,
			)
			if fallbackErr == nil {
				c.Data(http.StatusGatewayTimeout, "application/json; charset=utf-8", encoded)
				return
			}
		}
		respondMultiExtractError(c, err)
		return
	}
	c.Data(status, "application/json; charset=utf-8", encoded)
}

func encodeMultiExtractResponse(ctx context.Context, service ExtractService, response *models.MultiExtractResponse) ([]byte, error) {
	if encoder, ok := service.(MultiExtractResponseEncoder); ok {
		return encoder.EncodeMultiResponse(ctx, response)
	}
	return encodeMultiExtractResponseFallback(ctx, response, fallbackMultiExtractEncodingSlots)
}

func encodeMultiExtractResponseFallback(
	ctx context.Context,
	response *models.MultiExtractResponse,
	slots chan struct{},
) ([]byte, error) {
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return nil, models.NewScrapeError(models.ErrCodeTimeout, "multi-source response encoding timed out", ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return nil, models.NewScrapeError(models.ErrCodeTimeout, "multi-source response encoding timed out", err)
	}
	preflightOK := preflightMultiExtractResponseSize(response)
	if err := ctx.Err(); err != nil {
		return nil, models.NewScrapeError(models.ErrCodeTimeout, "multi-source response encoding timed out", err)
	}
	if !preflightOK {
		return nil, errors.New("multi-source response exceeds its output budget")
	}
	encoded, err := json.Marshal(response)
	if err := ctx.Err(); err != nil {
		return nil, models.NewScrapeError(models.ErrCodeTimeout, "multi-source response encoding timed out", err)
	}
	if err != nil {
		return nil, err
	}
	if len(encoded) > models.MaxMultiExtractResponseBytes {
		return nil, errors.New("multi-source response exceeds its output budget")
	}
	return encoded, nil
}

func multiExtractEncodingTimedOut(err error) bool {
	var scrapeError *models.ScrapeError
	return errors.As(err, &scrapeError) && scrapeError.Code == models.ErrCodeTimeout
}

func multiExtractTimeoutFailure(response *models.MultiExtractResponse) *models.MultiExtractResponse {
	failure := *response
	failure.Success = false
	failure.Status = ""
	failure.Data = nil
	failure.Consensus = nil
	failure.Violations = nil
	failure.Sources = append([]models.MultiExtractSource(nil), response.Sources...)
	for index := range failure.Sources {
		if failure.Sources[index].LLMUsage != nil {
			usage := *failure.Sources[index].LLMUsage
			failure.Sources[index].LLMUsage = &usage
		}
		if failure.Sources[index].Error != nil {
			detail := *failure.Sources[index].Error
			failure.Sources[index].Error = &detail
		}
	}
	if response.LLMUsage != nil {
		usage := *response.LLMUsage
		failure.LLMUsage = &usage
	}
	failure.Error = &models.ErrorDetail{
		Code:    models.ErrCodeTimeout,
		Message: "multi-source extraction timed out",
	}
	return &failure
}

func preflightMultiExtractResponseSize(response *models.MultiExtractResponse) bool {
	if response == nil {
		return false
	}
	remaining := models.MaxMultiExtractResponseBytes
	if !reserveMultiExtractResponseBytes(&remaining, 2) {
		return false
	}
	fields := 0
	addField := func(name string, value any) bool {
		encodedName, err := json.Marshal(name)
		if err != nil {
			return false
		}
		encodedValue, err := json.Marshal(value)
		if err != nil {
			return false
		}
		if fields > 0 && !reserveMultiExtractResponseBytes(&remaining, 1) {
			return false
		}
		if !reserveMultiExtractResponseBytes(&remaining, len(encodedName)) ||
			!reserveMultiExtractResponseBytes(&remaining, 1) ||
			!reserveMultiExtractResponseBytes(&remaining, len(encodedValue)) {
			return false
		}
		fields++
		return true
	}
	valid := addField("success", response.Success)
	if response.Status != "" {
		valid = valid && addField("status", response.Status)
	}
	if len(response.Data) > 0 {
		valid = valid && addField("data", response.Data)
	}
	if response.Consensus != nil {
		valid = valid && addField("consensus", response.Consensus)
	}
	valid = valid && addField("sources", response.Sources)
	if len(response.Violations) > 0 {
		valid = valid && addField("violations", response.Violations)
	}
	valid = valid && addField("tokens", response.Tokens) && addField("timing", response.Timing)
	if response.LLMUsage != nil {
		valid = valid && addField("llm_usage", response.LLMUsage)
	}
	valid = valid && addField("usage_complete", response.UsageComplete)
	if response.Error != nil {
		valid = valid && addField("error", response.Error)
	}
	return valid
}

func reserveMultiExtractResponseBytes(remaining *int, requested int) bool {
	if remaining == nil || requested < 0 || requested > *remaining {
		return false
	}
	*remaining -= requested
	return true
}

func respondMultiExtractError(c *gin.Context, err error) {
	detail := &models.ErrorDetail{
		Code:    models.ErrCodeInternal,
		Message: "internal multi-source extraction failure",
	}
	status := http.StatusInternalServerError
	var scrapeError *models.ScrapeError
	if errors.As(err, &scrapeError) {
		switch scrapeError.Code {
		case models.ErrCodeInvalidInput:
			detail = &models.ErrorDetail{Code: scrapeError.Code, Message: "invalid multi-source extract request"}
			status = http.StatusBadRequest
		case models.ErrCodeEvidenceUnavailable, models.ErrCodeMultiSourceUnavailable:
			detail = &models.ErrorDetail{Code: scrapeError.Code, Message: "multi-source extraction is unavailable"}
			status = http.StatusServiceUnavailable
		case models.ErrCodeNoValidSource:
			detail = &models.ErrorDetail{Code: scrapeError.Code, Message: "no valid extraction source"}
			status = http.StatusBadGateway
		case models.ErrCodeTimeout:
			detail = &models.ErrorDetail{Code: scrapeError.Code, Message: "multi-source extraction timed out"}
			status = http.StatusGatewayTimeout
		case models.ErrCodeExtractorUnavailable:
			detail = &models.ErrorDetail{Code: scrapeError.Code, Message: "multi-source extractor is unavailable"}
			status = http.StatusConflict
		case models.ErrCodeLLMAuthFailure, models.ErrCodeUnauthorized:
			detail = &models.ErrorDetail{Code: models.ErrCodeLLMAuthFailure, Message: "multi-source authentication failed"}
			status = http.StatusUnauthorized
		case models.ErrCodeLLMRateLimited, models.ErrCodeRateLimited:
			detail = &models.ErrorDetail{Code: models.ErrCodeLLMRateLimited, Message: "multi-source rate limit exceeded"}
			status = http.StatusTooManyRequests
		case models.ErrCodeLLMFailure:
			detail = &models.ErrorDetail{Code: scrapeError.Code, Message: "multi-source extraction failed"}
			status = http.StatusBadGateway
		}
	}
	c.JSON(status, models.MultiExtractResponse{
		Success:       false,
		Sources:       []models.MultiExtractSource{},
		UsageComplete: false,
		Error:         detail,
	})
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
