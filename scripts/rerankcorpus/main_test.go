package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/use-agent/purify/internal/rerankeval"
	"github.com/use-agent/purify/search/rerank"
	"github.com/use-agent/purify/search/rerank/replay"
)

type fakeRecorder struct {
	calls        int
	failAt       int
	closed       int
	beforeRecord func(int)
}

func (recorder *fakeRecorder) Record(_ context.Context, request rerank.ScoreRequest) (rerank.ReferenceRecording, error) {
	call := recorder.calls
	recorder.calls++
	if recorder.beforeRecord != nil {
		recorder.beforeRecord(call)
	}
	if recorder.failAt >= 0 && call == recorder.failAt {
		return rerank.ReferenceRecording{}, errors.New("private recorder detail")
	}
	digest, err := rerank.ReferenceInputDigest(request)
	if err != nil {
		return rerank.ReferenceRecording{}, err
	}
	candidateDigest, err := rerank.ReferenceCandidateDigest(request)
	if err != nil {
		return rerank.ReferenceRecording{}, err
	}
	scores := make([]rerank.ScoreResult, len(request.Candidates))
	for index := range request.Candidates {
		candidate := request.Candidates[len(request.Candidates)-1-index]
		grade := recordingTestGrades[candidate.ProviderRank-1]
		scores[index] = rerank.ScoreResult{StableID: candidate.StableID, RelevanceScore: float64(grade) / 3}
	}
	return rerank.ReferenceRecording{
		InputDigest: digest, CandidateDigest: candidateDigest,
		Observation: rerank.ReferenceObservation{
			ResponseID: "fixture-response", Usage: rerank.ReferenceUsage{PromptTokens: 10, TotalTokens: 10}, Scores: scores,
		},
		LatencyUS: 1000,
	}, nil
}

func (recorder *fakeRecorder) Close() { recorder.closed++ }

var recordingTestGrades = []int{0, 3, 2, 1, 0, 3, 2, 1, 0, 0}

func TestRunRecordsEachStrictInputOnceWithoutReadingLabels(t *testing.T) {
	docsPath, labelsPath := writeRecordingCommandCorpus(t)
	outputPath := filepath.Join(t.TempDir(), "recordings.jsonl")
	fake := &fakeRecorder{failAt: -1}
	factoryCalls := 0
	err := run(context.Background(), []string{
		"-docs", docsPath, "-out", outputPath, "-endpoint", "http://127.0.0.1:8000/v1/rerank", "-allow-private",
	}, func(name string) string {
		if name != recorderAPIKeyEnvironment {
			t.Fatalf("unexpected environment lookup %q", name)
		}
		return "recording-secret"
	}, func(config rerank.ReferenceRecorderConfig) (referenceRecorder, error) {
		factoryCalls++
		if config.APIKey != "recording-secret" || config.Endpoint != "http://127.0.0.1:8000/v1/rerank" ||
			!config.AllowPrivate || config.Timeout != rerank.DefaultScorerTimeout {
			t.Fatalf("recorder config = %#v", config)
		}
		return fake, nil
	})
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if factoryCalls != 1 || fake.calls != rerankeval.MinimumCases || fake.closed != 1 {
		t.Fatalf("factory/record/close calls = %d/%d/%d", factoryCalls, fake.calls, fake.closed)
	}
	manifest, err := replay.ReferenceManifest()
	if err != nil {
		t.Fatal(err)
	}
	corpus, err := rerankeval.LoadCorpus(docsPath, labelsPath, outputPath, rerankeval.LoadOptions{ExpectedManifestID: manifest.ManifestID})
	if err != nil || len(corpus.Cases) != rerankeval.MinimumCases {
		t.Fatalf("recorded corpus = %#v, %v", corpus, err)
	}
	if info, err := os.Stat(outputPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("output mode = %#v, %v", info, err)
	}
}

