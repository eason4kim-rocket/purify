package watch

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/publicnet"
)

// FactAt returns the one fact whose valid-time interval contains asOf. Fact
// intervals are half-open: valid_from is inclusive and valid_to is exclusive.
// An overlap is durable corruption rather than an arbitrary winner.
func (store *Store) FactAt(
	ctx context.Context,
	subject string,
	predicate string,
	asOf time.Time,
) (Fact, bool, error) {
	if err := store.validateContext(ctx); err != nil {
		return Fact{}, false, err
	}
	normalized, err := normalizeFactQuery(subject, predicate)
	if err != nil || !validFactAsOf(asOf) {
		return Fact{}, false, ErrInvalidWatchSpec
	}
	stamp := formatTime(asOf)

	var result Fact
	found := false
	err = store.ledger.View(ctx, func(tx ledger.ReadTx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+factColumns+`
			FROM facts
			WHERE subject = ? AND predicate = ? AND valid_from <= ?
				AND (valid_to IS NULL OR ? < valid_to)
			ORDER BY valid_from DESC, id LIMIT 2`,
			normalized.Subject, normalized.Predicate, stamp, stamp)
		if err != nil {
			return databaseError(ctx, err)
		}
		values := make([]Fact, 0, 2)
		for rows.Next() {
			value, err := scanFact(rows)
			if err != nil {
				_ = rows.Close()
				return databaseError(ctx, err)
			}
			if !validDurableFact(value, normalized.Subject, normalized.Predicate) {
				_ = rows.Close()
				return ErrCorruptStore
			}
			values = append(values, cloneFact(value))
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return databaseError(ctx, err)
		}
		if err := rows.Close(); err != nil {
			return databaseError(ctx, err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(values) > 1 {
			return ErrCorruptStore
		}
		if len(values) == 1 {
			if err := validateFactRelations(ctx, tx, values[0]); err != nil {
				return err
			}
			result = cloneFact(values[0])
			found = true
		}
		return nil
	})
	// A cancellation that races the final read or commit is more informative
	// than a derived durable-state error and prevents returning stale data.
	if contextErr := ctx.Err(); contextErr != nil {
		return Fact{}, false, contextErr
	}
	if err != nil {
		return Fact{}, false, storeError(err)
	}
	if !found {
		return Fact{}, false, nil
	}
	return cloneFact(result), true, nil
}

type durableFactVerification struct {
	URL           string
	FinalURL      sql.NullString
	Path          string
	OldValue      json.RawMessage
	NewValue      json.RawMessage
	Outcome       ledger.Outcome
	GoneScope     ledger.GoneScope
	OldSnapshotID string
	NewSnapshotID sql.NullString
	OldReceipt    sql.NullString
	Receipt       sql.NullString
	VerifiedAt    time.Time
}

func validateFactRelations(ctx context.Context, tx ledger.ReadTx, value Fact) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var linked int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM watches
		WHERE id = ? AND subject = ? AND predicate = ?
	)`, value.WatchID, value.Subject, value.Predicate).Scan(&linked); err != nil {
		return databaseError(ctx, err)
	}
	if linked != 1 {
		return ErrCorruptStore
	}

	created, found, err := loadFactVerification(ctx, tx, value.CreatedVerificationID, value.CreatedClaimIndex)
	if err != nil {
		return err
	}
	if !found || !validCreatedFactVerification(created, value) {
		return ErrCorruptStore
	}

	latest := created
	if value.LatestVerificationID != value.CreatedVerificationID || value.LatestClaimIndex != value.CreatedClaimIndex {
		latest, found, err = loadFactVerification(ctx, tx, value.LatestVerificationID, value.LatestClaimIndex)
		if err != nil {
			return err
		}
		if !found {
			return ErrCorruptStore
		}
	}
	if !validLatestFactVerification(latest, value,
		value.LatestVerificationID == value.CreatedVerificationID && value.LatestClaimIndex == value.CreatedClaimIndex) {
		return ErrCorruptStore
	}

	if value.ValidTo == nil {
		return nil
	}
	closed, found, err := loadFactVerification(ctx, tx, value.ClosedVerificationID, *value.ClosedClaimIndex)
	if err != nil {
		return err
	}
	if !found || !validClosedFactVerification(closed, value) {
		return ErrCorruptStore
	}

	if value.ClosedOutcome == ledger.OutcomeChanged {
		successor, err := scanFact(tx.QueryRowContext(ctx,
			`SELECT `+factColumns+` FROM facts WHERE id = ?`, value.SupersededBy))
		if err != nil {
			return databaseError(ctx, err)
		}
		if !validDurableFact(successor, value.Subject, value.Predicate) ||
			successor.ID != value.SupersededBy ||
			successor.WatchID != value.WatchID || successor.Path != value.Path ||
			successor.CreatedVerificationID != value.ClosedVerificationID ||
			successor.CreatedClaimIndex != *value.ClosedClaimIndex ||
			successor.ValidFrom != *value.ValidTo || successor.ObservedAt != *value.ValidTo ||
			!bytes.Equal(successor.Value, closed.NewValue) {
			return ErrCorruptStore
		}
		if !validCreatedFactVerification(closed, successor) {
			return ErrCorruptStore
		}
		successorLatest := closed
		sameVerification := successor.LatestVerificationID == successor.CreatedVerificationID &&
			successor.LatestClaimIndex == successor.CreatedClaimIndex
		if !sameVerification {
			successorLatest, found, err = loadFactVerification(ctx, tx,
				successor.LatestVerificationID, successor.LatestClaimIndex)
			if err != nil {
				return err
			}
			if !found {
				return ErrCorruptStore
			}
		}
		if !validLatestFactVerification(successorLatest, successor, sameVerification) {
			return ErrCorruptStore
		}
		return nil
	}

	var successorExists int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM facts
		WHERE watch_id = ? AND subject = ? AND predicate = ? AND path = ?
			AND created_verification_id = ? AND created_claim_index = ? AND valid_from = ?
	)`, value.WatchID, value.Subject, value.Predicate, value.Path,
		value.ClosedVerificationID, *value.ClosedClaimIndex, formatTime(*value.ValidTo)).Scan(&successorExists); err != nil {
		return databaseError(ctx, err)
	}
	if successorExists != 0 {
		return ErrCorruptStore
	}
	return nil
}

