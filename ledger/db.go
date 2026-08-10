// Package ledger provides Purify's embedded, append-only verification ledger.
// Writes are serialized and transactional so every multi-claim verification is
// recorded completely or not at all.
package ledger

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	// Filename is the stable database name beneath PURIFY_DATA_DIR.
	Filename = "purify.db"
	// busyTimeoutMilliseconds applies to every connection through the DSN.
	busyTimeoutMilliseconds = 5000
)

var (
	ErrClosed              = errors.New("ledger: store is closed")
	ErrInvalidConfig       = errors.New("ledger: invalid configuration")
	ErrInvalidVerification = errors.New("ledger: invalid verification")
	ErrUnsupportedOutcome  = errors.New("ledger: unsupported outcome")
	// ErrVerificationBatchHook identifies a hook failure without exposing the
	// hook's potentially sensitive error text.
	ErrVerificationBatchHook = errors.New("ledger: verification batch hook failed")
	// ErrVerificationBatchHookPanic identifies a recovered hook panic. Its
	// fixed text never includes the recovered value or stack.
	ErrVerificationBatchHookPanic = &verificationBatchHookPanicError{}
	ledgerOpenCoordinator         openCoordinator
)

// Outcome is the durable three-state verdict for a claim.
type Outcome string

const (
	OutcomeConfirmed Outcome = "confirmed"
	OutcomeChanged   Outcome = "changed"
	OutcomeGone      Outcome = "gone"
)

// GoneScope distinguishes a missing field from a definite 404/410 page.
type GoneScope string

const (
	GoneScopeField GoneScope = "field"
	GoneScopePage  GoneScope = "page"
)

// Verification is one claim verdict. Rows sharing VerificationID are written
// atomically by RecordVerifications and retain the provenance required for
// replay, drift analysis, and future extractor healing.
type Verification struct {
	ID                string
	VerificationID    string
	ClaimIndex        int
	URL               string
	FinalURL          string
	Path              string
	OldValue          json.RawMessage
	NewValue          json.RawMessage
	Outcome           Outcome
	GoneScope         GoneScope
	PageSimilarity    *float64
	OldSnapshotID     string
	NewSnapshotID     string
	OldReceipt        string
	Receipt           string
	SchemaHash        string
	TemplateClusterID string
	ExtractorID       string
	VerifiedAt        time.Time
}

// VerificationBatchState identifies the durable verification batch visible to
// a VerificationBatchHook. Existing is true for an exact idempotent retry.
type VerificationBatchState struct {
	VerificationID string
	Existing       bool
}

// VerificationBatchHook extends a verification batch transaction after its
// normalized rows are durable within tx and before its outbox event is written.
// The hook must use only the supplied tx, must not retain it, and must not call
// Store methods: re-entering the Store would deadlock on its writer lock. It
// should not perform external side effects because a later rollback cannot undo
// them. Returned errors and panics roll back the entire transaction; panic
// values are never exposed to callers.
type VerificationBatchHook func(context.Context, WriteTx, VerificationBatchState) error

type verificationBatchHookError struct {
	cause error
}

func (e verificationBatchHookError) Error() string {
	return ErrVerificationBatchHook.Error()
}

func (e verificationBatchHookError) Unwrap() []error {
	return []error{ErrVerificationBatchHook, e.cause}
}

type verificationBatchHookPanicError struct{}

func (*verificationBatchHookPanicError) Error() string {
	return "ledger: verification batch hook panicked"
}

func (*verificationBatchHookPanicError) Unwrap() error {
	return ErrVerificationBatchHook
}

// Store owns the SQLite connection pool. gate prevents Close from racing an
// in-flight operation; writeMu gives SQLite a single process-local writer.
type Store struct {
	db      *sql.DB
	path    string
	gate    sync.RWMutex
	writeMu sync.Mutex
	closed  bool
}

