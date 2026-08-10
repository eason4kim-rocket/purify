package models

import (
	"encoding/json"
	"time"
)

const (
	// MaxWatchRequestBytes is shared by every JSON Watch request. Bodyless
	// Watch actions are stricter and accept exactly zero bytes.
	MaxWatchRequestBytes = 1 << 20
	// MaxWatchResponseBytes bounds every successful Watch and Facts response.
	MaxWatchResponseBytes = 32 << 20

	ErrCodeWatchUnavailable  = "WATCH_UNAVAILABLE"
	ErrCodeWatchNotFound     = "WATCH_NOT_FOUND"
	ErrCodeWatchLimitReached = "WATCH_LIMIT_REACHED"
	ErrCodeFactUnavailable   = "FACT_UNAVAILABLE"
)

// CreateWatchRequest is the only body-bearing Watch CRUD request. The
// specification remains nested so the Answer and Watch APIs share one fact
// vocabulary without making transport-specific fields part of FactSpec.
type CreateWatchRequest struct {
	Spec FactSpec `json:"spec"`
}

// WatchView is the credential-free public projection of a durable watch.
// Scheduler lease capabilities and verification row identities deliberately
// have no representation here.
type WatchView struct {
	ID                  string     `json:"id"`
	Spec                FactSpec   `json:"spec"`
	State               string     `json:"state"`
	NextCheckAt         *time.Time `json:"next_check_at"`
	EWMAIntervalSeconds float64    `json:"ewma_interval_s"`
	LastChangeAt        *time.Time `json:"last_change_at"`
	LastCheckedAt       *time.Time `json:"last_checked_at"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	LastErrorCode       string     `json:"last_error_code,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
	PausedAt            *time.Time `json:"paused_at"`
}

// CreateWatchResponse distinguishes a new durable row from an exact
// idempotent retry. HTTP status communicates the same distinction while this
// field preserves it for clients behind status-normalizing gateways.
type CreateWatchResponse struct {
	Watch   WatchView `json:"watch"`
	Created bool      `json:"created"`
}

// WatchResponse is shared by Get, Pause, and Resume.
type WatchResponse struct {
	Watch WatchView `json:"watch"`
}

// WatchListResponse is a stable keyset page. Watches is always non-nil; an
// absent NextCursor means the page is terminal.
type WatchListResponse struct {
	Watches    []WatchView `json:"watches"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

// FactView is the public bitemporal fact projection. Verification database
// row identities are intentionally omitted; portable evidence is represented
// by SourceURL, SnapshotID, and Receipt.
type FactView struct {
	ID             string          `json:"id"`
	WatchID        string          `json:"watch_id"`
	Subject        string          `json:"subject"`
	Predicate      string          `json:"predicate"`
	Path           string          `json:"path"`
	Value          json.RawMessage `json:"value"`
	Root           string          `json:"root"`
	SourceURL      string          `json:"source_url"`
	Receipt        string          `json:"receipt"`
	SnapshotID     string          `json:"snapshot_id"`
	ObservedAt     time.Time       `json:"observed_at"`
	ValidFrom      time.Time       `json:"valid_from"`
	LastVerifiedAt time.Time       `json:"last_verified_at"`
	ValidTo        *time.Time      `json:"valid_to"`
	SupersededBy   string          `json:"superseded_by,omitempty"`
	ClosedOutcome  string          `json:"closed_outcome,omitempty"`
	GoneScope      string          `json:"gone_scope,omitempty"`
}

// FactLookupResponse returns a successful temporal gap as fact:null rather
// than turning the absence of history into a transport error.
type FactLookupResponse struct {
	Fact *FactView `json:"fact"`
	AsOf time.Time `json:"as_of"`
}

// WatchErrorResponse is the only non-2xx envelope used by Watch and Facts.
type WatchErrorResponse struct {
	Error *ErrorDetail `json:"error"`
}
