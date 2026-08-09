// Package verify re-executes evidence-backed scalar claims against a current
// page observation. It is transport neutral: production fetching, HTTP/MCP
// adapters, persistence, receipt signing, and change delivery are dependencies.
package verify

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/andybalholm/cascadia"
	"github.com/use-agent/purify/compiler"
	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/receipts"
	"github.com/use-agent/purify/simhash"
	"github.com/use-agent/purify/snapshot"
	"github.com/use-agent/purify/webhook"
	"golang.org/x/net/idna"
)

const (
	factChangedEventType      = "fact.changed"
	maximumClaims             = 100
	maximumURLBytes           = 16 << 10
	maximumReceiptBytes       = 2 << 20
	maximumPathBytes          = 4 << 10
	maximumScalarBytes        = 8 << 10
	maximumQuoteBytes         = 8 << 10
	maximumSelectorBytes      = 4 << 10
	maximumClaimsBytes        = 512 << 10
	maximumWebhookSecretBytes = 16 << 10
	maximumPageBytes          = 4 << 20
	maximumCanonicalTextBytes = 2 << 20
	maximumNumberTokenBytes   = 256
	maximumQuoteOccurrences   = 10_000
)

var (
	ErrNotConfigured       = errors.New("verify: service is not configured")
	ErrInvalidRequest      = errors.New("verify: invalid request")
	ErrInvalidClaim        = errors.New("verify: invalid claim")
	ErrInvalidReceipt      = errors.New("verify: invalid receipt")
	ErrSnapshot            = errors.New("verify: read old snapshot")
	ErrRevisit             = errors.New("verify: revisit failed")
	ErrRevisitStatus       = errors.New("verify: revisit returned unusable status")
	ErrReceiptSigning      = errors.New("verify: sign refreshed receipt")
	ErrRecord              = errors.New("verify: record verification")
	ErrEvidenceUnavailable = errors.New("verify: compiled evidence is unavailable")
)

// HTTPStatusError reports a definite response that cannot be interpreted as
// either a current page or page-gone. In particular, authorization, rate-limit,
// and server failures remain request-level errors rather than fact verdicts.
type HTTPStatusError struct {
	StatusCode int
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("verify: revisit returned HTTP status %d", e.StatusCode)
}

func (e *HTTPStatusError) Is(target error) bool { return target == ErrRevisitStatus }

// RevisitResult is the complete current observation needed by the core. A
// production adapter may use any fetch/clean implementation, but is not part
// of this package.
type RevisitResult struct {
	StatusCode int
	FinalURL   string
	RawHTML    string
	SnapshotID string
	FetchedAt  time.Time
}

// Revisitor obtains one current page observation.
type Revisitor interface {
	Revisit(context.Context, string) (RevisitResult, error)
}

// SnapshotReader loads the immutable old HTML identified by a claim anchor.
type SnapshotReader interface {
	Content(snapshot.ID, int) ([]byte, error)
	HasObservation(snapshot.ID, func(snapshot.Meta) bool) (bool, error)
}

// ReceiptCodec authenticates old claims and signs refreshed evidence.
type ReceiptCodec interface {
	Verify(string) (*receipts.Payload, error)
	Sign(receipts.Payload) (string, error)
}

// VerificationRecorder commits all claim rows and an optional delivery event
// as one transaction.
type VerificationRecorder interface {
	RecordVerificationBatch(context.Context, []ledger.Verification, *ledger.OutboxEvent) error
}

// ExtractorRevisionResolver resolves an immutable compiled revision by UUID.
// compiler.Store satisfies this interface directly.
type ExtractorRevisionResolver interface {
	Get(context.Context, string) (compiler.Extractor, error)
}

// FactChange is one changed or gone scalar delivered only after its ledger
// transaction commits successfully. Gone changes deliberately omit a new
// value, evidence, and receipt rather than fabricating replacement evidence.
type FactChange struct {
	Path      string                 `json:"path"`
	Status    models.VerifyStatus    `json:"status"`
	GoneScope models.VerifyGoneScope `json:"gone_scope,omitempty"`
	OldValue  json.RawMessage        `json:"old_value"`
	NewValue  json.RawMessage        `json:"new_value,omitempty"`
	Evidence  evidence.Anchor        `json:"evidence,omitempty"`
	Receipt   string                 `json:"receipt,omitempty"`
}

// MarshalJSON preserves the historical value-typed Evidence field for Go API
// compatibility while omitting replacement evidence from gone wire events.
func (change FactChange) MarshalJSON() ([]byte, error) {
	type wireFactChange struct {
		Path      string                 `json:"path"`
		Status    models.VerifyStatus    `json:"status"`
		GoneScope models.VerifyGoneScope `json:"gone_scope,omitempty"`
		OldValue  json.RawMessage        `json:"old_value"`
		NewValue  json.RawMessage        `json:"new_value,omitempty"`
		Evidence  *evidence.Anchor       `json:"evidence,omitempty"`
		Receipt   string                 `json:"receipt,omitempty"`
	}
	var replacement *evidence.Anchor
	if change.Status != models.VerifyStatusGone {
		anchor := change.Evidence
		replacement = &anchor
	}
	return json.Marshal(wireFactChange{
		Path:      change.Path,
		Status:    change.Status,
		GoneScope: change.GoneScope,
		OldValue:  change.OldValue,
		NewValue:  change.NewValue,
		Evidence:  replacement,
		Receipt:   change.Receipt,
	})
}

// ChangedEvent groups every changed or gone claim from one atomic
// verification. The historical event name remains fact.changed for wire
// compatibility.
type ChangedEvent struct {
	VerificationID string       `json:"verification_id"`
	URL            string       `json:"url"`
	FinalURL       string       `json:"final_url"`
	VerifiedAt     time.Time    `json:"verified_at"`
	Changes        []FactChange `json:"changes"`
}

// IDGenerator returns one transaction identity shared by all claim rows.
type IDGenerator func() (string, error)

