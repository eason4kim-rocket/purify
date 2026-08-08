package scrape

import "github.com/use-agent/purify/models"

// EventType is the stable event name emitted by Service.Run. HTTP SSE adapters
// forward these names without maintaining a second orchestration path.
type EventType string

const (
	EventStarted   EventType = "scrape.started"
	EventAttempt   EventType = "scrape.attempt"
	EventNavigated EventType = "scrape.navigated"
	EventCompleted EventType = "scrape.completed"
	EventError     EventType = "scrape.error"
)

// Navigation describes the selected fetch candidate.
type Navigation struct {
	StatusCode   int    `json:"status_code"`
	FinalURL     string `json:"final_url"`
	EngineUsed   string `json:"engine_used"`
	FetchMethod  string `json:"fetch_method"`
	NavigationMs int64  `json:"navigation_ms"`
}

// Event is transport-neutral. Response is populated for completed and error
// events so JSON and SSE can expose the same final response contract.
type Event struct {
	Type       EventType              `json:"type"`
	URL        string                 `json:"url,omitempty"`
	Attempt    *models.FetchAttempt   `json:"attempt,omitempty"`
	Navigation *Navigation            `json:"navigation,omitempty"`
	Response   *models.ScrapeResponse `json:"response,omitempty"`
}

// Observer receives synchronous progress events. Implementations must return
// quickly and must not mutate values referenced by the event.
type Observer func(Event)
