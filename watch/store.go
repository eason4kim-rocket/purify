package watch

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/netip"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/publicnet"
	"github.com/use-agent/purify/search"
	"golang.org/x/net/publicsuffix"
)

const (
	watchTimeLayout      = "2006-01-02T15:04:05.000000000Z"
	maximumRetryInterval = 7 * 24 * time.Hour
	maximumFactValue     = 64 << 10
	maximumFactReceipt   = 2 << 20
)

type storeConfig struct {
	clock         func() time.Time
	idGenerator   func() (string, error)
	leaseDuration time.Duration
}

// StoreOption configures a durable watch store.
type StoreOption func(*storeConfig) error

// WithClock supplies the operation clock. Returned timestamps must be valid;
// Store canonicalizes them to UTC with nanosecond precision.
func WithClock(clock func() time.Time) StoreOption {
	return func(config *storeConfig) error {
		if clock == nil {
			return ErrInvalidStore
		}
		config.clock = clock
		return nil
	}
}

// WithIDGenerator supplies UUIDv4 identities for watches and leases.
func WithIDGenerator(generator func() (string, error)) StoreOption {
	return func(config *storeConfig) error {
		if generator == nil {
			return ErrInvalidStore
		}
		config.idGenerator = generator
		return nil
	}
}

// WithLeaseDuration changes the scheduler claim duration.
func WithLeaseDuration(duration time.Duration) StoreOption {
	return func(config *storeConfig) error {
		if duration <= 0 || duration > DefaultLeaseDuration {
			return ErrInvalidStore
		}
		config.leaseDuration = duration
		return nil
	}
}

// Store owns watch lifecycle operations over an already-open ledger. It does
// not own or close the ledger.
type Store struct {
	ledger        *ledger.Store
	clock         func() time.Time
	idGenerator   func() (string, error)
	leaseDuration time.Duration
}

// NewStore constructs a watch repository over the shared ledger.
func NewStore(durable *ledger.Store, options ...StoreOption) (*Store, error) {
	if durable == nil {
		return nil, ErrInvalidStore
	}
	config := storeConfig{
		clock:         time.Now,
		idGenerator:   randomUUIDv4,
		leaseDuration: DefaultLeaseDuration,
	}
	for _, option := range options {
		if option == nil {
			return nil, ErrInvalidStore
		}
		if err := option(&config); err != nil {
			return nil, ErrInvalidStore
		}
	}
	if config.clock == nil || config.idGenerator == nil || config.leaseDuration <= 0 ||
		config.leaseDuration > DefaultLeaseDuration {
		return nil, ErrInvalidStore
	}
	return &Store{
		ledger: durable, clock: config.clock, idGenerator: config.idGenerator,
		leaseDuration: config.leaseDuration,
	}, nil
}

// Create normalizes and fingerprints a FactSpec. An existing live identical
// specification is returned without consuming capacity or replacing its ID.
func (store *Store) Create(ctx context.Context, spec models.FactSpec) (Watch, bool, error) {
	if err := store.validateContext(ctx); err != nil {
		return Watch{}, false, err
	}
	normalized, hash, err := normalizeFactSpec(spec)
	if err != nil {
		return Watch{}, false, err
	}
	now, err := store.operationTime()
	if err != nil {
		return Watch{}, false, err
	}

	var result Watch
	created := false
	err = store.ledger.Update(ctx, func(tx ledger.WriteTx) error {
		stored, found, err := loadLiveWatchByHash(ctx, tx, hash)
		if err != nil {
			return err
		}
		if found {
			if stored.specHash != hash || stored.Watch.Spec != normalized {
				return ErrCorruptStore
			}
			result = cloneWatch(stored.Watch)
			return nil
		}

		var count int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM watches WHERE state <> 'deleted'`).Scan(&count); err != nil {
			return databaseError(ctx, err)
		}
		if count < 0 || count > MaxLiveWatches {
			return ErrCorruptStore
		}
		if count == MaxLiveWatches {
			return ErrWatchLimit
		}

		id, err := store.generateID()
		if err != nil {
			return err
		}
		var duplicate int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM watches WHERE id = ?)`, id).Scan(&duplicate); err != nil {
			return databaseError(ctx, err)
		}
		if duplicate != 0 {
			return ErrInvalidStore
		}
		ewma := initialEWMA(normalized.Freshness)
		timestamp := formatTime(now)
		if _, err := tx.ExecContext(ctx, `INSERT INTO watches (
			id, spec_hash, subject, predicate, freshness, min_independent_sources,
			on_conflict, state, next_check_at, ewma_interval_s, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?, ?)`,
			id, hash, normalized.Subject, normalized.Predicate, normalized.Freshness,
			normalized.MinIndependentSources, string(normalized.OnConflict), timestamp,
			float64(ewma)/float64(time.Second), timestamp, timestamp); err != nil {
			return databaseError(ctx, err)
		}
		result = Watch{
			ID: id, Spec: normalized, State: StatePending, NextCheckAt: timePointer(now),
			EWMAInterval: ewma, CreatedAt: now, UpdatedAt: now,
		}
		created = true
		return nil
	})
	if err != nil {
		return Watch{}, false, storeError(err)
	}
	if err := ctx.Err(); err != nil {
		return Watch{}, false, err
	}
	return cloneWatch(result), created, nil
}

