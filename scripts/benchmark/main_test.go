package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/use-agent/purify/ledger"
)

func TestPercentileDurationNearestRank(t *testing.T) {
	durations := []time.Duration{
		1 * time.Millisecond,
		2 * time.Millisecond,
		3 * time.Millisecond,
		4 * time.Millisecond,
		5 * time.Millisecond,
	}
	for _, test := range []struct {
		name     string
		quantile float64
		want     time.Duration
	}{
		{name: "minimum", quantile: 0, want: time.Millisecond},
		{name: "median", quantile: 0.5, want: 3 * time.Millisecond},
		{name: "p95", quantile: 0.95, want: 5 * time.Millisecond},
		{name: "maximum", quantile: 1, want: 5 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := percentileDuration(durations, test.quantile); got != test.want {
				t.Fatalf("percentileDuration() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestPercentileDurationEmpty(t *testing.T) {
	if got := percentileDuration(nil, 0.95); got != 0 {
		t.Fatalf("percentileDuration(nil) = %v, want 0", got)
	}
}

func TestMeasureCompiledBenchmarkUsesRealStoreAndZeroLLM(t *testing.T) {
	dataDir := t.TempDir()
	report, err := measureCompiledBenchmark(context.Background(), dataDir, 7, 2)
	if err != nil {
		t.Fatalf("measureCompiledBenchmark() error = %v", err)
	}
	if report.Runs != 7 || report.WarmupRuns != 2 || report.LLMCalls != 0 ||
		report.ThresholdMs != compiledP95ThresholdMilliseconds {
		t.Fatalf("compiled report = %#v", report)
	}
	if report.P50Ms < 0 || report.P95Ms < report.P50Ms || report.P99Ms < report.P95Ms {
		t.Fatalf("compiled percentiles are not ordered: %#v", report)
	}
	if _, err := os.Stat(filepath.Join(dataDir, ledger.Filename)); err != nil {
		t.Fatalf("real ledger was not created: %v", err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("Marshal(report) error = %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("Unmarshal(report) error = %v", err)
	}
	for _, name := range []string{"runs", "p50_ms", "p95_ms", "p99_ms", "llm_calls"} {
		if _, exists := fields[name]; !exists {
			t.Errorf("compiled report omitted %q: %s", name, encoded)
		}
	}
}

func TestMeasureCompiledBenchmarkRejectsInvalidConfiguration(t *testing.T) {
	dataDir := t.TempDir()
	tests := []struct {
		name    string
		ctx     context.Context
		dataDir string
		runs    int
		warmup  int
	}{
		{name: "nil context", dataDir: dataDir, runs: 1},
		{name: "missing data dir", ctx: context.Background(), runs: 1},
		{name: "zero runs", ctx: context.Background(), dataDir: dataDir},
		{name: "negative warmup", ctx: context.Background(), dataDir: dataDir, runs: 1, warmup: -1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := measureCompiledBenchmark(test.ctx, test.dataDir, test.runs, test.warmup); err == nil {
				t.Fatal("measureCompiledBenchmark() succeeded")
			}
		})
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := measureCompiledBenchmark(canceled, dataDir, 1, 0); err == nil {
		t.Fatal("measureCompiledBenchmark(canceled) succeeded")
	}
}

func TestCompiledThresholdIsStrictlyBelowFiftyMilliseconds(t *testing.T) {
	for _, test := range []struct {
		p95  float64
		want bool
	}{
		{p95: 49.999, want: true},
		{p95: 50, want: false},
		{p95: 50.001, want: false},
	} {
		got := test.p95 < compiledP95ThresholdMilliseconds
		if got != test.want {
			t.Errorf("P95 %.3f threshold result = %v, want %v", test.p95, got, test.want)
		}
	}
}