// Config supplies all transport- and environment-specific dependencies.
type Config struct {
	Revisitor          Revisitor
	Snapshots          SnapshotReader
	Receipts           ReceiptCodec
	Recorder           VerificationRecorder
	IDs                IDGenerator
	ExtractorRevisions ExtractorRevisionResolver
}

// Service executes three-state verification.
type Service struct {
	revisitor          Revisitor
	snapshots          SnapshotReader
	receipts           ReceiptCodec
	recorder           VerificationRecorder
	ids                IDGenerator
	extractorRevisions ExtractorRevisionResolver
}

// NewService validates the core dependencies. Every successful verification is
// signed, compared to its old snapshot, and recorded atomically.
func NewService(config Config) (*Service, error) {
	if config.Revisitor == nil || config.Snapshots == nil || config.Receipts == nil || config.Recorder == nil {
		return nil, ErrNotConfigured
	}
	ids := config.IDs
	if ids == nil {
		ids = randomVerificationID
	}
	return &Service{
		revisitor:          config.Revisitor,
		snapshots:          config.Snapshots,
		receipts:           config.Receipts,
		recorder:           config.Recorder,
		ids:                ids,
		extractorRevisions: config.ExtractorRevisions,
	}, nil
}

type compiledClaim struct {
	id                string
	version           int
	schemaHash        string
	templateClusterID string
	ir                compiler.IR
}

type resolvedClaim struct {
	claim            models.Claim
	scalar           scalarValue
	quoteVerifiable  bool
	oldReceipt       string
	extractorVersion string
	compiled         *compiledClaim
}

// Verify revisits one URL, adjudicates every claim, and records all rows plus
// at most one aggregate changed event in one transaction.
func (s *Service) Verify(ctx context.Context, request models.VerifyRequest) (*models.VerifyResponse, error) {
	if s == nil || s.revisitor == nil || s.snapshots == nil || s.receipts == nil || s.recorder == nil || s.ids == nil {
		return nil, ErrNotConfigured
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is nil", ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	webhookURL, webhookSecret, err := resolveWebhook(request.WebhookURL, request.WebhookSecret)
	if err != nil {
		return nil, err
	}

	targetURL, claims, err := s.resolveRequest(ctx, request)
	if err != nil {
		return nil, err
	}
	oldHTML, err := s.loadOldSnapshot(targetURL, claims)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	observation, err := s.revisitor.Revisit(ctx, targetURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRevisit, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pageGone := observation.StatusCode == 404 || observation.StatusCode == 410
	if !pageGone && (observation.StatusCode < 200 || observation.StatusCode >= 300) {
		return nil, &HTTPStatusError{StatusCode: observation.StatusCode}
	}
	if observation.FetchedAt.IsZero() {
		return nil, fmt.Errorf("%w: revisit fetched_at is required", ErrRevisit)
	}
	observation.SnapshotID = strings.TrimSpace(observation.SnapshotID)
	if observation.SnapshotID == "" {
		return nil, fmt.Errorf("%w: definitive revisit snapshot_id is required", ErrRevisit)
	}
	if strings.TrimSpace(observation.FinalURL) == "" {
		observation.FinalURL = targetURL
	}
	observation.FinalURL, err = canonicalHTTPURL(observation.FinalURL)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid final URL: %v", ErrRevisit, err)
	}
	if !pageGone && observation.RawHTML == "" {
		return nil, fmt.Errorf("%w: successful revisit raw HTML is required", ErrRevisit)
	}
	if len(observation.RawHTML) > maximumPageBytes {
		return nil, fmt.Errorf("%w: revisit raw HTML exceeds %d bytes", ErrRevisit, maximumPageBytes)
	}
	if err := validateSnapshotID(observation.SnapshotID); err != nil {
		return nil, fmt.Errorf("%w: invalid revisit snapshot_id: %v", ErrRevisit, err)
	}
	if err := s.validateCurrentSnapshot(observation); err != nil {
		return nil, err
	}

	var pageSimilarity *float64
	var currentPage *canonicalPage
	var currentQuotes quoteCorpus
	if !pageGone {
		pageSimilarity = floatPointer(similarity(string(oldHTML), observation.RawHTML))
		currentPage, err = parseCanonicalPage(observation.RawHTML)
		if err != nil {
			return nil, fmt.Errorf("%w: canonicalize current HTML: %v", ErrRevisit, err)
		}
		currentQuotes = newQuoteCorpus(currentPage.text)
	}
	verifiedAt := observation.FetchedAt.UTC()

	results := make([]models.ClaimResult, len(claims))
	rows := make([]ledger.Verification, len(claims))
	changes := make([]FactChange, 0, len(claims))
	for index := range claims {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result, row, change, err := s.verifyOne(
			targetURL,
			observation,
			verifiedAt,
			pageSimilarity,
			pageGone,
			currentPage,
			currentQuotes,
			claims[index],
		)
		if err != nil {
			return nil, err
		}
		results[index] = result
		rows[index] = row
		rows[index].ClaimIndex = index
		if change != nil {
			changes = append(changes, cloneFactChange(*change))
		}
	}

	verificationID, err := s.ids()
	if err != nil {
		return nil, fmt.Errorf("verify: generate verification id: %w", err)
	}
	if strings.TrimSpace(verificationID) == "" {
		return nil, fmt.Errorf("verify: generate verification id: empty id")
	}
	for index := range rows {
		rows[index].VerificationID = verificationID
	}
	var outboxEvent *ledger.OutboxEvent
	if len(changes) > 0 && webhookURL != "" {
		changed := ChangedEvent{
			VerificationID: verificationID,
			URL:            targetURL,
			FinalURL:       observation.FinalURL,
			VerifiedAt:     verifiedAt,
			Changes:        cloneFactChanges(changes),
		}
		outboxEvent, err = changedOutboxEvent(webhookURL, webhookSecret, changed)
		if err != nil {
			return nil, err
		}
	}
	if err := s.recorder.RecordVerificationBatch(ctx, rows, outboxEvent); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRecord, err)
	}

	return &models.VerifyResponse{
		VerificationID: verificationID,
		URL:            targetURL,
		FinalURL:       observation.FinalURL,
		StatusCode:     observation.StatusCode,
		Results:        results,
		PageSimilarity: cloneFloatPointer(pageSimilarity),
		SnapshotID:     observation.SnapshotID,
		VerifiedAt:     verifiedAt,
	}, nil
}

