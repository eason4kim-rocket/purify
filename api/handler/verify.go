package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/use-agent/purify/models"
	verifydomain "github.com/use-agent/purify/verify"
)

const maximumVerifyRequestBytes = 4 << 20

// VerifyService is the transport-neutral fact re-verification boundary.
type VerifyService interface {
	Verify(context.Context, models.VerifyRequest) (*models.VerifyResponse, error)
}

var _ VerifyService = (*verifydomain.Service)(nil)

// Verify returns the thin HTTP adapter for POST /api/v1/verify.
func Verify(service VerifyService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if service == nil {
			respondVerifyError(c, http.StatusServiceUnavailable, models.ErrCodeEvidenceUnavailable, "fact verification is unavailable")
			return
		}

		request, err := decodeVerifyRequest(c)
		if err != nil {
			var maximumBytesError *http.MaxBytesError
			if errors.As(err, &maximumBytesError) {
				respondVerifyError(c, http.StatusRequestEntityTooLarge, models.ErrCodeInvalidInput, "verify request is too large")
				return
			}
			respondVerifyError(c, http.StatusBadRequest, models.ErrCodeInvalidInput, "invalid verify request")
			return
		}

		response, err := service.Verify(c.Request.Context(), *request)
		if err != nil {
			status, code, message := mapVerifyError(err)
			respondVerifyError(c, status, code, message)
			return
		}
		if response == nil {
			respondVerifyError(c, http.StatusInternalServerError, models.ErrCodeInternal, "verify service returned an empty response")
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func decodeVerifyRequest(c *gin.Context) (*models.VerifyRequest, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maximumVerifyRequestBytes)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()

	var request models.VerifyRequest
	if err := decoder.Decode(&request); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("verify request contains multiple JSON values")
		}
		return nil, err
	}
	return &request, nil
}

func respondVerifyError(c *gin.Context, status int, code, message string) {
	c.JSON(status, models.VerifyErrorResponse{Error: &models.ErrorDetail{Code: code, Message: message}})
}

func mapVerifyError(err error) (int, string, string) {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return http.StatusGatewayTimeout, models.ErrCodeTimeout, "fact verification timed out"
	case errors.Is(err, verifydomain.ErrInvalidReceipt):
		return http.StatusBadRequest, models.ErrCodeInvalidReceipt, "receipt is invalid or not re-verifiable"
	case errors.Is(err, verifydomain.ErrInvalidRequest), errors.Is(err, verifydomain.ErrInvalidClaim):
		return http.StatusBadRequest, models.ErrCodeInvalidInput, "verify request is invalid"
	case errors.Is(err, verifydomain.ErrNotConfigured), errors.Is(err, verifydomain.ErrSnapshot), errors.Is(err, verifydomain.ErrEvidenceUnavailable):
		return http.StatusServiceUnavailable, models.ErrCodeEvidenceUnavailable, "verification evidence is unavailable"
	case errors.Is(err, verifydomain.ErrRecord):
		return http.StatusServiceUnavailable, models.ErrCodeInternal, "verification could not be recorded"
	}

	var statusError *verifydomain.HTTPStatusError
	if errors.As(err, &statusError) {
		return http.StatusBadGateway, models.ErrCodeNavigation, "source page returned an unusable status"
	}
	if errors.Is(err, verifydomain.ErrRevisit) || errors.Is(err, verifydomain.ErrRevisitStatus) {
		return http.StatusBadGateway, models.ErrCodeNavigation, "source page could not be revisited"
	}
	return http.StatusInternalServerError, models.ErrCodeInternal, "fact verification failed"
}
