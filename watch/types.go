// Package watch persists long-lived fact specifications and owns their
// scheduler leases. Verification remains the authority that changes facts.
package watch

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/models"
)

const (
	DefaultListLimit     = 50
	MaxListLimit         = 100
	MaxLiveWatches       = 10_000
	DefaultLeaseDuration = 3 * time.Minute
)

var (
	ErrInvalidStore     = errors.New("watch: invalid store")
	ErrInvalidWatchSpec = errors.New("watch: invalid fact specification")
	ErrInvalidWatchID   = errors.New("watch: invalid watch ID")
	ErrWatchNotFound    = errors.New("watch: watch not found")
	ErrWatchLimit       = errors.New("watch: live watch limit reached")
	ErrInvalidCursor    = errors.New("watch: invalid list cursor")
	ErrInvalidLease     = errors.New("watch: invalid lease")
	ErrCorruptStore     = errors.New("watch: corrupt durable state")
)

// State is the durable lifecycle of a watch. Deleted watches are retained for
// audit history but are deliberately excluded from Get and List.
type State string

const (
	StatePending State = "pending"
	StateActive  State = "active"
	StatePaused  State = "paused"
	StateDeleted State = "deleted"
)

// Watch is the public, credential-free view of one durable specification.
// Lease capabilities are intentionally absent.
type Watch struct {
	ID                         string
	Spec                       models.FactSpec
	State                      State
	NextCheckAt                *time.Time
	EWMAInterval               time.Duration
	LastChangeAt               *time.Time
	LastCheckedAt              *time.Time
	ConsecutiveFailures        int
	LastErrorCode              string
	LastVerificationID         string
	LastVerificationClaimIndex *int
	CreatedAt                  time.Time
	UpdatedAt                  time.Time
	PausedAt                   *time.Time
}

// WatchCursor is the stable keyset cursor for the live-watch creation order.
type WatchCursor struct {
	CreatedAt time.Time
	ID        string
}

// WatchListOptions bounds one live-watch page. A zero Limit selects the
// default. Cursor is copied before the database operation starts.
type WatchListOptions struct {
	Cursor *WatchCursor
	Limit  int
}

// WatchPage is always returned with a non-nil Items slice.
type WatchPage struct {
	Items []Watch
	Next  *WatchCursor
}

// Lease is an unforgeable scheduler capability. Every field is excluded from
// JSON so embedding a Claim in a response cannot disclose it accidentally.
type Lease struct {
	WatchID string    `json:"-"`
	ID      string    `json:"-"`
	Until   time.Time `json:"-"`
}

// Fact is the complete durable audit row associated with a watch. Nullable
// database values use pointers where zero is otherwise meaningful.
type Fact struct {
	ID                    string
	WatchID               string
	Subject               string
	Predicate             string
	Path                  string
	Value                 json.RawMessage
	Root                  string
	SourceURL             string
	Receipt               string
	SnapshotID            string
	CreatedVerificationID string
	CreatedClaimIndex     int
	LatestVerificationID  string
	LatestClaimIndex      int
	ClosedVerificationID  string
	ClosedClaimIndex      *int
	ObservedAt            time.Time
	ValidFrom             time.Time
	LastVerifiedAt        time.Time
	ValidTo               *time.Time
	SupersededBy          string
	ClosedOutcome         ledger.Outcome
	GoneScope             ledger.GoneScope
}

// Claim is an atomic snapshot of one due watch, its optional open fact, and
// the exact lease capability that guards subsequent work.
type Claim struct {
	Watch Watch
	Fact  *Fact
	Lease Lease
}
