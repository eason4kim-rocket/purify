package compiler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/use-agent/purify/cleaner"
	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/publicnet"
	"github.com/use-agent/purify/simhash"
	"github.com/use-agent/purify/snapshot"
)

const (
	CoordinatorQueueSize       = 32
	CoordinatorTaskTimeout     = 2 * time.Minute
	CoordinatorLeaseDuration   = 3 * time.Minute
	CoordinatorTransientDelay  = 15 * time.Minute
	CoordinatorWeakDelay       = 24 * time.Hour
	CoordinatorAttemptTTL      = 30 * 24 * time.Hour
	MaxCoordinatorAttemptRows  = 10_000
	coordinatorFinalizeTimeout = 5 * time.Second
)

var (
	ErrInvalidCoordinator   = errors.New("compiler: invalid coordinator configuration")
	ErrCoordinatorClosed    = errors.New("compiler: coordinator is closed")
	ErrCoordinatorQueueFull = errors.New("compiler: coordinator queue is full")
	ErrCoordinatorCapacity  = errors.New("compiler: coordinator attempt capacity reached")
	ErrCoordinatorLeaseLost = errors.New("compiler: coordinator attempt lease lost")
)

type attemptOutcome string
type attemptReason string

const (
	attemptSuccess     attemptOutcome = "success"
	attemptWeak        attemptOutcome = "weak"
	attemptNoCandidate attemptOutcome = "no_candidate"
	attemptTransient   attemptOutcome = "transient"

	reasonCompiled                  attemptReason = "compiled"
	reasonValidationBelowThreshold  attemptReason = "validation_below_threshold"
	reasonSamplesUnavailable        attemptReason = "samples_unavailable"
	reasonSampleProvenanceMismatch  attemptReason = "sample_provenance_mismatch"
	reasonSampleFingerprintMismatch attemptReason = "sample_fingerprint_mismatch"
	reasonNoExtractorCandidate      attemptReason = "no_extractor_candidate"
	reasonCatalogUnavailable        attemptReason = "catalog_unavailable"
	reasonSnapshotUnavailable       attemptReason = "snapshot_unavailable"
	reasonCompileFailed             attemptReason = "compile_failed"
	reasonSaveFailed                attemptReason = "save_failed"
	reasonTaskTimeout               attemptReason = "task_timeout"
	reasonTaskCanceled              attemptReason = "task_canceled"
	reasonPanicRecovered            attemptReason = "panic_recovered"
)

type coordinatorTask struct {
	set    SampleSet
	schema json.RawMessage
}

type coordinatorWork struct {
	latest  coordinatorTask
	queued  bool
	running bool
	dirty   bool
}

type attemptResult struct {
	outcome attemptOutcome
	reason  attemptReason
}

type hydrationFailure struct {
	result attemptResult
}

func (failure hydrationFailure) Error() string { return string(failure.result.reason) }

// Coordinator asynchronously turns bounded catalog references into active
// deterministic extractors. It never accepts provider or request credentials;
// its TruthExtractor is a process-owned managed dependency supplied at startup.
type Coordinator struct {
	ledger    *ledger.Store
	catalog   *SampleCatalog
	registry  *Store
	snapshots *snapshot.Store
	extractor TruthExtractor
	cleaner   *cleaner.Cleaner
	clock     func() time.Time

	ctx    context.Context
	cancel context.CancelFunc
	queue  chan CompileKey

	mu        sync.Mutex
	closed    bool
	work      map[CompileKey]*coordinatorWork
	wg        sync.WaitGroup
	closeOnce sync.Once
	done      chan struct{}

	taskTimeout   time.Duration
	leaseDuration time.Duration

	observeCatalog func(context.Context, PageKey, string, time.Time) (SampleSet, bool, error)
	loadCatalog    func(context.Context, CompileKey, int) (SampleSet, bool, error)
	readContent    func(snapshot.ID, int) ([]byte, error)
	hasObservation func(context.Context, snapshot.ID, func(snapshot.Meta) bool) (bool, error)
	cleanContent   func(string, string) (string, error)
	compileIR      func(context.Context, []Sample, json.RawMessage, TruthExtractor) (IR, ValidationReport, error)
	saveExtractor  func(context.Context, PageKey, IR, ValidationReport) (Extractor, error)
}

