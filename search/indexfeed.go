package search

import (
	"context"
	"strings"
	"sync"

	"golang.org/x/net/publicsuffix"

	"github.com/use-agent/purify/extract"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/publicnet"
	"github.com/use-agent/purify/searchindex"
)

const (
	// indexFeedQueueDepth bounds the pages waiting to be written. A full queue
	// drops the page rather than slowing the request that produced it.
	indexFeedQueueDepth = 256
	indexFeedBatchSize  = 16
	// indexFeedMinBodyBytes skips error pages and near-empty shells.
	indexFeedMinBodyBytes = 200
)

// IndexFeeder wraps an ArtifactService so pages fetched while serving a request
// also land in the local index. Enrichment already fetches and cleans these
// pages, so the index grows for free, and it grows where callers actually look
// instead of where a crawl guessed.
//
// Feeding is off unless the operator enables it: the URLs a caller asked about
// become searchable content, which is their decision to make, not ours.
type IndexFeeder struct {
	inner ArtifactService
	store *searchindex.Store

	queue    chan searchindex.Page
	closeNow sync.Once
	done     chan struct{}
}

var _ ArtifactService = (*IndexFeeder)(nil)

// NewIndexFeeder returns inner unchanged when either dependency is missing, so
// callers can wire it unconditionally.
func NewIndexFeeder(inner ArtifactService, store *searchindex.Store) ArtifactService {
	if isNilSearchDependency(inner) || store == nil {
		return inner
	}
	feeder := &IndexFeeder{
		inner: inner,
		store: store,
		queue: make(chan searchindex.Page, indexFeedQueueDepth),
		done:  make(chan struct{}),
	}
	go feeder.drain()
	return feeder
}

// Close stops the writer and waits for the queued pages already accepted.
func (feeder *IndexFeeder) Close() error {
	if feeder == nil {
		return nil
	}
	feeder.closeNow.Do(func() { close(feeder.queue) })
	<-feeder.done
	return nil
}

func (feeder *IndexFeeder) FetchPublicArtifact(ctx context.Context, rawURL string) (*extract.Artifact, error) {
	artifact, err := feeder.inner.FetchPublicArtifact(ctx, rawURL)
	if err == nil {
		feeder.offer(artifact)
	}
	return artifact, err
}

func (feeder *IndexFeeder) ExtractArtifact(ctx context.Context, artifact *extract.Artifact, request *models.ExtractRequest) (*models.ExtractResponse, error) {
	return feeder.inner.ExtractArtifact(ctx, artifact, request)
}

// offer never blocks and never reports failure: indexing is a side effect of
// serving a request and must not change its outcome.
func (feeder *IndexFeeder) offer(artifact *extract.Artifact) {
	page, ok := pageFromArtifact(artifact)
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

func (feeder *IndexFeeder) drain() {
	defer close(feeder.done)
	batch := make([]searchindex.Page, 0, indexFeedBatchSize)
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
	for page := range feeder.queue {
		batch = append(batch, page)
		if len(batch) >= indexFeedBatchSize {
			flush()
		}
	}
	flush()
}

// pageFromArtifact keeps only public, successful, substantive pages.
func pageFromArtifact(artifact *extract.Artifact) (searchindex.Page, bool) {
	if artifact == nil || artifact.Public == nil || artifact.Source == nil {
		return searchindex.Page{}, false
	}
	public := artifact.Public
	if public.StatusCode < 200 || public.StatusCode >= 300 {
		return searchindex.Page{}, false
	}
	body := strings.TrimSpace(public.Content)
	if len(body) < indexFeedMinBodyBytes {
		return searchindex.Page{}, false
	}
	canonical, root, ok := canonicalPublicRoot(public.FinalURL)
	if !ok {
		return searchindex.Page{}, false
	}
	return searchindex.Page{
		URL:       canonical,
		Root:      root,
		Title:     strings.TrimSpace(public.Metadata.Title),
		Body:      body,
		Lang:      searchindex.DetectLanguage(public.Metadata.Title + " " + body),
		FetchedAt: artifact.Source.FetchedAt,
	}, true
}

// canonicalPublicRoot rejects anything the public search path would refuse to
// serve, so a private-network fetch can never enter the index.
func canonicalPublicRoot(rawURL string) (string, string, bool) {
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
