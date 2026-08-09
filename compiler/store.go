package compiler

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
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/llm"
	"github.com/use-agent/purify/publicnet"
	"github.com/use-agent/purify/simhash"
	"golang.org/x/net/publicsuffix"
)

const (
	TemplateDistanceThreshold = 6
	MaxTemplateClusters       = 256
	MaxExtractorWindow        = 20
	MaxExtractorReportBytes   = 512 << 10
	MaxExtractorURLBytes      = 16 << 10

	extractorTimeLayout = "2006-01-02T15:04:05.000000000Z"
)

var (
	ErrInvalidStoreInput = errors.New("compiler: invalid extractor store input")
	ErrExtractorNotFound = errors.New("compiler: extractor not found")
	ErrExtractorStore    = errors.New("compiler: extractor store is corrupt")
	ErrClusterLimit      = errors.New("compiler: extractor template cluster limit exceeded")
	ErrInvalidState      = errors.New("compiler: invalid extractor state transition")
)

// ExtractorState is monotonic durable lifecycle state.
type ExtractorState string

const (
	StateActive  ExtractorState = "active"
	StateStale   ExtractorState = "stale"
	StateRetired ExtractorState = "retired"
)

// UseOutcome feeds the bounded drift window. NotExecuted deliberately does
// not enter the window because it says nothing about extractor health.
type UseOutcome string

const (
	UseSucceeded     UseOutcome = "succeeded"
	UseRequiredEmpty UseOutcome = "required_empty"
	UseNotExecuted   UseOutcome = "not_executed"
)

// PageKey contains only canonical, bounded values suitable for persistence.
// TemplateSimHash is encoded as an unsigned big-endian BLOB8 in SQLite.
type PageKey struct {
	URL             string
	PageHash        string
	Host            string
	Schema          json.RawMessage
	SchemaHash      string
	TemplateSimHash uint64
}

// Extractor is one immutable compiled revision plus its monotonic health
// state. EmptyWindow is returned as a copy.
type Extractor struct {
	ID                string
	Host              string
	Schema            json.RawMessage
	SchemaHash        string
	TemplateClusterID string
	TemplateSimHash   uint64
	IR                IR
	IRHash            string
	Version           int
	Validation        ValidationReport
	State             ExtractorState
	EmptyWindow       []UseOutcome
	StaleReason       string
	CreatedAt         time.Time
	LastUsedAt        *time.Time
	UpdatedAt         time.Time
}

type storeConfig struct {
	clock func() time.Time
}

// StoreOption configures an extractor repository.
type StoreOption func(*storeConfig) error

// WithStoreClock supplies a deterministic UTC clock for tests and embedding.
func WithStoreClock(clock func() time.Time) StoreOption {
	return func(config *storeConfig) error {
		if clock == nil {
			return fmt.Errorf("%w: clock is nil", ErrInvalidStoreInput)
		}
		config.clock = clock
		return nil
	}
}

// Store persists compiled extractors through ledger's narrow transaction API.
type Store struct {
	ledger *ledger.Store
	clock  func() time.Time
}

// NewStore builds a repository over an already-open ledger.
func NewStore(durable *ledger.Store, options ...StoreOption) (*Store, error) {
	if durable == nil {
		return nil, fmt.Errorf("%w: ledger is nil", ErrInvalidStoreInput)
	}
	config := storeConfig{clock: time.Now}
	for index, option := range options {
		if option == nil {
			return nil, fmt.Errorf("%w: option %d is nil", ErrInvalidStoreInput, index)
		}
		if err := option(&config); err != nil {
			return nil, err
		}
	}
	return &Store{ledger: durable, clock: config.clock}, nil
}

// BuildPageKey canonicalizes the URL, reduces a DNS host to eTLD+1 (literal
// IPs remain literal), canonicalizes the schema JSON, and fingerprints the DOM.
func BuildPageKey(rawURL string, schemaJSON json.RawMessage, html string) (PageKey, error) {
	if len(rawURL) > MaxExtractorURLBytes {
		return PageKey{}, storeInputError("URL exceeds %d bytes", MaxExtractorURLBytes)
	}
	canonicalURL, parsed, err := publicnet.NormalizeHTTPURL(rawURL, nil, false)
	if err != nil {
		return PageKey{}, storeInputError("URL: %v", err)
	}
	host, err := cacheHost(parsed.Hostname())
	if err != nil {
		return PageKey{}, err
	}
	canonicalSchema, err := canonicalSchemaJSON(schemaJSON)
	if err != nil {
		return PageKey{}, storeInputError("normalized schema: %v", err)
	}
	if len(html) == 0 {
		return PageKey{}, storeInputError("HTML is empty")
	}
	if len(html) > MaxHTMLBytes {
		return PageKey{}, fmt.Errorf("%w: HTML is %d bytes, maximum is %d", ErrResourceLimit, len(html), MaxHTMLBytes)
	}
	templateHash := simhash.FingerprintDOM(html)
	if templateHash == 0 {
		return PageKey{}, storeInputError("HTML has no fingerprintable DOM structure")
	}
	return PageKey{
		URL:             canonicalURL,
		PageHash:        sha256Hex([]byte(canonicalURL)),
		Host:            host,
		Schema:          canonicalSchema,
		SchemaHash:      sha256Hex(canonicalSchema),
		TemplateSimHash: templateHash,
	}, nil
}

