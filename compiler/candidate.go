package compiler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/simhash"
)

const MaxCandidateSamplesJSONBytes = 16 << 10

var (
	// ErrHealingRequired means an immutable cluster history already exists and
	// a candidate cannot be installed through the initial-registration path.
	ErrHealingRequired = errors.New("compiler: verified healing is required")
	// ErrCandidateAmbiguous means candidate samples attribute the proposed
	// revision to more than one durable extractor lineage.
	ErrCandidateAmbiguous = errors.New("compiler: candidate source is ambiguous")
	// ErrCandidateRetired means the only attributable lineage is terminal and
	// cannot be reactivated or silently replaced.
	ErrCandidateRetired = errors.New("compiler: candidate source is retired")
)

// Candidate is one validated compiler result tied to the exact fixed catalog
// cluster and immutable sample revision that produced it. SubmitCandidate
// clones every byte and reference before beginning a durable transaction.
type Candidate struct {
	Key             CompileKey
	CatalogRevision string
	Schema          json.RawMessage
	IR              IR
	Validation      ValidationReport
	Samples         []SampleRef
}

// SubmissionStatus makes every non-destructive admission decision explicit.
type SubmissionStatus string

const (
	SubmissionInitialActive     SubmissionStatus = "initial_active"
	SubmissionCurrent           SubmissionStatus = "current"
	SubmissionActiveUnchanged   SubmissionStatus = "active_unchanged"
	SubmissionPendingHeal       SubmissionStatus = "pending_heal"
	SubmissionRejectedRetired   SubmissionStatus = "rejected_retired"
	SubmissionRejectedAmbiguous SubmissionStatus = "rejected_ambiguous"
)

// Submission identifies either the active extractor retained by admission or
// the durable pending heal run. Rejected submissions return a stable status
// together with ErrHealingRequired.
type Submission struct {
	Status            SubmissionStatus
	Extractor         Extractor
	HealRunID         string
	SourceExtractorID string
	SourceVersion     int
}

// CandidateSubmitter separates synthesis from publication. A successful call
// means the candidate decision is durable before the coordinator records a
// successful compiler attempt.
type CandidateSubmitter interface {
	SubmitCandidate(context.Context, Candidate) (Submission, error)
}

type candidateSubmitterFunc func(context.Context, Candidate) (Submission, error)

func (submit candidateSubmitterFunc) SubmitCandidate(ctx context.Context, candidate Candidate) (Submission, error) {
	return submit(ctx, candidate)
}

type durableCandidateSample struct {
	PageHash      string `json:"page_hash"`
	SnapshotID    string `json:"snapshot_id"`
	SampleSimHash string `json:"sample_simhash"`
	FetchedAt     string `json:"fetched_at"`
}

type validatedCandidate struct {
	value       Candidate
	irJSON      json.RawMessage
	reportJSON  json.RawMessage
	irHash      string
	samplesJSON json.RawMessage
}