func resolveWebhook(rawURL, secret string) (string, string, error) {
	if len(rawURL) > ledger.MaxOutboxURLBytes {
		return "", "", fmt.Errorf("%w: webhook_url exceeds %d bytes", ErrInvalidRequest, ledger.MaxOutboxURLBytes)
	}
	if len(secret) > maximumWebhookSecretBytes || len(secret) > ledger.MaxOutboxSecretBytes {
		return "", "", fmt.Errorf("%w: webhook_secret exceeds %d bytes", ErrInvalidRequest, maximumWebhookSecretBytes)
	}
	if strings.TrimSpace(rawURL) == "" {
		if secret != "" {
			return "", "", fmt.Errorf("%w: webhook_secret requires webhook_url", ErrInvalidRequest)
		}
		return "", "", nil
	}
	canonical, err := canonicalHTTPURL(rawURL)
	if err != nil {
		return "", "", fmt.Errorf("%w: invalid webhook_url: %v", ErrInvalidRequest, err)
	}
	return canonical, secret, nil
}

func changedOutboxEvent(destinationURL, secret string, changed ChangedEvent) (*ledger.OutboxEvent, error) {
	payload, err := json.Marshal(webhook.Event{
		Type:      factChangedEventType,
		JobID:     changed.VerificationID,
		Timestamp: changed.VerifiedAt.Unix(),
		Data:      changed,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: encode fact.changed event: %v", ErrInvalidRequest, err)
	}
	if len(payload) > ledger.MaxOutboxPayloadBytes {
		return nil, fmt.Errorf(
			"%w: fact.changed event exceeds %d bytes",
			ErrInvalidRequest,
			ledger.MaxOutboxPayloadBytes,
		)
	}
	return &ledger.OutboxEvent{
		ID:             changed.VerificationID,
		VerificationID: changed.VerificationID,
		Type:           factChangedEventType,
		URL:            destinationURL,
		Secret:         secret,
		Payload:        append(json.RawMessage(nil), payload...),
		CreatedAt:      changed.VerifiedAt.UTC(),
		NextAttemptAt:  changed.VerifiedAt.UTC(),
	}, nil
}

func (s *Service) resolveRequest(ctx context.Context, request models.VerifyRequest) (string, []resolvedClaim, error) {
	hasClaims := len(request.Claims) > 0
	hasReceipt := strings.TrimSpace(request.Receipt) != ""
	if hasClaims == hasReceipt {
		return "", nil, fmt.Errorf("%w: claims and receipt must be provided in strict XOR", ErrInvalidRequest)
	}
	if len(request.URL) > maximumURLBytes {
		return "", nil, fmt.Errorf("%w: URL exceeds %d bytes", ErrInvalidRequest, maximumURLBytes)
	}
	if len(request.Claims) > maximumClaims {
		return "", nil, fmt.Errorf("%w: claims exceeds the limit of %d", ErrInvalidRequest, maximumClaims)
	}
	if len(request.Receipt) > maximumReceiptBytes {
		return "", nil, fmt.Errorf("%w: receipt exceeds %d bytes", ErrInvalidReceipt, maximumReceiptBytes)
	}

	var targetURL string
	resolved := make([]resolvedClaim, 0, max(len(request.Claims), 1))
	if hasReceipt {
		payload, err := s.receipts.Verify(request.Receipt)
		if err != nil {
			return "", nil, fmt.Errorf("%w: %w", ErrInvalidReceipt, err)
		}
		if payload == nil {
			return "", nil, fmt.Errorf("%w: receipt verifier returned no payload", ErrInvalidReceipt)
		}
		targetURL, err = canonicalHTTPURL(payload.URL)
		if err != nil {
			return "", nil, fmt.Errorf("%w: receipt URL is invalid: %v", ErrInvalidReceipt, err)
		}
		if strings.TrimSpace(request.URL) != "" {
			requestURL, requestErr := canonicalHTTPURL(request.URL)
			if requestErr != nil {
				return "", nil, fmt.Errorf("%w: invalid request URL: %v", ErrInvalidRequest, requestErr)
			}
			if requestURL != targetURL {
				return "", nil, fmt.Errorf("%w: receipt URL does not match request URL", ErrInvalidReceipt)
			}
		}
		claim := models.Claim{Path: payload.Path, Value: cloneRaw(payload.Value), Anchor: payload.Anchor}
		receiptClaim := resolvedClaim{
			claim:            claim,
			oldReceipt:       request.Receipt,
			extractorVersion: payload.ExtractorVersion,
		}
		if payload.Anchor.Method == evidence.MethodCompiled {
			if strings.TrimSpace(payload.ExtractorVersion) == "" {
				return "", nil, fmt.Errorf("%w: compiled receipt has no extractor revision", ErrInvalidReceipt)
			}
			compiled, resolveErr := s.resolveCompiledRevision(ctx, payload.ExtractorVersion)
			if resolveErr != nil {
				return "", nil, resolveErr
			}
			receiptClaim.compiled = compiled
		} else if strings.TrimSpace(payload.ExtractorVersion) != "" {
			return "", nil, fmt.Errorf("%w: extractor revision requires compiled evidence", ErrInvalidReceipt)
		}
		resolved = append(resolved, receiptClaim)
	} else {
		var err error
		targetURL, err = canonicalHTTPURL(request.URL)
		if err != nil {
			return "", nil, fmt.Errorf("%w: invalid request URL: %v", ErrInvalidRequest, err)
		}
		for index := range request.Claims {
			claim := request.Claims[index]
			claim.Value = cloneRaw(claim.Value)
			if claim.Anchor.Method == evidence.MethodCompiled {
				return "", nil, fmt.Errorf("%w %d: compiled evidence requires a signed extractor revision", ErrInvalidClaim, index)
			}
			resolved = append(resolved, resolvedClaim{claim: claim})
		}
	}

	paths := make(map[string]struct{}, len(resolved))
	oldSnapshotID := ""
	aggregateBytes := 0
	for index := range resolved {
		claim := &resolved[index]
		claimInputError := ErrInvalidClaim
		if claim.oldReceipt != "" {
			claimInputError = ErrInvalidReceipt
		}
		claim.claim.Path = strings.TrimSpace(claim.claim.Path)
		if claim.claim.Path == "" {
			return "", nil, fmt.Errorf("%w %d: path is required", claimInputError, index)
		}
		if len(claim.claim.Path) > maximumPathBytes {
			return "", nil, fmt.Errorf("%w %d: path exceeds %d bytes", claimInputError, index, maximumPathBytes)
		}
		if len(claim.claim.Value) > maximumScalarBytes {
			return "", nil, fmt.Errorf("%w %d: value exceeds %d bytes", claimInputError, index, maximumScalarBytes)
		}
		if len(claim.claim.Anchor.Quote) > maximumQuoteBytes {
			return "", nil, fmt.Errorf("%w %d: quote exceeds %d bytes", claimInputError, index, maximumQuoteBytes)
		}
		if len(claim.claim.Anchor.Selector) > maximumSelectorBytes {
			return "", nil, fmt.Errorf("%w %d: selector exceeds %d bytes", claimInputError, index, maximumSelectorBytes)
		}
		aggregateBytes += len(claim.claim.Path) + len(claim.claim.Value) + len(claim.claim.Anchor.Quote) + len(claim.claim.Anchor.Selector)
		if aggregateBytes > maximumClaimsBytes {
			aggregateError := ErrInvalidRequest
			if claim.oldReceipt != "" {
				aggregateError = ErrInvalidReceipt
			}
			return "", nil, fmt.Errorf("%w: aggregate claim data exceeds %d bytes", aggregateError, maximumClaimsBytes)
		}
		if _, duplicate := paths[claim.claim.Path]; duplicate {
			return "", nil, fmt.Errorf("%w %d: duplicate path %q", claimInputError, index, claim.claim.Path)
		}
		paths[claim.claim.Path] = struct{}{}

		scalar, err := decodeScalar(claim.claim.Value)
		if err != nil {
			return "", nil, fmt.Errorf("%w %d: %v", claimInputError, index, err)
		}
		claim.scalar = scalar
		claim.quoteVerifiable = quoteRepresentsScalar(claim.claim.Anchor.Quote, scalar)
		if err := validateAnchor(claim.claim.Anchor); err != nil {
			return "", nil, fmt.Errorf("%w %d: %v", claimInputError, index, err)
		}
		if oldSnapshotID == "" {
			oldSnapshotID = claim.claim.Anchor.SnapshotID
		} else if claim.claim.Anchor.SnapshotID != oldSnapshotID {
			return "", nil, fmt.Errorf("%w: all claims must reference the same old snapshot", ErrInvalidRequest)
		}
	}
	return targetURL, resolved, nil
}

func (s *Service) resolveCompiledRevision(ctx context.Context, encoded string) (*compiledClaim, error) {
	id, version, err := parseExtractorVersion(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidReceipt, err)
	}
	if s.extractorRevisions == nil {
		return nil, fmt.Errorf("%w: extractor revision resolver is not configured", ErrEvidenceUnavailable)
	}
	revision, err := s.extractorRevisions.Get(ctx, id)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("%w: resolve extractor revision: %w", ErrEvidenceUnavailable, err)
	}
	if revision.ID != id || revision.Version != version {
		return nil, fmt.Errorf("%w: extractor revision identity mismatch", ErrEvidenceUnavailable)
	}
	if !validLowercaseHex(revision.SchemaHash, 64) ||
		!validLowercaseHex(revision.TemplateClusterID, 64) {
		return nil, fmt.Errorf("%w: extractor revision provenance is invalid", ErrEvidenceUnavailable)
	}
	// ReplayField performs the definitive resource and IR validation against
	// each snapshot. Keep a defensive copy so a mutable test or adapter cannot
	// change the revision after identity validation.
	ir := cloneIR(revision.IR)
	return &compiledClaim{
		id:                id,
		version:           version,
		schemaHash:        revision.SchemaHash,
		templateClusterID: revision.TemplateClusterID,
		ir:                ir,
	}, nil
}

