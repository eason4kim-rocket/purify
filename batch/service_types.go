// Package batch owns the transport-neutral lifecycle of asynchronous batch
// scrape jobs. HTTP adapters should delegate here instead of starting their
// own goroutines or maintaining a second scrape pipeline.
package batch

import (
	"context"
	"time"

	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/scrape"
	"github.com/use-agent/purify/webhook"
)

// Runner is the canonical scrape operation used for every URL in a batch.
// *scrape.Service satisfies this interface.
type Runner interface {
	Run(context.Context, *models.ScrapeRequest, scrape.Observer) (*scrape.Result, error)
}

// IDGenerator returns a unique batch identifier.
type IDGenerator func() (string, error)

// CompletionNotifier receives a detached terminal snapshot. Implementations
// may retain or mutate the event without changing state returned by Get.
type CompletionNotifier interface {
	Notify(url, secret string, event *webhook.Event)
}

// CompletionNotifierFunc adapts a function to CompletionNotifier.
type CompletionNotifierFunc func(url, secret string, event *webhook.Event)

// Notify implements CompletionNotifier.
func (notify CompletionNotifierFunc) Notify(url, secret string, event *webhook.Event) {
	if notify != nil {
		notify(url, secret, event)
	}
}

// Config bounds retained jobs and controls deterministic lifecycle behavior.
type Config struct {
	Capacity      int
	TTL           time.Duration
	SweepInterval time.Duration
	IDGenerator   IDGenerator
	Notifier      CompletionNotifier
	Now           func() time.Time
}
