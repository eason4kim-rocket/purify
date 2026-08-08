package receipts

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/use-agent/purify/evidence"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	priv, pub := testKey(t, 0x11)
	issuedAt := time.Date(2026, time.August, 9, 8, 7, 6, 123, time.FixedZone("test", 8*60*60))
	payload := Payload{
		URL:   "https://example.com/plans",
		Path:  "plans.0.price",
		Value: json.RawMessage(`29.99`),
		Anchor: evidence.Anchor{
			Quote:      "$29.99/month",
			TextRange:  [2]int{12, 24},
			Selector:   "#pro .price",
			Method:     evidence.MethodExact,
			SnapshotID: "sha256:abcdef",
			FetchedAt:  issuedAt.Add(-time.Minute),
		},
		ExtractorVersion: "extractor-3",
		IssuedAt:         issuedAt,
	}

	token, err := Sign(payload, priv)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	if strings.Contains(token, "=") || len(strings.Split(token, ".")) != 3 {
		t.Fatalf("token is not unpadded compact JWS: %q", token)
	}
	verified, err := Verify(token, pub)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	wantKID, _ := KeyID(pub)
	if verified.V != Version || verified.KID != wantKID {
		t.Fatalf("version/kid = %q/%q, want %q/%q", verified.V, verified.KID, Version, wantKID)
	}
	if verified.URL != payload.URL || verified.Path != payload.Path || string(verified.Value) != string(payload.Value) {
		t.Fatalf("verified claim = %#v, want %#v", verified, payload)
	}
	if !verified.IssuedAt.Equal(issuedAt) || verified.IssuedAt.Location() != time.UTC {
		t.Fatalf("issued_at = %v, want same instant normalized to UTC", verified.IssuedAt)
	}
	anchorWithoutTime := verified.Anchor
	anchorWithoutTime.FetchedAt = time.Time{}
	wantAnchorWithoutTime := payload.Anchor
	wantAnchorWithoutTime.FetchedAt = time.Time{}
	if anchorWithoutTime != wantAnchorWithoutTime || !verified.Anchor.FetchedAt.Equal(payload.Anchor.FetchedAt) || verified.ExtractorVersion != payload.ExtractorVersion {
		t.Fatalf("verified evidence fields differ: %#v", verified)
	}

	// Ed25519 and the JSON encoding are deterministic when IssuedAt is fixed.
	again, err := Sign(payload, priv)
	if err != nil || again != token {
		t.Fatalf("second Sign() = %q, %v; want identical token", again, err)
	}
}

func TestSignPopulatesIssuedAt(t *testing.T) {
	priv, pub := testKey(t, 0x12)
	before := time.Now().UTC()
	payload := validPayload()
	payload.IssuedAt = time.Time{}
	token, err := Sign(payload, priv)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	verified, err := Verify(token, pub)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if verified.IssuedAt.Before(before) || verified.IssuedAt.After(time.Now().UTC()) {
		t.Fatalf("auto issued_at = %v, want current time", verified.IssuedAt)
	}
}

func TestCompactJWSHeader(t *testing.T) {
	priv, pub := testKey(t, 0x13)
	token := mustSign(t, priv, validPayload())
	parts := strings.Split(token, ".")
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var header protectedHeader
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		t.Fatal(err)
	}
	wantKID, _ := KeyID(pub)
	if header.Alg != Algorithm || header.KID != wantKID {
		t.Fatalf("header = %#v, want alg=%q kid=%q", header, Algorithm, wantKID)
	}
}

func TestSignerDefensivelyOwnsKeyMaterial(t *testing.T) {
	priv, pub := testKey(t, 0x14)
	signer, err := NewSigner(priv)
	if err != nil {
		t.Fatalf("NewSigner() error = %v", err)
	}
	wantKID, _ := KeyID(pub)

	// Mutating the caller-owned private key must not alter the signer.
	priv[0] ^= 0xff
	token, err := signer.Sign(validPayload())
	if err != nil {
		t.Fatalf("Signer.Sign() error after input mutation = %v", err)
	}
	if _, err := signer.Verify(token); err != nil {
		t.Fatalf("Signer.Verify() error = %v", err)
	}

	returned := signer.PublicKey()
	returned[0] ^= 0xff
	if signer.KID() != wantKID {
		t.Fatalf("Signer.KID() = %q, want %q", signer.KID(), wantKID)
	}
	if _, err := signer.Verify(token); err != nil {
		t.Fatalf("mutating returned public key altered signer: %v", err)
	}
}

