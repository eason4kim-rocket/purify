package compiler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/publicnet"
	"github.com/use-agent/purify/simhash"
	"github.com/use-agent/purify/snapshot"
)

const (
	HealTaskTimeout        = 2 * time.Minute
	HealLeaseDuration      = 3 * time.Minute
	HealFinalizeTimeout    = 5 * time.Second
	MaxHealHistoryScan     = 1000
	MaxHealReplayFacts     = 100
	MaxHealReplaySnapshots = 20
	MaxHealSnapshotBytes   = MaxHTMLBytes
	MaxHealSnapshotTotal   = MaxHealReplaySnapshots * MaxHealSnapshotBytes

	maxHealHistoryScalarBytes = 8 << 10
	maxHealHistoryTimeBytes   = 64
)

var (
	ErrInvalidHealer   = errors.New("compiler: invalid healer configuration")
	ErrHealRunNotFound = errors.New("compiler: extractor heal run not found")
	ErrHealLeaseBusy   = errors.New("compiler: extractor heal lease is busy")
	ErrHealLeaseLost   = errors.New("compiler: extractor heal lease was lost")
)

// HealState is the durable state machine owned by extractor_heal_runs.
type HealState string

const (
	HealPending   HealState = "pending"
	HealReplaying HealState = "replaying"
	HealPromoted  HealState = "promoted"
	HealDegraded  HealState = "degraded"
	HealFailed    HealState = "failed"
)

// HealResult is the bounded public view of one durable replay. Candidate IR,
// schemas, samples, webhook configuration, and storage errors are deliberately
// absent so lifecycle reporting cannot disclose persisted extraction inputs.
type HealResult struct {
	ID                  string
	SourceExtractorID   string
	SourceVersion       int
	Key                 CompileKey
	State               HealState
	TerminalReason      string
	ReplayTotal         int
	ReplayMatched       int
	ReplayRatio         *float64
	PromotedExtractorID string
	CreatedAt           time.Time
	UpdatedAt           time.Time
	CompletedAt         *time.Time
}

// HealSnapshotReader is the exact immutable-content and provenance surface a
// Healer needs. snapshot.Store satisfies it directly.
type HealSnapshotReader interface {
	Content(snapshot.ID, int) ([]byte, error)
	HasObservationContext(context.Context, snapshot.ID, func(snapshot.Meta) bool) (bool, error)
}

var _ HealSnapshotReader = (*snapshot.Store)(nil)

type healerConfig struct {
	webhookURL    string
	webhookSecret string
}

// HealerOption configures process-owned healing behavior.
type HealerOption func(*healerConfig) error

// WithHealWebhook configures an optional process-owned lifecycle destination.
// It accepts no per-request override. Literal private/local destinations and
// userinfo are rejected through the same public-network URL policy used by
// outbound fetches; errors never echo the supplied URL or secret.
func WithHealWebhook(rawURL, secret string) HealerOption {
	return func(config *healerConfig) error {
		if config == nil {
			return fmt.Errorf("%w: option target is nil", ErrInvalidHealer)
		}
		if len(rawURL) > ledger.MaxOutboxURLBytes || len(secret) > ledger.MaxOutboxSecretBytes ||
			!utf8.ValidString(rawURL) || !utf8.ValidString(secret) {
			return fmt.Errorf("%w: webhook configuration exceeds its resource limit", ErrInvalidHealer)
		}
		if strings.TrimSpace(rawURL) == "" {
			if secret != "" {
				return fmt.Errorf("%w: webhook secret requires a destination", ErrInvalidHealer)
			}
			config.webhookURL = ""
			config.webhookSecret = ""
			return nil
		}
		canonical, _, err := publicnet.NormalizeHTTPURL(rawURL, nil, false)
		if err != nil || len(canonical) > ledger.MaxOutboxURLBytes {
			return fmt.Errorf("%w: webhook destination is invalid", ErrInvalidHealer)
		}
		config.webhookURL = canonical
		config.webhookSecret = secret
		return nil
	}
}

// Healer claims durable candidates, replays exact confirmed source history,
// and promotes a passing revision in one serialized ledger transaction.
type Healer struct {
	registry  *Store
	ledger    *ledger.Store
	snapshots HealSnapshotReader
	clock     func() time.Time
	ids       func() (string, error)

	webhookURL     string
	webhookSecret  string
	taskTimeout    time.Duration
	leaseDuration  time.Duration
	releaseTimeout time.Duration
}

// NewHealer constructs the transport-neutral self-heal core. It starts no
// worker and owns no request credentials.
func NewHealer(registry *Store, snapshots HealSnapshotReader, options ...HealerOption) (*Healer, error) {
	if registry == nil || registry.ledger == nil || registry.clock == nil || isNilHealSnapshotReader(snapshots) {
		return nil, fmt.Errorf("%w: dependencies are nil or uninitialized", ErrInvalidHealer)
	}
	config := healerConfig{}
	for index, option := range options {
		if option == nil {
			return nil, fmt.Errorf("%w: option %d is nil", ErrInvalidHealer, index)
		}
		if err := option(&config); err != nil {
			return nil, err
		}
	}
	if _, err := normalizeCatalogTime(registry.clock()); err != nil {
		return nil, fmt.Errorf("%w: clock is invalid", ErrInvalidHealer)
	}
	return &Healer{
		registry: registry, ledger: registry.ledger, snapshots: snapshots,
		clock: registry.clock, ids: newExtractorID,
		webhookURL: config.webhookURL, webhookSecret: config.webhookSecret,
		taskTimeout: HealTaskTimeout, leaseDuration: HealLeaseDuration,
		releaseTimeout: HealFinalizeTimeout,
	}, nil
}

