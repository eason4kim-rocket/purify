package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ArchiveEngine is a last-resort fetch engine: when every live engine has
// failed, it serves the closest Wayback Machine snapshot so a blocked or
// offline origin can still yield content. It is deliberately not evidence: the
// result is labeled with the engine name "wayback-archive" and the verify path
// always revisits the live page, so an archived copy never stands in for a
// first-party observation.
type ArchiveEngine struct {
	client          *http.Client
	availabilityURL string
	snapshotBase    string
}

// ArchiveConfig configures the archive engine. The endpoints default to the
// public Wayback Machine and are overridable for tests.
type ArchiveConfig struct {
	Client               *http.Client
	AvailabilityEndpoint string // default: https://archive.org/wayback/available
	SnapshotBase         string // default: https://web.archive.org/web
}

const (
	defaultWaybackAvailability = "https://archive.org/wayback/available"
	defaultWaybackSnapshotBase = "https://web.archive.org/web"
	archiveUserAgent           = "PurifyBot/1.0 (+https://purify.ai/bot; archive fallback)"
	availabilityBodyLimit      = 64 << 10
)

// NewArchiveEngine builds an archive engine. A nil client gets a bounded
// default with a 20s timeout and a redirect cap.
func NewArchiveEngine(cfg ArchiveConfig) *ArchiveEngine {
	client := cfg.Client
	if client == nil {
		client = &http.Client{
			Timeout: 20 * time.Second,
			CheckRedirect: func(_ *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		}
	}
	availabilityURL := cfg.AvailabilityEndpoint
	if availabilityURL == "" {
		availabilityURL = defaultWaybackAvailability
	}
	snapshotBase := cfg.SnapshotBase
	if snapshotBase == "" {
		snapshotBase = defaultWaybackSnapshotBase
	}
	return &ArchiveEngine{
		client:          client,
		availabilityURL: availabilityURL,
		snapshotBase:    strings.TrimRight(snapshotBase, "/"),
	}
}

func (e *ArchiveEngine) Name() string { return "wayback-archive" }

// Supports admits only plain default-mode GETs. It never serves verify
// observations (they must be live) or browser-only options an archive cannot
// reproduce.
func (e *ArchiveEngine) Supports(req *FetchRequest) bool {
	return req != nil &&
		req.Mode == FetchModeDefault &&
		!req.Stealth &&
		!req.RemoveOverlays &&
		len(req.Actions) == 0 &&
		req.CDPURL == ""
}

func (e *ArchiveEngine) Fetch(ctx context.Context, req *FetchRequest) (*FetchResult, error) {
	if req == nil {
		return nil, fmt.Errorf("archive_engine: nil fetch request")
	}
	if !e.Supports(req) {
		return nil, fmt.Errorf("%w: archive engine cannot honor browser-only options", ErrUnsupportedRequest)
	}

	fetchCtx, cancel := requestContext(ctx, req.Timeout)
	defer cancel()

	maximumBodyBytes, err := responseBodyLimit(req.MaximumBodyBytes)
	if err != nil {
		return nil, err
	}

	timestamp, err := e.closestSnapshot(fetchCtx, req.URL)
	if err != nil {
		return nil, err
	}

	// The id_ suffix returns the archived resource verbatim, without the Wayback
	// toolbar or link rewriting, so the cleaner sees the original document.
	snapshotURL := fmt.Sprintf("%s/%sid_/%s", e.snapshotBase, timestamp, req.URL)
	httpReq, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, snapshotURL, nil)
	if err != nil {
		return nil, fmt.Errorf("archive_engine: build snapshot request: %w", err)
	}
	httpReq.Header.Set("User-Agent", archiveUserAgent)

	resp, err := e.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("archive_engine: fetch snapshot: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("archive_engine: snapshot status %d", resp.StatusCode)
	}

	body, err := readResponseBody(resp.Body, maximumBodyBytes)
	if err != nil {
		return nil, err
	}
	htmlBody := string(body)

	return &FetchResult{
		HTML:       htmlBody,
		Title:      extractTitle(htmlBody),
		StatusCode: http.StatusOK,
		// Present the original URL so cleaning and quality work normally; the
		// archive provenance travels in EngineName, which the index feed uses to
		// skip archived copies.
		FinalURL:    req.URL,
		EngineName:  e.Name(),
		ContentType: "text/html",
	}, nil
}

// closestSnapshot asks the availability API for the nearest usable capture and
// returns its timestamp, or an error when no HTTP-200 snapshot exists.
func (e *ArchiveEngine) closestSnapshot(ctx context.Context, target string) (string, error) {
	endpoint := e.availabilityURL + "?url=" + url.QueryEscape(target)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("archive_engine: build availability request: %w", err)
	}
	httpReq.Header.Set("User-Agent", archiveUserAgent)

	resp, err := e.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("archive_engine: availability request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("archive_engine: availability status %d", resp.StatusCode)
	}

	var payload struct {
		ArchivedSnapshots struct {
			Closest struct {
				Available bool   `json:"available"`
				Status    string `json:"status"`
				Timestamp string `json:"timestamp"`
			} `json:"closest"`
		} `json:"archived_snapshots"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, availabilityBodyLimit)).Decode(&payload); err != nil {
		return "", fmt.Errorf("archive_engine: decode availability: %w", err)
	}

	closest := payload.ArchivedSnapshots.Closest
	if !closest.Available || closest.Status != "200" || closest.Timestamp == "" {
		return "", fmt.Errorf("archive_engine: no usable snapshot for %s", target)
	}
	return closest.Timestamp, nil
}
