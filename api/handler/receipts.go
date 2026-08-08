package handler

import (
	"encoding/base64"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/receipts"
)

// VerifyReceipt returns a public, authentication-free cryptographic verdict.
func VerifyReceipt(signer *receipts.Signer) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request models.ReceiptVerifyRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, models.ReceiptVerifyResponse{
				Valid: false,
				Error: &models.ErrorDetail{Code: models.ErrCodeInvalidInput, Message: err.Error()},
			})
			return
		}
		if signer == nil {
			c.JSON(http.StatusInternalServerError, models.ReceiptVerifyResponse{
				Valid: false,
				Error: &models.ErrorDetail{Code: models.ErrCodeInternal, Message: "receipt verifier is unavailable"},
			})
			return
		}

		payload, err := signer.Verify(request.Receipt)
		if err != nil {
			c.JSON(http.StatusOK, models.ReceiptVerifyResponse{
				Valid: false,
				Error: &models.ErrorDetail{Code: models.ErrCodeInvalidReceipt, Message: "receipt signature or payload is invalid"},
			})
			return
		}
		c.JSON(http.StatusOK, models.ReceiptVerifyResponse{Valid: true, Payload: payload})
	}
}

// ReceiptPublicKey publishes the active signing key as an OKP JWK.
func ReceiptPublicKey(signer *receipts.Signer) gin.HandlerFunc {
	return func(c *gin.Context) {
		if signer == nil {
			c.JSON(http.StatusInternalServerError, models.ErrorDetail{Code: models.ErrCodeInternal, Message: "receipt public key is unavailable"})
			return
		}
		c.JSON(http.StatusOK, models.ReceiptPublicKeyResponse{
			KTY: "OKP",
			CRV: "Ed25519",
			Alg: receipts.Algorithm,
			Use: "sig",
			KID: signer.KID(),
			X:   base64.RawURLEncoding.EncodeToString(signer.PublicKey()),
		})
	}
}