func loadFactVerification(
	ctx context.Context,
	tx ledger.ReadTx,
	verificationID string,
	claimIndex int,
) (durableFactVerification, bool, error) {
	var value durableFactVerification
	var oldValue string
	var newValue sql.NullString
	var outcome string
	var goneScope sql.NullString
	var verifiedAt string
	err := tx.QueryRowContext(ctx, `SELECT url, final_url, path, old_value, new_value,
		outcome, gone_scope, old_snapshot_id, new_snapshot_id, old_receipt, receipt, verified_at
		FROM verifications WHERE verification_id = ? AND claim_index = ?`,
		verificationID, claimIndex).Scan(
		&value.URL, &value.FinalURL, &value.Path, &oldValue, &newValue,
		&outcome, &goneScope, &value.OldSnapshotID, &value.NewSnapshotID,
		&value.OldReceipt, &value.Receipt, &verifiedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return durableFactVerification{}, false, nil
	}
	if err != nil {
		return durableFactVerification{}, false, databaseError(ctx, err)
	}
	value.OldValue = append(json.RawMessage(nil), oldValue...)
	if newValue.Valid {
		value.NewValue = append(json.RawMessage(nil), newValue.String...)
	}
	value.Outcome = ledger.Outcome(outcome)
	if goneScope.Valid {
		value.GoneScope = ledger.GoneScope(goneScope.String)
	}
	parsed, err := time.Parse(time.RFC3339Nano, verifiedAt)
	if err != nil || parsed.Location() != time.UTC || parsed.Format(time.RFC3339Nano) != verifiedAt || !validPublicTime(parsed) {
		return durableFactVerification{}, false, ErrCorruptStore
	}
	value.VerifiedAt = parsed
	return value, true, nil
}

func validCreatedFactVerification(verification durableFactVerification, value Fact) bool {
	if verification.Path != value.Path || verification.VerifiedAt != value.ObservedAt ||
		!validVerificationOutput(verification) {
		return false
	}
	switch verification.Outcome {
	case ledger.OutcomeConfirmed:
		return bytes.Equal(verification.OldValue, value.Value)
	case ledger.OutcomeChanged:
		return bytes.Equal(verification.NewValue, value.Value)
	default:
		return false
	}
}

func validLatestFactVerification(verification durableFactVerification, value Fact, isCreation bool) bool {
	if verification.Path != value.Path || verification.VerifiedAt != value.LastVerifiedAt ||
		!validVerificationOutput(verification) ||
		verification.NewSnapshotID.String != value.SnapshotID ||
		verification.Receipt.String != value.Receipt || verificationEffectiveURL(verification) != value.SourceURL {
		return false
	}
	if isCreation {
		return validCreatedFactVerification(verification, value)
	}
	return verification.Outcome == ledger.OutcomeConfirmed &&
		verification.GoneScope == "" && bytes.Equal(verification.OldValue, value.Value)
}

