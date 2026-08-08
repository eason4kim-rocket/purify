package models

// ErrCodeContentUnusable is returned when every available fetch candidate is
// rejected by the content-quality gate. It lives with the additive quality
// contract so existing error-model files do not need to change.
const ErrCodeContentUnusable = "CONTENT_UNUSABLE"

// QualityStatus is the public quality classification for a cleaned page.
type QualityStatus string

const (
	// QualityStatusGood means the candidate meets the preferred quality bar.
	QualityStatusGood QualityStatus = "good"

	// QualityStatusDegraded means the candidate is usable but should carry a
	// warning because it did not meet the preferred quality bar.
	QualityStatusDegraded QualityStatus = "degraded"

	// QualityStatusUnusable means the candidate must not be returned as a
	// successful scrape result.
	QualityStatusUnusable QualityStatus = "unusable"
)

// QualityReason is a stable, machine-readable reason used by quality warnings
// and rejected fetch attempts.
type QualityReason string

const (
	QualityReasonMissingBody       QualityReason = "missing_body"
	QualityReasonEmptyContent      QualityReason = "empty_content"
	QualityReasonLoadingPage       QualityReason = "loading_page"
	QualityReasonErrorPage         QualityReason = "error_page"
	QualityReasonChallengePage     QualityReason = "challenge_page"
	QualityReasonLowContentQuality QualityReason = "low_content_quality"
)

// FetchAttemptOutcome describes how an ordered fetch candidate was handled.
type FetchAttemptOutcome string

const (
	FetchAttemptSelected FetchAttemptOutcome = "selected"
	FetchAttemptRejected FetchAttemptOutcome = "rejected"
	FetchAttemptFailed   FetchAttemptOutcome = "failed"
)

// FetchAttempt records one engine invocation in an ordered fetch sequence.
// Reason is normally set for rejected or failed attempts and omitted for the
// selected candidate.
type FetchAttempt struct {
	Engine     string              `json:"engine"`
	Outcome    FetchAttemptOutcome `json:"outcome"`
	Reason     QualityReason       `json:"reason,omitempty"`
	DurationMs int64               `json:"duration_ms"`
}

// QualityInfo is the additive public quality block returned with successful
// scrape results. Warnings is intentionally not omitempty so a good result is
// encoded as an explicit empty array rather than null or an absent field.
type QualityInfo struct {
	Score           float64         `json:"score"`
	Status          QualityStatus   `json:"status"`
	Warnings        []QualityReason `json:"warnings"`
	ExtractModeUsed string          `json:"extract_mode_used,omitempty"`
	FetchAttempts   []FetchAttempt  `json:"fetch_attempts,omitempty"`
}
