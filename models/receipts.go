package models

import "github.com/use-agent/purify/receipts"

// ReceiptVerifyRequest is the public payload for receipt verification.
type ReceiptVerifyRequest struct {
	Receipt string `json:"receipt" binding:"required"`
}

// ReceiptVerifyResponse reports cryptographic and semantic validity. Invalid
// signed input is represented as valid:false rather than an HTTP transport
// failure so callers can use the endpoint as a stable verification oracle.
type ReceiptVerifyResponse struct {
	Valid   bool              `json:"valid"`
	Payload *receipts.Payload `json:"payload,omitempty"`
	Error   *ErrorDetail      `json:"error,omitempty"`
}

// ReceiptPublicKeyResponse is an RFC 8037-compatible OKP JWK. X contains the
// raw Ed25519 public key encoded with unpadded base64url.
type ReceiptPublicKeyResponse struct {
	KTY string `json:"kty"`
	CRV string `json:"crv"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	KID string `json:"kid"`
	X   string `json:"x"`
}