// Open creates or opens <dataDir>/purify.db, configures every connection for
// WAL operation, and applies all migrations before returning.
func Open(dataDir string) (*Store, error) {
	dataDir = strings.TrimSpace(dataDir)
	if dataDir == "" {
		return nil, fmt.Errorf("%w: data directory is required", ErrInvalidConfig)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("ledger: create data directory: %w", err)
	}
	dbPath := filepath.Join(dataDir, Filename)
	// A relative path would become the file URI's authority component below
	// and fail to open, so resolve it before deriving identity or the DSN.
	dbPath, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("ledger: resolve data directory: %w", err)
	}
	releaseOpen := ledgerOpenCoordinator.lock(databaseIdentityPath(dbPath))
	defer releaseOpen()

	if err := os.Chmod(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("ledger: secure data directory: %w", err)
	}

	dsn := (&url.URL{
		Scheme: "file",
		Path:   dbPath,
		RawQuery: url.Values{
			"_busy_timeout": []string{fmt.Sprint(busyTimeoutMilliseconds)},
			"_foreign_keys": []string{"on"},
			"_journal_mode": []string{"wal"},
			"_synchronous":  []string{"normal"},
			"_txlock":       []string{"immediate"},
		}.Encode(),
	}).String()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("ledger: open database: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db, path: dbPath}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ledger: connect database: %w", err)
	}
	if err := s.applyMigrations(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := os.Chmod(dbPath, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ledger: secure database: %w", err)
	}
	return s, nil
}

// Path returns the absolute or caller-relative database filename passed to
// SQLite. It is intended for diagnostics and backup tooling.
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Close waits for in-flight operations, rejects future writes, and closes the
// database. It is safe to call concurrently and repeatedly.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.gate.Lock()
	defer s.gate.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.db.Close()
}

// RecordVerifications atomically appends all claim verdicts without an outbox
// event. It retains the original API and delegates to RecordVerificationBatch.
func (s *Store) RecordVerifications(ctx context.Context, rows []Verification) error {
	return s.RecordVerificationBatch(ctx, rows, nil)
}

// RecordVerificationBatch atomically appends all claim verdicts and, when
// supplied, one durable outbox event in the same SQLite transaction. An exact
// retry of an already-durable batch is idempotent; conflicting content fails
// closed and never appends a partial batch or orphaned event.
func (s *Store) RecordVerificationBatch(
	ctx context.Context,
	rows []Verification,
	event *OutboxEvent,
) error {
	return s.RecordVerificationBatchWithHook(ctx, rows, event, nil)
}

