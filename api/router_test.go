package api

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/use-agent/purify/cache"
	"github.com/use-agent/purify/config"
	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/receipts"
)

func TestReceiptRoutesRemainPublicWhenAPIAuthIsEnabled(t *testing.T) {
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x71}, ed25519.SeedSize))
	signer, err := receipts.NewSigner(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Sign(receipts.Payload{
		URL:   "https://example.com",
		Path:  "name",
		Value: json.RawMessage(`"Ada"`),
		Anchor: evidence.Anchor{
			Method:     evidence.MethodUnlocated,
			SnapshotID: "sha256:abc",
			FetchedAt:  time.Date(2026, time.August, 9, 7, 59, 0, 0, time.UTC),
		},
		IssuedAt: time.Date(2026, time.August, 9, 8, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Server:    config.ServerConfig{Mode: "test"},
		Auth:      config.AuthConfig{Enabled: true, APIKeys: []string{"required-secret"}},
		RateLimit: config.RateLimitConfig{RequestsPerSecond: 100, Burst: 100},
	}
	router := NewRouter(nil, nil, signer, cfg, cache.New(1), time.Now(), nil, nil, nil, nil)

	requestBody, _ := json.Marshal(map[string]string{"receipt": token})
	verifyRequest := httptest.NewRequest(http.MethodPost, "/api/v1/receipts/verify", bytes.NewReader(requestBody))
	verifyRequest.Header.Set("Content-Type", "application/json")
	verifyResponse := httptest.NewRecorder()
	router.ServeHTTP(verifyResponse, verifyRequest)
	if verifyResponse.Code != http.StatusOK {
		t.Fatalf("public verify status = %d, body = %s", verifyResponse.Code, verifyResponse.Body)
	}

	pubkeyResponse := httptest.NewRecorder()
	router.ServeHTTP(pubkeyResponse, httptest.NewRequest(http.MethodGet, "/api/v1/receipts/pubkey", nil))
	if pubkeyResponse.Code != http.StatusOK {
		t.Fatalf("public pubkey status = %d, body = %s", pubkeyResponse.Code, pubkeyResponse.Body)
	}

	protectedRequest := httptest.NewRequest(http.MethodPost, "/api/v1/extract", bytes.NewReader([]byte(`{}`)))
	protectedRequest.Header.Set("Content-Type", "application/json")
	protectedResponse := httptest.NewRecorder()
	router.ServeHTTP(protectedResponse, protectedRequest)
	if protectedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("protected extract status = %d, want 401; body = %s", protectedResponse.Code, protectedResponse.Body)
	}
}