// SubmitCandidate admits a compiler result without ever replacing an active
// revision. A brand-new exact cluster is registered as v1; an attributable
// stale lineage creates an immutable pending heal run; active, retired, and
// ambiguous histories remain unchanged.
func (s *Store) SubmitCandidate(ctx context.Context, candidate Candidate) (Submission, error) {
	if err := s.validate(); err != nil {
		return Submission{}, err
	}
	validated, err := validateCandidate(candidate)
	if err != nil {
		return Submission{}, err
	}

	var submission Submission
	err = s.ledger.Update(ctx, func(tx ledger.WriteTx) error {
		catalogSet, ready, err := loadCatalogSet(ctx, tx, validated.value.Key, MaxCompileSamples)
		if err != nil {
			return err
		}
		if !ready || catalogSet.Revision != validated.value.CatalogRevision ||
			!sameCandidateSamples(catalogSet.Samples, validated.value.Samples) {
			return storeInputError("candidate no longer matches the durable catalog revision")
		}
		target, hasTarget, err := loadLatestCandidateTarget(ctx, tx, validated.value.Key)
		if err != nil {
			return err
		}
		history, err := loadCandidateClusterHistory(ctx, tx, validated.value.Key)
		if err != nil {
			return err
		}
		bound, err := loadCandidateBoundSources(ctx, tx, validated.value)
		if err != nil {
			return err
		}

		sources := make(map[string]extractorMetadata, len(bound)+1)
		for _, source := range history {
			if simhash.Distance(source.TemplateSimHash, validated.value.Key.ClusterSimHash) <= TemplateDistanceThreshold {
				sources[source.ID] = source
			}
		}
		for _, source := range bound {
			if existing, duplicate := sources[source.ID]; duplicate {
				if existing.Version != source.Version || existing.State != source.State {
					return storeCorruption("candidate source %q has inconsistent metadata", source.ID)
				}
				continue
			}
			sources[source.ID] = source
		}

		if hasTarget && target.State == StateActive {
			active, err := loadExtractorByID(ctx, tx, target.ID)
			if err != nil {
				return err
			}
			if exactCandidateRegistration(active, validated) {
				if len(sources) > 1 {
					submission = Submission{Status: SubmissionRejectedAmbiguous}
					return fmt.Errorf("%w: %w", ErrHealingRequired, ErrCandidateAmbiguous)
				}
				if err := bindCandidateSamples(ctx, tx, validated.value, active.ID,
					maxExtractorTime(s.now(), active.UpdatedAt)); err != nil {
					return err
				}
				submission = Submission{Status: SubmissionCurrent, Extractor: active}
				return nil
			}
			// A healthy active revision always wins. In particular, do not
			// rewrite its bindings or enqueue an implicit replacement.
			submission = Submission{Status: SubmissionActiveUnchanged, Extractor: active}
			return nil
		}

		if len(sources) > 1 {
			submission = Submission{Status: SubmissionRejectedAmbiguous}
			return fmt.Errorf("%w: %w", ErrHealingRequired, ErrCandidateAmbiguous)
		}
		for _, source := range sources {
			switch source.State {
			case StateActive:
				// This is a healthy page-bound lineage for a different exact
				// target. Preserve it and every binding byte-for-byte.
				submission = Submission{
					Status:            SubmissionActiveUnchanged,
					SourceExtractorID: source.ID,
					SourceVersion:     source.Version,
				}
				return nil
			case StateStale:
				value, err := s.submitPendingHeal(ctx, tx, validated, source)
				if err != nil {
					return err
				}
				submission = value
				return nil
			case StateRetired:
				submission = Submission{
					Status:            SubmissionRejectedRetired,
					SourceExtractorID: source.ID,
					SourceVersion:     source.Version,
				}
				return fmt.Errorf("%w: %w", ErrHealingRequired, ErrCandidateRetired)
			default:
				return storeCorruption("candidate source %q has invalid state %q", source.ID, source.State)
			}
		}

		active, err := s.insertInitialCandidate(ctx, tx, validated)
		if err != nil {
			return err
		}
		submission = Submission{Status: SubmissionInitialActive, Extractor: active}
		return nil
	})
	if err != nil {
		return submission, err
	}
	submission.Extractor = cloneExtractor(submission.Extractor)
	return submission, nil
}

func loadCandidateClusterHistory(
	ctx context.Context,
	tx ledger.ReadTx,
	key CompileKey,
) ([]extractorMetadata, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+prefixedColumns("e", extractorMetadataColumns)+` FROM extractors e
		WHERE e.host = ? AND e.schema_hash = ? AND e.version = (
			SELECT MAX(newer.version) FROM extractors newer
			WHERE newer.host = e.host AND newer.schema_hash = e.schema_hash
				AND newer.template_cluster_id = e.template_cluster_id
		)
		ORDER BY e.template_cluster_id LIMIT ?`, key.Host, key.SchemaHash, MaxTemplateClusters+1)
	if err != nil {
		return nil, fmt.Errorf("compiler: query candidate cluster history: %w", err)
	}
	defer rows.Close()
	values := make([]extractorMetadata, 0)
	for rows.Next() {
		value, err := scanExtractorMetadata(rows)
		if err != nil {
			return nil, fmt.Errorf("compiler: scan candidate cluster history: %w", err)
		}
		if value.Host != key.Host || value.SchemaHash != key.SchemaHash {
			return nil, storeCorruption("candidate cluster history returned mismatched row")
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("compiler: iterate candidate cluster history: %w", err)
	}
	if len(values) > MaxTemplateClusters {
		return nil, fmt.Errorf("%w: host %q schema %q", ErrClusterLimit, key.Host, key.SchemaHash)
	}
	return values, nil
}