func isNilHealSnapshotReader(reader HealSnapshotReader) bool {
	if reader == nil {
		return true
	}
	value := reflect.ValueOf(reader)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// Heal claims the exact active run for key. If no pending or replaying row
// remains, the latest terminal row for the complete fixed key is returned
// idempotently. Histories for a nearby or different profile never match.
func (h *Healer) Heal(ctx context.Context, key CompileKey) (HealResult, error) {
	if err := h.validateContext(ctx); err != nil {
		return HealResult{}, err
	}
	if err := validateCompileKey(key); err != nil {
		return HealResult{}, fmt.Errorf("%w: compile key is invalid", ErrInvalidHealer)
	}
	return h.heal(ctx, healSelector{key: &key})
}

// HealRun claims one durable run by lowercase UUID. Terminal rows are decoded,
// revalidated, and returned without mutation or duplicate notification.
func (h *Healer) HealRun(ctx context.Context, runID string) (HealResult, error) {
	if err := h.validateContext(ctx); err != nil {
		return HealResult{}, err
	}
	if !validExtractorID(runID) {
		return HealResult{}, fmt.Errorf("%w: heal run ID is invalid", ErrInvalidHealer)
	}
	return h.heal(ctx, healSelector{id: runID})
}

func (h *Healer) validateContext(ctx context.Context) error {
	if h == nil || h.registry == nil || h.ledger == nil || h.clock == nil || h.ids == nil ||
		isNilHealSnapshotReader(h.snapshots) || h.taskTimeout <= 0 || h.leaseDuration <= 0 ||
		h.releaseTimeout <= 0 {
		return fmt.Errorf("%w: healer is nil or uninitialized", ErrInvalidHealer)
	}
	if ctx == nil {
		return fmt.Errorf("%w: context is nil", ErrInvalidHealer)
	}
	return ctx.Err()
}

type healSelector struct {
	id  string
	key *CompileKey
}

func (h *Healer) heal(ctx context.Context, selector healSelector) (HealResult, error) {
	taskCtx, cancel := context.WithTimeout(ctx, h.taskTimeout)
	defer cancel()
	run, leaseID, terminal, err := h.claim(taskCtx, selector)
	if err != nil {
		return cloneHealResult(run.result), err
	}
	if terminal {
		if _, err := validateStoredHealCandidate(run); err != nil {
			return HealResult{}, fmt.Errorf("%w: terminal run candidate is invalid", ErrInvalidHealer)
		}
		return cloneHealResult(run.result), nil
	}

	result, err := h.processClaim(taskCtx, run, leaseID)
	if err == nil || errors.Is(err, ErrHealLeaseLost) {
		return cloneHealResult(result), err
	}
	releaseErr := h.releaseClaim(run, leaseID)
	if releaseErr != nil {
		return cloneHealResult(result), errors.Join(err, releaseErr)
	}
	return cloneHealResult(result), err
}

type storedHealRun struct {
	result HealResult

	schemaJSON               string
	targetClusterHash        []byte
	catalogRevision          string
	candidateIRJSON          string
	candidateIRHash          string
	candidateIRFormatVersion int
	validationReportJSON     string
	validation               float64
	samplesJSON              string
	leaseID                  string
	leaseUntil               *time.Time
}

const healRunColumns = `id, source_extractor_id, source_version, host,
	schema_json, schema_hash, content_profile, target_template_cluster_id,
	target_cluster_simhash, catalog_revision, candidate_ir, candidate_ir_hash,
	candidate_ir_format_version, validation_report, validation, samples_json,
	state, terminal_reason, lease_id, lease_until, replay_total, replay_matched,
	replay_ratio, promoted_extractor_id, created_at, updated_at, completed_at`

func scanStoredHealRun(scanner rowScanner) (storedHealRun, error) {
	var (
		run                                          storedHealRun
		state, createdAt, updatedAt                  string
		leaseID, leaseUntil, promotedID, completedAt sql.NullString
		ratio                                        sql.NullFloat64
	)
	if err := scanner.Scan(
		&run.result.ID, &run.result.SourceExtractorID, &run.result.SourceVersion,
		&run.result.Key.Host, &run.schemaJSON, &run.result.Key.SchemaHash,
		&run.result.Key.ContentProfile, &run.result.Key.TemplateClusterID,
		&run.targetClusterHash, &run.catalogRevision, &run.candidateIRJSON,
		&run.candidateIRHash, &run.candidateIRFormatVersion,
		&run.validationReportJSON, &run.validation, &run.samplesJSON,
		&state, &run.result.TerminalReason, &leaseID, &leaseUntil,
		&run.result.ReplayTotal, &run.result.ReplayMatched, &ratio,
		&promotedID, &createdAt, &updatedAt, &completedAt,
	); err != nil {
		return storedHealRun{}, err
	}
	if !validExtractorID(run.result.ID) || !validExtractorID(run.result.SourceExtractorID) ||
		run.result.SourceVersion < 1 || len(run.targetClusterHash) != 8 ||
		!validLowerHex(run.catalogRevision, 64) || !validLowerHex(run.candidateIRHash, 64) ||
		run.candidateIRFormatVersion < 1 || math.IsNaN(run.validation) ||
		math.IsInf(run.validation, 0) || run.validation < ValidationThreshold || run.validation > 1 ||
		len(run.schemaJSON) > MaxCompileSchemaBytes || len(run.candidateIRJSON) > MaxRuleDefinitionBytes ||
		len(run.validationReportJSON) > MaxExtractorReportBytes || len(run.samplesJSON) > MaxCandidateSamplesJSONBytes {
		return storedHealRun{}, storeCorruption("heal run has invalid bounded metadata")
	}
	run.result.Key.ClusterSimHash = binary.BigEndian.Uint64(run.targetClusterHash)
	if err := validateCompileKey(run.result.Key); err != nil {
		return storedHealRun{}, storeCorruption("heal run has invalid target identity")
	}
	run.result.State = HealState(state)
	if !validHealState(run.result.State) || len(run.result.TerminalReason) > 256 ||
		run.result.ReplayTotal < 0 || run.result.ReplayMatched < 0 ||
		run.result.ReplayMatched > run.result.ReplayTotal {
		return storedHealRun{}, storeCorruption("heal run %q has invalid lifecycle metadata", run.result.ID)
	}
	if ratio.Valid {
		if math.IsNaN(ratio.Float64) || math.IsInf(ratio.Float64, 0) || ratio.Float64 < 0 || ratio.Float64 > 1 {
			return storedHealRun{}, storeCorruption("heal run %q has invalid replay ratio", run.result.ID)
		}
		run.result.ReplayRatio = healFloatPointer(ratio.Float64)
	}
	if promotedID.Valid {
		if !validExtractorID(promotedID.String) {
			return storedHealRun{}, storeCorruption("heal run %q has invalid promoted extractor", run.result.ID)
		}
		run.result.PromotedExtractorID = promotedID.String
	}
	var err error
	run.result.CreatedAt, err = parseExtractorTime(createdAt)
	if err != nil {
		return storedHealRun{}, storeCorruption("heal run %q has invalid created_at", run.result.ID)
	}
	run.result.UpdatedAt, err = parseExtractorTime(updatedAt)
	if err != nil || run.result.UpdatedAt.Before(run.result.CreatedAt) {
		return storedHealRun{}, storeCorruption("heal run %q has invalid updated_at", run.result.ID)
	}
	if completedAt.Valid {
		parsed, parseErr := parseExtractorTime(completedAt.String)
		if parseErr != nil || parsed.Before(run.result.UpdatedAt) {
			return storedHealRun{}, storeCorruption("heal run %q has invalid completed_at", run.result.ID)
		}
		run.result.CompletedAt = timePointer(parsed)
	}
	if leaseID.Valid {
		if !validExtractorID(leaseID.String) {
			return storedHealRun{}, storeCorruption("heal run %q has invalid lease", run.result.ID)
		}
		run.leaseID = leaseID.String
	}
	if leaseUntil.Valid {
		parsed, parseErr := parseExtractorTime(leaseUntil.String)
		if parseErr != nil || !parsed.After(run.result.UpdatedAt) {
			return storedHealRun{}, storeCorruption("heal run %q has invalid lease deadline", run.result.ID)
		}
		run.leaseUntil = timePointer(parsed)
	}
	if err := validateStoredHealLifecycle(run); err != nil {
		return storedHealRun{}, err
	}
	return run, nil
}

func validHealState(state HealState) bool {
	switch state {
	case HealPending, HealReplaying, HealPromoted, HealDegraded, HealFailed:
		return true
	default:
		return false
	}
}

func validateStoredHealLifecycle(run storedHealRun) error {
	terminal := run.result.State == HealPromoted || run.result.State == HealDegraded || run.result.State == HealFailed
	if run.result.ReplayTotal == 0 {
		if run.result.ReplayMatched != 0 || run.result.ReplayRatio != nil {
			return storeCorruption("heal run %q has inconsistent empty replay", run.result.ID)
		}
	} else if run.result.ReplayRatio == nil || math.Abs(
		*run.result.ReplayRatio-float64(run.result.ReplayMatched)/float64(run.result.ReplayTotal),
	) > 1e-12 {
		return storeCorruption("heal run %q has inconsistent replay ratio", run.result.ID)
	}
	switch run.result.State {
	case HealPending:
		if run.leaseID != "" || run.leaseUntil != nil || run.result.TerminalReason != "" ||
			run.result.ReplayTotal != 0 || run.result.PromotedExtractorID != "" || run.result.CompletedAt != nil {
			return storeCorruption("pending heal run %q has mutable state", run.result.ID)
		}
	case HealReplaying:
		if run.leaseID == "" || run.leaseUntil == nil || run.result.TerminalReason != "" ||
			run.result.PromotedExtractorID != "" || run.result.CompletedAt != nil {
			return storeCorruption("replaying heal run %q has invalid lease state", run.result.ID)
		}
	case HealPromoted:
		if run.result.TerminalReason != "replay_passed" || run.leaseID != "" || run.leaseUntil != nil ||
			run.result.ReplayTotal == 0 || run.result.ReplayRatio == nil || *run.result.ReplayRatio < ValidationThreshold ||
			run.result.PromotedExtractorID == "" || run.result.CompletedAt == nil {
			return storeCorruption("promoted heal run %q has invalid terminal state", run.result.ID)
		}
	case HealDegraded, HealFailed:
		if run.result.TerminalReason == "" || run.leaseID != "" || run.leaseUntil != nil ||
			run.result.PromotedExtractorID != "" || run.result.CompletedAt == nil {
			return storeCorruption("terminal heal run %q has invalid terminal state", run.result.ID)
		}
	}
	if terminal != (run.result.CompletedAt != nil) {
		return storeCorruption("heal run %q terminal marker is inconsistent", run.result.ID)
	}
	return nil
}

func (h *Healer) claim(ctx context.Context, selector healSelector) (storedHealRun, string, bool, error) {
	var claimed storedHealRun
	terminal := false
	leaseID := ""
	err := h.ledger.Update(ctx, func(tx ledger.WriteTx) error {
		run, found, err := loadSelectedHealRun(ctx, tx, selector)
		if err != nil {
			return err
		}
		if !found {
			return ErrHealRunNotFound
		}
		if run.result.State == HealPromoted || run.result.State == HealDegraded || run.result.State == HealFailed {
			claimed, terminal = run, true
			return nil
		}
		now, err := normalizeCatalogTime(maxExtractorTime(h.clock(), run.result.UpdatedAt))
		if err != nil {
			return fmt.Errorf("%w: clock is invalid", ErrInvalidHealer)
		}
		if run.result.State == HealReplaying && run.leaseUntil != nil && run.leaseUntil.After(now) {
			claimed = run
			return ErrHealLeaseBusy
		}
		leaseID, err = h.ids()
		if err != nil {
			return fmt.Errorf("compiler: generate heal lease ID: %w", err)
		}
		if !validExtractorID(leaseID) {
			return fmt.Errorf("%w: heal lease generator returned an invalid UUID", ErrInvalidHealer)
		}
		leaseUntil, err := normalizeCatalogTime(now.Add(h.leaseDuration))
		if err != nil {
			return fmt.Errorf("%w: lease deadline is invalid", ErrInvalidHealer)
		}
		writeResult, err := tx.ExecContext(ctx, `UPDATE extractor_heal_runs SET
			state = 'replaying', terminal_reason = '', lease_id = ?, lease_until = ?,
			replay_total = 0, replay_matched = 0, replay_ratio = NULL,
			promoted_extractor_id = NULL, completed_at = NULL, updated_at = ?
			WHERE id = ? AND (
				state = 'pending' OR
				(state = 'replaying' AND lease_id = ? AND lease_until <= ?)
			)`, leaseID, formatExtractorTime(leaseUntil), formatExtractorTime(now),
			run.result.ID, nullableLeaseID(run.leaseID), formatExtractorTime(now))
		if err != nil {
			return fmt.Errorf("compiler: claim extractor heal run: %w", err)
		}
		changed, err := writeResult.RowsAffected()
		if err != nil {
			return fmt.Errorf("compiler: inspect extractor heal claim: %w", err)
		}
		if changed != 1 {
			return ErrHealLeaseLost
		}
		run.result.State = HealReplaying
		run.result.TerminalReason = ""
		run.result.ReplayTotal = 0
		run.result.ReplayMatched = 0
		run.result.ReplayRatio = nil
		run.result.PromotedExtractorID = ""
		run.result.CompletedAt = nil
		run.result.UpdatedAt = now
		run.leaseID = leaseID
		run.leaseUntil = timePointer(leaseUntil)
		claimed = run
		return nil
	})
	if err != nil {
		return claimed, "", false, err
	}
	return claimed, leaseID, terminal, nil
}

func nullableLeaseID(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func loadSelectedHealRun(ctx context.Context, tx ledger.ReadTx, selector healSelector) (storedHealRun, bool, error) {
	if selector.id != "" {
		run, err := scanStoredHealRun(tx.QueryRowContext(ctx,
			`SELECT `+healRunColumns+` FROM extractor_heal_runs WHERE id = ?`, selector.id))
		if errors.Is(err, sql.ErrNoRows) {
			return storedHealRun{}, false, nil
		}
		if err != nil {
			return storedHealRun{}, false, fmt.Errorf("compiler: load extractor heal run: %w", err)
		}
		return run, true, nil
	}
	if selector.key == nil {
		return storedHealRun{}, false, fmt.Errorf("%w: heal selector is empty", ErrInvalidHealer)
	}
	key := *selector.key
	arguments := []any{key.Host, key.SchemaHash, key.ContentProfile, key.TemplateClusterID, encodeSimHash(key.ClusterSimHash)}
	run, err := scanStoredHealRun(tx.QueryRowContext(ctx, `SELECT `+healRunColumns+`
		FROM extractor_heal_runs
		WHERE host = ? AND schema_hash = ? AND content_profile = ? AND
			target_template_cluster_id = ? AND target_cluster_simhash = ? AND
			state IN ('pending', 'replaying')
		ORDER BY created_at DESC, id DESC LIMIT 1`, arguments...))
	if errors.Is(err, sql.ErrNoRows) {
		run, err = scanStoredHealRun(tx.QueryRowContext(ctx, `SELECT `+healRunColumns+`
			FROM extractor_heal_runs
			WHERE host = ? AND schema_hash = ? AND content_profile = ? AND
				target_template_cluster_id = ? AND target_cluster_simhash = ? AND
				state IN ('promoted', 'degraded', 'failed')
			ORDER BY completed_at DESC, id DESC LIMIT 1`, arguments...))
	}
	if errors.Is(err, sql.ErrNoRows) {
		return storedHealRun{}, false, nil
	}
	if err != nil {
		return storedHealRun{}, false, fmt.Errorf("compiler: load exact extractor heal target: %w", err)
	}
	if run.result.Key != key {
		return storedHealRun{}, false, storeCorruption("exact heal target returned a mismatched key")
	}
	return run, true, nil
}

func validateStoredHealCandidate(run storedHealRun) (validatedCandidate, error) {
	canonicalSchema, err := canonicalSchemaJSON(json.RawMessage(run.schemaJSON))
	if err != nil || string(canonicalSchema) != run.schemaJSON || sha256Hex(canonicalSchema) != run.result.Key.SchemaHash {
		return validatedCandidate{}, storeCorruption("heal run %q schema is not canonical", run.result.ID)
	}
	var ir IR
	if err := strictJSONDecode([]byte(run.candidateIRJSON), &ir); err != nil ||
		ir.Version != run.candidateIRFormatVersion || sha256Hex([]byte(run.candidateIRJSON)) != run.candidateIRHash {
		return validatedCandidate{}, storeCorruption("heal run %q candidate IR is invalid", run.result.ID)
	}
	var report ValidationReport
	if err := strictJSONDecode([]byte(run.validationReportJSON), &report); err != nil || report.Overall != run.validation {
		return validatedCandidate{}, storeCorruption("heal run %q validation report is invalid", run.result.ID)
	}
	var durableSamples []durableCandidateSample
	if err := strictJSONDecode([]byte(run.samplesJSON), &durableSamples); err != nil {
		return validatedCandidate{}, storeCorruption("heal run %q samples are invalid", run.result.ID)
	}
	samples := make([]SampleRef, len(durableSamples))
	for index, value := range durableSamples {
		fingerprint, parseErr := strconv.ParseUint(value.SampleSimHash, 16, 64)
		fetchedAt, timeErr := parseExtractorTime(value.FetchedAt)
		if parseErr != nil || timeErr != nil || len(value.SampleSimHash) != 16 ||
			strings.ToLower(value.SampleSimHash) != value.SampleSimHash {
			return validatedCandidate{}, storeCorruption("heal run %q sample %d is invalid", run.result.ID, index)
		}
		samples[index] = SampleRef{
			PageHash: value.PageHash, SnapshotID: value.SnapshotID,
			SampleSimHash: fingerprint, FetchedAt: fetchedAt,
		}
	}
	candidate := Candidate{
		Key: run.result.Key, CatalogRevision: run.catalogRevision,
		Schema: canonicalSchema, IR: ir, Validation: report, Samples: samples,
	}
	validated, err := validateCandidate(candidate)
	if err != nil || string(validated.irJSON) != run.candidateIRJSON ||
		validated.irHash != run.candidateIRHash || string(validated.reportJSON) != run.validationReportJSON ||
		string(validated.samplesJSON) != run.samplesJSON {
		return validatedCandidate{}, storeCorruption("heal run %q immutable candidate failed revalidation", run.result.ID)
	}
	return validated, nil
}

func strictJSONDecode(encoded []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("JSON contains more than one value")
		}
		return err
	}
	return nil
}