func parseExtractorVersion(value string) (string, int, error) {
	if value == "" || strings.TrimSpace(value) != value || strings.Count(value, "@") != 1 {
		return "", 0, errors.New("extractor_version must be <lowercase UUID>@<positive version>")
	}
	id, encodedVersion, _ := strings.Cut(value, "@")
	if !validLowercaseUUID(id) || encodedVersion == "" || encodedVersion[0] == '0' {
		return "", 0, errors.New("extractor_version must be <lowercase UUID>@<positive version>")
	}
	for _, character := range encodedVersion {
		if character < '0' || character > '9' {
			return "", 0, errors.New("extractor_version must be <lowercase UUID>@<positive version>")
		}
	}
	version, err := strconv.Atoi(encodedVersion)
	if err != nil || version <= 0 {
		return "", 0, errors.New("extractor_version must be <lowercase UUID>@<positive version>")
	}
	return id, version, nil
}

func validLowercaseUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
			return false
		}
	}
	return true
}

func validLowercaseHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, character := range value {
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
			return false
		}
	}
	return true
}

func cloneIR(value compiler.IR) compiler.IR {
	copy := compiler.IR{Version: value.Version, Fields: make([]compiler.FieldRule, len(value.Fields))}
	for index := range value.Fields {
		copy.Fields[index] = value.Fields[index]
		copy.Fields[index].Transforms = append([]string(nil), value.Fields[index].Transforms...)
	}
	return copy
}