// NewCoordinator starts one background compiler worker. An optional clock is
// accepted for deterministic embedding and tests; all other limits are fixed.
func NewCoordinator(
	catalog *SampleCatalog,
	registry *Store,
	snapshots *snapshot.Store,
	extractor TruthExtractor,
	clocks ...func() time.Time,
) (*Coordinator, error) {
	if catalog == nil || registry == nil || snapshots == nil ||
		catalog.ledger == nil || registry.ledger == nil || catalog.ledger != registry.ledger ||
		isNilTruthExtractor(extractor) {
		return nil, fmt.Errorf("%w: dependencies are nil or use different ledgers", ErrInvalidCoordinator)
	}
	if len(clocks) > 1 || len(clocks) == 1 && clocks[0] == nil {
		return nil, fmt.Errorf("%w: expected at most one non-nil clock", ErrInvalidCoordinator)
	}
	clock := time.Now
	if len(clocks) == 1 {
		clock = clocks[0]
	}
	if _, err := normalizeCatalogTime(clock()); err != nil {
		return nil, fmt.Errorf("%w: clock is invalid", ErrInvalidCoordinator)
	}
	contentCleaner := cleaner.NewCleaner()
	lifecycle, cancel := context.WithCancel(context.Background())
	c := &Coordinator{
		ledger:    catalog.ledger,
		catalog:   catalog,
		registry:  registry,
		snapshots: snapshots,
		extractor: extractor,
		cleaner:   contentCleaner,
		clock:     clock,
		ctx:       lifecycle,
		cancel:    cancel,
		// One physical slot is reserved for a dirty revision already accepted
		// while the sole worker is running. Public admission remains exactly 32.
		queue:          make(chan CompileKey, CoordinatorQueueSize+1),
		work:           make(map[CompileKey]*coordinatorWork),
		done:           make(chan struct{}),
		taskTimeout:    CoordinatorTaskTimeout,
		leaseDuration:  CoordinatorLeaseDuration,
		observeCatalog: catalog.Observe,
		loadCatalog:    catalog.Load,
		readContent:    snapshots.Content,
		hasObservation: snapshots.HasObservationContext,
		cleanContent: func(rawHTML, sourceURL string) (string, error) {
			response, err := contentCleaner.Clean(rawHTML, sourceURL, "markdown", "readability")
			if err != nil {
				return "", err
			}
			return response.Content, nil
		},
		compileIR:     Compile,
		saveExtractor: registry.Save,
	}
	c.wg.Add(1)
	go c.worker()
	return c, nil
}

// Observe records a successful snapshot and non-blockingly schedules a ready
// fixed template cluster. Request cancellation affects only catalog admission;
// accepted work runs under the coordinator lifecycle.
func (c *Coordinator) Observe(
	ctx context.Context,
	page PageKey,
	snapshotID string,
	fetchedAt time.Time,
	canonicalSchema json.RawMessage,
) error {
	if err := c.validateOpen(); err != nil {
		return err
	}
	if err := validatePageKey(page); err != nil {
		return fmt.Errorf("%w: page key is invalid", ErrInvalidCoordinator)
	}
	schema, err := canonicalSchemaJSON(canonicalSchema)
	if err != nil {
		return fmt.Errorf("%w: schema is invalid", ErrInvalidCoordinator)
	}
	if sha256Hex(schema) != page.SchemaHash || !bytes.Equal(schema, page.Schema) {
		return fmt.Errorf("%w: schema does not match page key", ErrInvalidCoordinator)
	}
	set, ready, err := c.observeCatalog(ctx, page, snapshotID, fetchedAt)
	if err != nil {
		return err
	}
	if !ready {
		return nil
	}
	if set.Key.SchemaHash != page.SchemaHash {
		return fmt.Errorf("%w: schema does not match page key", ErrInvalidCoordinator)
	}
	task := coordinatorTask{set: cloneSampleSet(set), schema: append(json.RawMessage(nil), schema...)}
	return c.schedule(task)
}

func (c *Coordinator) validateOpen() error {
	if c == nil || c.ledger == nil || c.catalog == nil || c.registry == nil ||
		c.snapshots == nil || c.extractor == nil || c.clock == nil || c.cancel == nil {
		return fmt.Errorf("%w: coordinator is nil or uninitialized", ErrInvalidCoordinator)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrCoordinatorClosed
	}
	return nil
}

func (c *Coordinator) schedule(task coordinatorTask) error {
	key := task.set.Key
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrCoordinatorClosed
	}
	if existing, ok := c.work[key]; ok {
		changed := existing.latest.set.Revision != task.set.Revision ||
			!bytes.Equal(existing.latest.schema, task.schema)
		if changed {
			existing.latest = cloneCoordinatorTask(task)
			if existing.running {
				existing.dirty = true
			}
		}
		return nil
	}

	work := &coordinatorWork{latest: cloneCoordinatorTask(task), queued: true}
	if len(c.queue) >= CoordinatorQueueSize {
		return ErrCoordinatorQueueFull
	}
	select {
	case c.queue <- key:
		c.work[key] = work
		return nil
	default:
		return ErrCoordinatorQueueFull
	}
}

func (c *Coordinator) worker() {
	defer c.wg.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		case key := <-c.queue:
			if c.ctx.Err() != nil {
				return
			}
			task, ok := c.beginWork(key)
			if !ok {
				continue
			}
			c.process(task)
			c.finishWork(key, task)
		}
	}
}