func TestRunRefusesOverwriteAndRemovesOnlyItsPartialOutput(t *testing.T) {
	docsPath, _ := writeRecordingCommandCorpus(t)
	directory := t.TempDir()
	outputPath := filepath.Join(directory, "recordings.jsonl")
	if err := os.WriteFile(outputPath, []byte("owned-by-user"), 0o600); err != nil {
		t.Fatal(err)
	}
	factoryCalls := 0
	err := run(context.Background(), []string{"-docs", docsPath, "-out", outputPath, "-endpoint", "https://example.test/v1/rerank"},
		func(string) string { return "key" }, func(rerank.ReferenceRecorderConfig) (referenceRecorder, error) {
			factoryCalls++
			return &fakeRecorder{failAt: -1}, nil
		})
	if err == nil || factoryCalls != 0 {
		t.Fatalf("overwrite run = %v, factory calls=%d", err, factoryCalls)
	}
	if raw, readErr := os.ReadFile(outputPath); readErr != nil || string(raw) != "owned-by-user" {
		t.Fatalf("existing output changed = %q, %v", raw, readErr)
	}
	assertNoRecorderTemporaryFiles(t, directory)

	partialPath := filepath.Join(directory, "partial.jsonl")
	fake := &fakeRecorder{failAt: 3}
	err = run(context.Background(), []string{"-docs", docsPath, "-out", partialPath, "-endpoint", "https://example.test/v1/rerank"},
		func(string) string { return "key" }, func(rerank.ReferenceRecorderConfig) (referenceRecorder, error) { return fake, nil })
	if err == nil || fake.calls != 4 || fake.closed != 1 {
		t.Fatalf("partial run = %v, calls/close=%d/%d", err, fake.calls, fake.closed)
	}
	if _, statErr := os.Stat(partialPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("partial output survived: %v", statErr)
	}
	assertNoRecorderTemporaryFiles(t, directory)
}

func TestRunConcurrentDestinationReplacementCannotOverwrite(t *testing.T) {
	docsPath, _ := writeRecordingCommandCorpus(t)
	directory := t.TempDir()
	outputPath := filepath.Join(directory, "recordings.jsonl")
	entered := make(chan struct{})
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	fake := &fakeRecorder{failAt: -1, beforeRecord: func(call int) {
		if call == 0 {
			close(entered)
			<-release
		}
	}}
	done := make(chan error, 1)
	go func() {
		done <- run(context.Background(), []string{
			"-docs", docsPath, "-out", outputPath, "-endpoint", "https://example.test/v1/rerank",
		}, func(string) string { return "key" }, func(rerank.ReferenceRecorderConfig) (referenceRecorder, error) {
			return fake, nil
		})
	}()
	waitForRecorderEntry(t, entered, done)
	assertSinglePrivateRecorderTemporaryFile(t, directory)
	replaceDestination(t, outputPath, "created-while-recording")
	close(release)
	released = true
	if err := waitForRunResult(t, done); !errors.Is(err, os.ErrExist) {
		t.Fatalf("run() error = %v, want destination-exists failure", err)
	}
	if fake.calls != rerankeval.MinimumCases || fake.closed != 1 {
		t.Fatalf("record/close calls = %d/%d", fake.calls, fake.closed)
	}
	assertFileContents(t, outputPath, "created-while-recording")
	assertNoRecorderTemporaryFiles(t, directory)
}

func TestRunFailureCleanupCannotRemoveConcurrentDestinationReplacement(t *testing.T) {
	docsPath, _ := writeRecordingCommandCorpus(t)
	directory := t.TempDir()
	outputPath := filepath.Join(directory, "recordings.jsonl")
	entered := make(chan struct{})
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	fake := &fakeRecorder{failAt: 0, beforeRecord: func(call int) {
		if call == 0 {
			close(entered)
			<-release
		}
	}}
	done := make(chan error, 1)
	go func() {
		done <- run(context.Background(), []string{
			"-docs", docsPath, "-out", outputPath, "-endpoint", "https://example.test/v1/rerank",
		}, func(string) string { return "key" }, func(rerank.ReferenceRecorderConfig) (referenceRecorder, error) {
			return fake, nil
		})
	}()
	waitForRecorderEntry(t, entered, done)
	assertSinglePrivateRecorderTemporaryFile(t, directory)
	replaceDestination(t, outputPath, "replacement-must-survive")
	close(release)
	released = true
	if err := waitForRunResult(t, done); err == nil {
		t.Fatal("run() succeeded, want recorder failure")
	}
	if fake.calls != 1 || fake.closed != 1 {
		t.Fatalf("record/close calls = %d/%d", fake.calls, fake.closed)
	}
	assertFileContents(t, outputPath, "replacement-must-survive")
	assertNoRecorderTemporaryFiles(t, directory)
}