func validateAnchor(anchor evidence.Anchor) error {
	if strings.TrimSpace(anchor.SnapshotID) == "" {
		return errors.New("anchor snapshot_id is required")
	}
	if strings.TrimSpace(anchor.SnapshotID) != anchor.SnapshotID {
		return errors.New("anchor snapshot_id cannot contain surrounding whitespace")
	}
	if err := validateSnapshotID(anchor.SnapshotID); err != nil {
		return fmt.Errorf("anchor %w", err)
	}
	if anchor.FetchedAt.IsZero() {
		return errors.New("anchor fetched_at is required")
	}
	if anchor.TextRange[0] < 0 || anchor.TextRange[1] < anchor.TextRange[0] {
		return errors.New("anchor text_range must be a non-negative half-open range")
	}
	if strings.TrimSpace(anchor.Quote) == "" && strings.TrimSpace(anchor.Selector) == "" {
		return errors.New("anchor must contain a quote or selector")
	}
	if anchor.Selector != "" {
		if _, err := cascadia.Compile(anchor.Selector); err != nil {
			return fmt.Errorf("anchor selector is invalid: %w", err)
		}
	}
	switch anchor.Method {
	case evidence.MethodExact, evidence.MethodNormalized, evidence.MethodFuzzy, evidence.MethodCompiled:
		return nil
	case evidence.MethodUnlocated:
		return errors.New("unlocated evidence cannot be verified")
	default:
		return fmt.Errorf("unsupported anchor method %q", anchor.Method)
	}
}

func validateSnapshotID(identifier string) error {
	const snapshotPrefix = "sha256:"
	if !strings.HasPrefix(identifier, snapshotPrefix) || len(identifier) != len(snapshotPrefix)+64 {
		return errors.New("snapshot_id must be sha256 followed by 64 hexadecimal characters")
	}
	digest := strings.TrimPrefix(identifier, snapshotPrefix)
	decodedDigest, err := hex.DecodeString(digest)
	if err != nil || len(decodedDigest) != 32 {
		return errors.New("snapshot_id must be sha256 followed by 64 hexadecimal characters")
	}
	return nil
}

func (s *Service) loadOldSnapshot(targetURL string, claims []resolvedClaim) ([]byte, error) {
	oldSnapshotID := snapshot.ID(claims[0].claim.Anchor.SnapshotID)
	oldHTML, err := s.snapshots.Content(oldSnapshotID, maximumPageBytes)
	if err != nil {
		return nil, fmt.Errorf("%w %q: %w", ErrSnapshot, oldSnapshotID, err)
	}
	if len(oldHTML) > maximumPageBytes {
		return nil, fmt.Errorf("%w %q: HTML exceeds %d bytes", ErrSnapshot, oldSnapshotID, maximumPageBytes)
	}
	requiredTimes := make(map[string]struct{}, len(claims))
	for index := range claims {
		requiredTimes[timeKey(claims[index].claim.Anchor.FetchedAt)] = struct{}{}
	}
	foundAll, err := s.snapshots.HasObservation(oldSnapshotID, func(observation snapshot.Meta) bool {
		observedURL, canonicalErr := canonicalHTTPURL(observation.URL)
		if canonicalErr != nil || observedURL != targetURL || observation.StatusCode < 200 || observation.StatusCode >= 300 {
			return false
		}
		delete(requiredTimes, timeKey(observation.FetchedAt))
		return len(requiredTimes) == 0
	})
	if err != nil {
		return nil, fmt.Errorf("%w %q observations: %w", ErrSnapshot, oldSnapshotID, err)
	}
	if !foundAll || len(requiredTimes) != 0 {
		for index := range claims {
			anchor := claims[index].claim.Anchor
			if _, missing := requiredTimes[timeKey(anchor.FetchedAt)]; missing {
				return nil, fmt.Errorf(
					"%w %q: claim %q has no observation for URL %q at %s",
					ErrSnapshot,
					oldSnapshotID,
					claims[index].claim.Path,
					targetURL,
					anchor.FetchedAt.UTC().Format(time.RFC3339Nano),
				)
			}
		}
		return nil, fmt.Errorf("%w %q: required observation is missing", ErrSnapshot, oldSnapshotID)
	}
	oldPage, err := parseCanonicalPage(string(oldHTML))
	if err != nil {
		return nil, fmt.Errorf("%w %q: canonicalize HTML: %v", ErrSnapshot, oldSnapshotID, err)
	}
	oldQuotes := newQuoteCorpus(oldPage.text)
	for index := range claims {
		if claims[index].compiled != nil {
			replayed, replayErr := replayCompiledClaim(claims[index], string(oldHTML))
			if replayErr != nil {
				return nil, fmt.Errorf("%w: replay old snapshot path %q: %v", ErrEvidenceUnavailable, claims[index].claim.Path, replayErr)
			}
			if !replayed.Found || !rawScalarEqual(replayed.Value, claims[index].claim.Value) {
				return nil, fmt.Errorf("%w: old snapshot does not match compiled receipt path %q", ErrEvidenceUnavailable, claims[index].claim.Path)
			}
			continue
		}
		if !oldSnapshotSupportsClaim(oldPage, oldQuotes, claims[index]) {
			return nil, fmt.Errorf(
				"%w %d: old snapshot %q does not support path %q and value %s",
				ErrInvalidClaim,
				index,
				oldSnapshotID,
				claims[index].claim.Path,
				claims[index].claim.Value,
			)
		}
	}
	return oldHTML, nil
}

