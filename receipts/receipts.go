// Package receipts creates and verifies portable, Ed25519-signed evidence
// receipts. Tokens use the compact JWS shape but intentionally support only
// the single algorithm and payload version defined by Purify.
package receipts

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/use-agent/purify/evidence"
)

const (
	// Version is the only receipt payload version understood by this package.
	Version = "purify-receipt/1"
	// Algorithm is the compact JWS alg value used for Ed25519 signatures.
	Algorithm = "EdDSA"

	maxTokenBytes = 1 << 20
)

var (
	ErrInvalidKey           = errors.New("receipts: invalid Ed25519 key")
	ErrInvalidPayload       = errors.New("receipts: invalid payload")
	ErrMalformedToken       = errors.New("receipts: malformed token")
	ErrUnsupportedAlgorithm = errors.New("receipts: unsupported algorithm")
	ErrKIDMismatch          = errors.New("receipts: key ID mismatch")
	ErrInvalidSignature     = errors.New("receipts: invalid signature")
)

// Payload is the self-contained claim carried by a signed receipt.
type Payload struct {
	V                string          `json:"v"`
	URL              string          `json:"url"`
	Path             string          `json:"path"`
	Value            json.RawMessage `json:"value"`
	Anchor           evidence.Anchor `json:"anchor"`
	ExtractorVersion string          `json:"extractor_version,omitempty"`
	IssuedAt         time.Time       `json:"issued_at"`
	KID              string          `json:"kid"`
}

type protectedHeader struct {
	Alg string `json:"alg"`
	KID string `json:"kid"`
}

// Signer owns an immutable Ed25519 key pair used by the HTTP service. It
// exposes only signing, verification, and defensive public-key access.
type Signer struct {
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
	kid        string
}

// NewSigner validates and defensively copies a private key.
func NewSigner(priv ed25519.PrivateKey) (*Signer, error) {
	pub, err := PublicKey(priv)
	if err != nil {
		return nil, err
	}
	kid, err := KeyID(pub)
	if err != nil {
		return nil, err
	}
	privateCopy := append(ed25519.PrivateKey(nil), priv...)
	publicCopy := append(ed25519.PublicKey(nil), pub...)
	return &Signer{privateKey: privateCopy, publicKey: publicCopy, kid: kid}, nil
}

// Sign creates a receipt with the signer's private key.
func (s *Signer) Sign(payload Payload) (string, error) {
	if s == nil {
		return "", ErrInvalidKey
	}
	return Sign(payload, s.privateKey)
}

// Verify validates a receipt against the signer's public key.
func (s *Signer) Verify(token string) (*Payload, error) {
	if s == nil {
		return nil, ErrInvalidKey
	}
	return Verify(token, s.publicKey)
}

// PublicKey returns a defensive copy of the signer's public key.
func (s *Signer) PublicKey() ed25519.PublicKey {
	if s == nil {
		return nil
	}
	return append(ed25519.PublicKey(nil), s.publicKey...)
}

// KID returns the stable key identifier advertised in receipt headers.
func (s *Signer) KID() string {
	if s == nil {
		return ""
	}
	return s.kid
}

// KeyID returns the lowercase hex encoding of the first eight bytes of an
// Ed25519 public key.
func KeyID(pub ed25519.PublicKey) (string, error) {
	if len(pub) != ed25519.PublicKeySize {
		return "", fmt.Errorf("%w: public key length %d, want %d", ErrInvalidKey, len(pub), ed25519.PublicKeySize)
	}
	return hex.EncodeToString(pub[:8]), nil
}

// PublicKey derives and returns a defensive copy of priv's public key. It also
// rejects malformed 64-byte values whose public suffix does not match the
// embedded seed.
func PublicKey(priv ed25519.PrivateKey) (ed25519.PublicKey, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%w: private key length %d, want %d", ErrInvalidKey, len(priv), ed25519.PrivateKeySize)
	}
	expected := ed25519.NewKeyFromSeed(priv[:ed25519.SeedSize])
	if !bytes.Equal(expected, priv) {
		return nil, fmt.Errorf("%w: private key seed and public suffix disagree", ErrInvalidKey)
	}
	pub := make(ed25519.PublicKey, ed25519.PublicKeySize)
	copy(pub, expected[ed25519.SeedSize:])
	return pub, nil
}

