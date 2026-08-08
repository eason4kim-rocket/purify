package handler

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/receipts"
)

func TestVerifyReceiptEndpoint(t *testing.T) {
	signer := testReceiptSigner(t)
	token, err := signer.Sign(receipts.Payload{
		URL:   "https://example.com/plans",
		Path:  "price",
		Value: json.RawMessage(`29.99`),
		Anchor: evidence.Anchor{
			Quote:      "$29.99",
			Method:     evidence.MethodExact,
			SnapshotID: "sha256:abc",
			FetchedAt:  time.Date(2026, time.August, 9, 8, 0, 0, 0, time.UTC),
		},
		IssuedAt: time.Date(2026, time.August, 9, 8, 1, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	router.POST("/verify", VerifyReceipt(signer))

	t.Run("valid", func(t *testing.T) {
		body, _ := json.Marshal(models.ReceiptVerifyRequest{Receipt: token})
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/verify", bytes.NewReader(body)))
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body)
		}
		var result models.ReceiptVerifyResponse
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if !result.Valid || result.Payload == nil || result.Payload.Path != "price" || result.Error != nil {
			t.Fatalf("response = %#v", result)
		}
	})

	t.Run("tampered", func(t *testing.T) {
		parts := strings.Split(token, ".")
		signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
		signature[0] ^= 0x80
		parts[2] = base64.RawURLEncoding.EncodeToString(signature)
		body, _ := json.Marshal(models.ReceiptVerifyRequest{Receipt: strings.Join(parts, ".")})
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/verify", bytes.NewReader(body)))
		var result models.ReceiptVerifyResponse
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusOK || result.Valid || result.Payload != nil || result.Error == nil || result.Error.Code != models.ErrCodeInvalidReceipt {
			t.Fatalf("status = %d, response = %#v", response.Code, result)
		}
	})

	t.Run("missing receipt", func(t *testing.T) {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/verify", strings.NewReader(`{}`)))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body)
		}
	})
}

func TestReceiptPublicKeyEndpoint(t *testing.T) {
	signer := testReceiptSigner(t)
	router := gin.New()
	router.GET("/pubkey", ReceiptPublicKey(signer))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/pubkey", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	var result models.ReceiptPublicKeyResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(result.X)
	if err != nil {
		t.Fatal(err)
	}
	if result.KTY != "OKP" || result.CRV != "Ed25519" || result.Alg != receipts.Algorithm || result.Use != "sig" || result.KID != signer.KID() || !bytes.Equal(decoded, signer.PublicKey()) {
		t.Fatalf("public key response = %#v", result)
	}
}

func testReceiptSigner(t *testing.T) *receipts.Signer {
	t.Helper()
	seed := bytes.Repeat([]byte{0x42}, ed25519.SeedSize)
	signer, err := receipts.NewSigner(ed25519.NewKeyFromSeed(seed))
	if err != nil {
		t.Fatal(err)
	}
	return signer
}
