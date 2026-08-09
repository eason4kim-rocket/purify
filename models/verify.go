package models

import (
	"encoding/json"
	"time"

	"github.com/use-agent/purify/evidence"
)

// VerifyStatus is the complete public verdict set for fact verification.
type VerifyStatus string

const (
	VerifyStatusConfirmed VerifyStatus = "confirmed"
	VerifyStatusChanged   VerifyStatus = "changed"
	VerifyStatusGone      VerifyStatus = "gone"
)

// VerifyGoneScope distinguishes a missing field from a definite missing page.
// It refines a gone verdict without adding a fourth verification state.
type VerifyGoneScope string

const (
	VerifyGoneScopeField VerifyGoneScope = "field"
	VerifyGoneScopePage  VerifyGoneScope = "page"
)

// Claim is a previously observed, evidence-backed JSON scalar. Anchor carries
// the complete old locator and snapshot identity needed for deterministic
// re-verification and DOM similarity measurement.
type Claim struct {
	Path   string          `json:"path"`
	Value  json.RawMessage `json:"value"`
	Anchor evidence.Anchor `json:"anchor"`
}

// VerifyRequest accepts either one or more explicit claims or one portable
// receipt. URL is required with Claims and restored from a verified receipt
// when Receipt is used. A URL supplied with Receipt is only an optional
// consistency check. The service enforces that exactly one input form is
// present. When WebhookURL is set, changed claims are queued transactionally
// with their ledger rows and delivered as one fact.changed event.
type VerifyRequest struct {
	URL           string  `json:"url,omitempty"`
	Claims        []Claim `json:"claims,omitempty"`
	Receipt       string  `json:"receipt,omitempty"`
	WebhookURL    string  `json:"webhook_url,omitempty" binding:"omitempty,url"`
	WebhookSecret string  `json:"webhook_secret,omitempty"`
}

// ClaimResult is the current verdict for one input claim. Confirmed and
// changed results carry refreshed evidence and a newly signed receipt. Gone
// results carry a scope and intentionally have neither.
type ClaimResult struct {
	Path      string           `json:"path"`
	Status    VerifyStatus     `json:"status"`
	GoneScope VerifyGoneScope  `json:"gone_scope,omitempty"`
	NewValue  json.RawMessage  `json:"new_value,omitempty"`
	Evidence  *evidence.Anchor `json:"evidence,omitempty"`
	Receipt   string           `json:"receipt,omitempty"`
}

// VerifyResponse is one atomic multi-claim observation. PageSimilarity is
// present only for a verified successful page body and is one minus the
// normalized 64-bit DOM SimHash distance from the old snapshot.
type VerifyResponse struct {
	VerificationID string        `json:"verification_id"`
	URL            string        `json:"url"`
	FinalURL       string        `json:"final_url"`
	StatusCode     int           `json:"status_code"`
	Results        []ClaimResult `json:"results"`
	PageSimilarity *float64      `json:"page_similarity,omitempty"`
	SnapshotID     string        `json:"snapshot_id,omitempty"`
	VerifiedAt     time.Time     `json:"verified_at"`
}