type replayScore struct {
	total   int
	matched int
}

func (score replayScore) ratio() *float64 {
	if score.total == 0 {
		return nil
	}
	return healFloatPointer(float64(score.matched) / float64(score.total))
}

func (score replayScore) passes() bool {
	return score.total > 0 && score.matched*10 >= score.total*9
}

func (h *Healer) processClaim(ctx context.Context, run storedHealRun, leaseID string) (HealResult, error) {
	validated, err := validateStoredHealCandidate(run)
	if err != nil {
		return h.finalize(ctx, run, leaseID, replayScore{}, HealFailed, "invalid_candidate")
	}
	program, err := newReplayProgram(validated.value.IR)
	if err != nil {
		return h.finalize(ctx, run, leaseID, replayScore{}, HealFailed, "invalid_candidate")
	}

	source, usable, err := h.loadReplaySource(ctx, run)
	if err != nil {
		return run.result, err
	}
	if !usable {
		return h.finalize(ctx, run, leaseID, replayScore{}, HealDegraded, "source_changed")
	}
	facts, score, err := h.loadReplayFacts(ctx, run, source)
	if err != nil {
		return run.result, err
	}
	if len(facts) != 0 {
		matched, replayErr := h.replayFacts(ctx, program, facts)
		if replayErr != nil {
			return run.result, replayErr
		}
		score.matched += matched
	}
	if score.total == 0 {
		return h.finalize(ctx, run, leaseID, score, HealDegraded, "insufficient_history")
	}
	if !score.passes() {
		return h.finalize(ctx, run, leaseID, score, HealDegraded, "replay_below_threshold")
	}
	return h.finalize(ctx, run, leaseID, score, HealPromoted, "replay_passed")
}