// TemplateMedoid chooses the order-independent sample with minimum total
// Hamming distance. Ties choose the numerically smallest fingerprint. Every
// sample must be within the public cluster threshold of the selected medoid.
func TemplateMedoid(fingerprints []uint64) (uint64, error) {
	if len(fingerprints) < MinCompileSamples || len(fingerprints) > MaxCompileSamples {
		return 0, storeInputError("template samples must contain %d..%d fingerprints", MinCompileSamples, MaxCompileSamples)
	}
	values := append([]uint64(nil), fingerprints...)
	for _, value := range values {
		if value == 0 {
			return 0, storeInputError("template fingerprint cannot be zero")
		}
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	best := values[0]
	bestTotal := math.MaxInt
	for _, candidate := range values {
		total := 0
		for _, value := range values {
			total += simhash.Distance(candidate, value)
		}
		if total < bestTotal {
			best = candidate
			bestTotal = total
		}
	}
	for _, value := range values {
		if simhash.Distance(best, value) > TemplateDistanceThreshold {
			return 0, storeInputError("template samples do not fit one distance-%d cluster", TemplateDistanceThreshold)
		}
	}
	return best, nil
}

// Lookup returns the nearest active extractor within distance six. Selection
// is phase one of a two-phase use protocol: a hit is never bound here because
// execution has not succeeded yet. The caller must follow a successful or
// required-empty execution with Touch or RecordEmpty; NotExecuted is a strict
// zero-write outcome. A generic cache miss never changes extractor state. A
// previously-bound page that no longer matches any active cluster atomically
// marks only its bound active extractor stale; all active candidates are
// re-read in that transaction immediately before the compare-and-swap.
func (s *Store) Lookup(ctx context.Context, key PageKey) (Extractor, bool, error) {
	if err := s.validate(); err != nil {
		return Extractor{}, false, err
	}
	if err := validatePageKey(key); err != nil {
		return Extractor{}, false, err
	}

	var result Extractor
	found := false
	needsDriftCheck := false
	err := s.ledger.View(ctx, func(tx ledger.ReadTx) error {
		bound, hasBinding, err := loadPageBindingMetadata(ctx, tx, key)
		if err != nil {
			return err
		}
		if hasBinding && bound.State == StateActive &&
			simhash.Distance(bound.TemplateSimHash, key.TemplateSimHash) <= TemplateDistanceThreshold {
			result, err = loadSelectedExtractor(ctx, tx, key, bound)
			found = err == nil
			return err
		}

		active, err := loadActiveMetadata(ctx, tx, key)
		if err != nil {
			return err
		}
		if candidate, ok := nearestMetadata(active, key.TemplateSimHash); ok {
			result, err = loadSelectedExtractor(ctx, tx, key, candidate)
			found = err == nil
			return err
		}
		needsDriftCheck = hasBinding && bound.State == StateActive
		return nil
	})
	if err != nil {
		return Extractor{}, false, err
	}
	if found || !needsDriftCheck {
		return result, found, nil
	}

	// Only the bound-drift path upgrades to a write transaction. Re-read both
	// the binding and every active cluster under the immediate transaction so a
	// concurrent promotion can win instead of being incorrectly degraded.
	err = s.ledger.Update(ctx, func(tx ledger.WriteTx) error {
		bound, hasBinding, err := loadPageBindingMetadata(ctx, tx, key)
		if err != nil {
			return err
		}
		if !hasBinding || bound.State != StateActive {
			return nil
		}
		if simhash.Distance(bound.TemplateSimHash, key.TemplateSimHash) <= TemplateDistanceThreshold {
			result, err = loadSelectedExtractor(ctx, tx, key, bound)
			found = err == nil
			return err
		}
		active, err := loadActiveMetadata(ctx, tx, key)
		if err != nil {
			return err
		}
		if candidate, ok := nearestMetadata(active, key.TemplateSimHash); ok {
			result, err = loadSelectedExtractor(ctx, tx, key, candidate)
			found = err == nil
			return err
		}
		now := maxExtractorTime(s.now(), bound.UpdatedAt)
		writeResult, err := tx.ExecContext(ctx, `UPDATE extractors
			SET state = 'stale', stale_reason = 'template_drift', updated_at = ?
			WHERE id = ? AND state = 'active'`, formatExtractorTime(now), bound.ID)
		if err != nil {
			return fmt.Errorf("compiler: mark bound extractor stale: %w", err)
		}
		changed, err := writeResult.RowsAffected()
		if err != nil {
			return fmt.Errorf("compiler: inspect template drift update: %w", err)
		}
		if changed > 1 {
			return storeCorruption("template drift updated %d rows", changed)
		}
		return nil
	})
	if err != nil {
		return Extractor{}, false, err
	}
	return result, found, nil
}

// RegisterInitial installs version one for one exact fixed template cluster.
// Exact retries return the current row. Once any revision or attributable page
// binding exists, this path never replaces, reactivates, or recenters it;
// callers must enter the verified healing workflow instead.
func (s *Store) RegisterInitial(ctx context.Context, key PageKey, ir IR, report ValidationReport) (Extractor, error) {
	if err := s.validate(); err != nil {
		return Extractor{}, err
	}
	if err := validatePageKey(key); err != nil {
		return Extractor{}, err
	}
	irJSON, reportJSON, irHash, err := validateRegistration(key.Schema, ir, report)
	if err != nil {
		return Extractor{}, err
	}

	var saved Extractor
	err = s.ledger.Update(ctx, func(tx ledger.WriteTx) error {
		compileKey := CompileKey{
			Host: key.Host, SchemaHash: key.SchemaHash, ContentProfile: DefaultCompilerProfile,
			TemplateClusterID: templateClusterID(key.TemplateSimHash),
			ClusterSimHash:    key.TemplateSimHash,
		}
		current, found, err := loadLatestCandidateTarget(ctx, tx, compileKey)
		if err != nil {
			return err
		}
		if found {
			if current.State == StateActive {
				active, err := loadExtractorByID(ctx, tx, current.ID)
				if err != nil {
					return err
				}
				if exactRegistration(active, key, key.TemplateSimHash, irJSON, reportJSON, irHash) {
					bound, hasBinding, err := loadPageBindingMetadata(ctx, tx, key)
					if err != nil {
						return err
					}
					if hasBinding && bound.ID != active.ID {
						return fmt.Errorf("%w: page is attributable to extractor %q in state %q",
							ErrHealingRequired, bound.ID, bound.State)
					}
					if err := bindPage(ctx, tx, key, active.ID, maxExtractorTime(s.now(), active.UpdatedAt)); err != nil {
						return err
					}
					saved = active
					return nil
				}
			}
			return fmt.Errorf("%w: exact cluster %q already has revision %d in state %q",
				ErrHealingRequired, current.TemplateClusterID, current.Version, current.State)
		}
		clusters, err := loadClusterMetadata(ctx, tx, key)
		if err != nil {
			return err
		}
		if nearest, matched := nearestMetadata(clusters, key.TemplateSimHash); matched {
			return fmt.Errorf("%w: page target is within distance %d of cluster %q",
				ErrHealingRequired, simhash.Distance(nearest.TemplateSimHash, key.TemplateSimHash),
				nearest.TemplateClusterID)
		}

		if bound, hasBinding, err := loadPageBindingMetadata(ctx, tx, key); err != nil {
			return err
		} else if hasBinding {
			return fmt.Errorf("%w: page is attributable to extractor %q in state %q",
				ErrHealingRequired, bound.ID, bound.State)
		}

		var clusterCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT template_cluster_id)
			FROM extractors WHERE host = ? AND schema_hash = ?`, key.Host, key.SchemaHash).Scan(&clusterCount); err != nil {
			return fmt.Errorf("compiler: count extractor clusters: %w", err)
		}
		if clusterCount >= MaxTemplateClusters {
			return fmt.Errorf("%w: host %q schema %q has %d clusters", ErrClusterLimit, key.Host, key.SchemaHash, clusterCount)
		}
		now, err := normalizeCatalogTime(s.now())
		if err != nil {
			return storeInputError("store clock is invalid: %v", err)
		}
		id, err := newExtractorID()
		if err != nil {
			return err
		}
		formattedNow := formatExtractorTime(now)
		if _, err := tx.ExecContext(ctx, `INSERT INTO extractors (
			id, host, schema_json, schema_hash, template_cluster_id,
			template_simhash, ir, ir_hash, ir_format_version, version,
			validation_report, validation, state, empty_window,
			stale_reason, created_at, last_used_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, 'active', '[]', '', ?, NULL, ?)`,
			id, key.Host, string(key.Schema), key.SchemaHash, compileKey.TemplateClusterID,
			encodeSimHash(key.TemplateSimHash), string(irJSON), irHash, ir.Version,
			string(reportJSON), report.Overall, formattedNow, formattedNow,
		); err != nil {
			return fmt.Errorf("compiler: insert initial active extractor: %w", err)
		}
		if err := bindPage(ctx, tx, key, id, now); err != nil {
			return err
		}
		saved = Extractor{
			ID: id, Host: key.Host, Schema: append(json.RawMessage(nil), key.Schema...),
			SchemaHash: key.SchemaHash, TemplateClusterID: compileKey.TemplateClusterID,
			TemplateSimHash: key.TemplateSimHash, IR: ir, IRHash: irHash, Version: 1,
			Validation: report, State: StateActive, EmptyWindow: []UseOutcome{},
			CreatedAt: now, UpdatedAt: now,
		}
		return nil
	})
	if err != nil {
		return Extractor{}, err
	}
	return cloneExtractor(saved), nil
}

// Save is retained for callers compiled against the Phase 2 API. It now has
// the same initial-only safety semantics as RegisterInitial.
func (s *Store) Save(ctx context.Context, key PageKey, ir IR, report ValidationReport) (Extractor, error) {
	return s.RegisterInitial(ctx, key, ir, report)
}

// RegisterActive is the legacy explicit spelling. Active replacement is no
// longer admitted through this API; verified healing owns every promotion.
func (s *Store) RegisterActive(ctx context.Context, key PageKey, ir IR, report ValidationReport) (Extractor, error) {
	return s.RegisterInitial(ctx, key, ir, report)
}

// Get returns one extractor revision by UUID.
func (s *Store) Get(ctx context.Context, id string) (Extractor, error) {
	if err := s.validate(); err != nil {
		return Extractor{}, err
	}
	if !validExtractorID(id) {
		return Extractor{}, storeInputError("extractor ID must be a lowercase UUID")
	}
	var result Extractor
	err := s.ledger.View(ctx, func(tx ledger.ReadTx) error {
		value, err := loadExtractorByID(ctx, tx, id)
		if err != nil {
			return err
		}
		result = value
		return nil
	})
	if err != nil {
		return Extractor{}, err
	}
	return cloneExtractor(result), nil
}

// Touch binds page to a successful active execution and updates last_used_at.
func (s *Store) Touch(ctx context.Context, key PageKey, id string) (Extractor, error) {
	return s.RecordUse(ctx, key, id, UseSucceeded)
}

// RecordEmpty binds page and records an execution whose required output was
// empty. Binding is transactional with the health update.
func (s *Store) RecordEmpty(ctx context.Context, key PageKey, id string) (Extractor, error) {
	return s.RecordUse(ctx, key, id, UseRequiredEmpty)
}

// RecordUse is phase two of Lookup. Succeeded and RequiredEmpty atomically bind
// the page and update the bounded 20-result health window. Drift is evaluated
// only once the window is full: six empty results remain active; seven mark the
// extractor stale. NotExecuted performs no write and changes neither binding,
// window, nor last_used_at.
func (s *Store) RecordUse(ctx context.Context, key PageKey, id string, outcome UseOutcome) (Extractor, error) {
	if err := s.validate(); err != nil {
		return Extractor{}, err
	}
	if err := validatePageKey(key); err != nil {
		return Extractor{}, err
	}
	if !validExtractorID(id) {
		return Extractor{}, storeInputError("extractor ID must be a lowercase UUID")
	}
	if outcome != UseSucceeded && outcome != UseRequiredEmpty && outcome != UseNotExecuted {
		return Extractor{}, storeInputError("unsupported use outcome %q", outcome)
	}
	if outcome == UseNotExecuted {
		return s.Get(ctx, id)
	}

	var result Extractor
	err := s.ledger.Update(ctx, func(tx ledger.WriteTx) error {
		current, err := loadExtractorByID(ctx, tx, id)
		if err != nil {
			return err
		}
		if current.State != StateActive {
			result = current
			return nil
		}
		if current.Host != key.Host || current.SchemaHash != key.SchemaHash ||
			!bytes.Equal(current.Schema, key.Schema) ||
			simhash.Distance(current.TemplateSimHash, key.TemplateSimHash) > TemplateDistanceThreshold {
			return storeInputError("page key does not match extractor %q", id)
		}
		window := append(append([]UseOutcome(nil), current.EmptyWindow...), outcome)
		if len(window) > MaxExtractorWindow {
			window = append([]UseOutcome(nil), window[len(window)-MaxExtractorWindow:]...)
		}
		windowJSON, err := json.Marshal(window)
		if err != nil {
			return fmt.Errorf("compiler: encode extractor health window: %w", err)
		}
		state := StateActive
		reason := ""
		if len(window) == MaxExtractorWindow {
			empty := 0
			for _, value := range window {
				if value == UseRequiredEmpty {
					empty++
				}
			}
			if empty*10 > MaxExtractorWindow*3 {
				state = StateStale
				reason = "required_empty_rate"
			}
		}
		now := maxExtractorTime(s.now(), current.UpdatedAt)
		if err := bindPage(ctx, tx, key, id, now); err != nil {
			return err
		}
		writeResult, err := tx.ExecContext(ctx, `UPDATE extractors
			SET empty_window = ?, last_used_at = ?, updated_at = ?, state = ?, stale_reason = ?
			WHERE id = ? AND state = 'active'`, string(windowJSON), formatExtractorTime(now),
			formatExtractorTime(now), string(state), reason, id)
		if err != nil {
			return fmt.Errorf("compiler: update extractor health: %w", err)
		}
		changed, err := writeResult.RowsAffected()
		if err != nil {
			return fmt.Errorf("compiler: inspect extractor health update: %w", err)
		}
		if changed != 1 {
			return storeCorruption("extractor health update changed %d rows", changed)
		}
		current.EmptyWindow = window
		current.State = state
		current.StaleReason = reason
		current.LastUsedAt = timePointer(now)
		current.UpdatedAt = now
		result = current
		return nil
	})
	if err != nil {
		return Extractor{}, err
	}
	return cloneExtractor(result), nil
}

// Retire performs the only explicit terminal transition. A retired extractor
// is never reactivated, and initial registration cannot append to its lineage.
func (s *Store) Retire(ctx context.Context, id string) (Extractor, error) {
	if err := s.validate(); err != nil {
		return Extractor{}, err
	}
	if !validExtractorID(id) {
		return Extractor{}, storeInputError("extractor ID must be a lowercase UUID")
	}
	var result Extractor
	err := s.ledger.Update(ctx, func(tx ledger.WriteTx) error {
		current, err := loadExtractorByID(ctx, tx, id)
		if err != nil {
			return err
		}
		if current.State == StateRetired {
			result = current
			return nil
		}
		now := maxExtractorTime(s.now(), current.UpdatedAt)
		writeResult, err := tx.ExecContext(ctx, `UPDATE extractors
			SET state = 'retired', updated_at = ?
			WHERE id = ? AND state IN ('active', 'stale')`, formatExtractorTime(now), id)
		if err != nil {
			return fmt.Errorf("compiler: retire extractor: %w", err)
		}
		changed, err := writeResult.RowsAffected()
		if err != nil {
			return fmt.Errorf("compiler: inspect extractor retirement: %w", err)
		}
		if changed != 1 {
			return fmt.Errorf("%w: extractor %q cannot transition from %q", ErrInvalidState, id, current.State)
		}
		current.State = StateRetired
		current.UpdatedAt = now
		result = current
		return nil
	})
	if err != nil {
		return Extractor{}, err
	}
	return cloneExtractor(result), nil
}

func (s *Store) validate() error {
	if s == nil || s.ledger == nil || s.clock == nil {
		return fmt.Errorf("%w: store is nil or uninitialized", ErrInvalidStoreInput)
	}
	return nil
}

func (s *Store) now() time.Time {
	return s.clock().UTC()
}

func validatePageKey(key PageKey) error {
	if len(key.URL) == 0 || len(key.URL) > MaxExtractorURLBytes || !utf8.ValidString(key.URL) {
		return storeInputError("page key URL is invalid")
	}
	canonicalURL, parsed, err := publicnet.NormalizeHTTPURL(key.URL, nil, false)
	if err != nil || canonicalURL != key.URL {
		return storeInputError("page key URL is not canonical")
	}
	host, err := cacheHost(parsed.Hostname())
	if err != nil || host != key.Host {
		return storeInputError("page key host does not match URL")
	}
	canonicalSchema, err := canonicalSchemaJSON(key.Schema)
	if err != nil || !bytes.Equal(canonicalSchema, key.Schema) {
		return storeInputError("page key schema is not canonical")
	}
	if key.SchemaHash != sha256Hex(key.Schema) || key.PageHash != sha256Hex([]byte(key.URL)) {
		return storeInputError("page key hash does not match canonical input")
	}
	if key.TemplateSimHash == 0 {
		return storeInputError("page key template fingerprint is zero")
	}
	return nil
}

func validateRegistration(schema json.RawMessage, ir IR, report ValidationReport) (json.RawMessage, json.RawMessage, string, error) {
	if len(ir.Fields) == 0 {
		return nil, nil, "", storeInputError("extractor IR has no fields")
	}
	if err := validateExecutionResources(ir, ""); err != nil {
		return nil, nil, "", err
	}
	if _, err := compileRules(ir.Fields); err != nil {
		return nil, nil, "", fmt.Errorf("%w: extractor IR: %v", ErrInvalidStoreInput, err)
	}
	_, schemaFields, err := parseCompileSchema(schema)
	if err != nil {
		return nil, nil, "", fmt.Errorf("%w: extractor schema: %v", ErrInvalidStoreInput, err)
	}
	if len(schemaFields) != len(ir.Fields) {
		return nil, nil, "", storeInputError("extractor IR field count does not match schema")
	}
	fieldsByName := make(map[string]compileField, len(schemaFields))
	for _, field := range schemaFields {
		fieldsByName[field.name] = field
	}
	for _, rule := range ir.Fields {
		field, ok := fieldsByName[rule.Name]
		if !ok || field.valueType != rule.Type || field.required != rule.Required {
			return nil, nil, "", storeInputError("extractor IR field %q does not match schema", rule.Name)
		}
	}
	if !report.CanEnable || report.Overall < ValidationThreshold || report.Overall > 1 ||
		math.IsNaN(report.Overall) || math.IsInf(report.Overall, 0) {
		return nil, nil, "", storeInputError("validation report cannot enable extractor")
	}
	if report.Threshold != ValidationThreshold || report.Samples < MinCompileSamples ||
		report.Samples > MaxCompileSamples || report.ValidSamples != report.Samples {
		return nil, nil, "", storeInputError("validation report sample gate is invalid")
	}
	if len(report.PerField) != len(ir.Fields) {
		return nil, nil, "", storeInputError("validation report field count does not match IR")
	}
	totalMatches := 0
	seen := make(map[string]struct{}, len(report.PerField))
	for _, field := range report.PerField {
		if _, duplicate := seen[field.Name]; duplicate {
			return nil, nil, "", storeInputError("validation report contains duplicate field %q", field.Name)
		}
		seen[field.Name] = struct{}{}
		if field.Samples != report.Samples || field.Matches < 0 || field.Matches > field.Samples ||
			math.IsNaN(field.Score) || math.IsInf(field.Score, 0) ||
			math.Abs(field.Score-float64(field.Matches)/float64(field.Samples)) > 1e-12 {
			return nil, nil, "", storeInputError("validation for field %q is inconsistent", field.Name)
		}
		totalMatches += field.Matches
	}
	for _, rule := range ir.Fields {
		if _, ok := seen[rule.Name]; !ok {
			return nil, nil, "", storeInputError("validation report is missing field %q", rule.Name)
		}
	}
	wantOverall := float64(totalMatches) / float64(len(ir.Fields)*report.Samples)
	if math.Abs(report.Overall-wantOverall) > 1e-12 {
		return nil, nil, "", storeInputError("validation overall score is inconsistent")
	}
	irJSON, err := json.Marshal(ir)
	if err != nil {
		return nil, nil, "", storeInputError("encode extractor IR: %v", err)
	}
	if len(irJSON) > MaxRuleDefinitionBytes {
		return nil, nil, "", fmt.Errorf("%w: encoded IR exceeds %d bytes", ErrResourceLimit, MaxRuleDefinitionBytes)
	}
	reportJSON, err := json.Marshal(report)
	if err != nil {
		return nil, nil, "", storeInputError("encode validation report: %v", err)
	}
	if len(reportJSON) > MaxExtractorReportBytes {
		return nil, nil, "", fmt.Errorf("%w: validation report exceeds %d bytes", ErrResourceLimit, MaxExtractorReportBytes)
	}
	return irJSON, reportJSON, sha256Hex(irJSON), nil
}

func cacheHost(hostname string) (string, error) {
	hostname = strings.TrimSuffix(strings.ToLower(hostname), ".")
	if address, err := netip.ParseAddr(hostname); err == nil {
		return address.Unmap().String(), nil
	}
	registrable, err := publicsuffix.EffectiveTLDPlusOne(hostname)
	if err != nil {
		return "", storeInputError("URL host has no registrable domain: %v", err)
	}
	return strings.ToLower(registrable), nil
}

func canonicalJSONObject(raw json.RawMessage, maximum int) (json.RawMessage, error) {
	if len(raw) == 0 || len(raw) > maximum || !utf8.Valid(raw) {
		return nil, errors.New("JSON is empty, oversized, or invalid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeCanonicalValue(decoder, 0)
	if err != nil {
		return nil, err
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, errors.New("JSON root must be an object")
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("unexpected trailing token %v", token)
		}
		return nil, fmt.Errorf("read trailing JSON: %w", err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("canonicalize JSON: %w", err)
	}
	return json.RawMessage(encoded), nil
}

func canonicalSchemaJSON(raw json.RawMessage) (json.RawMessage, error) {
	// Parse before legacy normalization so duplicate object members cannot be
	// silently collapsed by a map decode. NormalizeSchema then makes legacy
	// shorthand and its equivalent JSON Schema share one cache key.
	precanonical, err := canonicalJSONObject(raw, MaxCompileSchemaBytes)
	if err != nil {
		return nil, err
	}
	normalized, err := llm.NormalizeSchema(precanonical)
	if err != nil {
		return nil, err
	}
	return canonicalJSONObject(normalized, MaxCompileSchemaBytes)
}

func decodeCanonicalValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 128 {
		return nil, errors.New("JSON nesting exceeds 128 levels")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("decode JSON: %w", err)
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return nil, fmt.Errorf("decode object name: %w", err)
			}
			name, ok := nameToken.(string)
			if !ok {
				return nil, errors.New("object name is not a string")
			}
			if _, duplicate := object[name]; duplicate {
				return nil, fmt.Errorf("duplicate object member %q", name)
			}
			value, err := decodeCanonicalValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[name] = value
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return nil, errors.New("object is not closed")
		}
		return object, nil
	case '[':
		array := make([]any, 0)
		for decoder.More() {
			value, err := decodeCanonicalValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return nil, errors.New("array is not closed")
		}
		return array, nil
	default:
		return nil, fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
}

type rowScanner interface {
	Scan(...any) error
}

type extractorMetadata struct {
	ID                string
	Host              string
	SchemaHash        string
	TemplateClusterID string
	TemplateSimHash   uint64
	Version           int
	Validation        float64
	State             ExtractorState
	StaleReason       string
	UpdatedAt         time.Time
}

// observeFullExtractorDecode is a no-op production hook. Tests replace it
// with an atomic counter to prove candidate scans remain metadata-only even at
// the 256-cluster bound.
var observeFullExtractorDecode = func() {}

const extractorColumns = `id, host, schema_json, schema_hash,
	template_cluster_id, template_simhash, ir, ir_hash, ir_format_version,
	version, validation_report, validation, state, empty_window,
	stale_reason, created_at, last_used_at, updated_at`

func scanExtractor(scanner rowScanner) (Extractor, error) {
	observeFullExtractorDecode()
	var (
		value                                      Extractor
		schemaJSON, irJSON, reportJSON, windowJSON string
		templateBlob                               []byte
		irFormatVersion                            int
		validation                                 float64
		state                                      string
		createdAt, updatedAt                       string
		lastUsedAt                                 sql.NullString
	)
	if err := scanner.Scan(
		&value.ID, &value.Host, &schemaJSON, &value.SchemaHash,
		&value.TemplateClusterID, &templateBlob, &irJSON, &value.IRHash,
		&irFormatVersion, &value.Version, &reportJSON, &validation, &state,
		&windowJSON, &value.StaleReason, &createdAt, &lastUsedAt, &updatedAt,
	); err != nil {
		return Extractor{}, err
	}
	if !validExtractorID(value.ID) || value.Host == "" || !validLowerHex(value.SchemaHash, 64) ||
		!validLowerHex(value.TemplateClusterID, 64) || !validLowerHex(value.IRHash, 64) ||
		len(templateBlob) != 8 || value.Version < 1 ||
		math.IsNaN(validation) || math.IsInf(validation, 0) {
		return Extractor{}, storeCorruption("extractor %q has invalid scalar metadata", value.ID)
	}
	value.TemplateSimHash = binary.BigEndian.Uint64(templateBlob)
	if value.TemplateSimHash == 0 {
		return Extractor{}, storeCorruption("extractor %q has zero template fingerprint", value.ID)
	}
	if value.TemplateClusterID != templateClusterID(value.TemplateSimHash) {
		return Extractor{}, storeCorruption("extractor %q cluster ID does not match its template fingerprint", value.ID)
	}
	canonicalSchema, err := canonicalSchemaJSON(json.RawMessage(schemaJSON))
	if err != nil || string(canonicalSchema) != schemaJSON || sha256Hex(canonicalSchema) != value.SchemaHash {
		return Extractor{}, storeCorruption("extractor %q has invalid canonical schema", value.ID)
	}
	value.Schema = canonicalSchema
	if err := json.Unmarshal([]byte(irJSON), &value.IR); err != nil ||
		value.IR.Version != irFormatVersion || sha256Hex([]byte(irJSON)) != value.IRHash {
		return Extractor{}, storeCorruption("extractor %q has invalid IR", value.ID)
	}
	canonicalIR, err := json.Marshal(value.IR)
	if err != nil || string(canonicalIR) != irJSON {
		return Extractor{}, storeCorruption("extractor %q IR is not canonical", value.ID)
	}
	if err := json.Unmarshal([]byte(reportJSON), &value.Validation); err != nil ||
		value.Validation.Overall != validation {
		return Extractor{}, storeCorruption("extractor %q has invalid validation report", value.ID)
	}
	canonicalReport, err := json.Marshal(value.Validation)
	if err != nil || string(canonicalReport) != reportJSON {
		return Extractor{}, storeCorruption("extractor %q validation report is not canonical", value.ID)
	}
	if _, _, _, err := validateRegistration(value.Schema, value.IR, value.Validation); err != nil {
		return Extractor{}, storeCorruption("extractor %q registration is invalid: %v", value.ID, err)
	}
	if err := json.Unmarshal([]byte(windowJSON), &value.EmptyWindow); err != nil || len(value.EmptyWindow) > MaxExtractorWindow {
		return Extractor{}, storeCorruption("extractor %q has invalid health window", value.ID)
	}
	for _, outcome := range value.EmptyWindow {
		if outcome != UseSucceeded && outcome != UseRequiredEmpty {
			return Extractor{}, storeCorruption("extractor %q has invalid health outcome", value.ID)
		}
	}
	value.State = ExtractorState(state)
	if !validStoredState(value.State, value.StaleReason) {
		return Extractor{}, storeCorruption("extractor %q has invalid state %q", value.ID, state)
	}
	value.CreatedAt, err = parseExtractorTime(createdAt)
	if err != nil {
		return Extractor{}, storeCorruption("extractor %q created_at: %v", value.ID, err)
	}
	value.UpdatedAt, err = parseExtractorTime(updatedAt)
	if err != nil || value.UpdatedAt.Before(value.CreatedAt) {
		return Extractor{}, storeCorruption("extractor %q updated_at is invalid", value.ID)
	}
	if lastUsedAt.Valid {
		parsed, err := parseExtractorTime(lastUsedAt.String)
		if err != nil || parsed.Before(value.CreatedAt) || parsed.After(value.UpdatedAt) {
			return Extractor{}, storeCorruption("extractor %q last_used_at is invalid", value.ID)
		}
		value.LastUsedAt = &parsed
	}
	return value, nil
}

func scanExtractorMetadata(scanner rowScanner) (extractorMetadata, error) {
	var (
		value        extractorMetadata
		templateBlob []byte
		state        string
		updatedAt    string
	)
	if err := scanner.Scan(
		&value.ID, &value.Host, &value.SchemaHash, &value.TemplateClusterID,
		&templateBlob, &value.Version, &value.Validation, &state, &value.StaleReason, &updatedAt,
	); err != nil {
		return extractorMetadata{}, err
	}
	if !validExtractorID(value.ID) || value.Host == "" ||
		!validLowerHex(value.SchemaHash, 64) || !validLowerHex(value.TemplateClusterID, 64) ||
		len(templateBlob) != 8 || value.Version < 1 ||
		math.IsNaN(value.Validation) || math.IsInf(value.Validation, 0) ||
		value.Validation < 0 || value.Validation > 1 {
		return extractorMetadata{}, storeCorruption("extractor %q has invalid candidate metadata", value.ID)
	}
	value.TemplateSimHash = binary.BigEndian.Uint64(templateBlob)
	if value.TemplateSimHash == 0 {
		return extractorMetadata{}, storeCorruption("extractor %q has zero template fingerprint", value.ID)
	}
	if value.TemplateClusterID != templateClusterID(value.TemplateSimHash) {
		return extractorMetadata{}, storeCorruption("extractor %q candidate cluster ID does not match its template fingerprint", value.ID)
	}
	value.State = ExtractorState(state)
	if !validStoredState(value.State, value.StaleReason) {
		return extractorMetadata{}, storeCorruption("extractor %q has invalid candidate state", value.ID)
	}
	if value.State == StateActive && value.Validation < ValidationThreshold {
		return extractorMetadata{}, storeCorruption("active extractor %q is below validation threshold", value.ID)
	}
	var err error
	value.UpdatedAt, err = parseExtractorTime(updatedAt)
	if err != nil {
		return extractorMetadata{}, storeCorruption("extractor %q candidate updated_at is invalid", value.ID)
	}
	return value, nil
}

const extractorMetadataColumns = `id, host, schema_hash, template_cluster_id,
	template_simhash, version, validation, state, stale_reason, updated_at`

func loadExtractorByID(ctx context.Context, tx ledger.ReadTx, id string) (Extractor, error) {
	value, err := scanExtractor(tx.QueryRowContext(ctx, "SELECT "+extractorColumns+" FROM extractors WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Extractor{}, fmt.Errorf("%w: %s", ErrExtractorNotFound, id)
	}
	if err != nil {
		return Extractor{}, fmt.Errorf("compiler: load extractor %q: %w", id, err)
	}
	return value, nil
}

func loadSelectedExtractor(
	ctx context.Context,
	tx ledger.ReadTx,
	key PageKey,
	metadata extractorMetadata,
) (Extractor, error) {
	value, err := loadExtractorByID(ctx, tx, metadata.ID)
	if err != nil {
		return Extractor{}, err
	}
	if value.Host != key.Host || value.SchemaHash != key.SchemaHash ||
		!bytes.Equal(value.Schema, key.Schema) ||
		value.TemplateClusterID != metadata.TemplateClusterID ||
		value.TemplateSimHash != metadata.TemplateSimHash ||
		value.Version != metadata.Version || value.State != metadata.State {
		return Extractor{}, storeCorruption("selected extractor changed or mismatched its cache key")
	}
	return value, nil
}

func loadPageBindingMetadata(ctx context.Context, tx ledger.ReadTx, key PageKey) (extractorMetadata, bool, error) {
	value, err := scanExtractorMetadata(tx.QueryRowContext(ctx, `SELECT `+prefixedColumns("e", extractorMetadataColumns)+`
		FROM extractor_page_bindings b
		JOIN extractors e ON e.id = b.extractor_id
		WHERE b.page_hash = ? AND b.schema_hash = ?`, key.PageHash, key.SchemaHash))
	if errors.Is(err, sql.ErrNoRows) {
		return extractorMetadata{}, false, nil
	}
	if err != nil {
		return extractorMetadata{}, false, fmt.Errorf("compiler: load page binding: %w", err)
	}
	if value.Host != key.Host || value.SchemaHash != key.SchemaHash {
		return extractorMetadata{}, false, storeCorruption("page binding points outside its host or schema")
	}
	return value, true, nil
}

func loadActiveMetadata(ctx context.Context, tx ledger.ReadTx, key PageKey) ([]extractorMetadata, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+extractorMetadataColumns+` FROM extractors
		WHERE host = ? AND schema_hash = ? AND state = 'active'
		ORDER BY template_cluster_id, version DESC LIMIT ?`, key.Host, key.SchemaHash, MaxTemplateClusters+1)
	if err != nil {
		return nil, fmt.Errorf("compiler: query active extractors: %w", err)
	}
	defer rows.Close()
	values := make([]extractorMetadata, 0)
	for rows.Next() {
		value, err := scanExtractorMetadata(rows)
		if err != nil {
			return nil, fmt.Errorf("compiler: scan active extractor: %w", err)
		}
		if value.State != StateActive || value.Host != key.Host || value.SchemaHash != key.SchemaHash {
			return nil, storeCorruption("active extractor query returned mismatched row")
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("compiler: iterate active extractors: %w", err)
	}
	if len(values) > MaxTemplateClusters {
		return nil, storeCorruption("host and schema exceed %d active clusters", MaxTemplateClusters)
	}
	return values, nil
}

func loadClusterMetadata(ctx context.Context, tx ledger.ReadTx, key PageKey) ([]extractorMetadata, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+prefixedColumns("e", extractorMetadataColumns)+` FROM extractors e
		WHERE e.host = ? AND e.schema_hash = ? AND e.version = (
			SELECT MAX(newer.version) FROM extractors newer
			WHERE newer.host = e.host AND newer.schema_hash = e.schema_hash
				AND newer.template_cluster_id = e.template_cluster_id
		)
		ORDER BY e.template_cluster_id LIMIT ?`, key.Host, key.SchemaHash, MaxTemplateClusters+1)
	if err != nil {
		return nil, fmt.Errorf("compiler: query template clusters: %w", err)
	}
	defer rows.Close()
	values := make([]extractorMetadata, 0)
	for rows.Next() {
		value, err := scanExtractorMetadata(rows)
		if err != nil {
			return nil, fmt.Errorf("compiler: scan template cluster: %w", err)
		}
		if value.Host != key.Host || value.SchemaHash != key.SchemaHash {
			return nil, storeCorruption("template cluster query returned mismatched row")
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("compiler: iterate template clusters: %w", err)
	}
	if len(values) > MaxTemplateClusters {
		return nil, fmt.Errorf("%w: host %q schema %q", ErrClusterLimit, key.Host, key.SchemaHash)
	}
	return values, nil
}

func loadActiveCluster(ctx context.Context, tx ledger.ReadTx, key PageKey, clusterID string) (Extractor, bool, error) {
	value, err := scanExtractor(tx.QueryRowContext(ctx, `SELECT `+extractorColumns+` FROM extractors
		WHERE host = ? AND schema_hash = ? AND template_cluster_id = ? AND state = 'active'`,
		key.Host, key.SchemaHash, clusterID))
	if errors.Is(err, sql.ErrNoRows) {
		return Extractor{}, false, nil
	}
	if err != nil {
		return Extractor{}, false, fmt.Errorf("compiler: load active template cluster: %w", err)
	}
	if value.Host != key.Host || value.SchemaHash != key.SchemaHash ||
		value.TemplateClusterID != clusterID || !bytes.Equal(value.Schema, key.Schema) {
		return Extractor{}, false, storeCorruption("active template cluster returned mismatched row")
	}
	return value, true, nil
}

func nextClusterVersion(ctx context.Context, tx ledger.ReadTx, key PageKey, clusterID string) (int, error) {
	var maximum sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(version) FROM extractors
		WHERE host = ? AND schema_hash = ? AND template_cluster_id = ?`,
		key.Host, key.SchemaHash, clusterID).Scan(&maximum); err != nil {
		return 0, fmt.Errorf("compiler: read extractor version: %w", err)
	}
	if !maximum.Valid {
		return 1, nil
	}
	if maximum.Int64 < 1 || maximum.Int64 >= int64(math.MaxInt) {
		return 0, storeCorruption("extractor version is out of range")
	}
	return int(maximum.Int64) + 1, nil
}

func bindPage(ctx context.Context, tx ledger.WriteTx, key PageKey, extractorID string, now time.Time) error {
	formatted := formatExtractorTime(now)
	_, err := tx.ExecContext(ctx, `INSERT INTO extractor_page_bindings (
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
		key.PageHash, key.SchemaHash, extractorID, formatted, formatted)
	if err != nil {
		return fmt.Errorf("compiler: bind page extractor: %w", err)
	}
	return nil
}

func rebindClusterPages(
	ctx context.Context,
	tx ledger.WriteTx,
	key PageKey,
	clusterID string,
	extractorID string,
	now time.Time,
) error {
	formatted := formatExtractorTime(now)
	_, err := tx.ExecContext(ctx, `UPDATE extractor_page_bindings
		SET extractor_id = ?, bound_at = ?, last_seen_at = CASE
			WHEN last_seen_at > ? THEN last_seen_at ELSE ? END
		WHERE schema_hash = ? AND extractor_id IN (
			SELECT id FROM extractors
			WHERE host = ? AND schema_hash = ? AND template_cluster_id = ? AND id <> ?
		)`, extractorID, formatted, formatted, formatted, key.SchemaHash,
		key.Host, key.SchemaHash, clusterID, extractorID)
	if err != nil {
		return fmt.Errorf("compiler: rebind promoted extractor pages: %w", err)
	}
	return nil
}

func nearestExtractor(values []Extractor, fingerprint uint64) (Extractor, bool) {
	bestDistance := TemplateDistanceThreshold + 1
	var best Extractor
	found := false
	for _, value := range values {
		distance := simhash.Distance(value.TemplateSimHash, fingerprint)
		if distance > TemplateDistanceThreshold {
			continue
		}
		if !found || distance < bestDistance || distance == bestDistance && value.TemplateClusterID < best.TemplateClusterID {
			best, bestDistance, found = value, distance, true
		}
	}
	return best, found
}

func nearestMetadata(values []extractorMetadata, fingerprint uint64) (extractorMetadata, bool) {
	bestDistance := TemplateDistanceThreshold + 1
	var best extractorMetadata
	found := false
	for _, value := range values {
		distance := simhash.Distance(value.TemplateSimHash, fingerprint)
		if distance > TemplateDistanceThreshold {
			continue
		}
		if !found || distance < bestDistance || distance == bestDistance && value.TemplateClusterID < best.TemplateClusterID {
			best, bestDistance, found = value, distance, true
		}
	}
	return best, found
}

func exactRegistration(
	value Extractor,
	key PageKey,
	clusterFingerprint uint64,
	irJSON, reportJSON json.RawMessage,
	irHash string,
) bool {
	storedIR, err := json.Marshal(value.IR)
	if err != nil {
		return false
	}
	storedReport, err := json.Marshal(value.Validation)
	if err != nil {
		return false
	}
	return value.State == StateActive && value.TemplateSimHash == clusterFingerprint &&
		bytes.Equal(value.Schema, key.Schema) && value.IRHash == irHash &&
		bytes.Equal(storedIR, irJSON) && bytes.Equal(storedReport, reportJSON)
}

func prefixedExtractorColumns(alias string) string {
	return prefixedColumns(alias, extractorColumns)
}

func prefixedColumns(alias, rawColumns string) string {
	columns := strings.Split(rawColumns, ",")
	for index := range columns {
		columns[index] = alias + "." + strings.TrimSpace(columns[index])
	}
	return strings.Join(columns, ", ")
}

func templateClusterID(fingerprint uint64) string {
	return sha256Hex(encodeSimHash(fingerprint))
}

func encodeSimHash(value uint64) []byte {
	encoded := make([]byte, 8)
	binary.BigEndian.PutUint64(encoded, value)
	return encoded
}

func newExtractorID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("compiler: generate extractor ID: %w", err)
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	encoded := hex.EncodeToString(raw[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

func validExtractorID(id string) bool {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return false
	}
	for index, current := range id {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if !(current >= '0' && current <= '9' || current >= 'a' && current <= 'f') {
			return false
		}
	}
	return true
}

func validLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, current := range value {
		if !(current >= '0' && current <= '9' || current >= 'a' && current <= 'f') {
			return false
		}
	}
	return true
}

func validStoredState(state ExtractorState, reason string) bool {
	switch state {
	case StateActive:
		return reason == ""
	case StateStale:
		return reason == "template_drift" || reason == "required_empty_rate" || reason == "superseded"
	case StateRetired:
		return reason == "" || reason == "template_drift" || reason == "required_empty_rate" || reason == "superseded"
	default:
		return false
	}
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func formatExtractorTime(value time.Time) string {
	return value.UTC().Format(extractorTimeLayout)
}

func parseExtractorTime(value string) (time.Time, error) {
	parsed, err := time.Parse(extractorTimeLayout, value)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}

func timePointer(value time.Time) *time.Time {
	copy := value
	return &copy
}

func maxExtractorTime(value, floor time.Time) time.Time {
	value = value.UTC()
	floor = floor.UTC()
	if value.Before(floor) {
		return floor
	}
	return value
}

func cloneExtractor(value Extractor) Extractor {
	value.Schema = append(json.RawMessage(nil), value.Schema...)
	value.IR.Fields = append([]FieldRule(nil), value.IR.Fields...)
	for index := range value.IR.Fields {
		value.IR.Fields[index].Transforms = append([]string(nil), value.IR.Fields[index].Transforms...)
	}
	value.Validation.PerField = append([]FieldValidation(nil), value.Validation.PerField...)
	value.EmptyWindow = append([]UseOutcome(nil), value.EmptyWindow...)
	if value.LastUsedAt != nil {
		value.LastUsedAt = timePointer(*value.LastUsedAt)
	}
	return value
}

func storeInputError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidStoreInput, fmt.Sprintf(format, args...))
}

func storeCorruption(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrExtractorStore, fmt.Sprintf(format, args...))
}
