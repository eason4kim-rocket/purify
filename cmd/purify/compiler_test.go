package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	compilerdomain "github.com/use-agent/purify/compiler"
	"github.com/use-agent/purify/config"
	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/llm"
	"github.com/use-agent/purify/publicnet"
	"github.com/use-agent/purify/snapshot"
)

func TestManagedTruthExtractorReturnsValidatedCloneWithoutRepair(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"],"additionalProperties":false}`)
	client := &managedExtractorStub{initial: &llm.ExtractResult{Data: json.RawMessage(`{"count":3}`)}}
	truth, err := newManagedTruthExtractor(client, validManagedCompilerConfig())
	if err != nil {
		t.Fatalf("newManagedTruthExtractor() error = %v", err)
	}

	result, err := truth.ExtractTruth(context.Background(), "Count: 3", schema)
	if err != nil || string(result) != `{"count":3}` {
		t.Fatalf("ExtractTruth() = %s, %v", result, err)
	}
	if client.extractCalls != 1 || client.repairCalls != 0 || client.content != "Count: 3" || string(client.schema) != string(schema) {
		t.Fatalf("managed extractor calls/content/schema = %d/%d/%q/%s", client.extractCalls, client.repairCalls, client.content, client.schema)
	}
	wantParams := llm.ExtractParams{
		APIKey:  "process-secret",
		Model:   "gpt-4o-mini",
		BaseURL: "https://api.openai.com/v1",
	}
	if client.params != wantParams {
		t.Fatalf("managed params = %#v, want %#v", client.params, wantParams)
	}
	result[0] = '['
	if string(client.initial.Data) != `{"count":3}` {
		t.Fatalf("returned truth aliases provider buffer: %s", client.initial.Data)
	}
}

func TestManagedTruthExtractorRepairsOnceAndFailsClosed(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"],"additionalProperties":false}`)
	tests := []struct {
		name        string
		client      *managedExtractorStub
		want        string
		wantError   error
		wantExtract int
		wantRepair  int
	}{
		{
			name: "valid repair",
			client: &managedExtractorStub{
				initial:  &llm.ExtractResult{Data: json.RawMessage(`{"count":"three"}`)},
				repaired: &llm.ExtractResult{Data: json.RawMessage(`{"count":3}`)},
			},
			want: `{"count":3}`, wantExtract: 1, wantRepair: 1,
		},
		{
			name: "repair remains partial",
			client: &managedExtractorStub{
				initial:  &llm.ExtractResult{Data: json.RawMessage(`{"count":"three"}`)},
				repaired: &llm.ExtractResult{Data: json.RawMessage(`{"count":"still-three"}`)},
			},
			wantError: errManagedCompilerTruthInvalid, wantExtract: 1, wantRepair: 1,
		},
		{
			name:      "initial invalid JSON",
			client:    &managedExtractorStub{initial: &llm.ExtractResult{Data: json.RawMessage(`{"count":`)}},
			wantError: errManagedCompilerTruthInvalid, wantExtract: 1,
		},
		{
			name: "empty repair",
			client: &managedExtractorStub{
				initial: &llm.ExtractResult{Data: json.RawMessage(`{"count":"three"}`)},
			},
			wantError: errManagedCompilerTruthInvalid, wantExtract: 1, wantRepair: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			truth, err := newManagedTruthExtractor(test.client, validManagedCompilerConfig())
			if err != nil {
				t.Fatalf("newManagedTruthExtractor() error = %v", err)
			}
			result, err := truth.ExtractTruth(context.Background(), "Count: 3", schema)
			if !errors.Is(err, test.wantError) || string(result) != test.want {
				t.Fatalf("ExtractTruth() = %s, %v; want %q, %v", result, err, test.want, test.wantError)
			}
			if test.client.extractCalls != test.wantExtract || test.client.repairCalls != test.wantRepair {
				t.Fatalf("extract/repair calls = %d/%d, want %d/%d", test.client.extractCalls, test.client.repairCalls, test.wantExtract, test.wantRepair)
			}
			if test.wantRepair == 1 && string(test.client.previous) != `{"count":"three"}` {
				t.Fatalf("repair previous = %s", test.client.previous)
			}
		})
	}
}