func sameCandidateSamples(first, second []SampleRef) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index].PageHash != second[index].PageHash ||
			first[index].SnapshotID != second[index].SnapshotID ||
			first[index].SampleSimHash != second[index].SampleSimHash ||
			!first[index].FetchedAt.Equal(second[index].FetchedAt) {
			return false
		}
	}
	return true
}

func validateCandidate(candidate Candidate) (validatedCandidate, error) {
	if err := validateCompileKey(candidate.Key); err != nil {
		return validatedCandidate{}, storeInputError("candidate compile key: %v", err)
	}
	canonicalSchema, err := canonicalSchemaJSON(candidate.Schema)
	if err != nil || !bytes.Equal(canonicalSchema, candidate.Schema) ||
		sha256Hex(canonicalSchema) != candidate.Key.SchemaHash {
		return validatedCandidate{}, storeInputError("candidate schema does not match its compile key")
	}
	irJSON, reportJSON, irHash, err := validateRegistration(canonicalSchema, candidate.IR, candidate.Validation)
	if err != nil {
		return validatedCandidate{}, err
	}
	if len(candidate.Samples) < MinCompileSamples || len(candidate.Samples) > MaxCompileSamples ||
		candidate.Validation.Samples != len(candidate.Samples) {
		return validatedCandidate{}, storeInputError("candidate must contain the exact %d..%d validation samples", MinCompileSamples, MaxCompileSamples)
	}
	if !validLowerHex(candidate.CatalogRevision, 64) {
		return validatedCandidate{}, storeInputError("candidate catalog revision is invalid")
	}

	cloned := append([]SampleRef(nil), candidate.Samples...)
	stored := make([]durableCandidateSample, len(cloned))
	seenPages := make(map[string]struct{}, len(cloned))
	seenSnapshots := make(map[string]struct{}, len(cloned))
	for index, sample := range cloned {
		fetchedAt, timeErr := normalizeCatalogTime(sample.FetchedAt)
		if timeErr != nil || !validLowerHex(sample.PageHash, 64) ||
			!validSnapshotID(sample.SnapshotID) || sample.SampleSimHash == 0 ||
			simhash.Distance(sample.SampleSimHash, candidate.Key.ClusterSimHash) > TemplateDistanceThreshold {
			return validatedCandidate{}, storeInputError("candidate sample %d is invalid", index)
		}
		if _, duplicate := seenPages[sample.PageHash]; duplicate {
			return validatedCandidate{}, storeInputError("candidate contains duplicate page %q", sample.PageHash)
		}
		if _, duplicate := seenSnapshots[sample.SnapshotID]; duplicate {
			return validatedCandidate{}, storeInputError("candidate contains duplicate snapshot %q", sample.SnapshotID)
		}
		if index > 0 && (cloned[index-1].PageHash > sample.PageHash ||
			cloned[index-1].PageHash == sample.PageHash && cloned[index-1].SnapshotID >= sample.SnapshotID) {
			return validatedCandidate{}, storeInputError("candidate samples are not in canonical identity order")
		}
		seenPages[sample.PageHash] = struct{}{}
		seenSnapshots[sample.SnapshotID] = struct{}{}
		cloned[index].FetchedAt = fetchedAt
		stored[index] = durableCandidateSample{
			PageHash: sample.PageHash, SnapshotID: sample.SnapshotID,
			SampleSimHash: fmt.Sprintf("%016x", sample.SampleSimHash),
			FetchedAt:     formatExtractorTime(fetchedAt),
		}
	}
	set := SampleSet{Key: candidate.Key, Count: len(cloned), Samples: cloned}
	if sampleSetRevision(set) != candidate.CatalogRevision {
		return validatedCandidate{}, storeInputError("candidate catalog revision does not match its samples")
	}
	samplesJSON, err := json.Marshal(stored)
	if err != nil {
		return validatedCandidate{}, storeInputError("encode candidate samples: %v", err)
	}
	if len(samplesJSON) > MaxCandidateSamplesJSONBytes {
		return validatedCandidate{}, fmt.Errorf("%w: candidate samples exceed %d bytes", ErrResourceLimit, MaxCandidateSamplesJSONBytes)
	}
	value := candidate
	value.Schema = append(json.RawMessage(nil), canonicalSchema...)
	value.Samples = cloned
	return validatedCandidate{
		value: value, irJSON: irJSON, reportJSON: reportJSON, irHash: irHash,
		samplesJSON: samplesJSON,
	}, nil
}

