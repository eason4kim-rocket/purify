package indexer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/use-agent/purify/cleaner"
	"github.com/use-agent/purify/searchindex"
)

const (
	defaultWorkers     = 4
	defaultBatchSize   = 16
	defaultMaxAttempts = 3
	defaultMaxPages    = 500
	defaultMaxPerHost  = 10_000

	// emptyLeaseBackoff separates "the frontier is drained" from "every
	// remaining host is momentarily leased by another worker".
	emptyLeaseBackoff = 100 * time.Millisecond
)

// RunConfig bounds one indexer pass.
type RunConfig struct {
	Workers      int
	BatchSize    int
	MaxAttempts  int
	MaxPages     int
	MaxPerHost   int
	AllowPrivate bool
}

// Stats is a coarse run summary.
type Stats struct {
	Discovered int
	Indexed    int
	Failed     int
	RobotsDeny int
	Skipped304 int
}

// Run discovers seeds, then fetches and indexes until MaxPages or the frontier
// is empty. One in-flight URL per registrable domain; hosts may run in parallel.
func Run(ctx context.Context, store *searchindex.Store, fetcher *Fetcher, discoverer Discoverer, seeds []string, cfg RunConfig) (Stats, error) {
	if store == nil || fetcher == nil || discoverer == nil {
		return Stats{}, fmt.Errorf("indexer: store, fetcher, and discoverer are required")
	}
	if cfg.Workers <= 0 {
		cfg.Workers = defaultWorkers
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultBatchSize
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaultMaxAttempts
	}
	if cfg.MaxPages <= 0 {
		cfg.MaxPages = defaultMaxPages
	}
	if cfg.MaxPerHost <= 0 {
		cfg.MaxPerHost = defaultMaxPerHost
	}

	// A killed run leaves its in-flight URLs leased, and Lease refuses every
	// root with a leased row, so reclaim them before this run starts.
	if _, err := store.ReleaseStaleLeases(ctx); err != nil {
		return Stats{}, err
	}
	discovered, err := SeedFrontier(ctx, store, discoverer, seeds, cfg.AllowPrivate)
	if err != nil {
		return Stats{}, err
	}
	run := &runState{
		store:     store,
		fetcher:   fetcher,
		cfg:       cfg,
		pipeline:  cleaner.NewCleaner(),
		stats:     Stats{Discovered: discovered},
		hostCount: map[string]int{},
		lastFetch: map[string]time.Time{},
		robots:    map[string]robotsEntry{},
	}

	// Lease already refuses a URL whose registrable domain has another leased
	// URL, so every worker is guaranteed a different host and crawl-delay only
	// ever stalls the worker that owns that host.
	var workers sync.WaitGroup
	workers.Add(cfg.Workers)
	for range cfg.Workers {
		go func() {
			defer workers.Done()
			run.work(ctx)
		}()
	}
	workers.Wait()

	if err := run.flush(ctx); err != nil {
		return run.snapshot(), err
	}
	if err := ctx.Err(); err != nil {
		return run.snapshot(), err
	}
	return run.snapshot(), run.failure()
}

// runState is the shared crawl state. Every field below mu is guarded; network
// and cleaning work always happens with mu released.
type runState struct {
	store    *searchindex.Store
	fetcher  *Fetcher
	cfg      RunConfig
	pipeline *cleaner.Cleaner

	mu        sync.Mutex
	stats     Stats
	hostCount map[string]int
	lastFetch map[string]time.Time
	robots    map[string]robotsEntry
	batch     []searchindex.Page
	err       error
	stopped   bool
}

type robotsEntry struct {
	policy  Robots
	blocked bool
}

func (r *runState) work(ctx context.Context) {
	for {
		if ctx.Err() != nil || r.done() {
			return
		}
		item, ok, leaseErr := r.store.Lease(ctx, time.Now())
		if leaseErr != nil {
			r.fail(leaseErr)
			return
		}
		if !ok {
			// Every remaining host may simply be leased by a peer. Discovery
			// only runs before the workers start, so a drained frontier stays
			// drained and one backoff is enough to tell the two apart.
			if !sleepContext(ctx, emptyLeaseBackoff) {
				return
			}
			retried, retriedOK, retryErr := r.store.Lease(ctx, time.Now())
			if retryErr != nil {
				r.fail(retryErr)
				return
			}
			if !retriedOK {
				return
			}
			item = retried
		}
		r.process(ctx, item)
	}
}

func (r *runState) process(ctx context.Context, item searchindex.FrontierItem) {
	if r.hostFull(item.Root) {
		_ = r.store.Complete(ctx, item.URL)
		return
	}
	policy, blocked := r.robotsFor(ctx, item.URL)
	if blocked || !policy.Allowed(r.fetcher.UserAgent(), item.URL) {
		r.countRobotsDeny()
		_ = r.store.Complete(ctx, item.URL)
		return
	}
	if !r.waitForHost(ctx, item.Root, policy.CrawlDelay(r.fetcher.UserAgent())) {
		_ = r.store.Fail(ctx, item.URL, r.cfg.MaxAttempts)
		return
	}

	page, fetchErr := indexOne(ctx, r.fetcher, r.pipeline, item, r.cfg.AllowPrivate)
	switch {
	case errors.Is(fetchErr, ErrNotModified):
		r.countSkipped()
		_ = r.store.Complete(ctx, item.URL)
		return
	case fetchErr != nil:
		r.countFailed()
		_ = r.store.Fail(ctx, item.URL, r.cfg.MaxAttempts)
		return
	}
	_ = r.store.Complete(ctx, item.URL)
	if r.append(item.Root, page) {
		if err := r.flush(ctx); err != nil {
			r.fail(err)
		}
	}
}