func TestManagedTruthExtractorSanitizesTransportFailures(t *testing.T) {
	providerDetail := "provider failure containing process-secret"
	for _, test := range []struct {
		name   string
		client *managedExtractorStub
	}{
		{name: "initial", client: &managedExtractorStub{extractErr: errors.New(providerDetail)}},
		{name: "repair", client: &managedExtractorStub{
			initial:   &llm.ExtractResult{Data: json.RawMessage(`{"count":"three"}`)},
			repairErr: errors.New(providerDetail),
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			truth, err := newManagedTruthExtractor(test.client, validManagedCompilerConfig())
			if err != nil {
				t.Fatal(err)
			}
			_, err = truth.ExtractTruth(
				context.Background(),
				"Count: 3",
				json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"]}`),
			)
			if !errors.Is(err, errManagedCompilerTruthUnavailable) || strings.Contains(err.Error(), providerDetail) {
				t.Fatalf("ExtractTruth() error = %v", err)
			}
		})
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	truth, err := newManagedTruthExtractor(&managedExtractorStub{}, validManagedCompilerConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := truth.ExtractTruth(canceled, "unused", json.RawMessage(`{}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ExtractTruth() error = %v", err)
	}
}

func TestManagedTruthExtractorRejectsOverLimitBeforeRepair(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`)
	atLimit := managedTruthObject(compilerdomain.MaxOutputBytes)
	if len(atLimit) != compilerdomain.MaxOutputBytes {
		t.Fatalf("at-limit fixture = %d bytes", len(atLimit))
	}
	if violations, err := validateManagedTruth(schema, &llm.ExtractResult{Data: atLimit}); err != nil || len(violations) != 0 {
		t.Fatalf("validateManagedTruth(at limit) = %#v, %v", violations, err)
	}

	overLimit := managedTruthObject(compilerdomain.MaxOutputBytes + 1)
	client := &managedExtractorStub{
		initial:  &llm.ExtractResult{Data: overLimit},
		repaired: &llm.ExtractResult{Data: json.RawMessage(`{"value":"small"}`)},
	}
	truth, err := newManagedTruthExtractor(client, validManagedCompilerConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := truth.ExtractTruth(context.Background(), "unused", schema)
	if !errors.Is(err, errManagedCompilerTruthInvalid) || result != nil || client.extractCalls != 1 || client.repairCalls != 0 {
		t.Fatalf("ExtractTruth(over limit) = %s, %v; calls = %d/%d", result, err, client.extractCalls, client.repairCalls)
	}
}

func TestManagedCompilerRuntimeConstructionAndCloseOrder(t *testing.T) {
	if runtime, err := newManagedCompilerRuntime(config.CompilerConfig{}, nil, nil, nil, nil); err != nil || runtime != nil {
		t.Fatalf("disabled newManagedCompilerRuntime() = %#v, %v", runtime, err)
	}

	durable, err := ledger.Open(t.TempDir())
	if err != nil {
		t.Fatalf("ledger.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = durable.Close() })
	registry, err := compilerdomain.NewStore(durable)
	if err != nil {
		t.Fatalf("compiler.NewStore() error = %v", err)
	}
	policy := publicnet.NewPolicy(publicnet.Options{})
	if runtime, err := newManagedCompilerRuntime(validManagedCompilerConfig(), durable, registry, nil, policy); err == nil || runtime != nil {
		t.Fatalf("enabled runtime without snapshots = %#v, %v", runtime, err)
	}

	snapshots, err := snapshot.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("snapshot.NewStore() error = %v", err)
	}
	t.Cleanup(snapshots.Close)
	if runtime, err := newManagedCompilerRuntime(validManagedCompilerConfig(), durable, registry, snapshots, nil); !errors.Is(err, errManagedCompilerHTTPUnavailable) || runtime != nil {
		t.Fatalf("enabled runtime without policy = %#v, %v", runtime, err)
	}
	runtime, err := newManagedCompilerRuntime(validManagedCompilerConfig(), durable, registry, snapshots, policy)
	if err != nil || runtime == nil {
		t.Fatalf("newManagedCompilerRuntime() = %#v, %v", runtime, err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if bindings := bindCompilerServices(registry, runtime); bindings.compiledRepository != registry ||
		bindings.extractorRevisions != registry || bindings.compileObserver != runtime {
		t.Fatalf("enabled compiler bindings = %#v", bindings)
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("runtime.Close() error = %v", err)
	}
}

func TestManagedCompilerRuntimeClosesCoordinatorBeforeHTTPOnce(t *testing.T) {
	closeError := errors.New("coordinator close failed")
	events := make([]string, 0, 2)
	coordinator := &managedCoordinatorStub{closeErr: closeError, onClose: func() {
		events = append(events, "coordinator")
	}}
	runtime := &managedCompilerRuntime{
		coordinator: coordinator,
		closeHTTP: func() {
			events = append(events, "http")
		},
	}
	for index := 0; index < 2; index++ {
		if err := runtime.Close(); !errors.Is(err, closeError) {
			t.Fatalf("Close() error = %v", err)
		}
	}
	if strings.Join(events, ",") != "coordinator,http" || coordinator.closeCalls != 1 {
		t.Fatalf("close events/calls = %v/%d", events, coordinator.closeCalls)
	}
}

func TestDisabledManagedCompilerStillBindsActiveStoreForExtractAndVerify(t *testing.T) {
	durable, err := ledger.Open(t.TempDir())
	if err != nil {
		t.Fatalf("ledger.Open() error = %v", err)
	}
	defer durable.Close()
	registry, err := compilerdomain.NewStore(durable)
	if err != nil {
		t.Fatalf("compiler.NewStore() error = %v", err)
	}
	bindings := bindCompilerServices(registry, nil)
	if bindings.compiledRepository != registry || bindings.extractorRevisions != registry || bindings.compileObserver != nil {
		t.Fatalf("disabled compiler bindings = %#v", bindings)
	}

	schema := json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"],"additionalProperties":false}`)
	html := `<html><body><span class="count">3</span></body></html>`
	key, err := compilerdomain.BuildPageKey("https://example.test/product", schema, html)
	if err != nil {
		t.Fatalf("BuildPageKey() error = %v", err)
	}
	revision, err := registry.Save(context.Background(), key, compilerdomain.IR{
		Version: compilerdomain.CurrentIRVersion,
		Fields: []compilerdomain.FieldRule{{
			Name: "count", Selector: ".count", Type: compilerdomain.TypeNumber, Required: true,
		}},
	}, compilerdomain.ValidationReport{
		PerField: []compilerdomain.FieldValidation{{Name: "count", Matches: 3, Samples: 3, Score: 1}},
		Overall:  1, Samples: 3, ValidSamples: 3,
		Threshold: compilerdomain.ValidationThreshold, CanEnable: true,
	})
	if err != nil {
		t.Fatalf("Store.Save() error = %v", err)
	}
	hit, found, err := bindings.compiledRepository.Lookup(context.Background(), key)
	if err != nil || !found || hit.ID != revision.ID {
		t.Fatalf("compiled Lookup() = %#v, %v, %v", hit, found, err)
	}
	resolved, err := bindings.extractorRevisions.Get(context.Background(), revision.ID)
	if err != nil || resolved.ID != revision.ID || resolved.Version != revision.Version {
		t.Fatalf("revision Get() = %#v, %v", resolved, err)
	}
}

type managedExtractorStub struct {
	initial    *llm.ExtractResult
	repaired   *llm.ExtractResult
	extractErr error
	repairErr  error

	extractCalls int
	repairCalls  int
	content      string
	schema       json.RawMessage
	previous     json.RawMessage
	params       llm.ExtractParams
}

func (stub *managedExtractorStub) Extract(
	_ context.Context,
	content string,
	schema json.RawMessage,
	params llm.ExtractParams,
) (*llm.ExtractResult, error) {
	stub.extractCalls++
	stub.content = content
	stub.schema = append(json.RawMessage(nil), schema...)
	stub.params = params
	return stub.initial, stub.extractErr
}

func (stub *managedExtractorStub) ExtractWithRepair(
	_ context.Context,
	_ string,
	_ json.RawMessage,
	previous json.RawMessage,
	_ []llm.Violation,
	params llm.ExtractParams,
) (*llm.ExtractResult, error) {
	stub.repairCalls++
	stub.previous = append(json.RawMessage(nil), previous...)
	stub.params = params
	return stub.repaired, stub.repairErr
}

type managedCoordinatorStub struct {
	closeCalls int
	closeErr   error
	onClose    func()
}

func (*managedCoordinatorStub) Observe(context.Context, compilerdomain.PageKey, string, time.Time, json.RawMessage) error {
	return nil
}

func (stub *managedCoordinatorStub) Close() error {
	stub.closeCalls++
	if stub.onClose != nil {
		stub.onClose()
	}
	return stub.closeErr
}

func validManagedCompilerConfig() config.CompilerConfig {
	return config.CompilerConfig{
		Enabled: true,
		APIKey:  " process-secret ",
		Model:   " gpt-4o-mini ",
		BaseURL: " https://api.openai.com/v1/ ",
	}
}

func managedTruthObject(size int) json.RawMessage {
	const prefix = `{"value":"`
	const suffix = `"}`
	return json.RawMessage(prefix + strings.Repeat("v", size-len(prefix)-len(suffix)) + suffix)
}
