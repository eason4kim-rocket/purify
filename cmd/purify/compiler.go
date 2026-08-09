package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	compilerdomain "github.com/use-agent/purify/compiler"
	"github.com/use-agent/purify/config"
	extractdomain "github.com/use-agent/purify/extract"
	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/llm"
	"github.com/use-agent/purify/snapshot"
	verifydomain "github.com/use-agent/purify/verify"
)

var (
	errManagedCompilerTruthUnavailable = errors.New("managed compiler truth extraction failed")
	errManagedCompilerTruthInvalid     = errors.New("managed compiler truth output is invalid")
	errManagedCompilerHTTPUnavailable  = errors.New("managed compiler HTTP transport is unavailable")
)

type managedStructuredExtractor interface {
	Extract(context.Context, string, json.RawMessage, llm.ExtractParams) (*llm.ExtractResult, error)
	ExtractWithRepair(context.Context, string, json.RawMessage, json.RawMessage, []llm.Violation, llm.ExtractParams) (*llm.ExtractResult, error)
}

// managedTruthExtractor captures only process-owned provider configuration.
// Request BYOK and per-request provider overrides never cross this boundary.
type managedTruthExtractor struct {
	extractor managedStructuredExtractor
	params    llm.ExtractParams
}

func newManagedTruthExtractor(extractor managedStructuredExtractor, cfg config.CompilerConfig) (*managedTruthExtractor, error) {
	if extractor == nil {
		return nil, errManagedCompilerTruthUnavailable
	}
	if err := config.ValidateCompilerConfig(cfg, true); err != nil {
		return nil, err
	}
	return &managedTruthExtractor{
		extractor: extractor,
		params: llm.ExtractParams{
			APIKey:  strings.TrimSpace(cfg.APIKey),
			Model:   strings.TrimSpace(cfg.Model),
			BaseURL: strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
		},
	}, nil
}

func (extractor *managedTruthExtractor) ExtractTruth(
	ctx context.Context,
	content string,
	schema json.RawMessage,
) (json.RawMessage, error) {
	if extractor == nil || extractor.extractor == nil || ctx == nil {
		return nil, errManagedCompilerTruthUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	initial, err := extractor.extractor.Extract(ctx, content, schema, extractor.params)
	if err != nil {
		return nil, managedTruthTransportError(ctx)
	}
	violations, err := validateManagedTruth(schema, initial)
	if err != nil {
		return nil, err
	}
	if len(violations) == 0 {
		return append(json.RawMessage(nil), initial.Data...), nil
	}

	repaired, err := extractor.extractor.ExtractWithRepair(
		ctx,
		content,
		schema,
		initial.Data,
		violations,
		extractor.params,
	)
	if err != nil {
		return nil, managedTruthTransportError(ctx)
	}
	remaining, err := validateManagedTruth(schema, repaired)
	if err != nil {
		return nil, err
	}
	if len(remaining) != 0 {
		return nil, errManagedCompilerTruthInvalid
	}
	return append(json.RawMessage(nil), repaired.Data...), nil
}

func validateManagedTruth(schema json.RawMessage, result *llm.ExtractResult) ([]llm.Violation, error) {
	if result == nil || len(result.Data) == 0 || len(result.Data) > compilerdomain.MaxOutputBytes || !json.Valid(result.Data) {
		return nil, errManagedCompilerTruthInvalid
	}
	violations, err := llm.ValidateAgainstSchema(schema, result.Data)
	if err != nil {
		return nil, errManagedCompilerTruthInvalid
	}
	return violations, nil
}

func managedTruthTransportError(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return errManagedCompilerTruthUnavailable
}

type managedCoordinator interface {
	extractdomain.CompileObserver
	Close() error
}

// managedCompilerRuntime owns resources used only by background compilation.
// Close is ordered: stop coordinator work before closing managed HTTP idles.
// main registers this runtime after snapshot and ledger cleanup, extending the
// same LIFO order to coordinator -> HTTP -> snapshot -> ledger.
type managedCompilerRuntime struct {
	coordinator managedCoordinator
	closeHTTP   func()
	closeOnce   sync.Once
	closeErr    error
}

func (runtime *managedCompilerRuntime) Observe(
	ctx context.Context,
	page compilerdomain.PageKey,
	snapshotID string,
	fetchedAt time.Time,
	schema json.RawMessage,
) error {
	if runtime == nil || runtime.coordinator == nil {
		return compilerdomain.ErrInvalidCoordinator
	}
	return runtime.coordinator.Observe(ctx, page, snapshotID, fetchedAt, schema)
}

func (runtime *managedCompilerRuntime) Close() error {
	if runtime == nil {
		return nil
	}
	runtime.closeOnce.Do(func() {
		if runtime.closeHTTP != nil {
			defer runtime.closeHTTP()
		}
		if runtime.coordinator != nil {
			runtime.closeErr = runtime.coordinator.Close()
		}
	})
	return runtime.closeErr
}

func newManagedCompilerRuntime(
	cfg config.CompilerConfig,
	durable *ledger.Store,
	registry *compilerdomain.Store,
	snapshots *snapshot.Store,
) (*managedCompilerRuntime, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if err := config.ValidateCompilerConfig(cfg, snapshots != nil); err != nil {
		return nil, err
	}
	catalog, err := compilerdomain.NewSampleCatalog(durable)
	if err != nil {
		return nil, err
	}

	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok || defaultTransport == nil {
		return nil, errManagedCompilerHTTPUnavailable
	}
	managedTransport := defaultTransport.Clone()
	httpClient := &http.Client{
		Transport: managedTransport,
		Timeout:   compilerdomain.CoordinatorTaskTimeout,
	}
	truth, err := newManagedTruthExtractor(llm.NewClient(httpClient), cfg)
	if err != nil {
		httpClient.CloseIdleConnections()
		return nil, err
	}
	coordinator, err := compilerdomain.NewCoordinator(catalog, registry, snapshots, truth)
	if err != nil {
		httpClient.CloseIdleConnections()
		return nil, err
	}
	return &managedCompilerRuntime{
		coordinator: coordinator,
		closeHTTP:   managedTransport.CloseIdleConnections,
	}, nil
}

type compilerServiceBindings struct {
	compiledRepository extractdomain.CompiledRepository
	compileObserver    extractdomain.CompileObserver
	extractorRevisions verifydomain.ExtractorRevisionResolver
}

func bindCompilerServices(registry *compilerdomain.Store, runtime *managedCompilerRuntime) compilerServiceBindings {
	bindings := compilerServiceBindings{
		compiledRepository: registry,
		extractorRevisions: registry,
	}
	if runtime != nil {
		bindings.compileObserver = runtime
	}
	return bindings
}
