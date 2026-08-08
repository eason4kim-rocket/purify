package engine

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"time"
)

// Dispatcher coordinates multi-engine racing with staged escalation.
// It starts the fastest engine first and progressively escalates to heavier
// engines if earlier ones fail or time out.
type Dispatcher struct {
	engines          []Engine
	escalationDelays []time.Duration
	memory           *DomainMemory
}

// NewDispatcher creates a Dispatcher with the given engines and escalation delays.
// engines[i] starts after escalationDelays[i] from the race beginning.
// The first delay should be 0 (immediate start).
func NewDispatcher(engines []Engine, escalationDelays []time.Duration, memory *DomainMemory) *Dispatcher {
	// Ensure we have at least as many delays as engines.
	delays := make([]time.Duration, len(engines))
	copy(delays, escalationDelays)
	return &Dispatcher{
		engines:          engines,
		escalationDelays: delays,
		memory:           memory,
	}
}

// Dispatch runs the multi-engine race for the given request and returns
// the first successful result. If all engines fail, it returns the last error.
func (d *Dispatcher) Dispatch(ctx context.Context, req *FetchRequest) (*FetchResult, error) {
	if req == nil {
		return nil, fmt.Errorf("dispatcher: nil fetch request")
	}
	domain := extractDomain(req.URL)

	// Check domain memory for a previously successful engine.
	if remembered := d.rememberedEngine(domain); remembered != "" {
		for _, eng := range d.engines {
			if eng.Name() == remembered && eng.Supports(req) {
				slog.Debug("domain memory hit", "domain", domain, "engine", remembered)
				result, err := eng.Fetch(ctx, req)
				if err == nil {
					return result, nil
				}
				// Memory entry failed; delete it and fall through to full race.
				slog.Info("domain memory miss (engine failed), running full race",
					"domain", domain, "engine", remembered, "error", err)
				if d.memory != nil {
					d.memory.Delete(domain)
				}
				break
			}
		}
	}

	return d.race(ctx, req, domain)
}

// race runs all engines with staged delays and returns the first success.
func (d *Dispatcher) race(ctx context.Context, req *FetchRequest, domain string) (*FetchResult, error) {
	type raceResult struct {
		result *FetchResult
		err    error
	}

	raceCtx, raceCancel := context.WithCancel(ctx)
	defer raceCancel()

	type candidate struct {
		engine Engine
		delay  time.Duration
	}
	candidates := make([]candidate, 0, len(d.engines))
	var firstDelay time.Duration
	for i, eng := range d.engines {
		if !eng.Supports(req) {
			slog.Debug("engine skipped: unsupported request options", "engine", eng.Name(), "url", req.URL)
			continue
		}
		delay := d.escalationDelays[i]
		if len(candidates) == 0 {
			firstDelay = delay
		}
		if delay > firstDelay {
			delay -= firstDelay
		} else {
			delay = 0
		}
		candidates = append(candidates, candidate{engine: eng, delay: delay})
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("%w: no configured engine supports request for %s", ErrUnsupportedRequest, req.URL)
	}

	results := make(chan raceResult, len(candidates))
	var wg sync.WaitGroup

	for _, candidate := range candidates {
		wg.Add(1)
		go func(e Engine, d time.Duration) {
			defer wg.Done()

			// Wait for the escalation delay or context cancellation.
			if d > 0 {
				select {
				case <-raceCtx.Done():
					return
				case <-time.After(d):
				}
			}

			// Check if another engine already won.
			select {
			case <-raceCtx.Done():
				return
			default:
			}

			slog.Debug("engine starting", "engine", e.Name(), "url", req.URL)
			result, err := e.Fetch(raceCtx, req)
			if err != nil {
				slog.Debug("engine failed", "engine", e.Name(), "url", req.URL, "error", err)
			}
			results <- raceResult{result: result, err: err}
		}(candidate.engine, candidate.delay)
	}

	// Close results channel when all goroutines finish.
	go func() {
		wg.Wait()
		close(results)
	}()

	var lastErr error
	for rr := range results {
		if rr.err != nil {
			lastErr = rr.err
			continue
		}
		// First success wins — cancel all other engines.
		raceCancel()
		slog.Info("engine won race", "engine", rr.result.EngineName, "url", req.URL)
		if d.memory != nil {
			d.memory.Set(domain, rr.result.EngineName)
		}
		return rr.result, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("dispatcher: all engines failed for %s", req.URL)
	}
	return nil, lastErr
}

func (d *Dispatcher) rememberedEngine(domain string) string {
	if d.memory == nil {
		return ""
	}
	return d.memory.Get(domain)
}

// extractDomain parses the hostname from a URL string.
func extractDomain(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Hostname()
}