func loadLatestCandidateTarget(
	ctx context.Context,
	tx ledger.ReadTx,
	key CompileKey,
) (extractorMetadata, bool, error) {
	value, err := scanExtractorMetadata(tx.QueryRowContext(ctx, `SELECT `+extractorMetadataColumns+`
		FROM extractors WHERE host = ? AND schema_hash = ? AND template_cluster_id = ?
		ORDER BY version DESC LIMIT 1`, key.Host, key.SchemaHash, key.TemplateClusterID))
	if errors.Is(err, sql.ErrNoRows) {
		return extractorMetadata{}, false, nil
	}
	if err != nil {
		return extractorMetadata{}, false, fmt.Errorf("compiler: load exact candidate target: %w", err)
	}
	if value.Host != key.Host || value.SchemaHash != key.SchemaHash ||
		value.TemplateClusterID != key.TemplateClusterID || value.TemplateSimHash != key.ClusterSimHash {
		return extractorMetadata{}, false, storeCorruption("exact candidate target has mismatched identity")
	}
	return value, true, nil
}

func loadCandidateBoundSources(
	ctx context.Context,
	tx ledger.ReadTx,
	candidate Candidate,
) ([]extractorMetadata, error) {
	arguments := make([]any, 0, len(candidate.Samples)+1)
	arguments = append(arguments, candidate.Key.SchemaHash)
	placeholders := make([]string, len(candidate.Samples))
	for index, sample := range candidate.Samples {
		placeholders[index] = "?"
		arguments = append(arguments, sample.PageHash)
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT `+prefixedColumns("e", extractorMetadataColumns)+`
		FROM extractor_page_bindings b
		JOIN extractors e ON e.id = b.extractor_id
		WHERE b.schema_hash = ? AND b.page_hash IN (`+strings.Join(placeholders, ",")+`)
		ORDER BY e.id LIMIT ?`, append(arguments, len(candidate.Samples)+1)...)
	if err != nil {
		return nil, fmt.Errorf("compiler: query candidate sample bindings: %w", err)
	}
	defer rows.Close()
	values := make([]extractorMetadata, 0, 1)
	for rows.Next() {
		value, err := scanExtractorMetadata(rows)
		if err != nil {
			return nil, fmt.Errorf("compiler: scan candidate sample binding: %w", err)
		}
		if value.Host != candidate.Key.Host || value.SchemaHash != candidate.Key.SchemaHash {
			return nil, storeCorruption("candidate sample binding points outside its host or schema")
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("compiler: iterate candidate sample bindings: %w", err)
	}
	if len(values) > len(candidate.Samples) {
		return nil, storeCorruption("candidate sample bindings exceed sample count")
	}
	return values, nil
}

func (s *Store) insertInitialCandidate(
	ctx context.Context,
	tx ledger.WriteTx,
	candidate validatedCandidate,
) (Extractor, error) {
	var clusterCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT template_cluster_id)
		FROM extractors WHERE host = ? AND schema_hash = ?`,
		candidate.value.Key.Host, candidate.value.Key.SchemaHash).Scan(&clusterCount); err != nil {
		return Extractor{}, fmt.Errorf("compiler: count extractor clusters: %w", err)
	}
	if clusterCount >= MaxTemplateClusters {
		return Extractor{}, fmt.Errorf("%w: host %q schema %q has %d clusters",
			ErrClusterLimit, candidate.value.Key.Host, candidate.value.Key.SchemaHash, clusterCount)
	}
	now, err := normalizeCatalogTime(s.now())
	if err != nil {
		return Extractor{}, storeInputError("store clock is invalid: %v", err)
	}
	id, err := newExtractorID()
	if err != nil {
		return Extractor{}, err
	}
	formattedNow := formatExtractorTime(now)
	if _, err := tx.ExecContext(ctx, `INSERT INTO extractors (
		id, host, schema_json, schema_hash, template_cluster_id,
		template_simhash, ir, ir_hash, ir_format_version, version,
		validation_report, validation, state, empty_window,
		stale_reason, created_at, last_used_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, 'active', '[]', '', ?, NULL, ?)`,
		id, candidate.value.Key.Host, string(candidate.value.Schema), candidate.value.Key.SchemaHash,
		candidate.value.Key.TemplateClusterID, encodeSimHash(candidate.value.Key.ClusterSimHash),
		string(candidate.irJSON), candidate.irHash, candidate.value.IR.Version,
		string(candidate.reportJSON), candidate.value.Validation.Overall,
		formattedNow, formattedNow); err != nil {
		return Extractor{}, fmt.Errorf("compiler: insert initial active extractor: %w", err)
	}
	if err := bindCandidateSamples(ctx, tx, candidate.value, id, now); err != nil {
		return Extractor{}, err
	}
	return Extractor{
		ID: id, Host: candidate.value.Key.Host,
		Schema:            append(json.RawMessage(nil), candidate.value.Schema...),
		SchemaHash:        candidate.value.Key.SchemaHash,
		TemplateClusterID: candidate.value.Key.TemplateClusterID,
		TemplateSimHash:   candidate.value.Key.ClusterSimHash,
		IR:                candidate.value.IR, IRHash: candidate.irHash, Version: 1,
		Validation: candidate.value.Validation, State: StateActive,
		EmptyWindow: []UseOutcome{}, CreatedAt: now, UpdatedAt: now,
	}, nil
}

