// Package indexfeed grows the local search index from pages the process
// already fetched while serving a request.
//
// It decorates the canonical scrape runner rather than any one route, because
// that is the single point every fetch passes through: /scrape, /crawl, /batch,
// /extract, and Search enrichment all reach the network here. Decorating a
// route instead would only ever re-index pages the index already returned.
package indexfeed

import (
	"context"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"

	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/publicnet"
	"github.com/use-agent/purify/scrape"
	"github.com/use-agent/purify/searchindex"
)

const (
	// queueDepth bounds the pages waiting to be written. A full queue drops the
	// page rather than slowing the request that produced it.
	queueDepth = 256
	batchSize  = 16
	// minBodyBytes skips error pages and near-empty shells.
	minBodyBytes = 200
	// flushInterval bounds how long a fetched page waits for a full batch.
	flushInterval = 5 * time.Second
	// archiveEngineName marks pages recovered from the Wayback fallback, which
	// must never enter the index. It mirrors engine.ArchiveEngine.Name().
	archiveEngineName = "wayback-archive"
)

// Runner is the canonical scrape boundary, matching extract.Runner and
// handler.ScrapeRunner.
type Runner interface {
	Run(context.Context, *models.ScrapeRequest, scrape.Observer) (*scrape.Result, error)
}

// Feeder runs pages through to the index after the caller has been served.
//
// The indexed pages are public web content owned by their publisher, not caller
// data, and a private address can never reach the index because publicPage
// rejects anything the public search path would refuse. What the index does
// expose is the URL set: one filling with a single company's pages shows that
// somebody is researching it.
type Feeder struct {
	inner Runner
	store *searchindex.Store

	queue     chan searchindex.Page
	closeOnce sync.Once
	done      chan struct{}
	// flushEvery is fixed at construction so the writer goroutine never races a
	// caller mutating it.
	flushEvery time.Duration
}

var _ Runner = (*Feeder)(nil)

// New returns inner unchanged when either dependency is missing, so callers can
// wire it unconditionally.
func New(inner Runner, store *searchindex.Store) Runner {
	if inner == nil || store == nil {
		return inner
	}
	return newFeeder(inner, store, flushInterval)
}

func newFeeder(inner Runner, store *searchindex.Store, flushEvery time.Duration) *Feeder {
	if flushEvery <= 0 {
		flushEvery = flushInterval
	}
	feeder := &Feeder{
		inner:      inner,
		store:      store,
		queue:      make(chan searchindex.Page, queueDepth),
		done:       make(chan struct{}),
		flushEvery: flushEvery,
	}
	go feeder.drain()
	return feeder
}

// Close stops the writer and waits for the pages already accepted.
func (feeder *Feeder) Close() error {
	if feeder == nil {
		return nil
	}
	feeder.closeOnce.Do(func() { close(feeder.queue) })
	<-feeder.done
	return nil
}

func (feeder *Feeder) Run(ctx context.Context, request *models.ScrapeRequest, observer scrape.Observer) (*scrape.Result, error) {
	result, err := feeder.inner.Run(ctx, request, observer)
	if err == nil {
		feeder.offer(result)
	}
	return result, err
}

// offer never blocks and never reports failure: indexing is a side effect of
// serving a request and must not change its outcome.
func (feeder *Feeder) offer(result *scrape.Result) {
	page, ok := publicPage(result)
	if !ok {
		return
	}
	defer func() {
		// A concurrent Close may have already closed the queue.
		_ = recover()
	}()
	select {
	case feeder.queue <- page:
	default:
	}
}

func (feeder *Feeder) drain() {
	defer close(feeder.done)
	batch := make([]searchindex.Page, 0, batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		// The request that produced these pages is long gone, so this uses a
		// background context and discards write errors: a failed index write is
		// lost coverage, not a lost response.
		_, _ = feeder.store.UpsertMany(context.Background(), batch)
		batch = batch[:0]
	}
	// Batching on count alone would strand pages until traffic happened to fill
	// a batch, which is exactly the early low-traffic case, and lose them on a
	// crash. The ticker bounds how long a fetched page waits.
	ticker := time.NewTicker(feeder.flushEvery)
	defer ticker.Stop()
	for {
		select {
		case page, open := <-feeder.queue:
			if !open {
				flush()
				return
			}
			batch = append(batch, page)
			if len(batch) >= batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// publicPage keeps only public, successful, substantive pages. A cache hit is
// still admitted: the index may not have the page even when the response cache
// does.
func publicPage(result *scrape.Result) (searchindex.Page, bool) {
	if result == nil || result.Response == nil || result.Source == nil {
		return searchindex.Page{}, false
	}
	response := result.Response
	if !response.Success || response.StatusCode < 200 || response.StatusCode >= 300 {
		return searchindex.Page{}, false
	}
	// Archive fallbacks recover content for the caller but must not enter the
	// index: an archived copy under the live URL would masquerade as a current
	// capture. The engine name is the provenance signal.
	if response.EngineUsed == archiveEngineName {
		return searchindex.Page{}, false
	}
	body := strings.TrimSpace(response.Content)
	if len(body) < minBodyBytes {
		return searchindex.Page{}, false
	}
	canonical, root, ok := publicRoot(response.FinalURL)
	if !ok {
		return searchindex.Page{}, false
	}
	title := strings.TrimSpace(response.Metadata.Title)
	return searchindex.Page{
		URL:       canonical,
		Root:      root,
		Title:     title,
		Body:      body,
		Lang:      searchindex.DetectLanguage(title + " " + body),
		FetchedAt: result.Source.FetchedAt,
	}, true
}

func publicRoot(rawURL string) (string, string, bool) {
	canonical, parsed, err := publicnet.NormalizeHTTPURL(rawURL, nil, false)
	if err != nil {
		return "", "", false
	}
	host := strings.ToLower(parsed.Hostname())
	root, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		root = host
	}
	if canonical == "" || root == "" {
		return "", "", false
	}
	return canonical, strings.ToLower(root), true
}