// Get returns one live watch. Deleted rows are indistinguishable from missing
// rows at this boundary.
func (store *Store) Get(ctx context.Context, id string) (Watch, error) {
	if err := store.validateContext(ctx); err != nil {
		return Watch{}, err
	}
	if !validUUIDv4(id) {
		return Watch{}, ErrInvalidWatchID
	}
	var result Watch
	err := store.ledger.View(ctx, func(tx ledger.ReadTx) error {
		stored, err := scanStoredWatch(tx.QueryRowContext(ctx,
			`SELECT `+watchColumns+` FROM watches WHERE id = ? AND state <> 'deleted'`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrWatchNotFound
		}
		if err != nil {
			return databaseError(ctx, err)
		}
		if err := validateStoredWatch(stored, false); err != nil {
			return err
		}
		result = cloneWatch(stored.Watch)
		return nil
	})
	if err != nil {
		return Watch{}, storeError(err)
	}
	if err := ctx.Err(); err != nil {
		return Watch{}, err
	}
	return cloneWatch(result), nil
}

// List returns live watches in stable (created_at,id) order.
func (store *Store) List(ctx context.Context, options WatchListOptions) (WatchPage, error) {
	empty := WatchPage{Items: []Watch{}}
	if err := store.validateContext(ctx); err != nil {
		return empty, err
	}
	limit := options.Limit
	if limit == 0 {
		limit = DefaultListLimit
	}
	if limit < 1 || limit > MaxListLimit {
		return empty, ErrInvalidCursor
	}
	var cursor *WatchCursor
	if options.Cursor != nil {
		copy := *options.Cursor
		if !validPublicTime(copy.CreatedAt) || copy.CreatedAt.Location() != time.UTC || !validUUIDv4(copy.ID) {
			return empty, ErrInvalidCursor
		}
		copy.CreatedAt = copy.CreatedAt.UTC()
		copy.ID = strings.Clone(copy.ID)
		cursor = &copy
	}

	page := empty
	err := store.ledger.View(ctx, func(tx ledger.ReadTx) error {
		query := `SELECT ` + watchColumns + ` FROM watches WHERE state <> 'deleted'`
		arguments := make([]any, 0, 3)
		if cursor != nil {
			query += ` AND (created_at > ? OR (created_at = ? AND id > ?))`
			stamp := formatTime(cursor.CreatedAt)
			arguments = append(arguments, stamp, stamp, cursor.ID)
		}
		query += ` ORDER BY created_at, id LIMIT ?`
		arguments = append(arguments, limit+1)
		rows, err := tx.QueryContext(ctx, query, arguments...)
		if err != nil {
			return databaseError(ctx, err)
		}
		defer rows.Close()
		values := make([]Watch, 0, limit+1)
		for rows.Next() {
			stored, err := scanStoredWatch(rows)
			if err != nil {
				return databaseError(ctx, err)
			}
			if validateStoredWatch(stored, false) != nil {
				return ErrCorruptStore
			}
			values = append(values, cloneWatch(stored.Watch))
		}
		if err := rows.Err(); err != nil {
			return databaseError(ctx, err)
		}
		if len(values) > limit {
			last := values[limit-1]
			page.Next = &WatchCursor{CreatedAt: last.CreatedAt, ID: strings.Clone(last.ID)}
			values = values[:limit]
		}
		page.Items = values
		return nil
	})
	if err != nil {
		return empty, storeError(err)
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	return clonePage(page), nil
}

// Pause stops scheduling and atomically clears any outstanding lease.
func (store *Store) Pause(ctx context.Context, id string) (Watch, error) {
	return store.setPaused(ctx, id, true)
}

// Resume makes a paused watch immediately due. Pending and active watches are
// already actionable and are returned unchanged.
func (store *Store) Resume(ctx context.Context, id string) (Watch, error) {
	return store.setPaused(ctx, id, false)
}

func (store *Store) setPaused(ctx context.Context, id string, pause bool) (Watch, error) {
	if err := store.validateContext(ctx); err != nil {
		return Watch{}, err
	}
	if !validUUIDv4(id) {
		return Watch{}, ErrInvalidWatchID
	}
	now, err := store.operationTime()
	if err != nil {
		return Watch{}, err
	}
	var result Watch
	err = store.ledger.Update(ctx, func(tx ledger.WriteTx) error {
		stored, err := loadWatchByID(ctx, tx, id)
		if err != nil {
			return err
		}
		if stored.Watch.State == StateDeleted {
			return ErrWatchNotFound
		}
		if pause && stored.Watch.State == StatePaused || !pause && stored.Watch.State != StatePaused {
			result = cloneWatch(stored.Watch)
			return nil
		}
		updated, err := advanceTime(now, stored.Watch.UpdatedAt)
		if err != nil {
			return err
		}
		if pause {
			writeResult, err := tx.ExecContext(ctx, `UPDATE watches SET
				state = 'paused', next_check_at = NULL, lease_id = NULL, lease_until = NULL,
				paused_at = ?, updated_at = ? WHERE id = ? AND state IN ('pending','active')`,
				formatTime(updated), formatTime(updated), id)
			if err != nil {
				return databaseError(ctx, err)
			}
			if err := requireOneRow(ctx, writeResult); err != nil {
				return err
			}
			stored.Watch.State = StatePaused
			stored.Watch.NextCheckAt = nil
			stored.Watch.PausedAt = timePointer(updated)
			stored.leaseID, stored.leaseUntil = "", nil
		} else {
			writeResult, err := tx.ExecContext(ctx, `UPDATE watches SET
				state = 'active', next_check_at = ?, paused_at = NULL, updated_at = ?
				WHERE id = ? AND state = 'paused'`, formatTime(updated), formatTime(updated), id)
			if err != nil {
				return databaseError(ctx, err)
			}
			if err := requireOneRow(ctx, writeResult); err != nil {
				return err
			}
			stored.Watch.State = StateActive
			stored.Watch.NextCheckAt = timePointer(updated)
			stored.Watch.PausedAt = nil
		}
		stored.Watch.UpdatedAt = updated
		result = cloneWatch(stored.Watch)
		return nil
	})
	if err != nil {
		return Watch{}, storeError(err)
	}
	if err := ctx.Err(); err != nil {
		return Watch{}, err
	}
	return cloneWatch(result), nil
}

// Delete soft-deletes a watch. Repeating deletion for the same durable row is
// successful; an identifier never present in the ledger is not found.
func (store *Store) Delete(ctx context.Context, id string) error {
	if err := store.validateContext(ctx); err != nil {
		return err
	}
	if !validUUIDv4(id) {
		return ErrInvalidWatchID
	}
	now, err := store.operationTime()
	if err != nil {
		return err
	}
	err = store.ledger.Update(ctx, func(tx ledger.WriteTx) error {
		stored, err := loadWatchByID(ctx, tx, id)
		if err != nil {
			return err
		}
		if stored.Watch.State == StateDeleted {
			return nil
		}
		updated, err := advanceTime(now, stored.Watch.UpdatedAt)
		if err != nil {
			return err
		}
		writeResult, err := tx.ExecContext(ctx, `UPDATE watches SET
			state = 'deleted', next_check_at = NULL, lease_id = NULL, lease_until = NULL,
			paused_at = NULL, deleted_at = ?, updated_at = ?
			WHERE id = ? AND state <> 'deleted'`, formatTime(updated), formatTime(updated), id)
		if err != nil {
			return databaseError(ctx, err)
		}
		return requireOneRow(ctx, writeResult)
	})
	if err != nil {
		return storeError(err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// ClaimDue leases the earliest actionable due watch and loads its open fact in
// the same transaction. Expired leases may be taken over; live leases cannot.
func (store *Store) ClaimDue(ctx context.Context) (Claim, bool, error) {
	if err := store.validateContext(ctx); err != nil {
		return Claim{}, false, err
	}
	now, err := store.operationTime()
	if err != nil {
		return Claim{}, false, err
	}
	var claim Claim
	found := false
	err = store.ledger.Update(ctx, func(tx ledger.WriteTx) error {
		stored, err := scanStoredWatch(tx.QueryRowContext(ctx, `SELECT `+watchColumns+`
			FROM watches WHERE state IN ('pending','active') AND next_check_at <= ?
				AND (lease_id IS NULL OR lease_until <= ?)
			ORDER BY next_check_at, lease_until, id LIMIT 1`, formatTime(now), formatTime(now)))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return databaseError(ctx, err)
		}
		if validateStoredWatch(stored, false) != nil {
			return ErrCorruptStore
		}
		if stored.Watch.NextCheckAt == nil || stored.Watch.NextCheckAt.After(now) ||
			stored.leaseUntil != nil && stored.leaseUntil.After(now) {
			return ErrCorruptStore
		}
		fact, hasFact, err := loadOpenFact(ctx, tx, stored.Watch)
		if err != nil {
			return err
		}
		leaseID, err := store.generateID()
		if err != nil {
			return err
		}
		if leaseID == stored.Watch.ID || leaseID == stored.leaseID {
			return ErrInvalidStore
		}
		updated, err := advanceTime(now, stored.Watch.UpdatedAt)
		if err != nil {
			return err
		}
		until := updated.Add(store.leaseDuration)
		if !validPublicTime(until) || !until.After(updated) {
			return ErrInvalidStore
		}
		writeResult, err := tx.ExecContext(ctx, `UPDATE watches SET
			lease_id = ?, lease_until = ?, updated_at = ? WHERE id = ?
				AND state IN ('pending','active') AND next_check_at <= ?
				AND (lease_id IS NULL OR lease_until <= ?)`,
			leaseID, formatTime(until), formatTime(updated), stored.Watch.ID,
			formatTime(now), formatTime(now))
		if err != nil {
			return databaseError(ctx, err)
		}
		if err := requireOneRow(ctx, writeResult); err != nil {
			return err
		}
		stored.Watch.UpdatedAt = updated
		claim.Watch = cloneWatch(stored.Watch)
		if hasFact {
			copy := cloneFact(fact)
			claim.Fact = &copy
		}
		claim.Lease = Lease{WatchID: stored.Watch.ID, ID: leaseID, Until: until}
		found = true
		return nil
	})
	if err != nil {
		return Claim{}, false, storeError(err)
	}
	if err := ctx.Err(); err != nil {
		return Claim{}, false, err
	}
	return cloneClaim(claim), found, nil
}

// ReleaseLease records a bounded retry after failed watch work. A stale or
// superseded exact lease returns (false,nil) and cannot alter the schedule.
func (store *Store) ReleaseLease(ctx context.Context, lease Lease, retryAt time.Time, errorCode string) (bool, error) {
	if err := store.validateContext(ctx); err != nil {
		return false, err
	}
	if !validUUIDv4(lease.WatchID) || !validUUIDv4(lease.ID) ||
		!validPublicTime(lease.Until) || lease.Until.Location() != time.UTC ||
		!validErrorCode(errorCode) || !validPublicTime(retryAt) || retryAt.Location() != time.UTC {
		return false, ErrInvalidLease
	}
	now, err := store.operationTime()
	if err != nil {
		return false, err
	}
	retryAt = retryAt.UTC()
	if !retryAt.After(now) || retryAt.After(now.Add(maximumRetryInterval)) {
		return false, ErrInvalidLease
	}
	matched := false
	err = store.ledger.Update(ctx, func(tx ledger.WriteTx) error {
		stored, err := loadWatchByID(ctx, tx, lease.WatchID)
		if errors.Is(err, ErrWatchNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if stored.Watch.State != StatePending && stored.Watch.State != StateActive {
			if stored.leaseID != "" || stored.leaseUntil != nil {
				return ErrCorruptStore
			}
			return nil
		}
		if stored.leaseID != lease.ID || stored.leaseUntil == nil || !stored.leaseUntil.Equal(lease.Until) {
			return nil
		}
		if !now.Before(*stored.leaseUntil) {
			return nil
		}
		updated, err := advanceTime(now, stored.Watch.UpdatedAt)
		if err != nil {
			return err
		}
		if !retryAt.After(updated) {
			return ErrInvalidLease
		}
		if stored.Watch.ConsecutiveFailures >= math.MaxInt32 {
			return ErrCorruptStore
		}
		writeResult, err := tx.ExecContext(ctx, `UPDATE watches SET
			lease_id = NULL, lease_until = NULL, next_check_at = ?,
			consecutive_failures = consecutive_failures + 1, last_error_code = ?, updated_at = ?
			WHERE id = ? AND state IN ('pending','active') AND lease_id = ? AND lease_until = ?`,
			formatTime(retryAt), errorCode, formatTime(updated), lease.WatchID,
			lease.ID, formatTime(lease.Until))
		if err != nil {
			return databaseError(ctx, err)
		}
		if err := requireOneRow(ctx, writeResult); err != nil {
			return err
		}
		matched = true
		return nil
	})
	if err != nil {
		return false, storeError(err)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return matched, nil
}

const watchColumns = `id, spec_hash, subject, predicate, freshness,
	min_independent_sources, on_conflict, state, next_check_at, ewma_interval_s,
	last_change_at, last_checked_at, consecutive_failures, last_error_code,
	last_verification_id, last_verification_claim_index, lease_id, lease_until,
	created_at, updated_at, paused_at, deleted_at`

type storedWatch struct {
	Watch
	specHash   string
	leaseID    string
	leaseUntil *time.Time
	deletedAt  *time.Time
}

type rowScanner interface {
	Scan(...any) error
}

func scanStoredWatch(row rowScanner) (storedWatch, error) {
	var value storedWatch
	var next, lastChange, lastChecked, leaseUntil, created, updated, paused, deleted sql.NullString
	var verificationID, leaseID sql.NullString
	var verificationIndex sql.NullInt64
	var state, conflict string
	var ewma float64
	err := row.Scan(
		&value.ID, &value.specHash, &value.Spec.Subject, &value.Spec.Predicate,
		&value.Spec.Freshness, &value.Spec.MinIndependentSources, &conflict, &state,
		&next, &ewma, &lastChange, &lastChecked, &value.ConsecutiveFailures,
		&value.LastErrorCode, &verificationID, &verificationIndex, &leaseID,
		&leaseUntil, &created, &updated, &paused, &deleted,
	)
	if err != nil {
		return storedWatch{}, err
	}
	value.Spec.OnConflict = models.FactConflictPolicy(conflict)
	value.State = State(state)
	value.LastVerificationID = verificationID.String
	value.leaseID = leaseID.String
	if verificationIndex.Valid {
		if verificationIndex.Int64 > math.MaxInt {
			return storedWatch{}, ErrCorruptStore
		}
		index := int(verificationIndex.Int64)
		value.LastVerificationClaimIndex = &index
	}
	var parseErr error
	if value.NextCheckAt, parseErr = parseNullableTime(next); parseErr != nil {
		return storedWatch{}, ErrCorruptStore
	}
	if value.LastChangeAt, parseErr = parseNullableTime(lastChange); parseErr != nil {
		return storedWatch{}, ErrCorruptStore
	}
	if value.LastCheckedAt, parseErr = parseNullableTime(lastChecked); parseErr != nil {
		return storedWatch{}, ErrCorruptStore
	}
	if value.leaseUntil, parseErr = parseNullableTime(leaseUntil); parseErr != nil {
		return storedWatch{}, ErrCorruptStore
	}
	if value.PausedAt, parseErr = parseNullableTime(paused); parseErr != nil {
		return storedWatch{}, ErrCorruptStore
	}
	if value.deletedAt, parseErr = parseNullableTime(deleted); parseErr != nil {
		return storedWatch{}, ErrCorruptStore
	}
	if !created.Valid || !updated.Valid {
		return storedWatch{}, ErrCorruptStore
	}
	if value.CreatedAt, parseErr = parseTime(created.String); parseErr != nil {
		return storedWatch{}, ErrCorruptStore
	}
	if value.UpdatedAt, parseErr = parseTime(updated.String); parseErr != nil {
		return storedWatch{}, ErrCorruptStore
	}
	if math.IsNaN(ewma) || math.IsInf(ewma, 0) || ewma < 600 || ewma > 604800 {
		return storedWatch{}, ErrCorruptStore
	}
	value.EWMAInterval = time.Duration(ewma * float64(time.Second))
	return value, nil
}

func loadLiveWatchByHash(ctx context.Context, tx ledger.ReadTx, hash string) (storedWatch, bool, error) {
	stored, err := scanStoredWatch(tx.QueryRowContext(ctx,
		`SELECT `+watchColumns+` FROM watches WHERE spec_hash = ? AND state <> 'deleted'`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return storedWatch{}, false, nil
	}
	if err != nil {
		return storedWatch{}, false, databaseError(ctx, err)
	}
	if validateStoredWatch(stored, false) != nil {
		return storedWatch{}, false, ErrCorruptStore
	}
	return stored, true, nil
}

func loadWatchByID(ctx context.Context, tx ledger.ReadTx, id string) (storedWatch, error) {
	stored, err := scanStoredWatch(tx.QueryRowContext(ctx,
		`SELECT `+watchColumns+` FROM watches WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return storedWatch{}, ErrWatchNotFound
	}
	if err != nil {
		return storedWatch{}, databaseError(ctx, err)
	}
	if validateStoredWatch(stored, true) != nil {
		return storedWatch{}, ErrCorruptStore
	}
	return stored, nil
}

func validateStoredWatch(value storedWatch, allowDeleted bool) error {
	if !validUUIDv4(value.ID) || !validHex(value.specHash, 64) ||
		value.ConsecutiveFailures < 0 || value.ConsecutiveFailures > math.MaxInt32 ||
		!validPublicTime(value.CreatedAt) || !validPublicTime(value.UpdatedAt) ||
		value.UpdatedAt.Before(value.CreatedAt) {
		return ErrCorruptStore
	}
	normalized, hash, err := normalizeFactSpec(value.Spec)
	if err != nil || normalized != value.Spec || hash != value.specHash {
		return ErrCorruptStore
	}
	if value.EWMAInterval < 10*time.Minute || value.EWMAInterval > 7*24*time.Hour {
		return ErrCorruptStore
	}
	if value.LastVerificationID == "" != (value.LastVerificationClaimIndex == nil) ||
		value.LastVerificationClaimIndex != nil && *value.LastVerificationClaimIndex < 0 ||
		value.LastVerificationID != "" && !validVerificationIdentity(value.LastVerificationID) ||
		value.LastVerificationID != "" && value.LastCheckedAt == nil {
		return ErrCorruptStore
	}
	if value.ConsecutiveFailures == 0 != (value.LastErrorCode == "") ||
		value.LastErrorCode != "" && !validErrorCode(value.LastErrorCode) {
		return ErrCorruptStore
	}
	if value.leaseID == "" != (value.leaseUntil == nil) ||
		value.leaseID != "" && !validUUIDv4(value.leaseID) {
		return ErrCorruptStore
	}
	if value.leaseUntil != nil && (!value.leaseUntil.After(value.UpdatedAt) || !validPublicTime(*value.leaseUntil)) {
		return ErrCorruptStore
	}
	for _, timestamp := range []*time.Time{
		value.NextCheckAt, value.LastChangeAt, value.LastCheckedAt, value.PausedAt, value.deletedAt,
	} {
		if timestamp != nil && !validPublicTime(*timestamp) {
			return ErrCorruptStore
		}
	}
	if value.NextCheckAt != nil && value.NextCheckAt.Before(value.CreatedAt) ||
		value.PausedAt != nil && (value.PausedAt.Before(value.CreatedAt) || value.PausedAt.After(value.UpdatedAt)) ||
		value.deletedAt != nil && (value.deletedAt.Before(value.CreatedAt) || value.deletedAt.After(value.UpdatedAt)) {
		return ErrCorruptStore
	}
	if value.LastCheckedAt != nil && (value.LastCheckedAt.Before(value.CreatedAt) || value.LastCheckedAt.After(value.UpdatedAt)) {
		return ErrCorruptStore
	}
	if value.LastChangeAt != nil && (value.LastCheckedAt == nil || value.LastChangeAt.Before(value.CreatedAt) ||
		value.LastChangeAt.After(*value.LastCheckedAt)) {
		return ErrCorruptStore
	}
	switch value.State {
	case StatePending, StateActive:
		if value.NextCheckAt == nil || value.PausedAt != nil || value.deletedAt != nil {
			return ErrCorruptStore
		}
	case StatePaused:
		if value.NextCheckAt != nil || value.PausedAt == nil || value.deletedAt != nil || value.leaseID != "" {
			return ErrCorruptStore
		}
	case StateDeleted:
		if !allowDeleted || value.NextCheckAt != nil || value.PausedAt != nil || value.deletedAt == nil || value.leaseID != "" {
			return ErrCorruptStore
		}
	default:
		return ErrCorruptStore
	}
	return nil
}

const factColumns = `id, watch_id, subject, predicate, path, value, root,
	source_url, receipt, snapshot_id, created_verification_id, created_claim_index,
	latest_verification_id, latest_claim_index, closed_verification_id,
	closed_claim_index, observed_at, valid_from, last_verified_at, valid_to,
	superseded_by, closed_outcome, gone_scope`

func loadOpenFact(ctx context.Context, tx ledger.ReadTx, watch Watch) (Fact, bool, error) {
	value, err := scanFact(tx.QueryRowContext(ctx,
		`SELECT `+factColumns+` FROM facts WHERE watch_id = ? AND valid_to IS NULL`, watch.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return Fact{}, false, nil
	}
	if err != nil {
		return Fact{}, false, databaseError(ctx, err)
	}
	if validateOpenFact(value, watch) != nil {
		return Fact{}, false, ErrCorruptStore
	}
	return cloneFact(value), true, nil
}

func scanFact(row rowScanner) (Fact, error) {
	var value Fact
	var rawValue string
	var closedID, validTo, superseded sql.NullString
	var closedIndex sql.NullInt64
	var observed, validFrom, lastVerified string
	var outcome, gone string
	err := row.Scan(
		&value.ID, &value.WatchID, &value.Subject, &value.Predicate, &value.Path,
		&rawValue, &value.Root, &value.SourceURL, &value.Receipt, &value.SnapshotID,
		&value.CreatedVerificationID, &value.CreatedClaimIndex,
		&value.LatestVerificationID, &value.LatestClaimIndex,
		&closedID, &closedIndex, &observed, &validFrom, &lastVerified, &validTo,
		&superseded, &outcome, &gone,
	)
	if err != nil {
		return Fact{}, err
	}
	value.Value = append(json.RawMessage(nil), rawValue...)
	value.ClosedVerificationID = closedID.String
	value.SupersededBy = superseded.String
	value.ClosedOutcome = ledger.Outcome(outcome)
	value.GoneScope = ledger.GoneScope(gone)
	if closedIndex.Valid {
		if closedIndex.Int64 > math.MaxInt {
			return Fact{}, ErrCorruptStore
		}
		index := int(closedIndex.Int64)
		value.ClosedClaimIndex = &index
	}
	var errTime error
	if value.ObservedAt, errTime = parseTime(observed); errTime != nil {
		return Fact{}, ErrCorruptStore
	}
	if value.ValidFrom, errTime = parseTime(validFrom); errTime != nil {
		return Fact{}, ErrCorruptStore
	}
	if value.LastVerifiedAt, errTime = parseTime(lastVerified); errTime != nil {
		return Fact{}, ErrCorruptStore
	}
	if value.ValidTo, errTime = parseNullableTime(validTo); errTime != nil {
		return Fact{}, ErrCorruptStore
	}
	return value, nil
}

func validateOpenFact(value Fact, watch Watch) error {
	if !validHex(value.ID, 64) || value.WatchID != watch.ID ||
		value.Subject != watch.Spec.Subject || value.Predicate != watch.Spec.Predicate ||
		value.Path == "" || len(value.Path) > 4096 || !validBoundedText(value.Path, true) ||
		!validVerificationIdentity(value.CreatedVerificationID) ||
		!validVerificationIdentity(value.LatestVerificationID) ||
		value.CreatedClaimIndex < 0 || value.LatestClaimIndex < 0 ||
		!validHex(strings.TrimPrefix(value.SnapshotID, "sha256:"), 64) ||
		!strings.HasPrefix(value.SnapshotID, "sha256:") ||
		len(value.Root) < 1 || len(value.Root) > 253 ||
		len(value.SourceURL) < 1 || len(value.SourceURL) > models.MaxSearchURLBytes ||
		len(value.Receipt) == 0 || len(value.Receipt) > maximumFactReceipt ||
		!validBoundedText(value.Receipt, true) ||
		len(value.Value) > maximumFactValue || !validScalar(value.Value) {
		return ErrCorruptStore
	}
	if value.ValidTo != nil || value.ClosedVerificationID != "" ||
		value.ClosedClaimIndex != nil || value.SupersededBy != "" ||
		value.ClosedOutcome != "" || value.GoneScope != "" {
		return ErrCorruptStore
	}
	if !value.ObservedAt.Equal(value.ValidFrom) || value.LastVerifiedAt.Before(value.ObservedAt) {
		return ErrCorruptStore
	}
	canonical, parsed, err := publicnet.NormalizeHTTPURL(value.SourceURL, nil, false)
	if err != nil || canonical != value.SourceURL || parsed == nil {
		return ErrCorruptStore
	}
	root, err := sourceRoot(parsed.Hostname())
	if err != nil || root != value.Root {
		return ErrCorruptStore
	}
	return nil
}

func validVerificationIdentity(value string) bool {
	return len(value) >= 1 && len(value) <= 512 && value == strings.TrimSpace(value) &&
		utf8.ValidString(value) && !containsControl(value)
}

func normalizeFactSpec(source models.FactSpec) (models.FactSpec, string, error) {
	source.Defaults()
	if len(source.Subject) > models.MaxAnswerSubjectBytes || !utf8.ValidString(source.Subject) ||
		containsControl(source.Subject) {
		return models.FactSpec{}, "", ErrInvalidWatchSpec
	}
	subject := strings.Join(strings.Fields(strings.TrimSpace(source.Subject)), " ")
	if subject == "" || utf8.RuneCountInString(subject) > models.MaxAnswerSubjectRunes ||
		len(strings.Fields(subject)) > models.MaxAnswerSubjectWords {
		return models.FactSpec{}, "", ErrInvalidWatchSpec
	}
	if source.Predicate == "" || len(source.Predicate) > models.MaxAnswerPredicateBytes ||
		!utf8.ValidString(source.Predicate) || containsControl(source.Predicate) || containsSpace(source.Predicate) {
		return models.FactSpec{}, "", ErrInvalidWatchSpec
	}
	if source.MinIndependentSources < 1 || source.MinIndependentSources > models.MaxAnswerMinIndependentSources ||
		source.OnConflict != models.FactConflictExpose {
		return models.FactSpec{}, "", ErrInvalidWatchSpec
	}
	if len(source.Freshness) > models.MaxAnswerFreshnessBytes || strings.TrimSpace(source.Freshness) == "" {
		return models.FactSpec{}, "", ErrInvalidWatchSpec
	}
	fresh, err := search.ParseFreshness(source.Freshness)
	if err != nil || fresh == search.FreshnessAny {
		return models.FactSpec{}, "", ErrInvalidWatchSpec
	}
	var freshness string
	switch fresh {
	case search.FreshnessDay:
		freshness = "day"
	case search.FreshnessWeek:
		freshness = "week"
	case search.FreshnessMonth:
		freshness = "month"
	case search.FreshnessYear:
		freshness = "year"
	default:
		return models.FactSpec{}, "", ErrInvalidWatchSpec
	}
	if utf8.RuneCountInString(subject)+1+utf8.RuneCountInString(source.Predicate) > models.MaxSearchQueryRunes ||
		len(strings.Fields(subject+" "+source.Predicate)) > models.MaxSearchQueryWords {
		return models.FactSpec{}, "", ErrInvalidWatchSpec
	}
	normalized := models.FactSpec{
		Subject: strings.Clone(subject), Predicate: strings.Clone(source.Predicate),
		Freshness: freshness, MinIndependentSources: source.MinIndependentSources,
		OnConflict: models.FactConflictExpose,
	}
	return normalized, factSpecHash(normalized), nil
}

func factSpecHash(spec models.FactSpec) string {
	hash := sha256.New()
	writeFrame(hash, "purify.watch.fact-spec.v1")
	writeFrame(hash, spec.Subject)
	writeFrame(hash, spec.Predicate)
	writeFrame(hash, spec.Freshness)
	writeFrame(hash, fmt.Sprintf("%d", spec.MinIndependentSources))
	writeFrame(hash, string(spec.OnConflict))
	return hex.EncodeToString(hash.Sum(nil))
}

type byteWriter interface{ Write([]byte) (int, error) }

func writeFrame(writer byteWriter, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = writer.Write(size[:])
	_, _ = writer.Write([]byte(value))
}

func initialEWMA(freshness string) time.Duration {
	if freshness == "day" {
		return 24 * time.Hour
	}
	return 7 * 24 * time.Hour
}

func (store *Store) validateContext(ctx context.Context) error {
	if store == nil || store.ledger == nil || store.clock == nil || store.idGenerator == nil ||
		store.leaseDuration <= 0 || store.leaseDuration > DefaultLeaseDuration || ctx == nil {
		return ErrInvalidStore
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func (store *Store) operationTime() (time.Time, error) {
	var now time.Time
	valid := false
	func() {
		defer func() { _ = recover() }()
		now = store.clock().UTC().Round(0)
		valid = true
	}()
	if !valid || !validPublicTime(now) {
		return time.Time{}, ErrInvalidStore
	}
	return now, nil
}

func (store *Store) generateID() (string, error) {
	var id string
	completed := false
	func() {
		defer func() { _ = recover() }()
		var err error
		id, err = store.idGenerator()
		completed = err == nil
	}()
	if !completed || !validUUIDv4(id) {
		return "", ErrInvalidStore
	}
	return strings.Clone(id), nil
}

func randomUUIDv4() (string, error) {
	var raw [16]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return "", err
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	encoded := hex.EncodeToString(raw[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

func validUUIDv4(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' ||
		value[14] != '4' || !strings.ContainsRune("89ab", rune(value[19])) {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}

func validHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}

func validErrorCode(value string) bool {
	if len(value) < 1 || len(value) > 64 || value != strings.TrimSpace(value) || value != strings.ToUpper(value) ||
		value[0] < 'A' || value[0] > 'Z' {
		return false
	}
	for _, character := range value {
		if !((character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '_') {
			return false
		}
	}
	return true
}

func validPublicTime(value time.Time) bool {
	if value.IsZero() || value.Year() < 1 || value.Year() > 9999 ||
		value.Location() != time.UTC || value != value.Round(0) {
		return false
	}
	_, err := value.MarshalJSON()
	return err == nil
}

func advanceTime(now, previous time.Time) (time.Time, error) {
	now = now.UTC().Round(0)
	if !now.After(previous) {
		if previous.Equal(time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)) {
			return time.Time{}, ErrCorruptStore
		}
		now = previous.Add(time.Nanosecond)
	}
	if !validPublicTime(now) {
		return time.Time{}, ErrCorruptStore
	}
	return now, nil
}

func formatTime(value time.Time) string { return value.UTC().Format(watchTimeLayout) }

func parseTime(value string) (time.Time, error) {
	if len(value) != 30 {
		return time.Time{}, ErrCorruptStore
	}
	parsed, err := time.Parse(watchTimeLayout, value)
	if err != nil || formatTime(parsed) != value || !validPublicTime(parsed) {
		return time.Time{}, ErrCorruptStore
	}
	return parsed, nil
}

func parseNullableTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	parsed, err := parseTime(value.String)
	if err != nil {
		return nil, err
	}
	return timePointer(parsed), nil
}

func requireOneRow(ctx context.Context, result sql.Result) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return databaseError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if changed != 1 {
		return ErrCorruptStore
	}
	return nil
}

func databaseError(ctx context.Context, err error) error {
	if ctx != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return ErrCorruptStore
}

func storeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, ledger.ErrClosed) {
		return ledger.ErrClosed
	}
	for _, stable := range []error{
		ErrInvalidStore, ErrInvalidWatchSpec, ErrInvalidWatchID, ErrWatchNotFound,
		ErrWatchLimit, ErrInvalidCursor, ErrInvalidLease, ErrCorruptStore,
	} {
		if errors.Is(err, stable) {
			return stable
		}
	}
	return ErrCorruptStore
}

func cloneWatch(value Watch) Watch {
	value.ID = strings.Clone(value.ID)
	value.Spec.Subject = strings.Clone(value.Spec.Subject)
	value.Spec.Predicate = strings.Clone(value.Spec.Predicate)
	value.Spec.Freshness = strings.Clone(value.Spec.Freshness)
	value.LastErrorCode = strings.Clone(value.LastErrorCode)
	value.LastVerificationID = strings.Clone(value.LastVerificationID)
	value.NextCheckAt = cloneTimePointer(value.NextCheckAt)
	value.LastChangeAt = cloneTimePointer(value.LastChangeAt)
	value.LastCheckedAt = cloneTimePointer(value.LastCheckedAt)
	value.PausedAt = cloneTimePointer(value.PausedAt)
	if value.LastVerificationClaimIndex != nil {
		index := *value.LastVerificationClaimIndex
		value.LastVerificationClaimIndex = &index
	}
	return value
}

func cloneFact(value Fact) Fact {
	value.Value = append(json.RawMessage(nil), value.Value...)
	value.ID = strings.Clone(value.ID)
	value.WatchID = strings.Clone(value.WatchID)
	value.Subject = strings.Clone(value.Subject)
	value.Predicate = strings.Clone(value.Predicate)
	value.Path = strings.Clone(value.Path)
	value.Root = strings.Clone(value.Root)
	value.SourceURL = strings.Clone(value.SourceURL)
	value.Receipt = strings.Clone(value.Receipt)
	value.SnapshotID = strings.Clone(value.SnapshotID)
	value.CreatedVerificationID = strings.Clone(value.CreatedVerificationID)
	value.LatestVerificationID = strings.Clone(value.LatestVerificationID)
	value.ClosedVerificationID = strings.Clone(value.ClosedVerificationID)
	value.SupersededBy = strings.Clone(value.SupersededBy)
	value.ValidTo = cloneTimePointer(value.ValidTo)
	if value.ClosedClaimIndex != nil {
		index := *value.ClosedClaimIndex
		value.ClosedClaimIndex = &index
	}
	return value
}

func cloneClaim(value Claim) Claim {
	value.Watch = cloneWatch(value.Watch)
	value.Lease.WatchID = strings.Clone(value.Lease.WatchID)
	value.Lease.ID = strings.Clone(value.Lease.ID)
	if value.Fact != nil {
		fact := cloneFact(*value.Fact)
		value.Fact = &fact
	}
	return value
}

func clonePage(value WatchPage) WatchPage {
	result := WatchPage{Items: make([]Watch, len(value.Items))}
	for index := range value.Items {
		result.Items[index] = cloneWatch(value.Items[index])
	}
	if value.Next != nil {
		copy := *value.Next
		copy.ID = strings.Clone(copy.ID)
		result.Next = &copy
	}
	return result
}

func timePointer(value time.Time) *time.Time { copy := value; return &copy }

func cloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	return timePointer(*value)
}

func containsControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}

func containsSpace(value string) bool {
	for _, character := range value {
		if unicode.IsSpace(character) {
			return true
		}
	}
	return false
}

func validBoundedText(value string, requireTrimmed bool) bool {
	if !utf8.ValidString(value) || containsControl(value) {
		return false
	}
	return !requireTrimmed || value == strings.TrimSpace(value)
}

func validScalar(raw json.RawMessage) bool {
	if len(raw) == 0 || !json.Valid(raw) || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return false
	}
	switch value.(type) {
	case string, bool, json.Number:
		return true
	default:
		return false
	}
}

func sourceRoot(hostname string) (string, error) {
	hostname = strings.ToLower(hostname)
	if address, err := netip.ParseAddr(hostname); err == nil {
		return address.Unmap().String(), nil
	}
	root, err := publicsuffix.EffectiveTLDPlusOne(hostname)
	if err != nil {
		return "", err
	}
	return strings.ToLower(root), nil
}
