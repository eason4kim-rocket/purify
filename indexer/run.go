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

	// PriorityURLs are exact operator-selected pages to lease before the
	// ordinary frontier. They still obey root concurrency, robots, crawl delay,
	// retry, content, and per-host limits.
	PriorityURLs []string

	// MaxFrontierPerHost caps a root's total frontier rows across runs so
	// link discovery cannot let one site flood the queue. Zero derives a
	// default from MaxPerHost; frontier rows outlive a single run's page
	// budget, so the default leaves room beyond MaxPerHost.
	MaxFrontierPerHost int

	// ReopenStarvedBelow reopens the closed rows of roots holding fewer
	// frontier rows than this before the crawl starts. Indexes built before
	// in-crawl link discovery never mined their fetched pages, so those
	// roots cannot grow until their pages are refetched. Zero disables it.
	ReopenStarvedBelow int

	// PruneFrontier drops pending rows the current admission rules reject
	// before the crawl starts, so a queue built under older, leakier rules
	// stops spending fetch budget on known junk.
	PruneFrontier bool
}

// Stats is a coarse run summary.
type Stats struct {
	Discovered     int
	Indexed        int
	Failed         int
	RobotsDeny     int
	Skipped304     int
	SkippedContent int
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
	if cfg.MaxFrontierPerHost <= 0 {
		cfg.MaxFrontierPerHost = 2 * cfg.MaxPerHost
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
	if cfg.ReopenStarvedBelow > 0 {
		if _, err := store.RequeueStarvedRoots(ctx, cfg.ReopenStarvedBelow); err != nil {
			return Stats{}, err
		}
	}
	// Pruning runs last so seed discovery and reopened rows are held to the
	// same admission rules as everything already queued.
	if cfg.PruneFrontier {
		if _, err := store.PrunePending(ctx, junkURL); err != nil {
			return Stats{}, err
		}
	}
	priorityURLs, priorityDiscovered, err := PreparePriorityFrontier(ctx, store, cfg.PriorityURLs, cfg.AllowPrivate)
	if err != nil {
		return Stats{}, err
	}
	run := &runState{
		store:        store,
		fetcher:      fetcher,
		cfg:          cfg,
		pipeline:     cleaner.NewCleaner(),
		priorityURLs: priorityURLs,
		stats:        Stats{Discovered: discovered + priorityDiscovered},
		hostCount:    map[string]int{},
		lastFetch:    map[string]time.Time{},
		robots:       map[string]robotsEntry{},
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
	store        *searchindex.Store
	fetcher      *Fetcher
	cfg          RunConfig
	pipeline     *cleaner.Cleaner
	priorityURLs []string

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
		item, ok, leaseErr := r.lease(ctx, time.Now())
		if leaseErr != nil {
			r.fail(leaseErr)
			return
		}
		if !ok {
			// An empty lease is not the end of the crawl: every leasable root
			// may be held by a peer, and a peer's in-flight page can enqueue
			// links that refill the frontier. Only a frontier with neither
			// pending nor leased rows is truly drained.
			active, activeErr := r.store.ActiveFrontier(ctx)
			if activeErr != nil {
				r.fail(activeErr)
				return
			}
			if active == 0 {
				return
			}
			if !sleepContext(ctx, emptyLeaseBackoff) {
				return
			}
			continue
		}
		r.process(ctx, item)
	}
}

func (r *runState) lease(ctx context.Context, now time.Time) (searchindex.FrontierItem, bool, error) {
	if len(r.priorityURLs) > 0 {
		item, ok, err := r.store.LeasePreferred(ctx, r.priorityURLs, now)
		if err != nil || ok {
			return item, ok, err
		}
	}
	return r.store.Lease(ctx, now)
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

	page, links, fetchErr := indexOne(ctx, r.fetcher, r.pipeline, item, r.cfg.AllowPrivate)
	switch {
	case errors.Is(fetchErr, ErrNotModified):
		r.countSkipped()
		_ = r.store.Complete(ctx, item.URL)
		return
	case errors.Is(fetchErr, ErrUnindexable):
		// The URL is spent, not broken: retrying a package blob or media
		// file would return the same bytes.
		r.countSkippedContent()
		_ = r.store.Complete(ctx, item.URL)
		return
	case fetchErr != nil:
		r.countFailed()
		_ = r.store.Fail(ctx, item.URL, r.cfg.MaxAttempts)
		return
	}
	_ = r.store.Complete(ctx, item.URL)
	if len(links) > 0 {
		grown, growErr := r.store.EnqueueBounded(ctx, links, r.cfg.MaxFrontierPerHost)
		if growErr != nil {
			r.fail(growErr)
			return
		}
		r.countDiscovered(grown)
	}
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

func (r *runState) countRobotsDeny()      { r.mu.Lock(); r.stats.RobotsDeny++; r.mu.Unlock() }
func (r *runState) countFailed()          { r.mu.Lock(); r.stats.Failed++; r.mu.Unlock() }
func (r *runState) countSkipped()         { r.mu.Lock(); r.stats.Skipped304++; r.mu.Unlock() }
func (r *runState) countSkippedContent()  { r.mu.Lock(); r.stats.SkippedContent++; r.mu.Unlock() }
func (r *runState) countDiscovered(n int) { r.mu.Lock(); r.stats.Discovered += n; r.mu.Unlock() }

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

func indexOne(ctx context.Context, fetcher *Fetcher, pipeline *cleaner.Cleaner, item searchindex.FrontierItem, allowPrivate bool) (searchindex.Page, []searchindex.FrontierItem, error) {
	fetched, err := fetcher.Get(ctx, item.URL, "", "")
	if err != nil {
		return searchindex.Page{}, nil, err
	}
	if !indexableContentType(fetched.ContentType, fetched.Body) {
		return searchindex.Page{}, nil, ErrUnindexable
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
	// Links come from the raw markup: a page too script-heavy to clean still
	// names its neighbors, and those anchors are how sitemap-less sites get
	// any coverage past their front page.
	links := collectLinks(string(fetched.Body), fetched.URL, root, allowPrivate)
	lang := searchindex.DetectLanguage(title + " " + body)
	return searchindex.Page{
		URL: canonical, Root: root, Title: title, Body: body, Lang: lang,
		FetchedAt: fetched.FetchedAt, ETag: fetched.ETag, LastMod: fetched.LastMod,
		NeedsRender: NeedsRender(body),
	}, links, nil
}
