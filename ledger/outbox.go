package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxPendingOutbox is the largest delivery batch returned by PendingOutbox.
	MaxPendingOutbox = 100
	// MaxPendingOutboxBytes is the hard aggregate payload budget accepted by
	// PendingOutbox.
	MaxPendingOutboxBytes = 64 << 20
	// MaxOutboxURLBytes is the largest persisted delivery URL.
	MaxOutboxURLBytes = 16 << 10
	// MaxOutboxSecretBytes is the largest persisted HMAC secret.
	MaxOutboxSecretBytes = 16 << 10
	// MaxOutboxPayloadBytes is the largest persisted JSON event body.
	MaxOutboxPayloadBytes = 32 << 20
	// MaxOutboxIDBytes is the largest event or verification identifier.
	MaxOutboxIDBytes = 512
	// MaxOutboxTypeBytes is the largest event type.
	MaxOutboxTypeBytes = 128
	// MaxOutboxLastErrorBytes is the largest persisted delivery error.
	MaxOutboxLastErrorBytes = 64 << 10

	// SubjectVerification retains the original verification-scoped outbox
	// identity. SubjectExtractorHeal scopes extractor lifecycle events to one
	// durable extractor_heal_runs row, rather than to an extractor lineage that
	// can produce more than one event over time.
	SubjectVerification  = "verification"
	SubjectExtractorHeal = "extractor_heal"

	ExtractorPromotedEvent = "extractor.promoted"
	ExtractorDegradedEvent = "extractor.degraded"

	outboxTimeLayout = "2006-01-02T15:04:05.000000000Z07:00"
)

var (
	ErrInvalidOutboxEvent = errors.New("ledger: invalid outbox event")
	ErrInvalidOutboxLimit = errors.New("ledger: invalid outbox limit")
	ErrOutboxNotFound     = errors.New("ledger: outbox event not found")
	ErrOutboxConflict     = errors.New("ledger: outbox event conflicts with durable state")
)

// OutboxEvent is one durable webhook delivery. ID is the downstream
// idempotency key; Type plus SubjectType plus SubjectID is the ledger's logical
// uniqueness key. VerificationID is retained as a compatibility alias for a
// verification subject. Mutable delivery fields are populated by PendingOutbox
// and changed only through the MarkOutbox methods.
type OutboxEvent struct {
	ID             string
	VerificationID string
	SubjectType    string
	SubjectID      string
	Type           string
	URL            string
	Secret         string
	Payload        json.RawMessage
	CreatedAt      time.Time
	Attempts       int
	LastAttemptAt  *time.Time
	LastError      string
	NextAttemptAt  time.Time
	DeliveredAt    *time.Time
	FailedAt       *time.Time
}

type validatedOutboxEvent struct {
	ID             string
	VerificationID string
	SubjectType    string
	SubjectID      string
	Type           string
	URL            string
	Secret         string
	Payload        string
	CreatedAt      string
	NextAttemptAt  string
}

