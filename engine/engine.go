package engine

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// ErrUnsupportedRequest marks a fetch request whose options cannot be honored
// by the selected engine set. Callers can use errors.Is to distinguish an
// explicit capability skip from a network or navigation failure.
var ErrUnsupportedRequest = errors.New("engine: unsupported request options")

// Engine is the interface that all fetch engines must implement.
type Engine interface {
	// Name returns the engine identifier (e.g. "http", "rod", "rod-stealth").
	Name() string

	// Supports reports whether this engine can honor every option in req.
	// Dispatchers must skip engines that return false instead of silently
	// dropping options they cannot implement.
	Supports(req *FetchRequest) bool

	// Fetch retrieves the page content for the given request.
	Fetch(ctx context.Context, req *FetchRequest) (*FetchResult, error)
}

// Action describes one browser interaction requested by the caller. It is
// deliberately defined in engine rather than importing the public API models,
// keeping the fetch layer independent from HTTP request types.
type Action struct {
	Type         string
	Selector     string
	Milliseconds int
	Direction    string
	Amount       int
	Code         string
}

// FetchRequest contains everything an engine needs to fetch a page.
type FetchRequest struct {
	URL                string
	Headers            map[string]string
	Cookies            []http.Cookie
	Timeout            time.Duration
	ProxyURL           string
	Stealth            bool
	WaitForNetworkIdle *bool
	RemoveOverlays     bool
	BlockAds           bool
	Actions            []Action
	CDPURL             string
}

// FetchResult is the output of a successful engine fetch.
type FetchResult struct {
	HTML        string
	Title       string
	StatusCode  int
	FinalURL    string
	EngineName  string
	ContentType string
}

// requestContext applies the request budget without extending an earlier
// caller deadline. A no-op cancel keeps call sites uniform when no timeout was
// requested.
func requestContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}