func validClosedFactVerification(verification durableFactVerification, value Fact) bool {
	if verification.Path != value.Path || verification.VerifiedAt != *value.ValidTo ||
		verification.URL != value.SourceURL || verification.Outcome != value.ClosedOutcome ||
		verification.GoneScope != value.GoneScope || !bytes.Equal(verification.OldValue, value.Value) ||
		verification.OldSnapshotID != value.SnapshotID || !verification.OldReceipt.Valid ||
		verification.OldReceipt.String != value.Receipt ||
		verification.FinalURL.Valid && verification.FinalURL.String == "" ||
		!validVerificationURL(verificationEffectiveURL(verification)) {
		return false
	}
	switch value.ClosedOutcome {
	case ledger.OutcomeChanged:
		return len(verification.NewValue) > 0 && validCanonicalFactScalar(verification.NewValue) &&
			validVerificationOutput(verification)
	case ledger.OutcomeGone:
		return len(verification.NewValue) == 0 && validVerificationSnapshot(verification.NewSnapshotID) &&
			(!verification.Receipt.Valid || verification.Receipt.String == "" ||
				len(verification.Receipt.String) <= maximumFactReceipt &&
					validBoundedText(verification.Receipt.String, true))
	default:
		return false
	}
}

func validVerificationOutput(value durableFactVerification) bool {
	return validVerificationSnapshot(value.NewSnapshotID) &&
		value.Receipt.Valid && len(value.Receipt.String) >= 1 && len(value.Receipt.String) <= maximumFactReceipt &&
		validBoundedText(value.Receipt.String, true) && validVerificationInput(value) &&
		validVerificationURL(verificationEffectiveURL(value))
}

func validVerificationInput(value durableFactVerification) bool {
	if !validCanonicalFactScalar(value.OldValue) || !validVerificationURL(value.URL) ||
		len(value.OldSnapshotID) != len("sha256:")+64 ||
		!strings.HasPrefix(value.OldSnapshotID, "sha256:") ||
		!validHex(value.OldSnapshotID[len("sha256:"):], 64) ||
		value.FinalURL.Valid && value.FinalURL.String == "" ||
		value.OldReceipt.Valid && (value.OldReceipt.String == "" ||
			len(value.OldReceipt.String) > maximumFactReceipt || !validBoundedText(value.OldReceipt.String, true)) {
		return false
	}
	return true
}

func validVerificationSnapshot(value sql.NullString) bool {
	return value.Valid && len(value.String) == len("sha256:")+64 &&
		strings.HasPrefix(value.String, "sha256:") && validHex(value.String[len("sha256:"):], 64)
}

func validVerificationURL(rawURL string) bool {
	if len(rawURL) < 1 || len(rawURL) > models.MaxSearchURLBytes {
		return false
	}
	canonical, parsed, err := publicnet.NormalizeHTTPURL(rawURL, nil, false)
	if err != nil || parsed == nil || canonical != rawURL {
		return false
	}
	_, err = sourceRoot(parsed.Hostname())
	return err == nil
}

func verificationEffectiveURL(value durableFactVerification) string {
	if value.FinalURL.Valid && value.FinalURL.String != "" {
		return value.FinalURL.String
	}
	return value.URL
}

func normalizeFactQuery(subject, predicate string) (models.FactSpec, error) {
	normalized, _, err := normalizeFactSpec(models.FactSpec{
		Subject:   subject,
		Predicate: predicate,
	})
	if err != nil {
		return models.FactSpec{}, ErrInvalidWatchSpec
	}
	return normalized, nil
}

func validFactAsOf(value time.Time) bool {
	if !validPublicTime(value) || value.Location() != time.UTC {
		return false
	}
	// SQLite rounds date-function inputs to milliseconds. At 500 microseconds
	// past the last representable millisecond this carries into year 10000 and
	// julianday returns NULL, so retain the exact upper edge accepted by the
	// migration's julianday(...) constraint.
	maximum := time.Date(9999, 12, 31, 23, 59, 59, 999499999, time.UTC)
	return !value.After(maximum)
}