func replayCompiledClaim(claim resolvedClaim, html string) (compiler.ReplayResult, error) {
	if claim.compiled == nil {
		return compiler.ReplayResult{}, errors.New("compiled revision is missing")
	}
	fieldName, err := compiledFieldName(claim.claim.Path)
	if err != nil {
		return compiler.ReplayResult{}, err
	}
	return compiler.ReplayField(claim.compiled.ir, fieldName, html)
}

// compiledFieldName reverses evidence's top-level path escaping exactly.
// Compiler IR emits scalar top-level fields only, so an unescaped dot would
// denote a nested path that no immutable FieldRule can truthfully represent.
func compiledFieldName(path string) (string, error) {
	var field strings.Builder
	field.Grow(len(path))
	for index := 0; index < len(path); index++ {
		switch path[index] {
		case '.':
			return "", errors.New("compiled receipt path cannot contain an unescaped separator")
		case '\\':
			if index+1 < len(path) && path[index+1] == '.' {
				field.WriteByte('.')
				index++
				continue
			}
			field.WriteByte('\\')
		default:
			field.WriteByte(path[index])
		}
	}
	name := field.String()
	if strings.ReplaceAll(name, ".", `\.`) != path {
		return "", errors.New("compiled receipt path is not canonically escaped")
	}
	return name, nil
}

func rawScalarEqual(first, second json.RawMessage) bool {
	left, err := decodeScalar(first)
	if err != nil {
		return false
	}
	right, err := decodeScalar(second)
	if err != nil || left.kind != right.kind {
		return false
	}
	// decodeScalar canonicalizes JSON string escapes and insignificant outer
	// whitespace while deliberately retaining a number's lexical form. A
	// compiled replay has already applied every declared transform and emitted
	// its typed value through json.Marshal, so verification must not add the
	// generic verifier's case/whitespace or rational-number equivalence here.
	return bytes.Equal(left.raw, right.raw)
}

func oldSnapshotSupportsClaim(page *canonicalPage, quotes quoteCorpus, claim resolvedClaim) bool {
	if claim.oldReceipt != "" {
		return true
	}
	if claim.claim.Anchor.Selector != "" {
		candidate, _, _, state := page.scalarAtSelector(claim.claim.Anchor.Selector, claim.scalar.kind)
		switch state {
		case selectorValue:
			return claim.scalar.equal(candidate)
		case selectorAmbiguous:
			return false
		}
	}
	if !claim.quoteVerifiable {
		return false
	}
	anchorRange := claim.claim.Anchor.TextRange
	if anchorRange[0] >= 0 && anchorRange[1] > anchorRange[0] && anchorRange[1] <= len(page.text) {
		if _, ok := quotes.matchAt(claim.claim.Anchor.Quote, claim.scalar, anchorRange[0], anchorRange[1]); ok {
			return true
		}
	}
	_, ok := quotes.match(claim.claim.Anchor.Quote, claim.scalar)
	return ok
}

func (s *Service) validateCurrentSnapshot(observation RevisitResult) error {
	currentSnapshotID := snapshot.ID(observation.SnapshotID)
	storedHTML, err := s.snapshots.Content(currentSnapshotID, maximumPageBytes)
	if err != nil {
		return fmt.Errorf("%w current %q: %w", ErrSnapshot, currentSnapshotID, err)
	}
	if !bytes.Equal(storedHTML, []byte(observation.RawHTML)) {
		return fmt.Errorf("%w current %q: stored HTML does not match revisit HTML", ErrSnapshot, currentSnapshotID)
	}
	statusCode := observation.StatusCode
	found, err := s.snapshots.HasObservation(currentSnapshotID, func(meta snapshot.Meta) bool {
		observedURL, canonicalErr := canonicalHTTPURL(meta.URL)
		return canonicalErr == nil && observedURL == observation.FinalURL &&
			meta.FetchedAt.Equal(observation.FetchedAt) && meta.StatusCode == statusCode
	})
	if err != nil {
		return fmt.Errorf("%w current %q observations: %w", ErrSnapshot, currentSnapshotID, err)
	}
	if !found {
		return fmt.Errorf(
			"%w current %q: no observation for URL %q, status %d, at %s",
			ErrSnapshot,
			currentSnapshotID,
			observation.FinalURL,
			observation.StatusCode,
			observation.FetchedAt.UTC().Format(time.RFC3339Nano),
		)
	}
	return nil
}

func timeKey(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func canonicalHTTPURL(rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", errors.New("URL is empty")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse URL: %w", err)
	}
	if !parsed.IsAbs() || parsed.Host == "" {
		return "", errors.New("URL must be absolute")
	}
	if parsed.Opaque != "" {
		return "", errors.New("opaque URLs are not supported")
	}
	if parsed.User != nil {
		return "", errors.New("URL userinfo is not supported")
	}

	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("URL scheme must be http or https")
	}
	hostname := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if hostname == "" {
		return "", errors.New("URL host is empty")
	}
	if strings.Contains(hostname, "%") {
		return "", errors.New("IPv6 zones are not supported")
	}

	canonicalHost := ""
	if address, parseErr := netip.ParseAddr(hostname); parseErr == nil {
		if address.Zone() != "" {
			return "", errors.New("IPv6 zones are not supported")
		}
		canonicalHost = address.String()
	} else {
		canonicalHost, err = idna.Lookup.ToASCII(hostname)
		if err != nil {
			return "", fmt.Errorf("normalize internationalized host: %w", err)
		}
		canonicalHost = strings.TrimSuffix(strings.ToLower(canonicalHost), ".")
		if canonicalHost == "" {
			return "", errors.New("URL host is empty")
		}
	}

	port := parsed.Port()
	if port == "" && hasExplicitEmptyPort(parsed.Host) {
		return "", errors.New("URL port is empty")
	}
	if port != "" {
		portNumber, parseErr := strconv.Atoi(port)
		if parseErr != nil || portNumber < 1 || portNumber > 65_535 {
			return "", errors.New("URL port must be between 1 and 65535")
		}
		port = strconv.Itoa(portNumber)
		if scheme == "http" && port == "80" || scheme == "https" && port == "443" {
			port = ""
		}
	}

	parsed.Scheme = scheme
	switch {
	case port != "":
		parsed.Host = canonicalHostPort(canonicalHost, port)
	case strings.Contains(canonicalHost, ":"):
		parsed.Host = "[" + canonicalHost + "]"
	default:
		parsed.Host = canonicalHost
	}
	if parsed.Path == "" {
		parsed.Path = "/"
		parsed.RawPath = ""
	}
	parsed.Fragment = ""
	parsed.RawFragment = ""
	return parsed.String(), nil
}