func TestVerifyRejectsTamperingAndWrongKey(t *testing.T) {
	priv, pub := testKey(t, 0x21)
	token := mustSign(t, priv, validPayload())

	t.Run("payload byte", func(t *testing.T) {
		parts := strings.Split(token, ".")
		payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatal(err)
		}
		changed := bytes.Replace(payloadJSON, []byte(`29.99`), []byte(`39.99`), 1)
		if bytes.Equal(changed, payloadJSON) {
			t.Fatal("test payload did not contain expected value")
		}
		parts[1] = base64.RawURLEncoding.EncodeToString(changed)
		_, err = Verify(strings.Join(parts, "."), pub)
		if !errors.Is(err, ErrInvalidSignature) {
			t.Fatalf("Verify() error = %v, want invalid signature", err)
		}
	})

	t.Run("signature byte", func(t *testing.T) {
		parts := strings.Split(token, ".")
		signature, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			t.Fatal(err)
		}
		signature[0] ^= 0x80
		parts[2] = base64.RawURLEncoding.EncodeToString(signature)
		_, err = Verify(strings.Join(parts, "."), pub)
		if !errors.Is(err, ErrInvalidSignature) {
			t.Fatalf("Verify() error = %v, want invalid signature", err)
		}
	})

	t.Run("wrong public key", func(t *testing.T) {
		_, wrongPub := testKey(t, 0x22)
		if _, err := Verify(token, wrongPub); err == nil {
			t.Fatal("Verify() unexpectedly accepted a different public key")
		}
	})
}

func TestVerifyRejectsMutationOfEveryPayloadByte(t *testing.T) {
	priv, pub := testKey(t, 0x23)
	parts := strings.Split(mustSign(t, priv, validPayload()), ".")
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	for index := range payloadJSON {
		mutated := append([]byte(nil), payloadJSON...)
		mutated[index] ^= 0x01
		parts[1] = base64.RawURLEncoding.EncodeToString(mutated)
		if _, err := Verify(strings.Join(parts, "."), pub); !errors.Is(err, ErrInvalidSignature) {
			t.Fatalf("payload byte %d mutation error = %v, want invalid signature", index, err)
		}
	}
}