// robotsFor fetches and caches one host's rules. A missing robots.txt allows
// everything, but a transport failure or a server error blocks the host: a
// crawler that treats an unreadable robots.txt as consent is how you get banned.
func (r *runState) robotsFor(ctx context.Context, pageURL string) (Robots, bool) {
	parsed, err := url.Parse(pageURL)
	if err != nil {
		return Robots{}, true
	}
	host := strings.ToLower(parsed.Hostname())
	r.mu.Lock()
	entry, cached := r.robots[host]
	r.mu.Unlock()
	if cached {
		return entry.policy, entry.blocked
	}

	fetched, fetchErr := r.fetcher.Get(ctx, parsed.Scheme+"://"+parsed.Host+"/robots.txt", "", "")
	entry = robotsEntry{}
	switch {
	case fetchErr == nil:
		entry.policy = ParseRobots(strings.NewReader(string(fetched.Body)))
	case fetched.Status == http.StatusNotFound || fetched.Status == http.StatusGone:
		// No robots.txt is an explicit "no rules", not an unknown.
	default:
		entry.blocked = true
	}
	r.mu.Lock()
	r.robots[host] = entry
	r.mu.Unlock()
	return entry.policy, entry.blocked
}

// waitForHost spaces consecutive fetches of one host. It reports false when the
// context ended during the wait.
func (r *runState) waitForHost(ctx context.Context, root string, delay time.Duration) bool {
	r.mu.Lock()
	wait := time.Duration(0)
	if last := r.lastFetch[root]; !last.IsZero() {
		wait = delay - time.Since(last)
	}
	r.lastFetch[root] = time.Now().Add(max(wait, 0))
	r.mu.Unlock()
	if wait <= 0 {
		return true
	}
	return sleepContext(ctx, wait)
}

func (r *runState) hostFull(root string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hostCount[root] >= r.cfg.MaxPerHost
}

// append records one fetched page and reports whether the batch should flush.
func (r *runState) append(root string, page searchindex.Page) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hostCount[root]++
	r.batch = append(r.batch, page)
	return len(r.batch) >= r.cfg.BatchSize || r.stats.Indexed+len(r.batch) >= r.cfg.MaxPages
}

func (r *runState) flush(ctx context.Context) error {
	r.mu.Lock()
	if len(r.batch) == 0 {
		r.mu.Unlock()
		return nil
	}
	pending := r.batch
	r.batch = nil
	r.mu.Unlock()

	written, err := r.store.UpsertMany(ctx, pending)
	r.mu.Lock()
	r.stats.Indexed += written
	r.mu.Unlock()
	return err
}

func (r *runState) done() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped || r.stats.Indexed+len(r.batch) >= r.cfg.MaxPages
}

func (r *runState) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = true
	if r.err == nil {
		r.err = err
	}
}

func (r *runState) failure() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *runState) snapshot() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats
}

func (r *runState) countRobotsDeny() { r.mu.Lock(); r.stats.RobotsDeny++; r.mu.Unlock() }
func (r *runState) countFailed()     { r.mu.Lock(); r.stats.Failed++; r.mu.Unlock() }
func (r *runState) countSkipped()    { r.mu.Lock(); r.stats.Skipped304++; r.mu.Unlock() }

func sleepContext(ctx context.Context, wait time.Duration) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func indexOne(ctx context.Context, fetcher *Fetcher, pipeline *cleaner.Cleaner, item searchindex.FrontierItem, allowPrivate bool) (searchindex.Page, error) {
	fetched, err := fetcher.Get(ctx, item.URL, "", "")
	if err != nil {
		return searchindex.Page{}, err
	}
	cleaned, cleanErr := pipeline.Clean(string(fetched.Body), fetched.URL, "text", "auto")
	title, body := "", ""
	if cleanErr == nil && cleaned != nil {
		title = cleaned.Metadata.Title
		body = cleaned.Content
	}
	// Falling back to the raw response would index markup, script, and style
	// tokens, so an unclean page carries its title only and is left for the
	// render pass instead.
	canonical, root, err := pageRoot(fetched.URL, allowPrivate)
	if err != nil {
		canonical, root = item.URL, item.Root
	}
	lang := searchindex.DetectLanguage(title + " " + body)
	return searchindex.Page{
		URL: canonical, Root: root, Title: title, Body: body, Lang: lang,
		FetchedAt: fetched.FetchedAt, ETag: fetched.ETag, LastMod: fetched.LastMod,
		NeedsRender: NeedsRender(body),
	}, nil
}
