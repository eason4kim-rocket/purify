package engine

import (
	"context"
	"fmt"
	"net/http"
)

// RodFetchFunc is the callback type that wraps the existing scraper.DoScrape logic.
// It is injected from main.go to avoid a circular import (engine/ -> scraper/).
type RodFetchFunc func(ctx context.Context, req *FetchRequest) (*FetchResult, error)

// RodEngine is a browser-based engine that delegates to the existing rod scraper
// logic via a callback function. The forceStealth flag distinguishes between
// Layer 3 (rod) and Layer 4 (rod-stealth).
type RodEngine struct {
	fetchFunc    RodFetchFunc
	forceStealth bool
	name         string
}

// NewRodEngine creates a RodEngine.
//   - fetchFunc: callback that invokes the rod-based scraper (injected from main.go).
//   - forceStealth: when true, the engine always sets Stealth=true on requests.
func NewRodEngine(fetchFunc RodFetchFunc, forceStealth bool) *RodEngine {
	name := "rod"
	if forceStealth {
		name = "rod-stealth"
	}
	return &RodEngine{
		fetchFunc:    fetchFunc,
		forceStealth: forceStealth,
		name:         name,
	}
}

func (e *RodEngine) Name() string { return e.name }

// Supports reports whether this Rod tier can honor every option in the
// request. Explicit stealth requests skip the ordinary Rod tier so the result
// is truthfully attributed to rod-stealth; both Rod tiers remain eligible for
// ordinary staged escalation.
func (e *RodEngine) Supports(req *FetchRequest) bool {
	return req != nil && (!req.Stealth || e.forceStealth)
}

func (e *RodEngine) Fetch(ctx context.Context, req *FetchRequest) (*FetchResult, error) {
	if e.fetchFunc == nil {
		return nil, fmt.Errorf("%s: fetchFunc not configured", e.name)
	}
	if req == nil {
		return nil, fmt.Errorf("%s: nil fetch request", e.name)
	}

	// Deep-clone reference fields so concurrent engine attempts never share
	// mutable option state with each other or with the caller.
	r := cloneFetchRequest(req)
	if e.forceStealth {
		r.Stealth = true
	}

	fetchCtx, cancel := requestContext(ctx, r.Timeout)
	defer cancel()

	result, err := e.fetchFunc(fetchCtx, r)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", e.name, err)
	}

	result.EngineName = e.name
	return result, nil
}

func cloneFetchRequest(req *FetchRequest) *FetchRequest {
	if req == nil {
		return nil
	}
	clone := *req
	clone.Headers = make(map[string]string, len(req.Headers))
	for key, value := range req.Headers {
		clone.Headers[key] = value
	}
	clone.Cookies = append([]http.Cookie(nil), req.Cookies...)
	clone.Actions = append([]Action(nil), req.Actions...)
	if req.WaitForNetworkIdle != nil {
		wait := *req.WaitForNetworkIdle
		clone.WaitForNetworkIdle = &wait
	}
	return &clone
}