func (h *Healer) loadReplaySource(ctx context.Context, run storedHealRun) (Extractor, bool, error) {
	var source Extractor
	err := h.ledger.View(ctx, func(tx ledger.ReadTx) error {
		value, err := loadExtractorByID(ctx, tx, run.result.SourceExtractorID)
		if err != nil {
			return err
		}
		source = value
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrExtractorNotFound) || errors.Is(err, ErrExtractorStore) {
			return Extractor{}, false, nil
		}
		return Extractor{}, false, err
	}
	if source.ID != run.result.SourceExtractorID || source.Version != run.result.SourceVersion ||
		source.Host != run.result.Key.Host || source.SchemaHash != run.result.Key.SchemaHash ||
		source.State != StateStale {
		return source, false, nil
	}
	return source, true, nil
}

type replayFact struct {
	snapshotID snapshot.ID
	fieldName  string
	expected   json.RawMessage
	url        string
	verifiedAt time.Time
}

type replaySnapshot struct {
	id    snapshot.ID
	facts []replayFact
}

func (h *Healer) loadReplayFacts(
	ctx context.Context,
	run storedHealRun,
	source Extractor,
) ([]replaySnapshot, replayScore, error) {
	groups := make([]replaySnapshot, 0, MaxHealReplaySnapshots)
	groupIndex := make(map[snapshot.ID]int, MaxHealReplaySnapshots)
	seen := make(map[string]struct{}, MaxHealReplayFacts)
	score := replayScore{}
	err := h.ledger.View(ctx, func(tx ledger.ReadTx) error {
		rows, err := tx.QueryContext(ctx, `SELECT
			substr(CAST(path AS BLOB), 1, ?), length(CAST(path AS BLOB)),
			substr(CAST(old_value AS BLOB), 1, ?), length(CAST(old_value AS BLOB)),
			substr(CAST(COALESCE(new_snapshot_id, '') AS BLOB), 1, ?),
				length(CAST(COALESCE(new_snapshot_id, '') AS BLOB)),
			substr(CAST(url AS BLOB), 1, ?), length(CAST(url AS BLOB)),
			substr(CAST(COALESCE(final_url, '') AS BLOB), 1, ?),
				length(CAST(COALESCE(final_url, '') AS BLOB)),
			substr(CAST(verified_at AS BLOB), 1, ?), length(CAST(verified_at AS BLOB))
			FROM verifications
			WHERE extractor_id = ? AND schema_hash = ? AND template_cluster_id = ? AND
				outcome = 'confirmed' AND julianday(verified_at) <= julianday(?)
			ORDER BY verified_at DESC, id DESC LIMIT ?`,
			MaxFieldNameBytes+1, maxHealHistoryScalarBytes+1, len("sha256:")+64+1,
			MaxExtractorURLBytes+1, MaxExtractorURLBytes+1, maxHealHistoryTimeBytes+1,
			run.result.SourceExtractorID, run.result.Key.SchemaHash,
			source.TemplateClusterID, formatExtractorTime(run.result.CreatedAt), MaxHealHistoryScan)
		if err != nil {
			return fmt.Errorf("compiler: query extractor replay history: %w", err)
		}
		defer rows.Close()
		for rows.Next() && score.total < MaxHealReplayFacts {
			var path, oldValue, replaySnapshotID, rawURL, rawFinalURL, rawVerifiedAt []byte
			var pathBytes, valueBytes, snapshotBytes, urlBytes, finalURLBytes, verifiedAtBytes int
			if err := rows.Scan(
				&path, &pathBytes, &oldValue, &valueBytes, &replaySnapshotID, &snapshotBytes,
				&rawURL, &urlBytes, &rawFinalURL, &finalURLBytes, &rawVerifiedAt, &verifiedAtBytes,
			); err != nil {
				return fmt.Errorf("compiler: scan extractor replay history: %w", err)
			}
			if pathBytes < 0 || valueBytes < 0 || snapshotBytes < 0 || urlBytes < 0 || finalURLBytes < 0 || verifiedAtBytes < 0 ||
				pathBytes > MaxFieldNameBytes || valueBytes > maxHealHistoryScalarBytes ||
				snapshotBytes != len("sha256:")+64 || urlBytes > MaxExtractorURLBytes ||
				finalURLBytes > MaxExtractorURLBytes ||
				verifiedAtBytes > maxHealHistoryTimeBytes || !utf8.Valid(path) || !utf8.Valid(oldValue) ||
				!utf8.Valid(replaySnapshotID) || !utf8.Valid(rawURL) || !utf8.Valid(rawFinalURL) ||
				!utf8.Valid(rawVerifiedAt) {
				score.total++
				continue
			}
			verifiedAt, timeErr := time.Parse(time.RFC3339Nano, string(rawVerifiedAt))
			if timeErr != nil {
				score.total++
				continue
			}
			verifiedAt = verifiedAt.UTC()
			if verifiedAt.After(run.result.CreatedAt) {
				// The SQL cutoff is indexed and normally definitive. Parse again
				// to fail closed across legacy RFC3339 fractional-width variants.
				continue
			}
			fieldName, pathErr := healCompiledFieldName(string(path))
			observationURL := rawURL
			if len(rawFinalURL) != 0 {
				observationURL = rawFinalURL
			}
			canonicalURL, _, urlErr := publicnet.NormalizeHTTPURL(string(observationURL), nil, true)
			canonicalExpected, scalarOK := canonicalStrictReplayScalar(json.RawMessage(oldValue))
			if pathErr != nil || urlErr != nil || len(canonicalURL) > MaxExtractorURLBytes ||
				!validSnapshotID(string(replaySnapshotID)) || !scalarOK {
				score.total++
				continue
			}
			identity := string(replaySnapshotID) + "\x00" + fieldName + "\x00" + string(canonicalExpected)
			if _, duplicate := seen[identity]; duplicate {
				continue
			}
			seen[identity] = struct{}{}
			score.total++
			snapshotID := snapshot.ID(string(replaySnapshotID))
			index, exists := groupIndex[snapshotID]
			if !exists {
				if len(groups) >= MaxHealReplaySnapshots {
					continue
				}
				index = len(groups)
				groupIndex[snapshotID] = index
				groups = append(groups, replaySnapshot{id: snapshotID})
			}
			groups[index].facts = append(groups[index].facts, replayFact{
				snapshotID: snapshotID, fieldName: fieldName,
				expected: append(json.RawMessage(nil), canonicalExpected...),
				url:      canonicalURL, verifiedAt: verifiedAt,
			})
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("compiler: iterate extractor replay history: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, replayScore{}, err
	}
	return groups, score, nil
}

func healCompiledFieldName(path string) (string, error) {
	var field strings.Builder
	field.Grow(len(path))
	for index := 0; index < len(path); index++ {
		switch path[index] {
		case '.':
			return "", errors.New("unescaped nested separator")
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
	if name == "" || strings.ReplaceAll(name, ".", `\.`) != path {
		return "", errors.New("path is not a canonical compiled scalar")
	}
	return name, nil
}

func strictReplayScalarEqual(first, second json.RawMessage) bool {
	left, leftOK := canonicalStrictReplayScalar(first)
	right, rightOK := canonicalStrictReplayScalar(second)
	return leftOK && rightOK && bytes.Equal(left, right)
}

func canonicalStrictReplayScalar(raw json.RawMessage) (json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, false
	}
	switch value := decoded.(type) {
	case string:
		encoded, err := json.Marshal(value)
		return json.RawMessage(encoded), err == nil
	case json.Number:
		if !json.Valid([]byte(value.String())) {
			return nil, false
		}
		return json.RawMessage(value.String()), true
	case bool:
		if value {
			return json.RawMessage("true"), true
		}
		return json.RawMessage("false"), true
	default:
		return nil, false
	}
}

func (h *Healer) replayFacts(
	ctx context.Context,
	program *replayProgram,
	groups []replaySnapshot,
) (int, error) {
	matched := 0
	totalBytes := 0
	for _, group := range groups {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		html, err := h.snapshots.Content(group.id, MaxHealSnapshotBytes)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return 0, ctxErr
			}
			continue
		}
		if len(html) > MaxHealSnapshotBytes || len(html) > MaxHealSnapshotTotal-totalBytes {
			continue
		}
		totalBytes += len(html)
		provenance := make([]bool, len(group.facts))
		remaining := len(group.facts)
		_, err = h.snapshots.HasObservationContext(ctx, group.id, func(meta snapshot.Meta) bool {
			if meta.StatusCode < 200 || meta.StatusCode >= 300 || meta.FetchedAt.IsZero() {
				return false
			}
			observedURL, _, normalizeErr := publicnet.NormalizeHTTPURL(meta.URL, nil, true)
			if normalizeErr != nil {
				return false
			}
			for index := range group.facts {
				if !provenance[index] && observedURL == group.facts[index].url &&
					!meta.FetchedAt.UTC().After(group.facts[index].verifiedAt) {
					provenance[index] = true
					remaining--
				}
			}
			return remaining == 0
		})
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return 0, ctxErr
			}
			continue
		}
		fieldNames := make([]string, 0, len(group.facts))
		for index := range group.facts {
			if provenance[index] {
				fieldNames = append(fieldNames, group.facts[index].fieldName)
			}
		}
		if len(fieldNames) == 0 {
			continue
		}
		results, err := program.replay(string(html), fieldNames)
		if err != nil {
			continue
		}
		for index, fact := range group.facts {
			if !provenance[index] {
				continue
			}
			evaluation, exists := results[fact.fieldName]
			if exists && evaluation.err == nil && evaluation.result.Found &&
				strictReplayScalarEqual(evaluation.result.Value, fact.expected) {
				matched++
			}
		}
	}
	return matched, nil
}

