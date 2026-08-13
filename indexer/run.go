package indexer

import (
	"context"
	"errors"
	"fmt"
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

	discovered, err := SeedFrontier(ctx, store, discoverer, seeds, cfg.AllowPrivate)
	if err != nil {
		return Stats{}, err
	}
	stats := Stats{Discovered: discovered}
	pipeline := cleaner.NewCleaner()
	hostCount := map[string]int{}
	lastFetch := map[string]time.Time{}
	robotsCache := map[string]Robots{}
	var mu sync.Mutex
	var batch []searchindex.Page
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		n, upsertErr := store.UpsertMany(ctx, batch)
		batch = batch[:0]
		if upsertErr != nil {
			return upsertErr
		}
		stats.Indexed += n
		return nil
	}

	for stats.Indexed < cfg.MaxPages {
		if err := ctx.Err(); err != nil {
			_ = flush()
			return stats, err
		}
		item, ok, leaseErr := store.Lease(ctx, time.Now())
		if leaseErr != nil {
			_ = flush()
			return stats, leaseErr
		}
		if !ok {
			break
		}

		mu.Lock()
		if hostCount[item.Root] >= cfg.MaxPerHost {
			mu.Unlock()
			_ = store.Complete(ctx, item.URL)
			continue
		}
		policy := robotsFor(ctx, fetcher, robotsCache, item.URL)
		if !policy.Allowed(fetcher.UserAgent(), item.URL) {
			stats.RobotsDeny++
			mu.Unlock()
			_ = store.Complete(ctx, item.URL)
			continue
		}
		delay := policy.CrawlDelay(fetcher.UserAgent())
		if last := lastFetch[item.Root]; !last.IsZero() {
			if wait := delay - time.Since(last); wait > 0 {
				mu.Unlock()
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					_ = store.Fail(ctx, item.URL, cfg.MaxAttempts)
					_ = flush()
					return stats, ctx.Err()
				case <-timer.C:
				}
				mu.Lock()
			}
		}
		lastFetch[item.Root] = time.Now()
		mu.Unlock()

		page, fetchErr := indexOne(ctx, fetcher, pipeline, item, cfg.AllowPrivate)
		if errors.Is(fetchErr, ErrNotModified) {
			stats.Skipped304++
			_ = store.Complete(ctx, item.URL)
			continue
		}
		if fetchErr != nil {
			stats.Failed++
			_ = store.Fail(ctx, item.URL, cfg.MaxAttempts)
			continue
		}
		mu.Lock()
		hostCount[item.Root]++
		batch = append(batch, page)
		full := len(batch) >= cfg.BatchSize || stats.Indexed+len(batch) >= cfg.MaxPages
		mu.Unlock()
		_ = store.Complete(ctx, item.URL)
		if full {
			if err := flush(); err != nil {
				return stats, err
			}
		}
	}
	if err := flush(); err != nil {
		return stats, err
	}
	return stats, nil
}

func robotsFor(ctx context.Context, fetcher *Fetcher, cache map[string]Robots, pageURL string) Robots {
	parsed, err := url.Parse(pageURL)
	if err != nil {
		return Robots{}
	}
	host := strings.ToLower(parsed.Hostname())
	if robots, ok := cache[host]; ok {
		return robots
	}
	robotsURL := parsed.Scheme + "://" + parsed.Host + "/robots.txt"
	fetched, fetchErr := fetcher.Get(ctx, robotsURL, "", "")
	robots := Robots{}
	if fetchErr == nil {
		robots = ParseRobots(strings.NewReader(string(fetched.Body)))
	}
	cache[host] = robots
	return robots
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
	if body == "" {
		body = string(fetched.Body)
	}
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