// RecordVerificationBatchWithHook has the same validation, idempotency, and
// outbox guarantees as RecordVerificationBatch and invokes hook in the same
// transaction after normalized verification rows are available through tx.
// Exact retries also invoke hook with Existing set to true. See
// VerificationBatchHook for the callback's non-reentrancy contract.
func (s *Store) RecordVerificationBatchWithHook(
	ctx context.Context,
	rows []Verification,
	event *OutboxEvent,
	hook VerificationBatchHook,
) error {
	if s == nil {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(rows) == 0 && event == nil {
		return nil
	}
	if len(rows) == 0 {
		return outboxEventError("an outbox event requires a non-empty verification batch")
	}

	validated := make([]validatedVerification, len(rows))
	verificationID := strings.TrimSpace(rows[0].VerificationID)
	seenClaims := make(map[int]struct{}, len(rows))
	for i := range rows {
		if strings.TrimSpace(rows[i].VerificationID) != verificationID {
			return fmt.Errorf("%w: rows must share verification_id", ErrInvalidVerification)
		}
		if _, duplicate := seenClaims[rows[i].ClaimIndex]; duplicate {
			return fmt.Errorf("%w: duplicate claim_index %d", ErrInvalidVerification, rows[i].ClaimIndex)
		}
		seenClaims[rows[i].ClaimIndex] = struct{}{}
		v, err := validateVerification(rows[i])
		if err != nil {
			return err
		}
		validated[i] = v
	}
	var validatedEvent *validatedOutboxEvent
	if event != nil {
		value, err := validateNewOutboxEvent(*event)
		if err != nil {
			return err
		}
		if value.SubjectType != SubjectVerification || value.SubjectID != verificationID {
			return outboxEventError("verification subject_id must match the verification batch")
		}
		validatedEvent = &value
	}

	return s.Update(ctx, func(tx WriteTx) error {
		var existingCount int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM verifications WHERE verification_id = ?",
			verificationID,
		).Scan(&existingCount); err != nil {
			return fmt.Errorf("ledger: inspect existing verification batch: %w", err)
		}
		existing := existingCount > 0
		if existing {
			if existingCount != len(validated) {
				return fmt.Errorf("%w: retry row count differs for verification_id %q", ErrInvalidVerification, verificationID)
			}
			for _, row := range validated {
				matches, err := existingVerificationMatches(ctx, tx, row)
				if err != nil {
					return err
				}
				if !matches {
					return fmt.Errorf(
						"%w: retry differs at claim_index %d for verification_id %q",
						ErrInvalidVerification,
						row.ClaimIndex,
						verificationID,
					)
				}
			}
		} else {
			const insert = `INSERT INTO verifications (
		id, verification_id, claim_index, url, host, final_url, path,
		old_value, new_value, outcome, gone_scope, page_similarity,
		old_snapshot_id, new_snapshot_id, old_receipt, receipt,
		schema_hash, template_cluster_id, extractor_id, verified_at
	) VALUES (?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''),
		NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?)`
			for _, row := range validated {
				if _, err := tx.ExecContext(ctx, insert,
					row.ID, row.VerificationID, row.ClaimIndex, row.URL, row.Host,
					row.FinalURL, row.Path, row.OldValue, row.NewValue,
					row.Outcome, row.GoneScope, row.PageSimilarity,
					row.OldSnapshotID, row.NewSnapshotID, row.OldReceipt, row.Receipt,
					row.SchemaHash, row.TemplateClusterID, row.ExtractorID, row.VerifiedAt,
				); err != nil {
					return fmt.Errorf("ledger: insert verification claim %d: %w", row.ClaimIndex, err)
				}
			}
		}

		if err := callVerificationBatchHook(ctx, hook, tx, VerificationBatchState{
			VerificationID: verificationID,
			Existing:       existing,
		}); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		if validatedEvent != nil {
			if existing {
				found, matches, err := findExistingOutbox(ctx, tx, *validatedEvent)
				if err != nil {
					return err
				}
				if !found || !matches {
					return fmt.Errorf("%w: type %q verification_id %q", ErrOutboxConflict, validatedEvent.Type, verificationID)
				}
			} else if err := insertOutboxEvent(ctx, tx, *validatedEvent); err != nil {
				return err
			}
		}
		return nil
	})
}

func callVerificationBatchHook(
	ctx context.Context,
	hook VerificationBatchHook,
	tx WriteTx,
	state VerificationBatchState,
) (err error) {
	if hook == nil {
		return nil
	}
	completed := false
	defer func() {
		if !completed {
			// recover can itself return nil for panic(nil) under the legacy
			// GODEBUG=panicnil=1 behavior. Normal completion is therefore
			// tracked independently from the recovered panic value.
			_ = recover()
			err = ErrVerificationBatchHookPanic
		}
	}()
	hookErr := hook(ctx, tx, state)
	completed = true
	if hookErr != nil {
		return verificationBatchHookError{cause: hookErr}
	}
	if err := ctx.Err(); err != nil {
		return verificationBatchHookError{cause: err}
	}
	return nil
}