func (h *Healer) finalize(
	ctx context.Context,
	run storedHealRun,
	leaseID string,
	score replayScore,
	desired HealState,
	reason string,
) (HealResult, error) {
	if desired != HealPromoted && desired != HealDegraded && desired != HealFailed {
		return run.result, fmt.Errorf("%w: unsupported terminal state", ErrInvalidHealer)
	}
	var result HealResult
	err := h.ledger.Update(ctx, func(tx ledger.WriteTx) error {
		current, err := scanStoredHealRun(tx.QueryRowContext(ctx,
			`SELECT `+healRunColumns+` FROM extractor_heal_runs WHERE id = ?`, run.result.ID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrHealLeaseLost
		}
		if err != nil {
			return fmt.Errorf("compiler: reload extractor heal run: %w", err)
		}
		if current.result.State != HealReplaying || current.leaseID != leaseID ||
			!sameStoredHealIdentity(current, run) {
			return ErrHealLeaseLost
		}
		now, err := normalizeCatalogTime(maxExtractorTime(h.clock(), current.result.UpdatedAt))
		if err != nil {
			return fmt.Errorf("%w: clock is invalid", ErrInvalidHealer)
		}

		terminalState, terminalReason := desired, reason
		promotedID := ""
		if desired == HealPromoted {
			promotedID, terminalState, terminalReason, err = h.promote(ctx, tx, current, score, now)
			if err != nil {
				return err
			}
		}
		result, err = h.writeTerminal(ctx, tx, current, leaseID, score,
			terminalState, terminalReason, promotedID, now)
		return err
	})
	if err != nil {
		return run.result, err
	}
	return result, nil
}

func sameStoredHealIdentity(first, second storedHealRun) bool {
	return first.result.ID == second.result.ID &&
		first.result.SourceExtractorID == second.result.SourceExtractorID &&
		first.result.SourceVersion == second.result.SourceVersion &&
		first.result.Key == second.result.Key && first.schemaJSON == second.schemaJSON &&
		bytes.Equal(first.targetClusterHash, second.targetClusterHash) &&
		first.catalogRevision == second.catalogRevision &&
		first.candidateIRJSON == second.candidateIRJSON &&
		first.candidateIRHash == second.candidateIRHash &&
		first.candidateIRFormatVersion == second.candidateIRFormatVersion &&
		first.validationReportJSON == second.validationReportJSON &&
		first.validation == second.validation && first.samplesJSON == second.samplesJSON &&
		first.result.CreatedAt.Equal(second.result.CreatedAt)
}

func (h *Healer) promote(
	ctx context.Context,
	tx ledger.WriteTx,
	run storedHealRun,
	score replayScore,
	now time.Time,
) (string, HealState, string, error) {
	validated, err := validateStoredHealCandidate(run)
	if err != nil {
		return "", HealFailed, "invalid_candidate", nil
	}
	source, err := loadExtractorByID(ctx, tx, run.result.SourceExtractorID)
	if err != nil {
		if errors.Is(err, ErrExtractorNotFound) || errors.Is(err, ErrExtractorStore) {
			return "", HealDegraded, "source_changed", nil
		}
		return "", "", "", err
	}
	if source.Version != run.result.SourceVersion || source.Host != run.result.Key.Host ||
		source.SchemaHash != run.result.Key.SchemaHash || source.State != StateStale {
		return "", HealDegraded, "source_changed", nil
	}
	latestSource, found, err := loadLatestHealClusterRevision(
		ctx, tx, source.Host, source.SchemaHash, source.TemplateClusterID,
	)
	if err != nil {
		return "", "", "", err
	}
	if !found || latestSource.ID != source.ID || latestSource.Version != source.Version ||
		latestSource.State != StateStale {
		return "", HealDegraded, "source_changed", nil
	}
	pageKey := PageKey{Host: run.result.Key.Host, Schema: validated.value.Schema,
		SchemaHash: run.result.Key.SchemaHash, TemplateSimHash: run.result.Key.ClusterSimHash}
	active, err := loadActiveMetadata(ctx, tx, pageKey)
	if err != nil {
		return "", "", "", err
	}
	for _, value := range active {
		if simhash.Distance(value.TemplateSimHash, run.result.Key.ClusterSimHash) <= TemplateDistanceThreshold {
			return "", HealDegraded, "active_conflict", nil
		}
	}
	sameCluster := source.TemplateClusterID == run.result.Key.TemplateClusterID
	if !sameCluster {
		if target, found, loadErr := loadLatestCandidateTarget(ctx, tx, run.result.Key); loadErr != nil {
			return "", "", "", loadErr
		} else if found || target.ID != "" {
			return "", HealDegraded, "target_conflict", nil
		}
		var clusterCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT template_cluster_id)
			FROM extractors WHERE host = ? AND schema_hash = ?`,
			run.result.Key.Host, run.result.Key.SchemaHash).Scan(&clusterCount); err != nil {
			return "", "", "", fmt.Errorf("compiler: count promoted extractor clusters: %w", err)
		}
		if clusterCount >= MaxTemplateClusters {
			return "", HealDegraded, "cluster_limit", nil
		}
		foreign, err := driftSamplesHaveForeignBinding(ctx, tx, run, source)
		if err != nil {
			return "", "", "", err
		}
		if foreign {
			return "", HealDegraded, "binding_conflict", nil
		}
	}
	if source.Version >= math.MaxInt {
		return "", HealDegraded, "source_changed", nil
	}
	version := source.Version + 1
	promotedID, err := h.ids()
	if err != nil {
		return "", "", "", fmt.Errorf("compiler: generate promoted extractor ID: %w", err)
	}
	if !validExtractorID(promotedID) {
		return "", "", "", fmt.Errorf("%w: promoted extractor generator returned an invalid UUID", ErrInvalidHealer)
	}
	formattedNow := formatExtractorTime(now)
	writeResult, err := tx.ExecContext(ctx, `UPDATE extractors
		SET state = 'retired', updated_at = ? WHERE id = ? AND version = ? AND state = 'stale'`,
		formattedNow, source.ID, source.Version)
	if err != nil {
		return "", "", "", fmt.Errorf("compiler: retire healed extractor source: %w", err)
	}
	changed, err := writeResult.RowsAffected()
	if err != nil {
		return "", "", "", fmt.Errorf("compiler: inspect healed source retirement: %w", err)
	}
	if changed != 1 {
		return "", HealDegraded, "source_changed", nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO extractors (
		id, host, schema_json, schema_hash, template_cluster_id,
		template_simhash, ir, ir_hash, ir_format_version, version,
		validation_report, validation, state, empty_window,
		stale_reason, created_at, last_used_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'active', '[]', '', ?, NULL, ?)`,
		promotedID, run.result.Key.Host, run.schemaJSON, run.result.Key.SchemaHash,
		run.result.Key.TemplateClusterID, encodeSimHash(run.result.Key.ClusterSimHash),
		run.candidateIRJSON, run.candidateIRHash, run.candidateIRFormatVersion, version,
		run.validationReportJSON, run.validation, formattedNow, formattedNow); err != nil {
		return "", "", "", fmt.Errorf("compiler: insert promoted extractor: %w", err)
	}
	if sameCluster {
		if err := rebindClusterPages(ctx, tx, pageKey, source.TemplateClusterID, promotedID, now); err != nil {
			return "", "", "", err
		}
	} else if err := bindCandidateSamples(ctx, tx, validated.value, promotedID, now); err != nil {
		return "", "", "", err
	}
	return promotedID, HealPromoted, "replay_passed", nil
}