func (c *Coordinator) beginWork(key CompileKey) (coordinatorTask, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	work, ok := c.work[key]
	if !ok || c.closed {
		delete(c.work, key)
		return coordinatorTask{}, false
	}
	work.queued = false
	work.running = true
	work.dirty = false
	return cloneCoordinatorTask(work.latest), true
}

func (c *Coordinator) finishWork(key CompileKey, completed coordinatorTask) {
	c.mu.Lock()
	defer c.mu.Unlock()
	work, ok := c.work[key]
	if !ok {
		return
	}
	work.running = false
	if c.closed {
		delete(c.work, key)
		return
	}
	changed := work.dirty && (work.latest.set.Revision != completed.set.Revision ||
		!bytes.Equal(work.latest.schema, completed.schema))
	if !changed {
		delete(c.work, key)
		return
	}
	// The dirty revision was already accepted while this key was running. The
	// physical queue has one reserved slot unavailable to public admission, so
	// this reliable tail requeue cannot block and preserves FIFO fairness.
	work.dirty = false
	work.queued = true
	c.queue <- key
}

func (c *Coordinator) process(task coordinatorTask) {
	var claimedToken, claimedRevision string
	catalogRevisionVerified := false
	defer func() {
		if recover() != nil && claimedToken != "" {
			result := attemptResult{outcome: attemptTransient, reason: reasonPanicRecovered}
			if catalogRevisionVerified {
				c.finalizeClaim(task.set.Key, claimedRevision, claimedToken, result)
			} else {
				c.abortUnverifiedClaim(task.set.Key, claimedToken, result)
			}
		}
	}()
	ctx, cancel := context.WithTimeout(c.ctx, c.taskTimeout)
	defer cancel()

	token, claimed, err := c.claim(ctx, task.set.Key, task.set.Revision)
	if err != nil || !claimed {
		return
	}
	claimedToken, claimedRevision = token, task.set.Revision
	set, ready, err := c.loadCatalog(ctx, task.set.Key, MaxCompileSamples)
	if err != nil {
		c.abortUnverifiedClaim(task.set.Key, token,
			contextAttemptResult(ctx, reasonCatalogUnavailable))
		claimedToken, claimedRevision = "", ""
		return
	}
	if set.Revision != "" && set.Revision != task.set.Revision {
		proceed, err := c.handoff(ctx, task.set.Key, task.set.Revision, set.Revision, token)
		if err != nil {
			c.abortUnverifiedClaim(task.set.Key, token,
				contextAttemptResult(ctx, reasonCatalogUnavailable))
			claimedToken, claimedRevision = "", ""
			return
		}
		if !proceed {
			claimedToken, claimedRevision = "", ""
			return
		}
		claimedToken, claimedRevision = token, set.Revision
	}
	if set.Revision == "" {
		c.abortUnverifiedClaim(task.set.Key, token,
			attemptResult{outcome: attemptNoCandidate, reason: reasonSamplesUnavailable})
		claimedToken, claimedRevision = "", ""
		return
	}
	catalogRevisionVerified = true
	if !ready {
		c.finalizeClaim(task.set.Key, claimedRevision, token,
			attemptResult{outcome: attemptNoCandidate, reason: reasonSamplesUnavailable})
		claimedToken, claimedRevision = "", ""
		return
	}
	task.set = cloneSampleSet(set)

	result := c.runClaimed(ctx, task)
	c.finalizeClaim(task.set.Key, task.set.Revision, token, result)
	claimedToken, claimedRevision = "", ""
}

func (c *Coordinator) finalizeClaim(key CompileKey, revision, token string, result attemptResult) {
	// Finalization must outlive task and lifecycle cancellation long enough to
	// release a durable lease. Close waits for this worker before its owner
	// closes the ledger, while the short independent bound prevents shutdown
	// from hanging on storage indefinitely.
	finalizeCtx, finalizeCancel := context.WithTimeout(context.Background(), coordinatorFinalizeTimeout)
	defer finalizeCancel()
	_ = c.finalize(finalizeCtx, key, revision, token, result)
}

// abortUnverifiedClaim settles a lease before the worker has established which
// catalog revision is current. A stale queued revision may have claimed over a
// durable success/weak/no-candidate watermark for a different revision; that
// watermark must survive catalog, handoff, cancellation, and panic failures.
// The token lookup also makes cleanup safe when a handoff commit result is
// ambiguous: whichever revision currently owns the same lease is released.
func (c *Coordinator) abortUnverifiedClaim(key CompileKey, token string, result attemptResult) {
	abortCtx, abortCancel := context.WithTimeout(context.Background(), coordinatorFinalizeTimeout)
	defer abortCancel()
	_ = c.abortUnverified(abortCtx, key, token, result)
}