func bindCandidateSamples(
	ctx context.Context,
	tx ledger.WriteTx,
	candidate Candidate,
	extractorID string,
	now time.Time,
) error {
	formatted := formatExtractorTime(now)
	for _, sample := range candidate.Samples {
		if _, err := tx.ExecContext(ctx, `INSERT INTO extractor_page_bindings (
			page_hash, schema_hash, extractor_id, bound_at, last_seen_at
		) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(page_hash, schema_hash) DO UPDATE SET
			extractor_id = excluded.extractor_id,
			bound_at = CASE
				WHEN extractor_page_bindings.extractor_id = excluded.extractor_id
				THEN extractor_page_bindings.bound_at ELSE excluded.bound_at END,
			last_seen_at = CASE
				WHEN extractor_page_bindings.last_seen_at > excluded.last_seen_at
				THEN extractor_page_bindings.last_seen_at ELSE excluded.last_seen_at END`,
			sample.PageHash, candidate.Key.SchemaHash, extractorID, formatted, formatted); err != nil {
			return fmt.Errorf("compiler: bind candidate sample: %w", err)
		}
	}
	return nil
}

func (s *Store) submitPendingHeal(
	ctx context.Context,
	tx ledger.WriteTx,
	candidate validatedCandidate,
	source extractorMetadata,
) (Submission, error) {
	if source.State != StateStale || source.Host != candidate.value.Key.Host ||
		source.SchemaHash != candidate.value.Key.SchemaHash {
		return Submission{}, storeCorruption("pending heal source does not match candidate")
	}
	var (
		existingID, existingSource, existingSchema, existingProfile, existingRevision string
		existingTarget, existingIR, existingIRHash, existingReport, existingSamples   string
		existingSourceVersion, existingIRVersion                                      int
		existingValidation                                                            float64
		existingTargetHash                                                            []byte
	)
	err := tx.QueryRowContext(ctx, `SELECT id, source_extractor_id, source_version,
		schema_json, content_profile, catalog_revision, target_template_cluster_id,
		target_cluster_simhash, candidate_ir, candidate_ir_hash,
		candidate_ir_format_version, validation_report, validation, samples_json
		FROM extractor_heal_runs
		WHERE host = ? AND schema_hash = ? AND target_template_cluster_id = ?
			AND state IN ('pending', 'replaying')`, candidate.value.Key.Host,
		candidate.value.Key.SchemaHash, candidate.value.Key.TemplateClusterID).Scan(
		&existingID, &existingSource, &existingSourceVersion, &existingSchema,
		&existingProfile, &existingRevision, &existingTarget, &existingTargetHash,
		&existingIR, &existingIRHash, &existingIRVersion, &existingReport,
		&existingValidation, &existingSamples)
	if err == nil {
		if existingSource == source.ID && existingSourceVersion == source.Version &&
			existingSchema == string(candidate.value.Schema) &&
			existingProfile == candidate.value.Key.ContentProfile &&
			existingRevision == candidate.value.CatalogRevision &&
			existingTarget == candidate.value.Key.TemplateClusterID &&
			bytes.Equal(existingTargetHash, encodeSimHash(candidate.value.Key.ClusterSimHash)) &&
			existingIR == string(candidate.irJSON) && existingIRHash == candidate.irHash &&
			existingIRVersion == candidate.value.IR.Version &&
			existingReport == string(candidate.reportJSON) &&
			existingValidation == candidate.value.Validation.Overall &&
			existingSamples == string(candidate.samplesJSON) {
			return Submission{
				Status: SubmissionPendingHeal, HealRunID: existingID,
				SourceExtractorID: source.ID, SourceVersion: source.Version,
			}, nil
		}
		return Submission{}, fmt.Errorf("%w: a different candidate is already pending for target %q",
			ErrHealingRequired, candidate.value.Key.TemplateClusterID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Submission{}, fmt.Errorf("compiler: load pending extractor heal: %w", err)
	}

	id, err := newExtractorID()
	if err != nil {
		return Submission{}, err
	}
	now, err := normalizeCatalogTime(maxExtractorTime(s.now(), source.UpdatedAt))
	if err != nil {
		return Submission{}, storeInputError("store clock is invalid: %v", err)
	}
	formattedNow := formatExtractorTime(now)
	if _, err := tx.ExecContext(ctx, `INSERT INTO extractor_heal_runs (
		id, source_extractor_id, source_version, host, schema_json, schema_hash,
		content_profile, target_template_cluster_id, target_cluster_simhash,
		catalog_revision, candidate_ir, candidate_ir_hash, candidate_ir_format_version,
		validation_report, validation, samples_json, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, source.ID, source.Version, candidate.value.Key.Host, string(candidate.value.Schema),
		candidate.value.Key.SchemaHash, candidate.value.Key.ContentProfile,
		candidate.value.Key.TemplateClusterID, encodeSimHash(candidate.value.Key.ClusterSimHash),
		candidate.value.CatalogRevision, string(candidate.irJSON), candidate.irHash,
		candidate.value.IR.Version, string(candidate.reportJSON), candidate.value.Validation.Overall,
		string(candidate.samplesJSON), formattedNow, formattedNow); err != nil {
		return Submission{}, fmt.Errorf("compiler: insert pending extractor heal: %w", err)
	}
	return Submission{
		Status: SubmissionPendingHeal, HealRunID: id,
		SourceExtractorID: source.ID, SourceVersion: source.Version,
	}, nil
}

func exactCandidateRegistration(value Extractor, candidate validatedCandidate) bool {
	storedIR, err := json.Marshal(value.IR)
	if err != nil {
		return false
	}
	storedReport, err := json.Marshal(value.Validation)
	if err != nil {
		return false
	}
	return value.State == StateActive && value.Host == candidate.value.Key.Host &&
		value.SchemaHash == candidate.value.Key.SchemaHash &&
		value.TemplateClusterID == candidate.value.Key.TemplateClusterID &&
		value.TemplateSimHash == candidate.value.Key.ClusterSimHash &&
		bytes.Equal(value.Schema, candidate.value.Schema) && value.IRHash == candidate.irHash &&
		bytes.Equal(storedIR, candidate.irJSON) && bytes.Equal(storedReport, candidate.reportJSON)
}