func canonicalHostPort(host, port string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

func hasExplicitEmptyPort(authority string) bool {
	if strings.HasPrefix(authority, "[") {
		closing := strings.LastIndex(authority, "]")
		return closing >= 0 && authority[closing+1:] == ":"
	}
	return strings.HasSuffix(authority, ":")
}

func (s *Service) verifyOne(
	targetURL string,
	observation RevisitResult,
	verifiedAt time.Time,
	pageSimilarity *float64,
	pageGone bool,
	page *canonicalPage,
	quotes quoteCorpus,
	claim resolvedClaim,
) (models.ClaimResult, ledger.Verification, *FactChange, error) {
	result := models.ClaimResult{Path: claim.claim.Path}
	row := ledger.Verification{
		URL:            targetURL,
		FinalURL:       observation.FinalURL,
		Path:           claim.claim.Path,
		OldValue:       cloneRaw(claim.claim.Value),
		PageSimilarity: cloneFloatPointer(pageSimilarity),
		OldSnapshotID:  claim.claim.Anchor.SnapshotID,
		NewSnapshotID:  observation.SnapshotID,
		OldReceipt:     claim.oldReceipt,
		VerifiedAt:     verifiedAt,
	}
	if claim.compiled != nil {
		row.SchemaHash = claim.compiled.schemaHash
		row.TemplateClusterID = claim.compiled.templateClusterID
		row.ExtractorID = claim.compiled.id
	}
	if pageGone {
		result.Status = models.VerifyStatusGone
		result.GoneScope = models.VerifyGoneScopePage
		row.Outcome = ledger.OutcomeGone
		row.GoneScope = ledger.GoneScopePage
		return result, row, goneFactChange(claim, models.VerifyGoneScopePage), nil
	}

	if claim.compiled != nil {
		replayed, err := replayCompiledClaim(claim, observation.RawHTML)
		if err != nil {
			return models.ClaimResult{}, ledger.Verification{}, nil, fmt.Errorf(
				"%w: replay current snapshot path %q: %v",
				ErrEvidenceUnavailable,
				claim.claim.Path,
				err,
			)
		}
		if !replayed.Found {
			result.Status = models.VerifyStatusGone
			result.GoneScope = models.VerifyGoneScopeField
			row.Outcome = ledger.OutcomeGone
			row.GoneScope = ledger.GoneScopeField
			return result, row, goneFactChange(claim, models.VerifyGoneScopeField), nil
		}

		anchor := hydrateCompiledAnchor(replayed.Anchor, page.text, observation, verifiedAt)
		if rawScalarEqual(replayed.Value, claim.claim.Value) {
			result.Status = models.VerifyStatusConfirmed
			result.Evidence = anchorPointer(anchor)
			row.Outcome = ledger.OutcomeConfirmed
			receipt, signErr := s.signCurrent(observation.FinalURL, claim, claim.claim.Value, anchor, verifiedAt)
			if signErr != nil {
				return models.ClaimResult{}, ledger.Verification{}, nil, signErr
			}
			result.Receipt = receipt
			row.Receipt = receipt
			return result, row, nil, nil
		}

		result.Status = models.VerifyStatusChanged
		result.NewValue = cloneRaw(replayed.Value)
		result.Evidence = anchorPointer(anchor)
		row.Outcome = ledger.OutcomeChanged
		row.NewValue = cloneRaw(replayed.Value)
		receipt, signErr := s.signCurrent(observation.FinalURL, claim, replayed.Value, anchor, verifiedAt)
		if signErr != nil {
			return models.ClaimResult{}, ledger.Verification{}, nil, signErr
		}
		result.Receipt = receipt
		row.Receipt = receipt
		change := &FactChange{
			Path:     claim.claim.Path,
			Status:   models.VerifyStatusChanged,
			OldValue: cloneRaw(claim.claim.Value),
			NewValue: cloneRaw(replayed.Value),
			Evidence: anchor,
			Receipt:  receipt,
		}
		return result, row, change, nil
	}

	if claim.claim.Anchor.Selector != "" {
		candidate, quote, quoteSpan, selectorState := page.scalarAtSelector(claim.claim.Anchor.Selector, claim.scalar.kind)
		if selectorState == selectorValue {
			anchor := selectorAnchor(quote, claim.claim.Anchor.Selector, quoteSpan, observation, verifiedAt)
			if claim.scalar.equal(candidate) {
				result.Status = models.VerifyStatusConfirmed
				result.Evidence = anchorPointer(anchor)
				row.Outcome = ledger.OutcomeConfirmed
				receipt, err := s.signCurrent(observation.FinalURL, claim, claim.claim.Value, anchor, verifiedAt)
				if err != nil {
					return models.ClaimResult{}, ledger.Verification{}, nil, err
				}
				result.Receipt = receipt
				row.Receipt = receipt
				return result, row, nil, nil
			}

			result.Status = models.VerifyStatusChanged
			result.NewValue = cloneRaw(candidate.raw)
			result.Evidence = anchorPointer(anchor)
			row.Outcome = ledger.OutcomeChanged
			row.NewValue = cloneRaw(candidate.raw)
			receipt, err := s.signCurrent(observation.FinalURL, claim, candidate.raw, anchor, verifiedAt)
			if err != nil {
				return models.ClaimResult{}, ledger.Verification{}, nil, err
			}
			result.Receipt = receipt
			row.Receipt = receipt
			change := &FactChange{
				Path:     claim.claim.Path,
				Status:   models.VerifyStatusChanged,
				OldValue: cloneRaw(claim.claim.Value),
				NewValue: cloneRaw(candidate.raw),
				Evidence: anchor,
				Receipt:  receipt,
			}
			return result, row, change, nil
		}
		if selectorState == selectorAmbiguous {
			result.Status = models.VerifyStatusGone
			result.GoneScope = models.VerifyGoneScopeField
			row.Outcome = ledger.OutcomeGone
			row.GoneScope = ledger.GoneScopeField
			return result, row, goneFactChange(claim, models.VerifyGoneScopeField), nil
		}
	}

	if claim.quoteVerifiable {
		match, matched := quotes.match(claim.claim.Anchor.Quote, claim.scalar)
		if matched {
			anchor, ok := quoteFallback(match, observation, verifiedAt)
			if !ok {
				return models.ClaimResult{}, ledger.Verification{}, nil, fmt.Errorf("%w: quote alignment became inconsistent", ErrRevisit)
			}
			result.Status = models.VerifyStatusConfirmed
			result.Evidence = anchorPointer(anchor)
			row.Outcome = ledger.OutcomeConfirmed
			receipt, err := s.signCurrent(observation.FinalURL, claim, claim.claim.Value, anchor, verifiedAt)
			if err != nil {
				return models.ClaimResult{}, ledger.Verification{}, nil, err
			}
			result.Receipt = receipt
			row.Receipt = receipt
			return result, row, nil, nil
		}
	}

	result.Status = models.VerifyStatusGone
	result.GoneScope = models.VerifyGoneScopeField
	row.Outcome = ledger.OutcomeGone
	row.GoneScope = ledger.GoneScopeField
	return result, row, goneFactChange(claim, models.VerifyGoneScopeField), nil
}

func goneFactChange(claim resolvedClaim, scope models.VerifyGoneScope) *FactChange {
	return &FactChange{
		Path:      strings.Clone(claim.claim.Path),
		Status:    models.VerifyStatusGone,
		GoneScope: scope,
		OldValue:  cloneRaw(claim.claim.Value),
	}
}

func hydrateCompiledAnchor(
	anchor evidence.Anchor,
	cleaned string,
	observation RevisitResult,
	fetchedAt time.Time,
) evidence.Anchor {
	anchor.TextRange = [2]int{}
	if anchor.Quote != "" {
		start := strings.Index(cleaned, anchor.Quote)
		if start >= 0 && strings.LastIndex(cleaned, anchor.Quote) == start {
			anchor.TextRange = [2]int{start, start + len(anchor.Quote)}
		}
	}
	anchor.Method = evidence.MethodCompiled
	anchor.SnapshotID = observation.SnapshotID
	anchor.FetchedAt = fetchedAt
	return anchor
}

func (s *Service) signCurrent(
	sourceURL string,
	claim resolvedClaim,
	value json.RawMessage,
	anchor evidence.Anchor,
	issuedAt time.Time,
) (string, error) {
	token, err := s.receipts.Sign(receipts.Payload{
		URL:              sourceURL,
		Path:             claim.claim.Path,
		Value:            cloneRaw(value),
		Anchor:           anchor,
		ExtractorVersion: claim.extractorVersion,
		IssuedAt:         issuedAt,
	})
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrReceiptSigning, err)
	}
	if strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("%w: receipt signer returned an empty token", ErrReceiptSigning)
	}
	return token, nil
}