func existingVerificationMatches(
	ctx context.Context,
	tx ReadTx,
	row validatedVerification,
) (bool, error) {
	var (
		url, host, path, oldValue, outcome, oldSnapshotID, verifiedAt string
		finalURL, newValue, goneScope, newSnapshotID                  sql.NullString
		oldReceipt, receipt, schemaHash, templateClusterID            sql.NullString
		extractorID                                                   sql.NullString
		pageSimilarity                                                sql.NullFloat64
	)
	err := tx.QueryRowContext(ctx, `SELECT
		url, host, final_url, path, old_value, new_value, outcome,
		gone_scope, page_similarity, old_snapshot_id, new_snapshot_id,
		old_receipt, receipt, schema_hash, template_cluster_id,
		extractor_id, verified_at
		FROM verifications WHERE verification_id = ? AND claim_index = ?`,
		row.VerificationID,
		row.ClaimIndex,
	).Scan(
		&url,
		&host,
		&finalURL,
		&path,
		&oldValue,
		&newValue,
		&outcome,
		&goneScope,
		&pageSimilarity,
		&oldSnapshotID,
		&newSnapshotID,
		&oldReceipt,
		&receipt,
		&schemaHash,
		&templateClusterID,
		&extractorID,
		&verifiedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("ledger: read existing verification claim %d: %w", row.ClaimIndex, err)
	}
	return url == row.URL &&
		host == row.Host &&
		nullIfEmptyMatches(finalURL, row.FinalURL) &&
		path == row.Path &&
		oldValue == row.OldValue &&
		nullableValueMatches(newValue, row.NewValue) &&
		outcome == row.Outcome &&
		nullableValueMatches(goneScope, row.GoneScope) &&
		nullableFloatMatches(pageSimilarity, row.PageSimilarity) &&
		oldSnapshotID == row.OldSnapshotID &&
		nullIfEmptyMatches(newSnapshotID, row.NewSnapshotID) &&
		nullIfEmptyMatches(oldReceipt, row.OldReceipt) &&
		nullIfEmptyMatches(receipt, row.Receipt) &&
		nullIfEmptyMatches(schemaHash, row.SchemaHash) &&
		nullIfEmptyMatches(templateClusterID, row.TemplateClusterID) &&
		nullIfEmptyMatches(extractorID, row.ExtractorID) &&
		verifiedAt == row.VerifiedAt, nil
}

func nullIfEmptyMatches(actual sql.NullString, expected string) bool {
	if expected == "" {
		return !actual.Valid
	}
	return actual.Valid && actual.String == expected
}

func nullableValueMatches(actual sql.NullString, expected any) bool {
	if expected == nil {
		return !actual.Valid
	}
	value, ok := expected.(string)
	return ok && actual.Valid && actual.String == value
}

func nullableFloatMatches(actual sql.NullFloat64, expected any) bool {
	if expected == nil {
		return !actual.Valid
	}
	value, ok := expected.(float64)
	return ok && actual.Valid && actual.Float64 == value
}

type validatedVerification struct {
	ID                string
	VerificationID    string
	ClaimIndex        int
	URL               string
	Host              string
	FinalURL          string
	Path              string
	OldValue          string
	NewValue          any
	Outcome           string
	GoneScope         any
	PageSimilarity    any
	OldSnapshotID     string
	NewSnapshotID     string
	OldReceipt        string
	Receipt           string
	SchemaHash        string
	TemplateClusterID string
	ExtractorID       string
	VerifiedAt        string
}

