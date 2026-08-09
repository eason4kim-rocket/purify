package compiler

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/simhash"
)

const (
	// DefaultCompilerProfile is the only content-cleaning profile whose
	// snapshots may enter the compiler catalog. Keeping the profile fixed avoids
	// learning one extractor from pages produced by incompatible cleaners.
	DefaultCompilerProfile = "extract-default-v1"

	SampleCatalogTTL               = 30 * 24 * time.Hour
	MaxSampleCatalogEntries        = 10_000
	MaxSampleCatalogClusters       = MaxTemplateClusters
	MaxSampleCatalogClusterEntries = MaxCompileSamples
)

var (
	ErrInvalidSampleCatalog = errors.New("compiler: invalid sample catalog input")
	ErrSampleCatalogCorrupt = errors.New("compiler: sample catalog is corrupt")
)

// SampleRef is a bounded reference to an immutable snapshot blob. Source URLs,
// page bytes, schemas, model output, credentials, and request metadata are
// deliberately absent from this type and from the durable catalog.
type SampleRef struct {
	PageHash      string
	SnapshotID    string
	SampleSimHash uint64
	FetchedAt     time.Time
}

// CompileKey identifies one fixed template cluster. Schema bytes are supplied
// by the current extract request and are intentionally not recoverable from the
// catalog after restart.
type CompileKey struct {
	Host              string
	SchemaHash        string
	ContentProfile    string
	TemplateClusterID string
	// ClusterSimHash is the immutable representative persisted by Store.Save.
	// Sample fingerprints validate membership but never recenter this value.
	ClusterSimHash uint64
}

// SampleSet is a deterministic, caller-owned selection. Revision hashes the
// ordered page/snapshot identities and their sample fingerprints, but not
// observation timestamps, so refreshing identical content does not schedule a
// redundant compilation.
type SampleSet struct {
	Key      CompileKey
	Revision string
	Count    int
	Samples  []SampleRef
}

// SampleCatalog stores only bounded references into snapshot CAS.
type SampleCatalog struct {
	ledger *ledger.Store
	clock  func() time.Time
}

// NewSampleCatalog builds a catalog over an already-open ledger. An optional
// clock is accepted for deterministic embedding and tests.
func NewSampleCatalog(durable *ledger.Store, clocks ...func() time.Time) (*SampleCatalog, error) {
	if durable == nil {
		return nil, catalogInputError("ledger is nil")
	}
	if len(clocks) > 1 || len(clocks) == 1 && clocks[0] == nil {
		return nil, catalogInputError("expected at most one non-nil clock")
	}
	clock := time.Now
	if len(clocks) == 1 {
		clock = clocks[0]
	}
	return &SampleCatalog{ledger: durable, clock: clock}, nil
}