// Sign serializes p and returns a compact JWS token signed with priv. Version,
// key ID, and issuance time are populated when omitted; explicitly supplied
// version or key ID values must match the signing key and current format.
func Sign(p Payload, priv ed25519.PrivateKey) (string, error) {
	pub, err := PublicKey(priv)
	if err != nil {
		return "", err
	}
	kid, err := KeyID(pub)
	if err != nil {
		return "", err
	}

	if p.V == "" {
		p.V = Version
	}
	if p.V != Version {
		return "", fmt.Errorf("%w: unsupported version %q", ErrInvalidPayload, p.V)
	}
	if p.KID == "" {
		p.KID = kid
	}
	if p.KID != kid {
		return "", fmt.Errorf("%w: payload has %q, signing key has %q", ErrKIDMismatch, p.KID, kid)
	}
	if p.IssuedAt.IsZero() {
		p.IssuedAt = time.Now().UTC()
	} else {
		p.IssuedAt = p.IssuedAt.UTC()
	}
	if err := validatePayload(&p); err != nil {
		return "", err
	}

	headerJSON, err := json.Marshal(protectedHeader{Alg: Algorithm, KID: kid})
	if err != nil {
		return "", fmt.Errorf("%w: encode protected header: %v", ErrMalformedToken, err)
	}
	payloadJSON, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("%w: encode payload: %v", ErrInvalidPayload, err)
	}

	headerPart := base64.RawURLEncoding.EncodeToString(headerJSON)
	payloadPart := base64.RawURLEncoding.EncodeToString(payloadJSON)
	signingInput := headerPart + "." + payloadPart
	signature := ed25519.Sign(priv, []byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// Verify authenticates token with pub and returns its validated v1 payload.
func Verify(token string, pub ed25519.PublicKey) (*Payload, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: public key length %d, want %d", ErrInvalidKey, len(pub), ed25519.PublicKeySize)
	}
	if token == "" || len(token) > maxTokenBytes || strings.TrimSpace(token) != token {
		return nil, ErrMalformedToken
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, ErrMalformedToken
	}

	headerJSON, err := decodePart(parts[0], "protected header")
	if err != nil {
		return nil, err
	}
	payloadJSON, err := decodePart(parts[1], "payload")
	if err != nil {
		return nil, err
	}
	signature, err := decodePart(parts[2], "signature")
	if err != nil {
		return nil, err
	}
	var header protectedHeader
	if err := decodeStrictJSON(headerJSON, &header); err != nil {
		return nil, fmt.Errorf("%w: protected header: %v", ErrMalformedToken, err)
	}
	if header.Alg != Algorithm {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedAlgorithm, header.Alg)
	}
	expectedKID, _ := KeyID(pub)
	if header.KID != expectedKID {
		return nil, fmt.Errorf("%w: header has %q, public key has %q", ErrKIDMismatch, header.KID, expectedKID)
	}

	if len(signature) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%w: signature length %d, want %d", ErrMalformedToken, len(signature), ed25519.SignatureSize)
	}
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), signature) {
		return nil, ErrInvalidSignature
	}

	var payload Payload
	if err := decodeStrictJSON(payloadJSON, &payload); err != nil {
		return nil, fmt.Errorf("%w: decode JSON: %v", ErrInvalidPayload, err)
	}
	if payload.KID != header.KID {
		return nil, fmt.Errorf("%w: payload has %q, header has %q", ErrKIDMismatch, payload.KID, header.KID)
	}
	if err := validatePayload(&payload); err != nil {
		return nil, err
	}
	return &payload, nil
}

func validatePayload(p *Payload) error {
	if p.V != Version {
		return fmt.Errorf("%w: unsupported version %q", ErrInvalidPayload, p.V)
	}
	if strings.TrimSpace(p.URL) == "" {
		return fmt.Errorf("%w: URL is required", ErrInvalidPayload)
	}
	if strings.TrimSpace(p.Path) == "" {
		return fmt.Errorf("%w: path is required", ErrInvalidPayload)
	}
	if len(p.Value) == 0 || !json.Valid(p.Value) {
		return fmt.Errorf("%w: value must be valid JSON", ErrInvalidPayload)
	}
	if p.Anchor.SnapshotID == "" {
		return fmt.Errorf("%w: anchor snapshot_id is required", ErrInvalidPayload)
	}
	if p.Anchor.FetchedAt.IsZero() {
		return fmt.Errorf("%w: anchor fetched_at is required", ErrInvalidPayload)
	}
	switch p.Anchor.Method {
	case evidence.MethodExact, evidence.MethodNormalized, evidence.MethodFuzzy, evidence.MethodUnlocated, evidence.MethodCompiled:
	default:
		return fmt.Errorf("%w: unsupported anchor method %q", ErrInvalidPayload, p.Anchor.Method)
	}
	if p.IssuedAt.IsZero() {
		return fmt.Errorf("%w: issued_at is required", ErrInvalidPayload)
	}
	if p.KID == "" {
		return fmt.Errorf("%w: kid is required", ErrInvalidPayload)
	}
	return nil
}

func decodePart(part, name string) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return nil, fmt.Errorf("%w: decode %s: %v", ErrMalformedToken, name, err)
	}
	return decoded, nil
}

func decodeStrictJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