func (c *Coordinator) runClaimed(ctx context.Context, task coordinatorTask) (result attemptResult) {
	defer func() {
		if recover() != nil {
			result = attemptResult{outcome: attemptTransient, reason: reasonPanicRecovered}
		}
	}()
	samples, saveKey, err := c.hydrate(ctx, task.set, task.schema)
	if err != nil {
		var failure hydrationFailure
		if errors.As(err, &failure) {
			return failure.result
		}
		return contextAttemptResult(ctx, reasonSnapshotUnavailable)
	}
	ir, report, err := c.compileIR(ctx, samples, task.schema, c.extractor)
	if err != nil {
		if errors.Is(err, ErrNoCandidate) || errors.Is(err, ErrUnsupportedSchema) ||
			errors.Is(err, ErrInvalidInput) || errors.Is(err, ErrResourceLimit) {
			return attemptResult{outcome: attemptNoCandidate, reason: reasonNoExtractorCandidate}
		}
		return contextAttemptResult(ctx, reasonCompileFailed)
	}
	if !report.CanEnable {
		return attemptResult{outcome: attemptWeak, reason: reasonValidationBelowThreshold}
	}
	if _, err := c.saveExtractor(ctx, saveKey, ir, report); err != nil {
		return contextAttemptResult(ctx, reasonSaveFailed)
	}
	return attemptResult{outcome: attemptSuccess, reason: reasonCompiled}
}

func contextAttemptResult(ctx context.Context, fallback attemptReason) attemptResult {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return attemptResult{outcome: attemptTransient, reason: reasonTaskTimeout}
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return attemptResult{outcome: attemptTransient, reason: reasonTaskCanceled}
	}
	return attemptResult{outcome: attemptTransient, reason: fallback}
}

func (c *Coordinator) hydrate(ctx context.Context, set SampleSet, schema json.RawMessage) ([]Sample, PageKey, error) {
	if len(set.Samples) < MinCompileSamples || len(set.Samples) > MaxCompileSamples {
		return nil, PageKey{}, hydrationFailure{attemptResult{attemptNoCandidate, reasonSamplesUnavailable}}
	}
	samples := make([]Sample, 0, len(set.Samples))
	totalInputBytes := 0
	var saveKey PageKey
	for _, ref := range set.Samples {
		if err := ctx.Err(); err != nil {
			return nil, PageKey{}, err
		}
		htmlBytes, err := c.readContent(snapshot.ID(ref.SnapshotID), MaxSampleContentBytes)
		if err != nil {
			if ctx.Err() != nil {
				return nil, PageKey{}, ctx.Err()
			}
			return nil, PageKey{}, hydrationFailure{attemptResult{attemptTransient, reasonSnapshotUnavailable}}
		}
		if len(htmlBytes) > MaxHTMLBytes || len(htmlBytes) > MaxCompileInputBytes-totalInputBytes {
			return nil, PageKey{}, hydrationFailure{attemptResult{attemptNoCandidate, reasonNoExtractorCandidate}}
		}
		totalInputBytes += len(htmlBytes)
		var canonicalURL string
		matched, err := c.hasObservation(ctx, snapshot.ID(ref.SnapshotID), func(meta snapshot.Meta) bool {
			if !meta.FetchedAt.UTC().Equal(ref.FetchedAt) || meta.StatusCode < 200 || meta.StatusCode >= 300 {
				return false
			}
			normalized, _, normalizeErr := publicnet.NormalizeHTTPURL(meta.URL, nil, false)
			if normalizeErr != nil || sha256Hex([]byte(normalized)) != ref.PageHash {
				return false
			}
			canonicalURL = normalized
			return true
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil, PageKey{}, ctx.Err()
			}
			return nil, PageKey{}, hydrationFailure{attemptResult{attemptTransient, reasonSnapshotUnavailable}}
		}
		if !matched || canonicalURL == "" {
			return nil, PageKey{}, hydrationFailure{attemptResult{attemptNoCandidate, reasonSampleProvenanceMismatch}}
		}
		html := string(htmlBytes)
		page, err := BuildPageKey(canonicalURL, schema, html)
		if err != nil || page.Host != set.Key.Host || page.SchemaHash != set.Key.SchemaHash ||
			page.PageHash != ref.PageHash || page.TemplateSimHash != ref.SampleSimHash ||
			simhash.Distance(page.TemplateSimHash, set.Key.ClusterSimHash) > TemplateDistanceThreshold {
			return nil, PageKey{}, hydrationFailure{attemptResult{attemptNoCandidate, reasonSampleFingerprintMismatch}}
		}
		content, err := c.cleanContent(html, canonicalURL)
		if err != nil {
			return nil, PageKey{}, contextErrorOrHydration(ctx, reasonSamplesUnavailable)
		}
		if len(content) > MaxSampleContentBytes || len(content) > MaxCompileInputBytes-totalInputBytes {
			return nil, PageKey{}, hydrationFailure{attemptResult{attemptNoCandidate, reasonNoExtractorCandidate}}
		}
		totalInputBytes += len(content)
		if len(samples) == 0 {
			saveKey = page
			saveKey.TemplateSimHash = set.Key.ClusterSimHash
		}
		samples = append(samples, Sample{Content: content, HTML: html})
	}
	return samples, saveKey, nil
}