func quoteFallback(match quoteMatch, observation RevisitResult, fetchedAt time.Time) (evidence.Anchor, bool) {
	if strings.TrimSpace(match.quote) == "" || match.start < 0 || match.end <= match.start {
		return evidence.Anchor{}, false
	}
	return evidence.Anchor{
		Quote:      match.quote,
		TextRange:  [2]int{match.start, match.end},
		Method:     match.method,
		SnapshotID: observation.SnapshotID,
		FetchedAt:  fetchedAt,
	}, true
}

func selectorAnchor(quote, selector string, span textSpan, observation RevisitResult, fetchedAt time.Time) evidence.Anchor {
	return evidence.Anchor{
		Quote:      quote,
		TextRange:  [2]int{span.start, span.end},
		Selector:   selector,
		Method:     evidence.MethodExact,
		SnapshotID: observation.SnapshotID,
		FetchedAt:  fetchedAt,
	}
}

func similarity(oldHTML, currentHTML string) float64 {
	distance := simhash.Distance(simhash.FingerprintDOM(oldHTML), simhash.FingerprintDOM(currentHTML))
	return 1 - float64(distance)/64
}

func randomVerificationID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func cloneRaw(value json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), value...)
}

func floatPointer(value float64) *float64 {
	copy := value
	return &copy
}

func cloneFloatPointer(value *float64) *float64 {
	if value == nil {
		return nil
	}
	return floatPointer(*value)
}

func anchorPointer(value evidence.Anchor) *evidence.Anchor {
	copy := value
	return &copy
}

func cloneFactChange(change FactChange) FactChange {
	change.Path = strings.Clone(change.Path)
	change.OldValue = cloneRaw(change.OldValue)
	change.NewValue = cloneRaw(change.NewValue)
	change.Evidence.Quote = strings.Clone(change.Evidence.Quote)
	change.Evidence.Selector = strings.Clone(change.Evidence.Selector)
	change.Evidence.Method = evidence.Method(strings.Clone(string(change.Evidence.Method)))
	change.Evidence.SnapshotID = strings.Clone(change.Evidence.SnapshotID)
	change.Receipt = strings.Clone(change.Receipt)
	return change
}

func cloneFactChanges(changes []FactChange) []FactChange {
	cloned := make([]FactChange, len(changes))
	for index := range changes {
		cloned[index] = cloneFactChange(changes[index])
	}
	return cloned
}