func validateNewOutboxEvent(event OutboxEvent) (validatedOutboxEvent, error) {
	id, err := validateOutboxIdentifier("id", event.ID, MaxOutboxIDBytes)
	if err != nil {
		return validatedOutboxEvent{}, err
	}
	subjectType, subjectID, eventVerificationID, err := validateOutboxSubject(event)
	if err != nil {
		return validatedOutboxEvent{}, err
	}
	eventType, err := validateOutboxIdentifier("type", event.Type, MaxOutboxTypeBytes)
	if err != nil {
		return validatedOutboxEvent{}, err
	}
	if subjectType == SubjectExtractorHeal &&
		eventType != ExtractorPromotedEvent && eventType != ExtractorDegradedEvent {
		return validatedOutboxEvent{}, outboxEventError(
			"extractor_heal events must use a supported extractor lifecycle type",
		)
	}
	if err := validateOutboxURL(event.URL); err != nil {
		return validatedOutboxEvent{}, err
	}
	if len(event.Secret) > MaxOutboxSecretBytes {
		return validatedOutboxEvent{}, outboxEventError("secret exceeds %d bytes", MaxOutboxSecretBytes)
	}
	if len(event.Payload) == 0 || len(event.Payload) > MaxOutboxPayloadBytes ||
		!utf8.Valid(event.Payload) || !json.Valid(event.Payload) {
		return validatedOutboxEvent{}, outboxEventError(
			"payload must be valid JSON no larger than %d bytes",
			MaxOutboxPayloadBytes,
		)
	}
	if err := validateOutboxTime("created_at", event.CreatedAt); err != nil {
		return validatedOutboxEvent{}, err
	}
	nextAttemptAt := event.NextAttemptAt
	if nextAttemptAt.IsZero() {
		nextAttemptAt = event.CreatedAt
	} else if err := validateOutboxTime("next_attempt_at", nextAttemptAt); err != nil {
		return validatedOutboxEvent{}, err
	}
	if nextAttemptAt.Before(event.CreatedAt) {
		return validatedOutboxEvent{}, outboxEventError("next_attempt_at cannot precede created_at")
	}
	if event.Attempts != 0 || event.LastAttemptAt != nil || event.LastError != "" ||
		event.DeliveredAt != nil || event.FailedAt != nil {
		return validatedOutboxEvent{}, outboxEventError("new event cannot contain delivery state")
	}

	return validatedOutboxEvent{
		ID:             id,
		VerificationID: eventVerificationID,
		SubjectType:    subjectType,
		SubjectID:      subjectID,
		Type:           eventType,
		URL:            event.URL,
		Secret:         event.Secret,
		Payload:        string(event.Payload),
		CreatedAt:      formatOutboxTime(event.CreatedAt),
		NextAttemptAt:  formatOutboxTime(nextAttemptAt),
	}, nil
}

func validateOutboxSubject(event OutboxEvent) (subjectType, subjectID, verificationID string, err error) {
	subjectType = event.SubjectType
	subjectID = event.SubjectID
	verificationID = event.VerificationID

	// The original public contract identified verification events with only
	// VerificationID. Preserve it exactly while normalizing every new write to
	// the subject-aware durable representation.
	if subjectType == "" && subjectID == "" {
		verificationID, err = validateOutboxIdentifier(
			"verification_id",
			verificationID,
			MaxOutboxIDBytes,
		)
		if err != nil {
			return "", "", "", err
		}
		return SubjectVerification, verificationID, verificationID, nil
	}
	if subjectType == "" || subjectID == "" {
		return "", "", "", outboxEventError("subject_type and subject_id must be supplied together")
	}
	subjectType, err = validateOutboxIdentifier("subject_type", subjectType, MaxOutboxTypeBytes)
	if err != nil {
		return "", "", "", err
	}
	subjectID, err = validateOutboxIdentifier("subject_id", subjectID, MaxOutboxIDBytes)
	if err != nil {
		return "", "", "", err
	}

	switch subjectType {
	case SubjectVerification:
		if verificationID == "" {
			verificationID = subjectID
		} else {
			verificationID, err = validateOutboxIdentifier(
				"verification_id",
				verificationID,
				MaxOutboxIDBytes,
			)
			if err != nil {
				return "", "", "", err
			}
			if verificationID != subjectID {
				return "", "", "", outboxEventError(
					"verification_id must equal the verification subject_id",
				)
			}
		}
	case SubjectExtractorHeal:
		if verificationID != "" {
			return "", "", "", outboxEventError(
				"extractor_heal events cannot contain verification_id",
			)
		}
		if !isLowercaseUUID(subjectID) {
			return "", "", "", outboxEventError(
				"extractor_heal subject_id must be a lowercase UUID",
			)
		}
	default:
		return "", "", "", outboxEventError("unsupported subject_type")
	}
	return subjectType, subjectID, verificationID, nil
}

func isLowercaseUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for index, current := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if current < '0' || current > '9' {
			if current < 'a' || current > 'f' {
				return false
			}
		}
	}
	return true
}