func loadLatestHealClusterRevision(
	ctx context.Context,
	tx ledger.ReadTx,
	host string,
	schemaHash string,
	clusterID string,
) (extractorMetadata, bool, error) {
	value, err := scanExtractorMetadata(tx.QueryRowContext(ctx, `SELECT `+extractorMetadataColumns+`
		FROM extractors WHERE host = ? AND schema_hash = ? AND template_cluster_id = ?
		ORDER BY version DESC LIMIT 1`, host, schemaHash, clusterID))
	if errors.Is(err, sql.ErrNoRows) {
		return extractorMetadata{}, false, nil
	}
	if err != nil {
		return extractorMetadata{}, false, fmt.Errorf("compiler: load latest heal source lineage: %w", err)
	}
	if value.Host != host || value.SchemaHash != schemaHash || value.TemplateClusterID != clusterID {
		return extractorMetadata{}, false, storeCorruption("latest heal source lineage returned mismatched metadata")
	}
	return value, true, nil
}

func driftSamplesHaveForeignBinding(
	ctx context.Context,
	tx ledger.ReadTx,
	run storedHealRun,
	source Extractor,
) (bool, error) {
	var samples []durableCandidateSample
	if err := strictJSONDecode([]byte(run.samplesJSON), &samples); err != nil {
		return false, storeCorruption("heal run samples cannot be decoded for binding validation")
	}
	arguments := make([]any, 0, len(samples)+1)
	arguments = append(arguments, run.result.Key.SchemaHash)
	placeholders := make([]string, len(samples))
	for index := range samples {
		placeholders[index] = "?"
		arguments = append(arguments, samples[index].PageHash)
	}
	rows, err := tx.QueryContext(ctx, `SELECT b.page_hash, e.host, e.schema_hash, e.template_cluster_id
		FROM extractor_page_bindings b JOIN extractors e ON e.id = b.extractor_id
		WHERE b.schema_hash = ? AND b.page_hash IN (`+strings.Join(placeholders, ",")+`)
		ORDER BY b.page_hash`, arguments...)
	if err != nil {
		return false, fmt.Errorf("compiler: query drift sample bindings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var pageHash, host, schemaHash, clusterID string
		if err := rows.Scan(&pageHash, &host, &schemaHash, &clusterID); err != nil {
			return false, fmt.Errorf("compiler: scan drift sample binding: %w", err)
		}
		if !validLowerHex(pageHash, 64) || host != source.Host || schemaHash != source.SchemaHash ||
			clusterID != source.TemplateClusterID {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("compiler: iterate drift sample bindings: %w", err)
	}
	return false, nil
}

func (h *Healer) writeTerminal(
	ctx context.Context,
	tx ledger.WriteTx,
	run storedHealRun,
	leaseID string,
	score replayScore,
	state HealState,
	reason string,
	promotedID string,
	now time.Time,
) (HealResult, error) {
	if state != HealPromoted && state != HealDegraded && state != HealFailed ||
		reason == "" || len(reason) > 256 || state == HealPromoted && reason != "replay_passed" {
		return HealResult{}, fmt.Errorf("%w: invalid terminal decision", ErrInvalidHealer)
	}
	var ratio any
	if value := score.ratio(); value != nil {
		ratio = *value
	}
	var promoted any
	if promotedID != "" {
		promoted = promotedID
	}
	formattedNow := formatExtractorTime(now)
	writeResult, err := tx.ExecContext(ctx, `UPDATE extractor_heal_runs SET
		state = ?, terminal_reason = ?, lease_id = NULL, lease_until = NULL,
		replay_total = ?, replay_matched = ?, replay_ratio = ?,
		promoted_extractor_id = ?, updated_at = ?, completed_at = ?
		WHERE id = ? AND state = 'replaying' AND lease_id = ? AND
			source_extractor_id = ? AND source_version = ?`,
		string(state), reason, score.total, score.matched, ratio, promoted,
		formattedNow, formattedNow, run.result.ID, leaseID,
		run.result.SourceExtractorID, run.result.SourceVersion)
	if err != nil {
		return HealResult{}, fmt.Errorf("compiler: terminalize extractor heal run: %w", err)
	}
	changed, err := writeResult.RowsAffected()
	if err != nil {
		return HealResult{}, fmt.Errorf("compiler: inspect extractor heal terminalization: %w", err)
	}
	if changed != 1 {
		return HealResult{}, ErrHealLeaseLost
	}
	result := cloneHealResult(run.result)
	result.State = state
	result.TerminalReason = reason
	result.ReplayTotal = score.total
	result.ReplayMatched = score.matched
	result.ReplayRatio = score.ratio()
	result.PromotedExtractorID = promotedID
	result.UpdatedAt = now
	result.CompletedAt = timePointer(now)
	if h.webhookURL != "" {
		if err := h.enqueueTerminalEvent(ctx, tx, result); err != nil {
			return HealResult{}, err
		}
	}
	return result, nil
}

type healWebhookPayload struct {
	RunID               string   `json:"run_id"`
	SourceExtractorID   string   `json:"source_extractor_id"`
	SourceVersion       int      `json:"source_version"`
	PromotedExtractorID string   `json:"promoted_extractor_id,omitempty"`
	ReplayRatio         *float64 `json:"replay_ratio,omitempty"`
	Reason              string   `json:"reason"`
}

func (h *Healer) enqueueTerminalEvent(ctx context.Context, tx ledger.WriteTx, result HealResult) error {
	eventID, err := h.ids()
	if err != nil {
		return fmt.Errorf("compiler: generate heal event ID: %w", err)
	}
	if !validExtractorID(eventID) {
		return fmt.Errorf("%w: heal event generator returned an invalid UUID", ErrInvalidHealer)
	}
	payload, err := json.Marshal(healWebhookPayload{
		RunID: result.ID, SourceExtractorID: result.SourceExtractorID,
		SourceVersion: result.SourceVersion, PromotedExtractorID: result.PromotedExtractorID,
		ReplayRatio: cloneFloatPointer(result.ReplayRatio), Reason: result.TerminalReason,
	})
	if err != nil || len(payload) > ledger.MaxOutboxPayloadBytes {
		return fmt.Errorf("compiler: encode extractor heal event")
	}
	eventType := ledger.ExtractorDegradedEvent
	if result.State == HealPromoted {
		eventType = ledger.ExtractorPromotedEvent
	}
	if err := ledger.EnqueueOutbox(ctx, tx, ledger.OutboxEvent{
		ID: eventID, SubjectType: ledger.SubjectExtractorHeal, SubjectID: result.ID,
		Type: eventType, URL: h.webhookURL, Secret: h.webhookSecret,
		Payload: payload, CreatedAt: result.UpdatedAt, NextAttemptAt: result.UpdatedAt,
	}); err != nil {
		return fmt.Errorf("compiler: enqueue extractor heal event: %w", err)
	}
	return nil
}

func (h *Healer) releaseClaim(run storedHealRun, leaseID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), h.releaseTimeout)
	defer cancel()
	return h.ledger.Update(ctx, func(tx ledger.WriteTx) error {
		current, err := scanStoredHealRun(tx.QueryRowContext(ctx,
			`SELECT `+healRunColumns+` FROM extractor_heal_runs WHERE id = ?`, run.result.ID))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("compiler: reload extractor heal lease for release: %w", err)
		}
		if current.result.State != HealReplaying || current.leaseID != leaseID {
			return nil
		}
		now, err := normalizeCatalogTime(maxExtractorTime(h.clock(), current.result.UpdatedAt))
		if err != nil {
			return fmt.Errorf("%w: release clock is invalid", ErrInvalidHealer)
		}
		writeResult, err := tx.ExecContext(ctx, `UPDATE extractor_heal_runs SET
			state = 'pending', terminal_reason = '', lease_id = NULL, lease_until = NULL,
			replay_total = 0, replay_matched = 0, replay_ratio = NULL,
			promoted_extractor_id = NULL, completed_at = NULL, updated_at = ?
			WHERE id = ? AND state = 'replaying' AND lease_id = ?`,
			formatExtractorTime(now), run.result.ID, leaseID)
		if err != nil {
			return fmt.Errorf("compiler: release extractor heal lease: %w", err)
		}
		changed, err := writeResult.RowsAffected()
		if err != nil {
			return fmt.Errorf("compiler: inspect extractor heal release: %w", err)
		}
		if changed > 1 {
			return storeCorruption("heal lease release changed %d rows", changed)
		}
		return nil
	})
}

func cloneHealResult(value HealResult) HealResult {
	value.ReplayRatio = cloneFloatPointer(value.ReplayRatio)
	if value.CompletedAt != nil {
		value.CompletedAt = timePointer(*value.CompletedAt)
	}
	return value
}

func cloneFloatPointer(value *float64) *float64 {
	if value == nil {
		return nil
	}
	return healFloatPointer(*value)
}

func healFloatPointer(value float64) *float64 {
	copy := value
	return &copy
}