func validDurableFact(value Fact, subject, predicate string) bool {
	if !validHex(value.ID, 64) || !validUUIDv4(value.WatchID) ||
		value.Subject != subject || value.Predicate != predicate {
		return false
	}
	normalized, err := normalizeFactQuery(value.Subject, value.Predicate)
	if err != nil || normalized.Subject != value.Subject || normalized.Predicate != value.Predicate {
		return false
	}
	if len(value.Path) < 1 || len(value.Path) > 4096 || !validBoundedText(value.Path, true) ||
		len(value.Value) < 1 || len(value.Value) > maximumFactValue || !validCanonicalFactScalar(value.Value) ||
		len(value.Root) < 1 || len(value.Root) > models.MaxSearchDomainBytes ||
		len(value.SourceURL) < 1 || len(value.SourceURL) > models.MaxSearchURLBytes ||
		len(value.Receipt) < 1 || len(value.Receipt) > maximumFactReceipt ||
		!validBoundedText(value.Receipt, true) ||
		len(value.SnapshotID) != len("sha256:")+64 ||
		!strings.HasPrefix(value.SnapshotID, "sha256:") || !validHex(value.SnapshotID[len("sha256:"):], 64) {
		return false
	}
	if !validVerificationIdentity(value.CreatedVerificationID) ||
		!validVerificationIdentity(value.LatestVerificationID) ||
		value.CreatedClaimIndex < 0 || value.LatestClaimIndex < 0 {
		return false
	}
	if !validPublicTime(value.ObservedAt) || !validPublicTime(value.ValidFrom) ||
		!validPublicTime(value.LastVerifiedAt) || !value.ObservedAt.Equal(value.ValidFrom) ||
		value.LastVerifiedAt.Before(value.ObservedAt) {
		return false
	}

	canonicalURL, parsedURL, err := publicnet.NormalizeHTTPURL(value.SourceURL, nil, false)
	if err != nil || parsedURL == nil || canonicalURL != value.SourceURL {
		return false
	}
	root, err := sourceRoot(parsedURL.Hostname())
	if err != nil || root != value.Root {
		return false
	}

	if value.ValidTo == nil {
		return value.ClosedVerificationID == "" && value.ClosedClaimIndex == nil &&
			value.SupersededBy == "" && value.ClosedOutcome == "" && value.GoneScope == ""
	}
	if !validPublicTime(*value.ValidTo) || !value.ValidTo.After(value.ValidFrom) ||
		value.ValidTo.Before(value.LastVerifiedAt) ||
		!validVerificationIdentity(value.ClosedVerificationID) ||
		value.ClosedClaimIndex == nil || *value.ClosedClaimIndex < 0 {
		return false
	}
	switch value.ClosedOutcome {
	case ledger.OutcomeChanged:
		return value.GoneScope == "" && validHex(value.SupersededBy, 64) && value.SupersededBy != value.ID
	case ledger.OutcomeGone:
		return value.SupersededBy == "" &&
			(value.GoneScope == ledger.GoneScopeField || value.GoneScope == ledger.GoneScopePage)
	default:
		return false
	}
}

func validCanonicalFactScalar(raw json.RawMessage) bool {
	if len(raw) == 0 || !utf8.Valid(raw) || !json.Valid(raw) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return false
	}
	switch value := decoded.(type) {
	case string:
		return bytes.Equal(raw, canonicalFactJSONString(value))
	case bool:
		if value {
			return bytes.Equal(raw, []byte("true"))
		}
		return bytes.Equal(raw, []byte("false"))
	case json.Number:
		return string(raw) == value.String() && validCanonicalFactNumber(value.String())
	default:
		return false
	}
}

func validCanonicalFactNumber(value string) bool {
	if value == "0" {
		return true
	}
	if value == "" || value == "-0" || strings.ContainsAny(value, "eE") {
		return false
	}
	if dot := strings.IndexByte(value, '.'); dot >= 0 {
		return dot+1 < len(value) && value[len(value)-1] != '0'
	}
	return true
}

func canonicalFactJSONString(value string) []byte {
	output := make([]byte, 0, len(value)+2)
	output = append(output, '"')
	for _, character := range value {
		switch character {
		case '"', '\\':
			output = append(output, '\\', byte(character))
		case '\b':
			output = append(output, `\b`...)
		case '\f':
			output = append(output, `\f`...)
		case '\n':
			output = append(output, `\n`...)
		case '\r':
			output = append(output, `\r`...)
		case '\t':
			output = append(output, `\t`...)
		default:
			if character < 0x20 {
				const hexadecimal = "0123456789abcdef"
				output = append(output, '\\', 'u', '0', '0', hexadecimal[character>>4], hexadecimal[character&0xf])
				continue
			}
			output = utf8.AppendRune(output, character)
		}
	}
	return append(output, '"')
}