func validateOutboxIdentifier(label, value string, maximum int) (string, error) {
	if value == "" {
		return "", outboxEventError("%s is required", label)
	}
	if strings.TrimSpace(value) != value {
		return "", outboxEventError("%s cannot contain surrounding whitespace", label)
	}
	if len(value) > maximum {
		return "", outboxEventError("%s exceeds %d bytes", label, maximum)
	}
	if !utf8.ValidString(value) {
		return "", outboxEventError("%s must be valid UTF-8", label)
	}
	for _, current := range value {
		if unicode.IsControl(current) {
			return "", outboxEventError("%s cannot contain control characters", label)
		}
	}
	return value, nil
}

func validateOutboxURL(rawURL string) error {
	if rawURL == "" || strings.TrimSpace(rawURL) != rawURL {
		return outboxEventError("url must be an absolute HTTP(S) URL without surrounding whitespace")
	}
	if len(rawURL) > MaxOutboxURLBytes {
		return outboxEventError("url exceeds %d bytes", MaxOutboxURLBytes)
	}
	if !utf8.ValidString(rawURL) {
		return outboxEventError("url must be valid UTF-8")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.Hostname() == "" || parsed.Opaque != "" {
		return outboxEventError("url must be an absolute HTTP(S) URL")
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		!strings.HasPrefix(rawURL, parsed.Scheme+"://") {
		return outboxEventError("url must use a lowercase http or https scheme")
	}
	if parsed.User != nil {
		return outboxEventError("url cannot contain userinfo")
	}
	if strings.Trim(parsed.Hostname(), ".") == "" || strings.HasSuffix(parsed.Host, ":") {
		return outboxEventError("url must contain a usable host and port")
	}
	if port := parsed.Port(); port != "" {
		value, err := strconv.ParseUint(port, 10, 16)
		if err != nil || value == 0 {
			return outboxEventError("url port must be between 1 and 65535")
		}
	}
	return nil
}

func validateOutboxTime(label string, value time.Time) error {
	if value.IsZero() {
		return outboxEventError("%s is required", label)
	}
	year := value.UTC().Year()
	if year < 1 || year > 9999 {
		return outboxEventError("%s is outside the supported time range", label)
	}
	return nil
}

func outboxEventError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidOutboxEvent, fmt.Sprintf(format, args...))
}

func formatOutboxTime(value time.Time) string {
	return value.UTC().Format(outboxTimeLayout)
}

func parseOutboxTime(label, value string) (time.Time, error) {
	parsed, err := time.Parse(outboxTimeLayout, value)
	if err != nil {
		return time.Time{}, outboxEventError("stored %s is invalid: %v", label, err)
	}
	return parsed.UTC(), nil
}