func TestRunRejectsInvalidInvocationBeforeRecorderConstruction(t *testing.T) {
	factoryCalls := 0
	factory := func(rerank.ReferenceRecorderConfig) (referenceRecorder, error) {
		factoryCalls++
		return &fakeRecorder{failAt: -1}, nil
	}
	for _, arguments := range [][]string{
		nil,
		{"-docs", "docs", "-out", "out", "-endpoint", "endpoint", "-api-key", "must-not-be-on-argv"},
		{"-docs", "docs", "-out", "out", "-endpoint", "endpoint", "-timeout-seconds", "0"},
	} {
		if err := run(context.Background(), arguments, func(string) string { return "key" }, factory); err == nil {
			t.Fatalf("run(%q) succeeded", arguments)
		}
	}
	if factoryCalls != 0 {
		t.Fatalf("factory calls = %d, want 0", factoryCalls)
	}
}

func writeRecordingCommandCorpus(t *testing.T) (string, string) {
	t.Helper()
	type candidate struct {
		ID      string `json:"candidate_id"`
		URL     string `json:"url"`
		Rank    int    `json:"provider_rank"`
		Title   string `json:"title"`
		Snippet string `json:"snippet"`
	}
	type document struct {
		CaseID     string      `json:"case_id"`
		Query      string      `json:"query"`
		Candidates []candidate `json:"candidates"`
	}
	type grade struct {
		ID    string `json:"candidate_id"`
		Grade int    `json:"grade"`
	}
	type label struct {
		CaseID        string  `json:"case_id"`
		Language      string  `json:"language"`
		Bucket        string  `json:"bucket"`
		Hard          bool    `json:"hard"`
		RubricVersion string  `json:"rubric_version"`
		Grades        []grade `json:"grades"`
	}
	buckets := rerankeval.Buckets()
	languages := rerankeval.Languages()
	documents := make([]document, rerankeval.MinimumCases)
	labels := make([]label, rerankeval.MinimumCases)
	for caseIndex := 0; caseIndex < rerankeval.MinimumCases; caseIndex++ {
		caseID := fmt.Sprintf("case-%02d", caseIndex)
		documents[caseIndex] = document{CaseID: caseID, Query: fmt.Sprintf("query %02d", caseIndex)}
		labels[caseIndex] = label{
			CaseID: caseID, Language: languages[(caseIndex/len(buckets))%len(languages)], Bucket: buckets[caseIndex%len(buckets)],
			Hard: true, RubricVersion: rerankeval.JudgmentRubricVersion,
		}
		for rank, value := range recordingTestGrades {
			url := fmt.Sprintf("https://example-%02d.test/page-%02d", caseIndex, rank+1)
			id, err := rerank.CandidateID(url)
			if err != nil {
				t.Fatal(err)
			}
			documents[caseIndex].Candidates = append(documents[caseIndex].Candidates, candidate{
				ID: id, URL: url, Rank: rank + 1, Title: fmt.Sprintf("title %d", rank+1), Snippet: fmt.Sprintf("snippet %d", rank+1),
			})
			labels[caseIndex].Grades = append(labels[caseIndex].Grades, grade{ID: id, Grade: value})
		}
	}
	directory := t.TempDir()
	docsPath := filepath.Join(directory, "docs.jsonl")
	labelsPath := filepath.Join(directory, "labels.jsonl")
	for path, rows := range map[string]any{docsPath: documents, labelsPath: labels} {
		encoded := encodeJSONLines(t, rows)
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return docsPath, labelsPath
}

func encodeJSONLines(t *testing.T, rows any) []byte {
	t.Helper()
	encoded, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	var lines []json.RawMessage
	if err := json.Unmarshal(encoded, &lines); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	for _, line := range lines {
		output.Write(line)
		output.WriteByte('\n')
	}
	return output.Bytes()
}

func waitForRecorderEntry(t *testing.T, entered <-chan struct{}, done <-chan error) {
	t.Helper()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("run() returned before recording: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for recording")
	}
}

func waitForRunResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for run()")
		return nil
	}
}

func replaceDestination(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != want {
		t.Fatalf("file contents = %q, %v; want %q", raw, err, want)
	}
}

func assertSinglePrivateRecorderTemporaryFile(t *testing.T, directory string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(directory, recorderTemporaryPattern))
	if err != nil || len(paths) != 1 {
		t.Fatalf("temporary paths = %q, %v; want one", paths, err)
	}
	info, err := os.Stat(paths[0])
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("temporary mode = %#v, %v; want 0600", info, err)
	}
}

func assertNoRecorderTemporaryFiles(t *testing.T, directory string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(directory, recorderTemporaryPattern))
	if err != nil || len(paths) != 0 {
		t.Fatalf("temporary paths survived = %q, %v", paths, err)
	}
}
