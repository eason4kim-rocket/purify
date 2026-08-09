package models

// ExtractorHealStatus is the immediate scheduling state returned by the
// protected extractor healing endpoint.
type ExtractorHealStatus string

const ExtractorHealStatusAccepted ExtractorHealStatus = "accepted"

// ExtractorHealResponse is a durable scheduling receipt. HealRunID, rather
// than an inferred compile key, is the identity clients may retain.
type ExtractorHealResponse struct {
	ExtractorID string              `json:"extractor_id"`
	HealRunID   string              `json:"heal_run_id"`
	Status      ExtractorHealStatus `json:"status"`
}

// ExtractorHealErrorResponse is the stable error envelope returned by the
// extractor healing endpoint.
type ExtractorHealErrorResponse struct {
	Error *ErrorDetail `json:"error"`
}