type outboxWriteTx interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// EnqueueOutbox validates and durably enqueues one terminal extractor-heal
// event inside a caller-owned Store.Update transaction. Verification events
// must use RecordVerificationBatch so they cannot become detached from their
// verification rows. An exact logical retry is an idempotent no-op. Reusing
// either its logical key with different immutable content or its downstream ID
// for another event fails with ErrOutboxConflict.
func EnqueueOutbox(ctx context.Context, tx WriteTx, event OutboxEvent) error {
	if ctx == nil {
		return outboxEventError("context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if tx == nil {
		return outboxEventError("write transaction is required")
	}
	validated, err := validateNewOutboxEvent(event)
	if err != nil {
		return err
	}
	if validated.SubjectType != SubjectExtractorHeal {
		return outboxEventError("verification events require RecordVerificationBatch")
	}
	return enqueueValidatedOutbox(ctx, tx, validated)
}

func insertOutboxEvent(ctx context.Context, tx outboxWriteTx, event validatedOutboxEvent) error {
	var verificationID any
	if event.VerificationID != "" {
		verificationID = event.VerificationID
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO outbox_events (
		id, verification_id, subject_type, subject_id, event_type,
		destination_url, secret, payload, created_at, next_attempt_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID,
		verificationID,
		event.SubjectType,
		event.SubjectID,
		event.Type,
		event.URL,
		event.Secret,
		event.Payload,
		event.CreatedAt,
		event.NextAttemptAt,
	)
	if err != nil {
		return fmt.Errorf("ledger: insert outbox event: %w", err)
	}
	return nil
}

func findExistingOutbox(
	ctx context.Context,
	tx outboxWriteTx,
	event validatedOutboxEvent,
) (found, matches bool, err error) {
	var id, destinationURL, secret, payload, createdAt string
	err = tx.QueryRowContext(ctx, `SELECT
		id, destination_url, secret, payload, created_at
		FROM outbox_events
		WHERE event_type = ? AND subject_type = ? AND subject_id = ?`,
		event.Type,
		event.SubjectType,
		event.SubjectID,
	).Scan(&id, &destinationURL, &secret, &payload, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("ledger: read existing outbox event: %w", err)
	}
	// The first durable ID remains the downstream key when a semantically
	// identical retry supplies a different generated ID.
	_ = id
	return true, destinationURL == event.URL && secret == event.Secret &&
		payload == event.Payload && createdAt == event.CreatedAt, nil
}

func enqueueValidatedOutbox(ctx context.Context, tx outboxWriteTx, event validatedOutboxEvent) error {
	if err := validateOutboxSubjectState(ctx, tx, event); err != nil {
		return err
	}
	found, matches, err := findExistingOutbox(ctx, tx, event)
	if err != nil {
		return err
	}
	if found {
		if matches {
			return nil
		}
		return fmt.Errorf("%w: logical event content differs", ErrOutboxConflict)
	}

	var existingSubjectType string
	err = tx.QueryRowContext(ctx,
		"SELECT subject_type FROM outbox_events WHERE id = ?",
		event.ID,
	).Scan(&existingSubjectType)
	if err == nil {
		return fmt.Errorf("%w: downstream event id is already in use", ErrOutboxConflict)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("ledger: inspect outbox event id: %w", err)
	}
	return insertOutboxEvent(ctx, tx, event)
}

func validateOutboxSubjectState(
	ctx context.Context,
	tx outboxWriteTx,
	event validatedOutboxEvent,
) error {
	if event.SubjectType != SubjectExtractorHeal {
		return nil
	}
	var state string
	err := tx.QueryRowContext(ctx,
		"SELECT state FROM extractor_heal_runs WHERE id = ?",
		event.SubjectID,
	).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return outboxEventError("extractor_heal subject must reference a durable terminal heal run")
	}
	if err != nil {
		return fmt.Errorf("ledger: validate outbox subject state: %w", err)
	}
	valid := event.Type == ExtractorPromotedEvent && state == "promoted" ||
		event.Type == ExtractorDegradedEvent && (state == "degraded" || state == "failed")
	if !valid {
		return outboxEventError("extractor_heal event type must match the durable terminal state")
	}
	return nil
}

// PendingOutbox returns at most limit due, non-terminal events in stable order.
// dueAt is supplied by the caller so worker clocks and tests remain explicit.
func (s *Store) PendingOutbox(
	ctx context.Context,
	dueAt time.Time,
	limit int,
	maximumBytes int,
) ([]OutboxEvent, error) {
	if s == nil {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 1 || limit > MaxPendingOutbox {
		return nil, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidOutboxLimit, MaxPendingOutbox)
	}
	if maximumBytes < MaxOutboxPayloadBytes || maximumBytes > MaxPendingOutboxBytes {
		return nil, fmt.Errorf(
			"%w: maximumBytes must be between %d and %d",
			ErrInvalidOutboxLimit,
			MaxOutboxPayloadBytes,
			MaxPendingOutboxBytes,
		)
	}
	if err := validateOutboxTime("due_at", dueAt); err != nil {
		return nil, err
	}

	s.gate.RLock()
	defer s.gate.RUnlock()
	if s.closed {
		return nil, ErrClosed
	}
	rows, err := s.db.QueryContext(ctx, `WITH candidates AS (
		SELECT id, next_attempt_at, created_at,
			length(CAST(payload AS BLOB)) AS payload_bytes
		FROM outbox_events
		WHERE delivered_at IS NULL AND failed_at IS NULL AND next_attempt_at <= ?
		ORDER BY next_attempt_at, created_at, id
		LIMIT ?
	), selected AS (
		SELECT id, next_attempt_at, created_at,
			SUM(payload_bytes) OVER (
				ORDER BY next_attempt_at, created_at, id
				ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW
			) AS cumulative_bytes
		FROM candidates
	)
	SELECT
		e.id, e.verification_id, e.subject_type, e.subject_id, e.event_type,
		e.destination_url, e.secret, e.payload,
		e.created_at, e.attempt_count, e.last_attempt_at, e.last_error,
		e.next_attempt_at, e.delivered_at, e.failed_at
	FROM selected AS s
	JOIN outbox_events AS e ON e.id = s.id
	WHERE s.cumulative_bytes <= ?
	ORDER BY s.next_attempt_at, s.created_at, s.id`,
		formatOutboxTime(dueAt),
		limit,
		maximumBytes,
	)
	if err != nil {
		return nil, fmt.Errorf("ledger: query pending outbox: %w", err)
	}
	defer rows.Close()

	events := make([]OutboxEvent, 0, limit)
	totalBytes := 0
	for rows.Next() {
		event, scanErr := scanOutboxEvent(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		if len(event.Payload) > maximumBytes-totalBytes {
			return nil, outboxEventError("pending outbox query exceeded its payload budget")
		}
		events = append(events, event)
		totalBytes += len(event.Payload)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: iterate pending outbox: %w", err)
	}
	return events, nil
}

type outboxScanner interface {
	Scan(...any) error
}

func scanOutboxEvent(scanner outboxScanner) (OutboxEvent, error) {
	var (
		event                                OutboxEvent
		payload, createdAt, nextAttemptAt    string
		verificationID                       sql.NullString
		lastAttemptAt, deliveredAt, failedAt sql.NullString
	)
	if err := scanner.Scan(
		&event.ID,
		&verificationID,
		&event.SubjectType,
		&event.SubjectID,
		&event.Type,
		&event.URL,
		&event.Secret,
		&payload,
		&createdAt,
		&event.Attempts,
		&lastAttemptAt,
		&event.LastError,
		&nextAttemptAt,
		&deliveredAt,
		&failedAt,
	); err != nil {
		return OutboxEvent{}, fmt.Errorf("ledger: scan outbox event: %w", err)
	}
	if _, err := validateOutboxIdentifier("stored id", event.ID, MaxOutboxIDBytes); err != nil {
		return OutboxEvent{}, err
	}
	if verificationID.Valid {
		event.VerificationID = verificationID.String
	}
	if _, _, _, err := validateOutboxSubject(event); err != nil {
		return OutboxEvent{}, outboxEventError("stored subject is invalid")
	}
	if eventType, err := validateOutboxIdentifier("stored type", event.Type, MaxOutboxTypeBytes); err != nil {
		return OutboxEvent{}, err
	} else if event.SubjectType == SubjectExtractorHeal &&
		eventType != ExtractorPromotedEvent && eventType != ExtractorDegradedEvent {
		return OutboxEvent{}, outboxEventError("stored extractor_heal event type is invalid")
	}
	if err := validateOutboxURL(event.URL); err != nil {
		return OutboxEvent{}, err
	}
	if len(event.Secret) > MaxOutboxSecretBytes || len(payload) == 0 ||
		len(payload) > MaxOutboxPayloadBytes || !utf8.ValidString(payload) || !json.Valid([]byte(payload)) {
		return OutboxEvent{}, outboxEventError("stored payload or secret is invalid")
	}
	var err error
	event.CreatedAt, err = parseOutboxTime("created_at", createdAt)
	if err != nil {
		return OutboxEvent{}, err
	}
	event.NextAttemptAt, err = parseOutboxTime("next_attempt_at", nextAttemptAt)
	if err != nil {
		return OutboxEvent{}, err
	}
	if event.NextAttemptAt.Before(event.CreatedAt) {
		return OutboxEvent{}, outboxEventError("stored next_attempt_at precedes created_at")
	}
	event.LastAttemptAt, err = parseNullableOutboxTime("last_attempt_at", lastAttemptAt)
	if err != nil {
		return OutboxEvent{}, err
	}
	event.DeliveredAt, err = parseNullableOutboxTime("delivered_at", deliveredAt)
	if err != nil {
		return OutboxEvent{}, err
	}
	event.FailedAt, err = parseNullableOutboxTime("failed_at", failedAt)
	if err != nil {
		return OutboxEvent{}, err
	}
	if event.Attempts < 0 || event.Attempts == 0 && (event.LastAttemptAt != nil || event.LastError != "") ||
		event.Attempts > 0 && (event.LastAttemptAt == nil || strings.TrimSpace(event.LastError) == "") {
		return OutboxEvent{}, outboxEventError("stored attempt state is inconsistent")
	}
	if event.DeliveredAt != nil && event.FailedAt != nil {
		return OutboxEvent{}, outboxEventError("stored event has conflicting terminal states")
	}
	event.Payload = append(json.RawMessage(nil), payload...)
	return event, nil
}

func parseNullableOutboxTime(label string, value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	parsed, err := parseOutboxTime(label, value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

// MarkOutboxAttempt records one failed delivery and schedules its next attempt.
// Replaying the same or an older attemptedAt is an idempotent no-op. Terminal
// events are never resurrected.
func (s *Store) MarkOutboxAttempt(
	ctx context.Context,
	id string,
	attemptedAt time.Time,
	nextAttemptAt time.Time,
	deliveryErr string,
) error {
	if _, err := validateOutboxIdentifier("id", id, MaxOutboxIDBytes); err != nil {
		return err
	}
	if err := validateOutboxTime("attempted_at", attemptedAt); err != nil {
		return err
	}
	if err := validateOutboxTime("next_attempt_at", nextAttemptAt); err != nil {
		return err
	}
	if !nextAttemptAt.After(attemptedAt) {
		return outboxEventError("next_attempt_at must be after attempted_at")
	}
	deliveryErr = strings.TrimSpace(deliveryErr)
	if deliveryErr == "" || len(deliveryErr) > MaxOutboxLastErrorBytes {
		return outboxEventError("last_error must be non-empty and no larger than %d bytes", MaxOutboxLastErrorBytes)
	}
	return s.updateOutboxState(ctx, id, func(tx *sql.Tx, state outboxState) (bool, error) {
		if state.DeliveredAt != nil || state.FailedAt != nil {
			return false, nil
		}
		if attemptedAt.Before(state.CreatedAt) {
			return false, outboxEventError("attempted_at cannot precede created_at")
		}
		if state.LastAttemptAt != nil && !attemptedAt.After(*state.LastAttemptAt) {
			return false, nil
		}
		_, err := tx.ExecContext(ctx, `UPDATE outbox_events SET
			attempt_count = attempt_count + 1,
			last_attempt_at = ?,
			last_error = ?,
			next_attempt_at = ?
			WHERE id = ?`,
			formatOutboxTime(attemptedAt),
			deliveryErr,
			formatOutboxTime(nextAttemptAt),
			id,
		)
		if err != nil {
			return false, fmt.Errorf("ledger: mark outbox attempt: %w", err)
		}
		return true, nil
	})
}

// MarkOutboxDelivered atomically sets the successful terminal state. The first
// terminal update wins; repeating either terminal operation is a no-op.
func (s *Store) MarkOutboxDelivered(ctx context.Context, id string, deliveredAt time.Time) error {
	if _, err := validateOutboxIdentifier("id", id, MaxOutboxIDBytes); err != nil {
		return err
	}
	if err := validateOutboxTime("delivered_at", deliveredAt); err != nil {
		return err
	}
	return s.updateOutboxState(ctx, id, func(tx *sql.Tx, state outboxState) (bool, error) {
		if state.DeliveredAt != nil || state.FailedAt != nil {
			return false, nil
		}
		if deliveredAt.Before(state.CreatedAt) || state.LastAttemptAt != nil && deliveredAt.Before(*state.LastAttemptAt) {
			return false, outboxEventError("delivered_at cannot precede event history")
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE outbox_events SET delivered_at = ? WHERE id = ?",
			formatOutboxTime(deliveredAt),
			id,
		); err != nil {
			return false, fmt.Errorf("ledger: mark outbox delivered: %w", err)
		}
		return true, nil
	})
}

// MarkOutboxFailed atomically sets the permanent-failure terminal state. A
// direct policy rejection counts as the first failed attempt; after retries the
// existing attempt count is preserved. The first terminal update wins.
func (s *Store) MarkOutboxFailed(ctx context.Context, id string, failedAt time.Time, lastError string) error {
	if _, err := validateOutboxIdentifier("id", id, MaxOutboxIDBytes); err != nil {
		return err
	}
	if err := validateOutboxTime("failed_at", failedAt); err != nil {
		return err
	}
	lastError = strings.TrimSpace(lastError)
	if lastError == "" || len(lastError) > MaxOutboxLastErrorBytes {
		return outboxEventError("last_error must be non-empty and no larger than %d bytes", MaxOutboxLastErrorBytes)
	}
	return s.updateOutboxState(ctx, id, func(tx *sql.Tx, state outboxState) (bool, error) {
		if state.DeliveredAt != nil || state.FailedAt != nil {
			return false, nil
		}
		if failedAt.Before(state.CreatedAt) || state.LastAttemptAt != nil && failedAt.Before(*state.LastAttemptAt) {
			return false, outboxEventError("failed_at cannot precede event history")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE outbox_events SET
			failed_at = ?,
			attempt_count = CASE WHEN attempt_count = 0 THEN 1 ELSE attempt_count END,
			last_attempt_at = CASE WHEN last_attempt_at IS NULL THEN ? ELSE last_attempt_at END,
			last_error = ?
			WHERE id = ?`,
			formatOutboxTime(failedAt),
			formatOutboxTime(failedAt),
			lastError,
			id,
		); err != nil {
			return false, fmt.Errorf("ledger: mark outbox failed: %w", err)
		}
		return true, nil
	})
}

type outboxState struct {
	CreatedAt     time.Time
	LastAttemptAt *time.Time
	DeliveredAt   *time.Time
	FailedAt      *time.Time
}

func (s *Store) updateOutboxState(
	ctx context.Context,
	id string,
	update func(*sql.Tx, outboxState) (bool, error),
) error {
	if s == nil {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.gate.RLock()
	defer s.gate.RUnlock()
	if s.closed {
		return ErrClosed
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ledger: begin outbox update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	state, err := readOutboxState(ctx, tx, id)
	if err != nil {
		return err
	}
	changed, err := update(tx, state)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ledger: commit outbox update: %w", err)
	}
	return nil
}

func readOutboxState(ctx context.Context, tx *sql.Tx, id string) (outboxState, error) {
	var createdAt string
	var lastAttemptAt, deliveredAt, failedAt sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT created_at, last_attempt_at, delivered_at, failed_at
		FROM outbox_events WHERE id = ?`, id).Scan(&createdAt, &lastAttemptAt, &deliveredAt, &failedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return outboxState{}, fmt.Errorf("%w: %q", ErrOutboxNotFound, id)
	}
	if err != nil {
		return outboxState{}, fmt.Errorf("ledger: read outbox state: %w", err)
	}
	state := outboxState{}
	state.CreatedAt, err = parseOutboxTime("created_at", createdAt)
	if err != nil {
		return outboxState{}, err
	}
	state.LastAttemptAt, err = parseNullableOutboxTime("last_attempt_at", lastAttemptAt)
	if err != nil {
		return outboxState{}, err
	}
	state.DeliveredAt, err = parseNullableOutboxTime("delivered_at", deliveredAt)
	if err != nil {
		return outboxState{}, err
	}
	state.FailedAt, err = parseNullableOutboxTime("failed_at", failedAt)
	if err != nil {
		return outboxState{}, err
	}
	if state.DeliveredAt != nil && state.FailedAt != nil {
		return outboxState{}, outboxEventError("stored event has conflicting terminal states")
	}
	return state, nil
}
