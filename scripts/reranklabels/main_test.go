package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/use-agent/purify/internal/rerankeval"
)

type commandProbe struct {
	packetCalls   [][2]string
	compileCalls  [][3]string
	validateCalls [][2]string
}

func (probe *commandProbe) operations() commandOperations {
	return commandOperations{
		buildPacket: func(casesPath, docsPath string) ([]byte, error) {
			probe.packetCalls = append(probe.packetCalls, [2]string{casesPath, docsPath})
			return []byte("blind-packet\n"), nil
		},
		compileLabels: func(casesPath, docsPath, judgmentsPath string) ([]byte, error) {
			probe.compileCalls = append(probe.compileCalls, [3]string{casesPath, docsPath, judgmentsPath})
			return []byte("compiled-labels\n"), nil
		},
		validateInputs: func(docsPath, labelsPath string) (rerankeval.JudgedInputsSummary, error) {
			probe.validateCalls = append(probe.validateCalls, [2]string{docsPath, labelsPath})
			return rerankeval.JudgedInputsSummary{
				Cases: 24, Candidates: 240,
				LanguageBucketCases: map[string]int{"must-not-leak": 1},
				HardCasesByLanguage: map[string]int{"must-not-leak": 2},
				HardCasesByBucket:   map[string]int{"must-not-leak": 3},
			}, nil
		},
	}
}

