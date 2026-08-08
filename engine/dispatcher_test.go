package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type supportFixtureEngine struct {
	name      string
	supported bool
	calls     atomic.Int32
}

func (engine *supportFixtureEngine) Name() string { return engine.name }
func (engine *supportFixtureEngine) Supports(*FetchRequest) bool {
	return engine.supported
}
func (engine *supportFixtureEngine) Fetch(context.Context, *FetchRequest) (*FetchResult, error) {
	engine.calls.Add(1)
	return &FetchResult{HTML: "ok", EngineName: engine.name}, nil
}

func TestDispatcherSkipsUnsupportedEnginesWithoutDelay(t *testing.T) {
	unsupported := &supportFixtureEngine{name: "unsupported"}
	supported := &supportFixtureEngine{name: "supported", supported: true}
	dispatcher := NewDispatcher(
		[]Engine{unsupported, supported},
		[]time.Duration{0, time.Hour},
		nil,
	)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := dispatcher.Dispatch(ctx, &FetchRequest{URL: "https://example.test"})
	if err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	if result.EngineName != "supported" || unsupported.calls.Load() != 0 || supported.calls.Load() != 1 {
		t.Fatalf("result=%+v unsupported calls=%d supported calls=%d", result, unsupported.calls.Load(), supported.calls.Load())
	}
}

func TestDispatcherReturnsUnsupportedError(t *testing.T) {
	dispatcher := NewDispatcher(
		[]Engine{&supportFixtureEngine{name: "unsupported"}},
		[]time.Duration{0},
		nil,
	)
	_, err := dispatcher.Dispatch(context.Background(), &FetchRequest{URL: "https://example.test"})
	if !errors.Is(err, ErrUnsupportedRequest) {
		t.Fatalf("Dispatch() error = %v, want ErrUnsupportedRequest", err)
	}
}