func TestVerifyRejectsProtectedHeaderConfusion(t *testing.T) {
	priv, pub := testKey(t, 0x31)
	kid, _ := KeyID(pub)
	payloadJSON, err := json.Marshal(withSigningFields(validPayload(), kid))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		headerJSON string
		want       error
	}{
		{name: "wrong algorithm", headerJSON: `{"alg":"HS256","kid":"` + kid + `"}`, want: ErrUnsupportedAlgorithm},
		{name: "wrong kid", headerJSON: `{"alg":"EdDSA","kid":"0000000000000000"}`, want: ErrKIDMismatch},
		{name: "unknown protected field", headerJSON: `{"alg":"EdDSA","kid":"` + kid + `","jku":"https://attacker.invalid"}`, want: ErrMalformedToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token := signRaw(priv, []byte(tt.headerJSON), payloadJSON)
			_, err := Verify(token, pub)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Verify() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestVerifyRejectsInvalidSignedPayload(t *testing.T) {
	priv, pub := testKey(t, 0x41)
	kid, _ := KeyID(pub)
	headerJSON, _ := json.Marshal(protectedHeader{Alg: Algorithm, KID: kid})

	tests := []struct {
		name        string
		payloadJSON string
		want        error
	}{
		{name: "wrong version", payloadJSON: `{"v":"purify-receipt/2","url":"https://example.com","path":"price","value":1,"issued_at":"2026-08-09T00:00:00Z","kid":"` + kid + `"}`, want: ErrInvalidPayload},
		{name: "payload kid differs", payloadJSON: `{"v":"purify-receipt/1","url":"https://example.com","path":"price","value":1,"issued_at":"2026-08-09T00:00:00Z","kid":"0000000000000000"}`, want: ErrKIDMismatch},
		{name: "missing URL", payloadJSON: `{"v":"purify-receipt/1","url":"","path":"price","value":1,"issued_at":"2026-08-09T00:00:00Z","kid":"` + kid + `"}`, want: ErrInvalidPayload},
		{name: "unknown payload field", payloadJSON: `{"v":"purify-receipt/1","url":"https://example.com","path":"price","value":1,"issued_at":"2026-08-09T00:00:00Z","kid":"` + kid + `","admin":true}`, want: ErrInvalidPayload},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token := signRaw(priv, headerJSON, []byte(tt.payloadJSON))
			_, err := Verify(token, pub)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Verify() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestSignRejectsInvalidInputs(t *testing.T) {
	priv, pub := testKey(t, 0x51)
	kid, _ := KeyID(pub)
	tests := []struct {
		name    string
		mutate  func(*Payload)
		wantErr error
	}{
		{name: "missing URL", mutate: func(p *Payload) { p.URL = "" }, wantErr: ErrInvalidPayload},
		{name: "missing path", mutate: func(p *Payload) { p.Path = "" }, wantErr: ErrInvalidPayload},
		{name: "invalid value JSON", mutate: func(p *Payload) { p.Value = json.RawMessage(`{`) }, wantErr: ErrInvalidPayload},
		{name: "wrong version", mutate: func(p *Payload) { p.V = "purify-receipt/2" }, wantErr: ErrInvalidPayload},
		{name: "wrong kid", mutate: func(p *Payload) { p.KID = "0000000000000000" }, wantErr: ErrKIDMismatch},
		{name: "missing anchor snapshot", mutate: func(p *Payload) { p.Anchor.SnapshotID = "" }, wantErr: ErrInvalidPayload},
		{name: "missing anchor time", mutate: func(p *Payload) { p.Anchor.FetchedAt = time.Time{} }, wantErr: ErrInvalidPayload},
		{name: "unknown anchor method", mutate: func(p *Payload) { p.Anchor.Method = "invented" }, wantErr: ErrInvalidPayload},
		{name: "right explicit kid", mutate: func(p *Payload) { p.KID = kid }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := validPayload()
			tt.mutate(&payload)
			_, err := Sign(payload, priv)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Sign() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestVerifyRejectsMalformedTokens(t *testing.T) {
	_, pub := testKey(t, 0x61)
	tests := []string{
		"",
		"one.two",
		"one.two.three.four",
		"..",
		"***.e30.AA",
		"e30.***.AA",
		" e30.e30.AA",
	}
	for _, token := range tests {
		if _, err := Verify(token, pub); !errors.Is(err, ErrMalformedToken) {
			t.Errorf("Verify(%q) error = %v, want malformed token", token, err)
		}
	}
}

func TestKeyValidation(t *testing.T) {
	priv, pub := testKey(t, 0x71)
	if _, err := KeyID(pub[:8]); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("KeyID(short) error = %v, want invalid key", err)
	}
	if _, err := PublicKey(priv[:32]); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("PublicKey(short) error = %v, want invalid key", err)
	}
	malformed := append(ed25519.PrivateKey(nil), priv...)
	malformed[len(malformed)-1] ^= 1
	if _, err := PublicKey(malformed); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("PublicKey(malformed) error = %v, want invalid key", err)
	}
	if _, err := Verify("a.b.c", pub[:8]); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("Verify(short key) error = %v, want invalid key", err)
	}
}

func validPayload() Payload {
	return Payload{
		URL:   "https://example.com/product",
		Path:  "price",
		Value: json.RawMessage(`29.99`),
		Anchor: evidence.Anchor{
			Quote:      "$29.99",
			Method:     evidence.MethodExact,
			SnapshotID: "sha256:abc",
			FetchedAt:  time.Date(2026, time.August, 9, 0, 0, 0, 0, time.UTC),
		},
		IssuedAt: time.Date(2026, time.August, 9, 0, 0, 0, 0, time.UTC),
	}
}

func withSigningFields(payload Payload, kid string) Payload {
	payload.V = Version
	payload.KID = kid
	return payload
}

func mustSign(t *testing.T, priv ed25519.PrivateKey, payload Payload) string {
	t.Helper()
	token, err := Sign(payload, priv)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	return token
}

func signRaw(priv ed25519.PrivateKey, headerJSON, payloadJSON []byte) string {
	headerPart := base64.RawURLEncoding.EncodeToString(headerJSON)
	payloadPart := base64.RawURLEncoding.EncodeToString(payloadJSON)
	input := headerPart + "." + payloadPart
	signature := ed25519.Sign(priv, []byte(input))
	return input + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func testKey(t *testing.T, seedByte byte) (ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	seed := bytes.Repeat([]byte{seedByte}, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)
	pub, err := PublicKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	// Make the expected KID visible when a failure prints the key.
	if _, err := hex.DecodeString(mustKeyID(t, pub)); err != nil {
		t.Fatal(err)
	}
	return priv, pub
}

func mustKeyID(t *testing.T, pub ed25519.PublicKey) string {
	t.Helper()
	kid, err := KeyID(pub)
	if err != nil {
		t.Fatal(err)
	}
	return kid
}