func contextErrorOrHydration(ctx context.Context, reason attemptReason) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return hydrationFailure{attemptResult{attemptNoCandidate, reason}}
}

func (c *Coordinator) claim(ctx context.Context, key CompileKey, revision string) (string, bool, error) {
	if err := validateCompileKey(key); err != nil || !validLowerHex(revision, 64) {
		return "", false, fmt.Errorf("%w: invalid claim identity", ErrInvalidCoordinator)
	}
	token, err := newExtractorID()
	if err != nil {
		return "", false, err
	}
	claimed := false
	err = c.ledger.Update(ctx, func(tx ledger.WriteTx) error {
		now, err := c.attemptTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := cleanupAttempts(ctx, tx, now); err != nil {
			return err
		}
		row, found, err := loadAttempt(ctx, tx, key)
		if err != nil {
			return err
		}
		if found {
			if row.clusterSimHash != key.ClusterSimHash {
				return fmt.Errorf("%w: fixed cluster representative changed", ErrInvalidCoordinator)
			}
			if row.leaseUntil != nil && row.leaseUntil.After(now) {
				return nil
			}
			if !attemptEligible(row, revision, now) {
				return nil
			}
		} else if err := makeAttemptCapacity(ctx, tx, now); err != nil {
			return err
		}
		updatedAt := now
		createdAt := now
		if found {
			updatedAt = maxExtractorTime(now, row.updatedAt)
			createdAt = row.createdAt
		}
		leaseUntil := updatedAt.Add(c.leaseDuration)
		if _, err := normalizeCatalogTime(leaseUntil); err != nil {
			return fmt.Errorf("%w: lease time is invalid", ErrInvalidCoordinator)
		}
		if found {
			_, err = tx.ExecContext(ctx, `UPDATE compiler_attempts SET
				lease_id = ?, lease_revision = ?, lease_until = ?
			WHERE host = ? AND schema_hash = ? AND content_profile = ? AND template_cluster_id = ?`,
				token, revision, formatExtractorTime(leaseUntil),
				key.Host, key.SchemaHash, key.ContentProfile, key.TemplateClusterID)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO compiler_attempts (
				host, schema_hash, content_profile, template_cluster_id, cluster_simhash,
				lease_id, lease_revision, lease_until, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				key.Host, key.SchemaHash, key.ContentProfile, key.TemplateClusterID,
				encodeSimHash(key.ClusterSimHash), token, revision, formatExtractorTime(leaseUntil),
				formatExtractorTime(createdAt), formatExtractorTime(updatedAt))
		}
		if err != nil {
			return fmt.Errorf("compiler: claim compilation attempt: %w", err)
		}
		claimed = true
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return token, claimed, nil
}

func (c *Coordinator) finalize(
	ctx context.Context,
	key CompileKey,
	revision, token string,
	result attemptResult,
) error {
	if !validAttemptResult(result) {
		return fmt.Errorf("%w: invalid attempt result", ErrInvalidCoordinator)
	}
	return c.ledger.Update(ctx, func(tx ledger.WriteTx) error {
		row, found, err := loadAttempt(ctx, tx, key)
		if err != nil {
			return err
		}
		if !found || row.leaseID != token || row.leaseRevision != revision {
			return ErrCoordinatorLeaseLost
		}
		now, err := c.attemptTime(ctx, tx)
		if err != nil {
			return err
		}
		now = maxExtractorTime(now, row.updatedAt)
		var cooldown any
		switch result.outcome {
		case attemptWeak, attemptNoCandidate:
			cooldown = formatExtractorTime(now.Add(CoordinatorWeakDelay))
		case attemptTransient:
			cooldown = formatExtractorTime(now.Add(CoordinatorTransientDelay))
		case attemptSuccess:
			cooldown = nil
		}
		write, err := tx.ExecContext(ctx, `UPDATE compiler_attempts SET
			attempted_revision = ?, outcome = ?, reason = ?, cooldown_until = ?,
			lease_id = NULL, lease_revision = NULL, lease_until = NULL, updated_at = ?
		WHERE host = ? AND schema_hash = ? AND content_profile = ? AND
			template_cluster_id = ? AND lease_id = ? AND lease_revision = ?`,
			revision, string(result.outcome), string(result.reason), cooldown,
			formatExtractorTime(now), key.Host, key.SchemaHash, key.ContentProfile,
			key.TemplateClusterID, token, revision)
		if err != nil {
			return fmt.Errorf("compiler: finalize compilation attempt: %w", err)
		}
		changed, err := write.RowsAffected()
		if err != nil {
			return fmt.Errorf("compiler: inspect compilation finalization: %w", err)
		}
		if changed != 1 {
			return ErrCoordinatorLeaseLost
		}
		return nil
	})
}

func (c *Coordinator) abortUnverified(
	ctx context.Context,
	key CompileKey,
	token string,
	result attemptResult,
) error {
	if !validAttemptResult(result) {
		return fmt.Errorf("%w: invalid unverified attempt result", ErrInvalidCoordinator)
	}
	return c.ledger.Update(ctx, func(tx ledger.WriteTx) error {
		row, found, err := loadAttempt(ctx, tx, key)
		if err != nil {
			return err
		}
		// A missing/mismatched token means the lease was already settled or
		// replaced. Never mutate the newer owner.
		if !found || row.leaseID != token {
			return nil
		}

		// A stale queued revision may claim while a different revision's
		// terminal watermark remains durable. Before catalog validation, only
		// release that lease; overwriting the watermark would make a healthy
		// compiled revision appear failed. A retry of the same transient
		// revision, however, records its latest bounded failure.
		if row.outcome != "" && row.attemptRevision != row.leaseRevision {
			write, err := tx.ExecContext(ctx, `UPDATE compiler_attempts SET
				lease_id = NULL, lease_revision = NULL, lease_until = NULL
			WHERE host = ? AND schema_hash = ? AND content_profile = ? AND
				template_cluster_id = ? AND lease_id = ?`,
				key.Host, key.SchemaHash, key.ContentProfile, key.TemplateClusterID, token)
			if err != nil {
				return fmt.Errorf("compiler: abort stale compilation attempt: %w", err)
			}
			changed, err := write.RowsAffected()
			if err != nil {
				return fmt.Errorf("compiler: inspect stale compilation abort: %w", err)
			}
			if changed != 1 {
				return ErrCoordinatorLeaseLost
			}
			return nil
		}

		now, err := c.attemptTime(ctx, tx)
		if err != nil {
			return err
		}
		now = maxExtractorTime(now, row.updatedAt)
		var cooldown any
		switch result.outcome {
		case attemptWeak, attemptNoCandidate:
			cooldown = formatExtractorTime(now.Add(CoordinatorWeakDelay))
		case attemptTransient:
			cooldown = formatExtractorTime(now.Add(CoordinatorTransientDelay))
		case attemptSuccess:
			cooldown = nil
		}
		write, err := tx.ExecContext(ctx, `UPDATE compiler_attempts SET
			attempted_revision = ?, outcome = ?, reason = ?, cooldown_until = ?,
			lease_id = NULL, lease_revision = NULL, lease_until = NULL, updated_at = ?
		WHERE host = ? AND schema_hash = ? AND content_profile = ? AND
			template_cluster_id = ? AND lease_id = ?`,
			row.leaseRevision, string(result.outcome), string(result.reason), cooldown,
			formatExtractorTime(now), key.Host, key.SchemaHash, key.ContentProfile,
			key.TemplateClusterID, token)
		if err != nil {
			return fmt.Errorf("compiler: settle unverified compilation attempt: %w", err)
		}
		changed, err := write.RowsAffected()
		if err != nil {
			return fmt.Errorf("compiler: inspect unverified compilation settlement: %w", err)
		}
		if changed != 1 {
			return ErrCoordinatorLeaseLost
		}
		return nil
	})
}

func (c *Coordinator) release(ctx context.Context, key CompileKey, revision, token string) error {
	return c.ledger.Update(ctx, func(tx ledger.WriteTx) error {
		row, found, err := loadAttempt(ctx, tx, key)
		if err != nil {
			return err
		}
		if !found || row.leaseID != token || row.leaseRevision != revision {
			return ErrCoordinatorLeaseLost
		}
		write, err := tx.ExecContext(ctx, `UPDATE compiler_attempts SET
			lease_id = NULL, lease_revision = NULL, lease_until = NULL
		WHERE host = ? AND schema_hash = ? AND content_profile = ? AND
			template_cluster_id = ? AND lease_id = ? AND lease_revision = ?`,
			key.Host, key.SchemaHash, key.ContentProfile,
			key.TemplateClusterID, token, revision)
		if err != nil {
			return fmt.Errorf("compiler: release compilation attempt: %w", err)
		}
		changed, err := write.RowsAffected()
		if err != nil {
			return fmt.Errorf("compiler: inspect compilation attempt release: %w", err)
		}
		if changed != 1 {
			return ErrCoordinatorLeaseLost
		}
		return nil
	})
}

func (c *Coordinator) handoff(ctx context.Context, key CompileKey, fromRevision, toRevision, token string) (bool, error) {
	if !validLowerHex(toRevision, 64) {
		return false, fmt.Errorf("%w: invalid handoff revision", ErrInvalidCoordinator)
	}
	proceed := false
	err := c.ledger.Update(ctx, func(tx ledger.WriteTx) error {
		row, found, err := loadAttempt(ctx, tx, key)
		if err != nil {
			return err
		}
		if !found || row.leaseID != token || row.leaseRevision != fromRevision {
			return ErrCoordinatorLeaseLost
		}
		now, err := c.attemptTime(ctx, tx)
		if err != nil {
			return err
		}
		now = maxExtractorTime(now, row.updatedAt)
		if !attemptEligible(row, toRevision, now) {
			write, err := tx.ExecContext(ctx, `UPDATE compiler_attempts SET
				lease_id = NULL, lease_revision = NULL, lease_until = NULL
			WHERE host = ? AND schema_hash = ? AND content_profile = ? AND
				template_cluster_id = ? AND lease_id = ? AND lease_revision = ?`,
				key.Host, key.SchemaHash, key.ContentProfile, key.TemplateClusterID,
				token, fromRevision)
			if err != nil {
				return fmt.Errorf("compiler: stop compilation attempt handoff: %w", err)
			}
			changed, err := write.RowsAffected()
			if err != nil {
				return fmt.Errorf("compiler: inspect stopped compilation handoff: %w", err)
			}
			if changed != 1 {
				return ErrCoordinatorLeaseLost
			}
			return nil
		}
		leaseUntil := now.Add(c.leaseDuration)
		write, err := tx.ExecContext(ctx, `UPDATE compiler_attempts SET
			lease_revision = ?, lease_until = ?
		WHERE host = ? AND schema_hash = ? AND content_profile = ? AND
			template_cluster_id = ? AND lease_id = ? AND lease_revision = ?`,
			toRevision, formatExtractorTime(leaseUntil),
			key.Host, key.SchemaHash, key.ContentProfile, key.TemplateClusterID,
			token, fromRevision)
		if err != nil {
			return fmt.Errorf("compiler: hand off compilation attempt: %w", err)
		}
		changed, err := write.RowsAffected()
		if err != nil {
			return fmt.Errorf("compiler: inspect compilation attempt handoff: %w", err)
		}
		if changed != 1 {
			return ErrCoordinatorLeaseLost
		}
		proceed = true
		return nil
	})
	return proceed, err
}

func validAttemptResult(result attemptResult) bool {
	switch result.outcome {
	case attemptSuccess:
		return result.reason == reasonCompiled
	case attemptWeak:
		return result.reason == reasonValidationBelowThreshold
	case attemptNoCandidate:
		return result.reason == reasonSamplesUnavailable ||
			result.reason == reasonSampleProvenanceMismatch ||
			result.reason == reasonSampleFingerprintMismatch ||
			result.reason == reasonNoExtractorCandidate
	case attemptTransient:
		return result.reason == reasonCatalogUnavailable ||
			result.reason == reasonSnapshotUnavailable ||
			result.reason == reasonCompileFailed || result.reason == reasonSaveFailed ||
			result.reason == reasonTaskTimeout || result.reason == reasonTaskCanceled ||
			result.reason == reasonPanicRecovered
	default:
		return false
	}
}

type attemptRow struct {
	clusterSimHash  uint64
	attemptRevision string
	outcome         attemptOutcome
	reason          attemptReason
	cooldownUntil   *time.Time
	leaseID         string
	leaseRevision   string
	leaseUntil      *time.Time
	createdAt       time.Time
	updatedAt       time.Time
}

func loadAttempt(ctx context.Context, tx ledger.ReadTx, key CompileKey) (attemptRow, bool, error) {
	var row attemptRow
	var cluster []byte
	var cooldown, leaseID, leaseRevision, leaseUntil sql.NullString
	var created, updated string
	err := tx.QueryRowContext(ctx, `SELECT cluster_simhash, attempted_revision, outcome, reason,
		cooldown_until, lease_id, lease_revision, lease_until, created_at, updated_at
		FROM compiler_attempts WHERE host = ? AND schema_hash = ? AND content_profile = ?
			AND template_cluster_id = ?`, key.Host, key.SchemaHash, key.ContentProfile,
		key.TemplateClusterID).Scan(&cluster, &row.attemptRevision, &row.outcome, &row.reason,
		&cooldown, &leaseID, &leaseRevision, &leaseUntil, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return attemptRow{}, false, nil
	}
	if err != nil {
		return attemptRow{}, false, fmt.Errorf("compiler: load compilation attempt: %w", err)
	}
	if len(cluster) != 8 {
		return attemptRow{}, false, fmt.Errorf("compiler: compilation attempt is corrupt")
	}
	row.clusterSimHash = binary.BigEndian.Uint64(cluster)
	row.leaseID = leaseID.String
	row.leaseRevision = leaseRevision.String
	var parseErr error
	row.createdAt, parseErr = parseExtractorTime(created)
	if parseErr != nil {
		return attemptRow{}, false, fmt.Errorf("compiler: compilation attempt is corrupt")
	}
	row.updatedAt, parseErr = parseExtractorTime(updated)
	if parseErr != nil {
		return attemptRow{}, false, fmt.Errorf("compiler: compilation attempt is corrupt")
	}
	if cooldown.Valid {
		value, err := parseExtractorTime(cooldown.String)
		if err != nil {
			return attemptRow{}, false, fmt.Errorf("compiler: compilation attempt is corrupt")
		}
		row.cooldownUntil = &value
	}
	if leaseUntil.Valid {
		value, err := parseExtractorTime(leaseUntil.String)
		if err != nil {
			return attemptRow{}, false, fmt.Errorf("compiler: compilation attempt is corrupt")
		}
		row.leaseUntil = &value
	}
	return row, true, nil
}

func attemptEligible(row attemptRow, revision string, now time.Time) bool {
	if row.outcome == attemptSuccess && row.attemptRevision == revision {
		return false
	}
	if row.outcome == attemptWeak || row.outcome == attemptNoCandidate {
		if row.attemptRevision == revision {
			return false
		}
		if row.cooldownUntil != nil && now.Before(*row.cooldownUntil) {
			return false
		}
	}
	if row.outcome == attemptTransient && row.cooldownUntil != nil && now.Before(*row.cooldownUntil) {
		return false
	}
	return true
}

func (c *Coordinator) attemptTime(ctx context.Context, tx ledger.ReadTx) (time.Time, error) {
	now, err := normalizeCatalogTime(c.clock())
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: clock is invalid", ErrInvalidCoordinator)
	}
	var maximum sql.NullString
	if err := tx.QueryRowContext(ctx, "SELECT MAX(updated_at) FROM compiler_attempts").Scan(&maximum); err != nil {
		return time.Time{}, fmt.Errorf("compiler: inspect compilation attempt clock: %w", err)
	}
	if maximum.Valid {
		floor, err := parseExtractorTime(maximum.String)
		if err != nil {
			return time.Time{}, fmt.Errorf("compiler: compilation attempt is corrupt")
		}
		now = maxExtractorTime(now, floor)
	}
	return now, nil
}

func cleanupAttempts(ctx context.Context, tx ledger.WriteTx, now time.Time) error {
	cutoff := formatExtractorTime(now.Add(-CoordinatorAttemptTTL))
	formattedNow := formatExtractorTime(now)
	if _, err := tx.ExecContext(ctx, `DELETE FROM compiler_attempts
		WHERE updated_at < ? AND (lease_until IS NULL OR lease_until <= ?)`, cutoff, formattedNow); err != nil {
		return fmt.Errorf("compiler: expire compilation attempts: %w", err)
	}
	return nil
}

func makeAttemptCapacity(ctx context.Context, tx ledger.WriteTx, now time.Time) error {
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM compiler_attempts").Scan(&count); err != nil {
		return fmt.Errorf("compiler: count compilation attempts: %w", err)
	}
	if count < MaxCoordinatorAttemptRows {
		return nil
	}
	excess := count - MaxCoordinatorAttemptRows + 1
	write, err := tx.ExecContext(ctx, `DELETE FROM compiler_attempts WHERE
		(host, schema_hash, content_profile, template_cluster_id) IN (
			SELECT host, schema_hash, content_profile, template_cluster_id
			FROM compiler_attempts
			WHERE lease_until IS NULL OR lease_until <= ?
			ORDER BY updated_at, host, schema_hash, content_profile, template_cluster_id
			LIMIT ?
		)`, formatExtractorTime(now), excess)
	if err != nil {
		return fmt.Errorf("compiler: prune compilation attempts: %w", err)
	}
	changed, err := write.RowsAffected()
	if err != nil {
		return fmt.Errorf("compiler: inspect compilation attempt pruning: %w", err)
	}
	if changed != int64(excess) {
		return ErrCoordinatorCapacity
	}
	return nil
}

// Close cancels running work, drops queued work, and waits for the sole worker.
// It is idempotent and rejects every later Observe call.
func (c *Coordinator) Close() error {
	if c == nil {
		return nil
	}
	if c.cancel == nil || c.done == nil {
		return fmt.Errorf("%w: coordinator is uninitialized", ErrInvalidCoordinator)
	}
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.cancel()
		c.mu.Unlock()
		c.wg.Wait()
		c.mu.Lock()
		clear(c.work)
		c.mu.Unlock()
		close(c.done)
	})
	<-c.done
	return nil
}

func cloneCoordinatorTask(value coordinatorTask) coordinatorTask {
	value.set = cloneSampleSet(value.set)
	value.schema = append(json.RawMessage(nil), value.schema...)
	return value
}