// Observe records one successful default-profile snapshot. Expiration,
// per-page convergence, clustering, and every capacity bound are enforced in
// the same serialized write transaction. Repeated content may occupy multiple
// page rows, but Load counts each snapshot identity at most once. ready is true
// only for at least three distinct page hashes backed by three distinct
// snapshot IDs.
func (c *SampleCatalog) Observe(
	ctx context.Context,
	page PageKey,
	snapshotID string,
	fetchedAt time.Time,
) (SampleSet, bool, error) {
	if err := c.validate(); err != nil {
		return SampleSet{}, false, err
	}
	if err := validatePageKey(page); err != nil {
		return SampleSet{}, false, fmt.Errorf("%w: %v", ErrInvalidSampleCatalog, err)
	}
	if !validSnapshotID(snapshotID) {
		return SampleSet{}, false, catalogInputError("snapshot ID must be sha256: followed by 64 lowercase hex characters")
	}
	fetchedAt, err := normalizeCatalogTime(fetchedAt)
	if err != nil {
		return SampleSet{}, false, catalogInputError("fetched time: %v", err)
	}

	var result SampleSet
	ready := false
	err = c.ledger.Update(ctx, func(tx ledger.WriteTx) error {
		now, err := c.transactionTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := expireCatalog(ctx, tx, now); err != nil {
			return err
		}
		if err := pruneCatalogGlobal(ctx, tx); err != nil {
			return err
		}

		pageRow, hasPage, err := loadCatalogPage(ctx, tx, page.Host, page.SchemaHash, page.PageHash)
		if err != nil {
			return err
		}
		if err := validateCatalogSnapshotFingerprint(
			ctx, tx, page.Host, page.SchemaHash, snapshotID, page.TemplateSimHash,
		); err != nil {
			return err
		}

		winsPage := !hasPage || newerPageCandidate(fetchedAt, snapshotID, pageRow)
		if !winsPage {
			if _, err := tx.ExecContext(ctx, `UPDATE compiler_samples
				SET last_seen_at = ?
				WHERE host = ? AND schema_hash = ? AND content_profile = ? AND page_hash = ?`,
				formatExtractorTime(now), pageRow.Key.Host, pageRow.Key.SchemaHash,
				pageRow.Key.ContentProfile, pageRow.Ref.PageHash); err != nil {
				return fmt.Errorf("compiler: refresh retained page sample: %w", err)
			}
			result, ready, err = loadCatalogSet(ctx, tx, pageRow.Key, MaxSampleCatalogClusterEntries)
			return err
		}

		clusters, err := loadCatalogClusters(ctx, tx, page.Host, page.SchemaHash)
		if err != nil {
			return err
		}
		selected, matched := nearestCatalogCluster(clusters, page.TemplateSimHash)

		firstSeen := now
		if hasPage {
			firstSeen = pageRow.FirstSeen
		}
		if hasPage {
			if _, err := tx.ExecContext(ctx, `DELETE FROM compiler_samples
				WHERE host = ? AND schema_hash = ? AND content_profile = ? AND page_hash = ?`,
				page.Host, page.SchemaHash, DefaultCompilerProfile, pageRow.Ref.PageHash); err != nil {
				return fmt.Errorf("compiler: replace catalog sample: %w", err)
			}
		}

		if !matched {
			clusters, err = loadCatalogClusters(ctx, tx, page.Host, page.SchemaHash)
			if err != nil {
				return err
			}
			if len(clusters) >= MaxSampleCatalogClusters {
				victim := oldestCatalogCluster(clusters)
				if _, err := tx.ExecContext(ctx, `DELETE FROM compiler_samples
					WHERE host = ? AND schema_hash = ? AND content_profile = ?
						AND template_cluster_id = ?`, page.Host, page.SchemaHash,
					DefaultCompilerProfile, victim.Key.TemplateClusterID); err != nil {
					return fmt.Errorf("compiler: evict template cluster: %w", err)
				}
			}
			selected = catalogCluster{
				Key: CompileKey{
					Host:              page.Host,
					SchemaHash:        page.SchemaHash,
					ContentProfile:    DefaultCompilerProfile,
					TemplateClusterID: templateClusterID(page.TemplateSimHash),
					ClusterSimHash:    page.TemplateSimHash,
				},
			}
		}

		formattedNow := formatExtractorTime(now)
		if _, err := tx.ExecContext(ctx, `INSERT INTO compiler_samples (
			host, schema_hash, content_profile, template_cluster_id,
			cluster_simhash, sample_simhash, page_hash, snapshot_id, fetched_at,
			first_seen_at, last_seen_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			page.Host, page.SchemaHash, DefaultCompilerProfile,
			selected.Key.TemplateClusterID, encodeSimHash(selected.Key.ClusterSimHash),
			encodeSimHash(page.TemplateSimHash), page.PageHash, snapshotID, formatExtractorTime(fetchedAt),
			formatExtractorTime(firstSeen), formattedNow); err != nil {
			return fmt.Errorf("compiler: insert catalog sample: %w", err)
		}
		if err := pruneCatalogCluster(ctx, tx, selected.Key); err != nil {
			return err
		}
		if err := pruneCatalogGlobal(ctx, tx); err != nil {
			return err
		}
		result, ready, err = loadCatalogSet(ctx, tx, selected.Key, MaxSampleCatalogClusterEntries)
		return err
	})
	if err != nil {
		return SampleSet{}, false, err
	}
	return cloneSampleSet(result), ready, nil
}

// Load returns the newest limit samples selected deterministically, then
// orders those references by stable identity. limit must be 3..20. Expired
// references are physically removed before the read.
func (c *SampleCatalog) Load(ctx context.Context, key CompileKey, limit int) (SampleSet, bool, error) {
	if err := c.validate(); err != nil {
		return SampleSet{}, false, err
	}
	if err := validateCompileKey(key); err != nil {
		return SampleSet{}, false, err
	}
	if limit < MinCompileSamples || limit > MaxSampleCatalogClusterEntries {
		return SampleSet{}, false, catalogInputError("sample limit must be %d..%d", MinCompileSamples, MaxSampleCatalogClusterEntries)
	}

	var result SampleSet
	ready := false
	err := c.ledger.Update(ctx, func(tx ledger.WriteTx) error {
		now, err := c.transactionTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := expireCatalog(ctx, tx, now); err != nil {
			return err
		}
		if err := pruneCatalogGlobal(ctx, tx); err != nil {
			return err
		}
		result, ready, err = loadCatalogSet(ctx, tx, key, limit)
		return err
	})
	if err != nil {
		return SampleSet{}, false, err
	}
	return cloneSampleSet(result), ready, nil
}

func (c *SampleCatalog) validate() error {
	if c == nil || c.ledger == nil || c.clock == nil {
		return catalogInputError("catalog is nil or uninitialized")
	}
	return nil
}

func (c *SampleCatalog) transactionTime(ctx context.Context, tx ledger.ReadTx) (time.Time, error) {
	now, err := normalizeCatalogTime(c.clock())
	if err != nil {
		return time.Time{}, catalogInputError("clock: %v", err)
	}
	var maximum sql.NullString
	if err := tx.QueryRowContext(ctx, "SELECT MAX(last_seen_at) FROM compiler_samples").Scan(&maximum); err != nil {
		return time.Time{}, fmt.Errorf("compiler: read catalog clock floor: %w", err)
	}
	if !maximum.Valid {
		return now, nil
	}
	floor, err := parseExtractorTime(maximum.String)
	if err != nil {
		return time.Time{}, catalogCorruption("last_seen_at clock floor is invalid")
	}
	if !now.After(floor) {
		now = floor.Add(time.Nanosecond)
		if _, err := normalizeCatalogTime(now); err != nil {
			return time.Time{}, catalogCorruption("last_seen_at clock floor cannot advance")
		}
	}
	return now, nil
}

type catalogRow struct {
	Key       CompileKey
	Ref       SampleRef
	FirstSeen time.Time
	LastSeen  time.Time
}

type catalogCluster struct {
	Key      CompileKey
	LastSeen time.Time
	Count    int
}

const catalogColumns = `host, schema_hash, content_profile,
	template_cluster_id, cluster_simhash, sample_simhash, page_hash, snapshot_id,
	fetched_at, first_seen_at, last_seen_at`

func loadCatalogPage(
	ctx context.Context,
	tx ledger.ReadTx,
	host, schemaHash, pageHash string,
) (catalogRow, bool, error) {
	row, err := scanCatalogRow(tx.QueryRowContext(ctx, `SELECT `+catalogColumns+`
		FROM compiler_samples
		WHERE host = ? AND schema_hash = ? AND content_profile = ? AND page_hash = ?`,
		host, schemaHash, DefaultCompilerProfile, pageHash))
	if errors.Is(err, sql.ErrNoRows) {
		return catalogRow{}, false, nil
	}
	if err != nil {
		return catalogRow{}, false, fmt.Errorf("compiler: load catalog page: %w", err)
	}
	return row, true, nil
}

func validateCatalogSnapshotFingerprint(
	ctx context.Context,
	tx ledger.ReadTx,
	host, schemaHash, snapshotID string,
	fingerprint uint64,
) error {
	rows, err := tx.QueryContext(ctx, `SELECT sample_simhash FROM compiler_samples
		WHERE host = ? AND schema_hash = ? AND content_profile = ? AND snapshot_id = ?
		LIMIT ?`, host, schemaHash, DefaultCompilerProfile, snapshotID,
		MaxSampleCatalogClusters*MaxSampleCatalogClusterEntries+1)
	if err != nil {
		return fmt.Errorf("compiler: query catalog snapshot fingerprints: %w", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			return fmt.Errorf("compiler: scan catalog snapshot fingerprint: %w", err)
		}
		count++
		if len(blob) != 8 || binary.BigEndian.Uint64(blob) != fingerprint {
			return catalogInputError("snapshot ID was observed with a different template fingerprint")
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("compiler: iterate catalog snapshot fingerprints: %w", err)
	}
	if count > MaxSampleCatalogClusters*MaxSampleCatalogClusterEntries {
		return catalogCorruption("snapshot identity exceeds the bounded host/schema catalog")
	}
	return nil
}

func loadCatalogClusters(
	ctx context.Context,
	tx ledger.ReadTx,
	host, schemaHash string,
) ([]catalogCluster, error) {
	rows, err := tx.QueryContext(ctx, `SELECT template_cluster_id, cluster_simhash,
		MAX(last_seen_at), COUNT(*)
		FROM compiler_samples
		WHERE host = ? AND schema_hash = ? AND content_profile = ?
		GROUP BY template_cluster_id, cluster_simhash
		ORDER BY template_cluster_id, cluster_simhash
		LIMIT ?`, host, schemaHash, DefaultCompilerProfile, MaxSampleCatalogClusters+2)
	if err != nil {
		return nil, fmt.Errorf("compiler: query catalog clusters: %w", err)
	}
	defer rows.Close()

	clusters := make([]catalogCluster, 0)
	seenIDs := make(map[string]uint64)
	seenHashes := make(map[uint64]string)
	for rows.Next() {
		var clusterID, lastSeenRaw string
		var blob []byte
		var count int
		if err := rows.Scan(&clusterID, &blob, &lastSeenRaw, &count); err != nil {
			return nil, fmt.Errorf("compiler: scan catalog cluster: %w", err)
		}
		if len(blob) != 8 || count < 1 || count > MaxSampleCatalogClusterEntries ||
			!validLowerHex(clusterID, 64) {
			return nil, catalogCorruption("template cluster has invalid scalar metadata")
		}
		fingerprint := binary.BigEndian.Uint64(blob)
		if fingerprint == 0 || clusterID != templateClusterID(fingerprint) {
			return nil, catalogCorruption("template cluster ID does not match its representative")
		}
		if previous, duplicate := seenIDs[clusterID]; duplicate && previous != fingerprint {
			return nil, catalogCorruption("template cluster ID has multiple representatives")
		}
		if previous, duplicate := seenHashes[fingerprint]; duplicate && previous != clusterID {
			return nil, catalogCorruption("template representative has multiple cluster IDs")
		}
		lastSeen, err := parseExtractorTime(lastSeenRaw)
		if err != nil {
			return nil, catalogCorruption("template cluster last_seen_at is invalid")
		}
		seenIDs[clusterID] = fingerprint
		seenHashes[fingerprint] = clusterID
		clusters = append(clusters, catalogCluster{Key: CompileKey{
			Host: host, SchemaHash: schemaHash, ContentProfile: DefaultCompilerProfile,
			TemplateClusterID: clusterID, ClusterSimHash: fingerprint,
		}, LastSeen: lastSeen, Count: count})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("compiler: iterate catalog clusters: %w", err)
	}
	if len(clusters) > MaxSampleCatalogClusters {
		return nil, catalogCorruption("host and schema exceed %d template clusters", MaxSampleCatalogClusters)
	}
	return clusters, nil
}

func nearestCatalogCluster(clusters []catalogCluster, fingerprint uint64) (catalogCluster, bool) {
	var selected catalogCluster
	bestDistance := TemplateDistanceThreshold + 1
	found := false
	for _, candidate := range clusters {
		distance := simhash.Distance(candidate.Key.ClusterSimHash, fingerprint)
		if distance > TemplateDistanceThreshold {
			continue
		}
		if !found || distance < bestDistance ||
			distance == bestDistance && candidate.Key.TemplateClusterID < selected.Key.TemplateClusterID {
			selected = candidate
			bestDistance = distance
			found = true
		}
	}
	return selected, found
}

func oldestCatalogCluster(clusters []catalogCluster) catalogCluster {
	oldest := clusters[0]
	for _, candidate := range clusters[1:] {
		if candidate.LastSeen.Before(oldest.LastSeen) ||
			candidate.LastSeen.Equal(oldest.LastSeen) &&
				candidate.Key.TemplateClusterID < oldest.Key.TemplateClusterID {
			oldest = candidate
		}
	}
	return oldest
}

func pruneCatalogCluster(ctx context.Context, tx ledger.WriteTx, key CompileKey) error {
	rows, err := tx.QueryContext(ctx, `SELECT page_hash FROM compiler_samples
		WHERE host = ? AND schema_hash = ? AND content_profile = ?
			AND template_cluster_id = ?
		ORDER BY fetched_at DESC, last_seen_at DESC, page_hash, snapshot_id
		LIMIT -1 OFFSET ?`, key.Host, key.SchemaHash, key.ContentProfile,
		key.TemplateClusterID, MaxSampleCatalogClusterEntries)
	if err != nil {
		return fmt.Errorf("compiler: select cluster overflow: %w", err)
	}
	overflow := make([]string, 0, 1)
	for rows.Next() {
		var pageHash string
		if err := rows.Scan(&pageHash); err != nil {
			_ = rows.Close()
			return fmt.Errorf("compiler: scan cluster overflow: %w", err)
		}
		if !validLowerHex(pageHash, 64) {
			_ = rows.Close()
			return catalogCorruption("cluster overflow contains invalid page hash")
		}
		overflow = append(overflow, pageHash)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("compiler: close cluster overflow: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("compiler: iterate cluster overflow: %w", err)
	}
	for _, pageHash := range overflow {
		if _, err := tx.ExecContext(ctx, `DELETE FROM compiler_samples
			WHERE host = ? AND schema_hash = ? AND content_profile = ? AND page_hash = ?`,
			key.Host, key.SchemaHash, key.ContentProfile, pageHash); err != nil {
			return fmt.Errorf("compiler: evict cluster sample: %w", err)
		}
	}
	return nil
}

func pruneCatalogGlobal(ctx context.Context, tx ledger.WriteTx) error {
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM compiler_samples").Scan(&count); err != nil {
		return fmt.Errorf("compiler: count catalog entries: %w", err)
	}
	if count <= MaxSampleCatalogEntries {
		return nil
	}
	excess := count - MaxSampleCatalogEntries
	result, err := tx.ExecContext(ctx, `DELETE FROM compiler_samples
		WHERE (host, schema_hash, content_profile, page_hash) IN (
			SELECT host, schema_hash, content_profile, page_hash
			FROM compiler_samples
			ORDER BY last_seen_at, fetched_at, host, schema_hash,
				content_profile, template_cluster_id, page_hash, snapshot_id
			LIMIT ?
		)`, excess)
	if err != nil {
		return fmt.Errorf("compiler: evict global catalog overflow: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("compiler: inspect global catalog eviction: %w", err)
	}
	if changed != int64(excess) {
		return catalogCorruption("global eviction removed %d rows, want %d", changed, excess)
	}
	return nil
}

func expireCatalog(ctx context.Context, tx ledger.WriteTx, now time.Time) error {
	cutoff := formatExtractorTime(now.Add(-SampleCatalogTTL))
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM compiler_samples WHERE last_seen_at < ?", cutoff); err != nil {
		return fmt.Errorf("compiler: expire catalog samples: %w", err)
	}
	return nil
}

func loadCatalogSet(
	ctx context.Context,
	tx ledger.ReadTx,
	key CompileKey,
	limit int,
) (SampleSet, bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+catalogColumns+`
		FROM compiler_samples
		WHERE host = ? AND schema_hash = ? AND content_profile = ?
			AND template_cluster_id = ?
		ORDER BY fetched_at DESC, page_hash, snapshot_id
		LIMIT ?`, key.Host, key.SchemaHash, key.ContentProfile,
		key.TemplateClusterID, MaxSampleCatalogClusterEntries+1)
	if err != nil {
		return SampleSet{}, false, fmt.Errorf("compiler: query catalog sample set: %w", err)
	}
	defer rows.Close()

	stored := make([]catalogRow, 0, MaxSampleCatalogClusterEntries)
	for rows.Next() {
		row, err := scanCatalogRow(rows)
		if err != nil {
			return SampleSet{}, false, fmt.Errorf("compiler: scan catalog sample set: %w", err)
		}
		if row.Key != key {
			return SampleSet{}, false, catalogCorruption("sample row does not match compile key")
		}
		stored = append(stored, row)
	}
	if err := rows.Err(); err != nil {
		return SampleSet{}, false, fmt.Errorf("compiler: iterate catalog sample set: %w", err)
	}
	if len(stored) > MaxSampleCatalogClusterEntries {
		return SampleSet{}, false, catalogCorruption("cluster exceeds %d samples", MaxSampleCatalogClusterEntries)
	}
	samples := make([]SampleRef, 0, min(limit, len(stored)))
	seenPages := make(map[string]struct{}, len(stored))
	seenSnapshots := make(map[string]uint64, len(stored))
	for index := range stored {
		ref := stored[index].Ref
		if _, duplicate := seenPages[ref.PageHash]; duplicate {
			return SampleSet{}, false, catalogCorruption("sample set contains a duplicate page")
		}
		seenPages[ref.PageHash] = struct{}{}
		if previous, duplicate := seenSnapshots[ref.SnapshotID]; duplicate {
			if previous != ref.SampleSimHash {
				return SampleSet{}, false, catalogCorruption("snapshot identity has inconsistent sample fingerprints")
			}
			continue
		}
		seenSnapshots[ref.SnapshotID] = ref.SampleSimHash
		if len(samples) < limit {
			samples = append(samples, ref)
		}
	}
	sort.Slice(samples, func(i, j int) bool {
		if samples[i].PageHash != samples[j].PageHash {
			return samples[i].PageHash < samples[j].PageHash
		}
		return samples[i].SnapshotID < samples[j].SnapshotID
	})
	result := SampleSet{Key: key, Count: len(samples), Samples: samples}
	result.Revision = sampleSetRevision(result)
	ready := len(samples) >= MinCompileSamples
	return result, ready, nil
}

func scanCatalogRow(scanner rowScanner) (catalogRow, error) {
	var value catalogRow
	var clusterBlob, sampleBlob []byte
	var fetchedAt, firstSeen, lastSeen string
	if err := scanner.Scan(
		&value.Key.Host, &value.Key.SchemaHash, &value.Key.ContentProfile,
		&value.Key.TemplateClusterID, &clusterBlob, &sampleBlob, &value.Ref.PageHash,
		&value.Ref.SnapshotID, &fetchedAt, &firstSeen, &lastSeen,
	); err != nil {
		return catalogRow{}, err
	}
	if len(clusterBlob) != 8 || len(sampleBlob) != 8 {
		return catalogRow{}, catalogCorruption("sample fingerprints are not BLOB8")
	}
	value.Key.ClusterSimHash = binary.BigEndian.Uint64(clusterBlob)
	value.Ref.SampleSimHash = binary.BigEndian.Uint64(sampleBlob)
	if err := validateCompileKey(value.Key); err != nil {
		return catalogRow{}, catalogCorruption("sample compile key is invalid: %v", err)
	}
	if value.Ref.SampleSimHash == 0 ||
		simhash.Distance(value.Ref.SampleSimHash, value.Key.ClusterSimHash) > TemplateDistanceThreshold {
		return catalogRow{}, catalogCorruption("sample fingerprint is outside its fixed template cluster")
	}
	if !validLowerHex(value.Ref.PageHash, 64) || !validSnapshotID(value.Ref.SnapshotID) {
		return catalogRow{}, catalogCorruption("sample reference identity is invalid")
	}
	var err error
	value.Ref.FetchedAt, err = parseExtractorTime(fetchedAt)
	if err != nil {
		return catalogRow{}, catalogCorruption("sample fetched_at is invalid")
	}
	value.FirstSeen, err = parseExtractorTime(firstSeen)
	if err != nil {
		return catalogRow{}, catalogCorruption("sample first_seen_at is invalid")
	}
	value.LastSeen, err = parseExtractorTime(lastSeen)
	if err != nil || value.LastSeen.Before(value.FirstSeen) {
		return catalogRow{}, catalogCorruption("sample last_seen_at is invalid")
	}
	return value, nil
}

func validateCompileKey(key CompileKey) error {
	if key.ContentProfile != DefaultCompilerProfile ||
		!validLowerHex(key.SchemaHash, 64) ||
		!validLowerHex(key.TemplateClusterID, 64) ||
		key.ClusterSimHash == 0 ||
		key.TemplateClusterID != templateClusterID(key.ClusterSimHash) {
		return catalogInputError("compile key has invalid profile, hash, or template representative")
	}
	host, err := cacheHost(key.Host)
	if err != nil || host != key.Host || strings.TrimSpace(key.Host) != key.Host {
		return catalogInputError("compile key host is not canonical")
	}
	return nil
}

func newerPageCandidate(fetchedAt time.Time, snapshotID string, existing catalogRow) bool {
	if !fetchedAt.Equal(existing.Ref.FetchedAt) {
		return fetchedAt.After(existing.Ref.FetchedAt)
	}
	return snapshotID <= existing.Ref.SnapshotID
}

func sampleSetRevision(set SampleSet) string {
	var builder strings.Builder
	builder.Grow(256 + len(set.Samples)*140)
	builder.WriteString("purify-compiler-sample-set-v1\x00")
	for _, value := range []string{
		set.Key.Host,
		set.Key.SchemaHash,
		set.Key.ContentProfile,
		set.Key.TemplateClusterID,
	} {
		builder.WriteString(value)
		builder.WriteByte(0)
	}
	for _, sample := range set.Samples {
		builder.WriteString(sample.PageHash)
		builder.WriteByte(0)
		builder.WriteString(sample.SnapshotID)
		builder.WriteByte(0)
		builder.Write(encodeSimHash(sample.SampleSimHash))
		builder.WriteByte(0)
	}
	return sha256Hex([]byte(builder.String()))
}

func validSnapshotID(value string) bool {
	return len(value) == len("sha256:")+64 &&
		strings.HasPrefix(value, "sha256:") &&
		validLowerHex(value[len("sha256:"):], 64)
}

func normalizeCatalogTime(value time.Time) (time.Time, error) {
	if value.IsZero() || value.Year() < 1 || value.Year() > 9999 {
		return time.Time{}, errors.New("time must have a four-digit UTC year")
	}
	value = value.UTC()
	encoded := formatExtractorTime(value)
	parsed, err := parseExtractorTime(encoded)
	if err != nil || !parsed.Equal(value) || len(encoded) != 30 {
		return time.Time{}, errors.New("time cannot be represented as fixed UTC nanoseconds")
	}
	return value, nil
}

func catalogInputError(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidSampleCatalog, fmt.Sprintf(format, arguments...))
}

func catalogCorruption(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrSampleCatalogCorrupt, fmt.Sprintf(format, arguments...))
}

func cloneSampleSet(value SampleSet) SampleSet {
	value.Samples = append([]SampleRef(nil), value.Samples...)
	return value
}
