// Package crawl owns the transport-neutral lifecycle and deterministic
// execution of asynchronous crawl jobs.
package crawl

import (
	"context"
	"time"

	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/scrape"
	"github.com/use-agent/purify/webhook"
)

// Runner is the canonical scrape operation used for every crawled page.
// *scrape.Service satisfies this interface.
type Runner interface {
	Run(context.Context, *models.ScrapeRequest, scrape.Observer) (*scrape.Result, error)
}

// IDGenerator returns a unique crawl identifier.
type IDGenerator func() (string, error)

// Notifier receives detached page and terminal events. Implementations may
// retain or mutate an event without changing state subsequently returned by
// Get.
type Notifier interface {
	Notify(url, secret string, event *webhook.Event)
}

// NotifierFunc adapts a function to Notifier.
type NotifierFunc func(url, secret string, event *webhook.Event)

// Notify implements Notifier.
func (notify NotifierFunc) Notify(url, secret string, event *webhook.Event) {
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
	Notifier      Notifier
	Now           func() time.Time
}