func TestRunRoutesExactModesAndEmitsOnlyBoundedSummary(t *testing.T) {
	directory := t.TempDir()
	packetPath := filepath.Join(directory, "packet.jsonl")
	labelsPath := filepath.Join(directory, "labels.jsonl")
	probe := &commandProbe{}
	operations := probe.operations()
	var stdout bytes.Buffer

	if err := run(context.Background(), []string{
		"packet", "--cases", "cases.jsonl", "--docs", "docs.jsonl", "--out", packetPath,
	}, &stdout, operations); err != nil {
		t.Fatalf("run(packet) error = %v", err)
	}
	if err := run(context.Background(), []string{
		"compile", "--cases", "cases.jsonl", "--docs", "docs.jsonl", "--judgments", "judgments.jsonl", "--out", labelsPath,
	}, &stdout, operations); err != nil {
		t.Fatalf("run(compile) error = %v", err)
	}
	if err := run(context.Background(), []string{
		"validate", "--docs", "docs.jsonl", "--labels", labelsPath,
	}, &stdout, operations); err != nil {
		t.Fatalf("run(validate) error = %v", err)
	}

	if got, want := probe.packetCalls, [][2]string{{"cases.jsonl", "docs.jsonl"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("packet calls = %#v, want %#v", got, want)
	}
	if got, want := probe.compileCalls, [][3]string{{"cases.jsonl", "docs.jsonl", "judgments.jsonl"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("compile calls = %#v, want %#v", got, want)
	}
	if got, want := probe.validateCalls, [][2]string{{"docs.jsonl", labelsPath}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("validate calls = %#v, want %#v", got, want)
	}
	assertPrivateFile(t, packetPath, "blind-packet\n")
	assertPrivateFile(t, labelsPath, "compiled-labels\n")
	if got, want := stdout.String(), "valid cases=24 candidates=240\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertNoCommandTemporaries(t, directory)
}

func TestRunRejectsInvalidModesAndFlagsBeforeCalls(t *testing.T) {
	probe := &commandProbe{}
	operations := probe.operations()
	validPacket := []string{"packet", "--cases", "cases", "--docs", "docs", "--out", filepath.Join(t.TempDir(), "out")}
	tests := []struct {
		name      string
		arguments []string
		cancelled bool
	}{
		{name: "missing mode"},
		{name: "mode is exact", arguments: []string{"Packet", "--cases", "cases", "--docs", "docs", "--out", "out"}},
		{name: "unknown mode", arguments: []string{"record", "--docs", "docs"}},
		{name: "packet missing out", arguments: []string{"packet", "--cases", "cases", "--docs", "docs"}},
		{name: "packet extra flag", arguments: []string{"packet", "--cases", "cases", "--docs", "docs", "--out", "out", "--judgments", "judgments"}},
		{name: "compile missing judgments", arguments: []string{"compile", "--cases", "cases", "--docs", "docs", "--out", "out"}},
		{name: "validate forbids out", arguments: []string{"validate", "--docs", "docs", "--labels", "labels", "--out", "out"}},
		{name: "trailing argument", arguments: []string{"validate", "--docs", "docs", "--labels", "labels", "extra"}},
		{name: "cancelled", arguments: validPacket, cancelled: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			if test.cancelled {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			var stdout bytes.Buffer
			err := run(ctx, test.arguments, &stdout, operations)
			if err == nil {
				t.Fatal("run() succeeded")
			}
			if test.cancelled && !errors.Is(err, context.Canceled) {
				t.Fatalf("run() error = %v, want context.Canceled", err)
			}
			if !test.cancelled && !errors.Is(err, errUsage) {
				t.Fatalf("run() error = %v, want errUsage", err)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q", stdout.String())
			}
		})
	}
	if len(probe.packetCalls)+len(probe.compileCalls)+len(probe.validateCalls) != 0 {
		t.Fatalf("invalid invocations reached operations: %#v", probe)
	}
}

func TestRunRefusesExistingZeroLengthOutputWithoutCallingBuilder(t *testing.T) {
	directory := t.TempDir()
	outputPath := filepath.Join(directory, "packet.jsonl")
	if err := os.WriteFile(outputPath, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	probe := &commandProbe{}
	err := run(context.Background(), []string{
		"packet", "--cases", "cases", "--docs", "docs", "--out", outputPath,
	}, &bytes.Buffer{}, probe.operations())
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("run() error = %v, want os.ErrExist", err)
	}
	if len(probe.packetCalls) != 0 {
		t.Fatalf("packet calls = %d, want 0", len(probe.packetCalls))
	}
	info, statErr := os.Stat(outputPath)
	if statErr != nil || info.Size() != 0 || info.Mode().Perm() != 0o640 {
		t.Fatalf("existing output changed: info=%#v err=%v", info, statErr)
	}
	assertNoCommandTemporaries(t, directory)
}

func TestRunConcurrentDestinationCreationCannotOverwrite(t *testing.T) {
	directory := t.TempDir()
	outputPath := filepath.Join(directory, "packet.jsonl")
	probe := &commandProbe{}
	operations := probe.operations()
	entered := make(chan string, 1)
	release := make(chan struct{})
	operations.beforeLink = func(temporaryPath string) error {
		entered <- temporaryPath
		<-release
		return nil
	}
	done := make(chan error, 1)
	go func() {
		done <- run(context.Background(), []string{
			"packet", "--cases", "cases", "--docs", "docs", "--out", outputPath,
		}, &bytes.Buffer{}, operations)
	}()

	var temporaryPath string
	select {
	case temporaryPath = <-entered:
	case err := <-done:
		t.Fatalf("run() returned before install barrier: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for install barrier")
	}
	assertPrivateFile(t, temporaryPath, "blind-packet\n")
	if filepath.Dir(temporaryPath) != directory {
		t.Fatalf("temporary directory = %q, want %q", filepath.Dir(temporaryPath), directory)
	}
	if err := os.WriteFile(outputPath, []byte("concurrent-owner"), 0o600); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrExist) {
			t.Fatalf("run() error = %v, want os.ErrExist", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for run result")
	}
	assertFileContents(t, outputPath, "concurrent-owner")
	assertNoCommandTemporaries(t, directory)
}

func TestRunInstallFailureRemovesOnlyItsPartialTemporary(t *testing.T) {
	directory := t.TempDir()
	outputPath := filepath.Join(directory, "labels.jsonl")
	probe := &commandProbe{}
	operations := probe.operations()
	installFailure := errors.New("install barrier failed")
	operations.beforeLink = func(temporaryPath string) error {
		assertPrivateFile(t, temporaryPath, "compiled-labels\n")
		return installFailure
	}
	err := run(context.Background(), []string{
		"compile", "--cases", "cases", "--docs", "docs", "--judgments", "judgments", "--out", outputPath,
	}, &bytes.Buffer{}, operations)
	if !errors.Is(err, installFailure) {
		t.Fatalf("run() error = %v, want install failure", err)
	}
	if _, statErr := os.Lstat(outputPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("partial destination exists: %v", statErr)
	}
	assertNoCommandTemporaries(t, directory)
}

func assertPrivateFile(t *testing.T, path, contents string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("private file %q: info=%#v err=%v", path, info, err)
	}
	assertFileContents(t, path, contents)
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != want {
		t.Fatalf("file %q = %q, %v; want %q", path, raw, err, want)
	}
}

func assertNoCommandTemporaries(t *testing.T, directory string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(directory, commandTemporaryPattern))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary files = %v, %v", matches, err)
	}
}