func validateVerification(v Verification) (validatedVerification, error) {
	fail := func(format string, args ...any) (validatedVerification, error) {
		return validatedVerification{}, fmt.Errorf("%w: %s", ErrInvalidVerification, fmt.Sprintf(format, args...))
	}

	verificationID := strings.TrimSpace(v.VerificationID)
	if verificationID == "" {
		return fail("verification_id is required")
	}
	if v.ClaimIndex < 0 {
		return fail("claim_index must be non-negative")
	}
	pageURL := strings.TrimSpace(v.URL)
	parsed, err := url.Parse(pageURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return fail("url must be an absolute HTTP(S) URL")
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	path := strings.TrimSpace(v.Path)
	if path == "" {
		return fail("path is required")
	}
	if len(v.OldValue) == 0 || !json.Valid(v.OldValue) {
		return fail("old_value must be valid JSON")
	}
	if strings.TrimSpace(v.OldSnapshotID) == "" {
		return fail("old_snapshot_id is required")
	}
	if v.VerifiedAt.IsZero() {
		return fail("verified_at is required")
	}

	var newValue any
	if len(v.NewValue) > 0 {
		if !json.Valid(v.NewValue) {
			return fail("new_value must be valid JSON")
		}
		newValue = string(v.NewValue)
	}

	var goneScope any
	switch v.Outcome {
	case OutcomeConfirmed:
		if v.GoneScope != "" {
			return fail("gone_scope is only valid for gone outcomes")
		}
	case OutcomeChanged:
		if v.GoneScope != "" {
			return fail("gone_scope is only valid for gone outcomes")
		}
		if newValue == nil {
			return fail("changed outcome requires new_value")
		}
	case OutcomeGone:
		if v.GoneScope != GoneScopeField && v.GoneScope != GoneScopePage {
			return fail("gone outcome requires field or page gone_scope")
		}
		if newValue != nil {
			return fail("gone outcome cannot have new_value")
		}
		goneScope = string(v.GoneScope)
	default:
		return validatedVerification{}, fmt.Errorf("%w: %q", ErrUnsupportedOutcome, v.Outcome)
	}

	var similarity any
	if v.PageSimilarity != nil {
		value := *v.PageSimilarity
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
			return fail("page_similarity must be between 0 and 1")
		}
		similarity = value
	}

	id := strings.TrimSpace(v.ID)
	if id == "" {
		id, err = randomID()
		if err != nil {
			return validatedVerification{}, err
		}
	} else if id != v.ID {
		return fail("id cannot contain surrounding whitespace")
	}

	schemaHash := strings.TrimSpace(v.SchemaHash)
	templateClusterID := strings.TrimSpace(v.TemplateClusterID)
	extractorID := strings.TrimSpace(v.ExtractorID)
	if schemaHash != v.SchemaHash || templateClusterID != v.TemplateClusterID || extractorID != v.ExtractorID {
		return fail("extractor provenance cannot contain surrounding whitespace")
	}
	provenanceCount := 0
	for _, value := range []string{schemaHash, templateClusterID, extractorID} {
		if value != "" {
			provenanceCount++
		}
	}
	if provenanceCount != 0 && provenanceCount != 3 {
		return fail("schema_hash, template_cluster_id, and extractor_id must be all empty or all present")
	}
	if provenanceCount == 3 {
		if !validLowerHexIdentity(schemaHash, 64) {
			return fail("schema_hash must be 64 lowercase hexadecimal characters")
		}
		if !validLowerHexIdentity(templateClusterID, 64) {
			return fail("template_cluster_id must be 64 lowercase hexadecimal characters")
		}
		if !validLowercaseUUIDIdentity(extractorID) {
			return fail("extractor_id must be a lowercase UUID")
		}
	}

	return validatedVerification{
		ID:                id,
		VerificationID:    verificationID,
		ClaimIndex:        v.ClaimIndex,
		URL:               pageURL,
		Host:              host,
		FinalURL:          strings.TrimSpace(v.FinalURL),
		Path:              path,
		OldValue:          string(v.OldValue),
		NewValue:          newValue,
		Outcome:           string(v.Outcome),
		GoneScope:         goneScope,
		PageSimilarity:    similarity,
		OldSnapshotID:     strings.TrimSpace(v.OldSnapshotID),
		NewSnapshotID:     strings.TrimSpace(v.NewSnapshotID),
		OldReceipt:        v.OldReceipt,
		Receipt:           v.Receipt,
		SchemaHash:        schemaHash,
		TemplateClusterID: templateClusterID,
		ExtractorID:       extractorID,
		VerifiedAt:        v.VerifiedAt.UTC().Format(time.RFC3339Nano),
	}, nil
}

func validLowerHexIdentity(value string, length int) bool {
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

func validLowercaseUUIDIdentity(value string) bool {
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

func randomID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("ledger: generate row id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func (s *Store) applyMigrations(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ledger: begin migrations: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("ledger: create migration table: %w", err)
	}

	for index, statement := range migrations {
		version := index + 1
		var exists int
		err := tx.QueryRowContext(ctx,
			"SELECT 1 FROM schema_migrations WHERE version = ?", version,
		).Scan(&exists)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("ledger: read migration %d: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("ledger: apply migration %d: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)",
			version, time.Now().UTC().Format(time.RFC3339Nano),
		); err != nil {
			return fmt.Errorf("ledger: record migration %d: %w", version, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ledger: commit migrations: %w", err)
	}
	return nil
}
