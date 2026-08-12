package rerankeval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/use-agent/purify/search/rerank"
)

const testManifestID = "bec55c54b64116c16242f8bab05b0dd6cbbf3ab963ea9d13ec20f1ccfcc8d0cf"

type testDocumentCase struct {
	CaseID     string                  `json:"case_id"`
	Query      string                  `json:"query"`
	Candidates []testDocumentCandidate `json:"candidates"`
}

type testDocumentCandidate struct {
	CandidateID  string `json:"candidate_id"`
	URL          string `json:"url"`
	ProviderRank int    `json:"provider_rank"`
	Title        string `json:"title"`
	Snippet      string `json:"snippet"`
}

type testLabelCase struct {
	CaseID   string      `json:"case_id"`
	Language string      `json:"language"`
	Bucket   string      `json:"bucket"`
	Hard     bool        `json:"hard"`
	Grades   []testGrade `json:"grades"`
	Note     string      `json:"note,omitempty"`
}

type testGrade struct {
	CandidateID string `json:"candidate_id"`
	Grade       int    `json:"grade"`
}

type testRecordingCase struct {
	CaseID          string      `json:"case_id"`
	ManifestID      string      `json:"manifest_id"`
	InputDigest     string      `json:"input_digest"`
	CandidateDigest string      `json:"candidate_digest"`
	Scores          []testScore `json:"scores"`
	Usage           testUsage   `json:"usage"`
	LatencyUS       int64       `json:"latency_us"`
}

type testScore struct {
	CandidateID    string  `json:"candidate_id"`
	RelevanceScore float64 `json:"relevance_score"`
}

type testUsage struct {
	PromptTokens int64 `json:"prompt_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

type testCorpusFiles struct {
	documents  []testDocumentCase
	labels     []testLabelCase
	recordings []testRecordingCase
}

func TestLoadCorpusAndEvaluateConstruction(t *testing.T) {
	files := validTestCorpus(t)
	docsPath, labelsPath, recordingsPath := writeTestCorpus(t, files)

	corpus, err := LoadCorpus(docsPath, labelsPath, recordingsPath, LoadOptions{ExpectedManifestID: testManifestID})
	if err != nil {
		t.Fatalf("LoadCorpus() error = %v", err)
	}
	if len(corpus.Cases) != MinimumCases {
		t.Fatalf("cases = %d, want %d", len(corpus.Cases), MinimumCases)
	}
	if len(corpus.Cases[0].Candidates) != 10 || corpus.Cases[0].Candidates[0].ProviderRank != 1 {
		t.Fatalf("first case drifted: %#v", corpus.Cases[0])
	}

	scorecard, err := Evaluate(corpus)
	if err != nil {
		t.Fatalf("Evaluate() error = %v; scorecard = %#v", err, scorecard)
	}
	if scorecard.TotalCandidates != MinimumCandidates || scorecard.NonRegressionHits != scorecard.NonRegressionTotal || scorecard.MacroDeltaNDCG5 < MinimumMacroDelta {
		t.Fatalf("scorecard gates drifted: %#v", scorecard)
	}
}

func TestLoadRecordingInputsUsesOnlyStrictDocsAndProductionBuilder(t *testing.T) {
	files := validTestCorpus(t)
	docs, _, _ := marshalTestCorpus(t, files)
	docsPath, _, _ := writeRawTestCorpus(t, docs, "labels are deliberately unread", "recordings are deliberately unread")
	inputs, err := LoadRecordingInputs(docsPath)
	if err != nil {
		t.Fatalf("LoadRecordingInputs() error = %v", err)
	}
	if len(inputs) != MinimumCases || inputs[0].CaseID != "case-00" || len(inputs[0].Request.Candidates) != MinimumCaseCandidates {
		t.Fatalf("recording inputs = %#v", inputs)
	}
	wantDigest, err := rerank.ReferenceInputDigest(buildTestRequest(t, files.documents[0]))
	gotDigest, gotErr := rerank.ReferenceInputDigest(inputs[0].Request)
	if err != nil || gotErr != nil || gotDigest != wantDigest {
		t.Fatalf("recording input digest = %q/%v, want %q/%v", gotDigest, gotErr, wantDigest, err)
	}
	inputs[0].Request.Candidates[0].StableID = "mutated"
	again, err := LoadRecordingInputs(docsPath)
	if err != nil || again[0].Request.Candidates[0].StableID == "mutated" {
		t.Fatalf("recording inputs share mutable state: %#v, %v", again, err)
	}
}

func TestEncodeRecordingLineCanonicalizesScoresAndBindsProductionInput(t *testing.T) {
	files := validTestCorpus(t)
	docs, _, _ := marshalTestCorpus(t, files)
	docsPath, _, _ := writeRawTestCorpus(t, docs, "unused", "unused")
	inputs, err := LoadRecordingInputs(docsPath)
	if err != nil {
		t.Fatal(err)
	}
	input := inputs[0]
	digest, err := rerank.ReferenceInputDigest(input.Request)
	if err != nil {
		t.Fatal(err)
	}
	candidateDigest, err := rerank.ReferenceCandidateDigest(input.Request)
	if err != nil {
		t.Fatal(err)
	}
	scores := make([]rerank.ScoreResult, len(input.Request.Candidates))
	for index := range input.Request.Candidates {
		candidate := input.Request.Candidates[len(input.Request.Candidates)-1-index]
		scores[index] = rerank.ScoreResult{StableID: candidate.StableID, RelevanceScore: float64(index) / float64(len(scores))}
	}
	recording := rerank.ReferenceRecording{
		InputDigest: digest, CandidateDigest: candidateDigest,
		Observation: rerank.ReferenceObservation{Usage: rerank.ReferenceUsage{PromptTokens: 17, TotalTokens: 17}, Scores: scores},
		LatencyUS:   2500,
	}
	encoded, err := EncodeRecordingLine(input, recording)
	if err != nil {
		t.Fatalf("EncodeRecordingLine() error = %v", err)
	}
	parsed, err := parseRecordingCase(encoded)
	if err != nil || parsed.id != input.CaseID || parsed.inputDigest != digest || parsed.candidateDigest != candidateDigest ||
		parsed.manifestID != testManifestID || parsed.latencyUS != 2500 {
		t.Fatalf("encoded recording = %s / %#v / %v", encoded, parsed, err)
	}
	for _, candidate := range input.Request.Candidates {
		if _, present := parsed.scores[candidate.StableID]; !present {
			t.Fatalf("encoded score set misses %q: %s", candidate.StableID, encoded)
		}
	}

	invalid := recording
	invalid.InputDigest = strings.Repeat("0", 64)
	if encoded, err := EncodeRecordingLine(input, invalid); err == nil || encoded != nil {
		t.Fatalf("EncodeRecordingLine(wrong digest) = %s, %v", encoded, err)
	}
	invalid = recording
	invalid.CandidateDigest = strings.Repeat("0", 64)
	if encoded, err := EncodeRecordingLine(input, invalid); err == nil || encoded != nil {
		t.Fatalf("EncodeRecordingLine(wrong candidate digest) = %s, %v", encoded, err)
	}
	invalid = recording
	invalid.Observation.Scores = append([]rerank.ScoreResult(nil), recording.Observation.Scores...)
	invalid.Observation.Scores[0].StableID = strings.Repeat("f", 64)
	if encoded, err := EncodeRecordingLine(input, invalid); err == nil || encoded != nil {
		t.Fatalf("EncodeRecordingLine(wrong score set) = %s, %v", encoded, err)
	}
}

func TestLoadCorpusRejectsACallerAssertedManifestIdentity(t *testing.T) {
	files := validTestCorpus(t)
	docsPath, labelsPath, recordingsPath := writeTestCorpus(t, files)
	if _, err := LoadCorpus(docsPath, labelsPath, recordingsPath, LoadOptions{
		ExpectedManifestID: strings.Repeat("a", 64),
	}); err == nil {
		t.Fatal("LoadCorpus accepted a caller-asserted manifest identity")
	}
}

func TestLoadCorpusJoinsByKeysAndProviderRankNotArrayPosition(t *testing.T) {
	files := validTestCorpus(t)
	for index := range files.documents {
		reverse(files.documents[index].Candidates)
		reverse(files.labels[index].Grades)
		reverse(files.recordings[index].Scores)
	}
	docsPath, labelsPath, recordingsPath := writeTestCorpus(t, files)
	corpus, err := LoadCorpus(docsPath, labelsPath, recordingsPath, LoadOptions{ExpectedManifestID: testManifestID})
	if err != nil {
		t.Fatalf("LoadCorpus() error = %v", err)
	}
	for _, goldenCase := range corpus.Cases {
		for index, candidate := range goldenCase.Candidates {
			if candidate.ProviderRank != index+1 {
				t.Fatalf("case %q candidate %d rank = %d", goldenCase.ID, index, candidate.ProviderRank)
			}
		}
	}
}

func TestLoadCorpusBindsIdenticalModelTextToOrderedCandidateIdentity(t *testing.T) {
	files := validTestCorpus(t)
	first, second := &files.documents[0].Candidates[0], &files.documents[0].Candidates[1]
	second.Title, second.Snippet = first.Title, first.Snippet
	refreshTestRecording(t, &files, 0)
	before := buildTestRequest(t, files.documents[0])
	beforeWire, err := rerank.ReferenceInputDigest(before)
	if err != nil {
		t.Fatal(err)
	}
	first.CandidateID, second.CandidateID = second.CandidateID, first.CandidateID
	first.URL, second.URL = second.URL, first.URL
	after := buildTestRequest(t, files.documents[0])
	afterWire, err := rerank.ReferenceInputDigest(after)
	if err != nil || afterWire != beforeWire {
		t.Fatalf("identity-only mutation changed wire digest = %q/%v, want %q", afterWire, err, beforeWire)
	}
	beforeIdentity := files.recordings[0].CandidateDigest
	afterIdentity, err := rerank.ReferenceCandidateDigest(after)
	if err != nil || afterIdentity == beforeIdentity {
		t.Fatalf("identity digest did not bind ordered mapping = %q/%v", afterIdentity, err)
	}
	docsPath, labelsPath, recordingsPath := writeTestCorpus(t, files)
	if _, err := LoadCorpus(docsPath, labelsPath, recordingsPath, LoadOptions{ExpectedManifestID: testManifestID}); err == nil {
		t.Fatal("LoadCorpus accepted a stale score-index to candidate mapping")
	}
}

func TestLoadCorpusRejectsStrictJSONAndJoinFailures(t *testing.T) {
	base := validTestCorpus(t)
	tests := []struct {
		name   string
		mutate func(*testCorpusFiles)
		raw    func(t *testing.T, files testCorpusFiles) (string, string, string)
	}{
		{name: "wrong manifest", mutate: func(files *testCorpusFiles) { files.recordings[0].ManifestID = strings.Repeat("b", 64) }},
		{name: "wrong input digest", mutate: func(files *testCorpusFiles) { files.recordings[0].InputDigest = strings.Repeat("0", 64) }},
		{name: "wrong candidate digest", mutate: func(files *testCorpusFiles) { files.recordings[0].CandidateDigest = strings.Repeat("0", 64) }},
		{name: "zero latency", mutate: func(files *testCorpusFiles) { files.recordings[0].LatencyUS = 0 }},
		{name: "latency N plus 1", mutate: func(files *testCorpusFiles) { files.recordings[0].LatencyUS = MaximumRecordingLatencyUS + 1 }},
		{name: "candidate id not URL digest", mutate: func(files *testCorpusFiles) {
			wrongID := strings.Repeat("f", 64)
			files.documents[0].Candidates[0].CandidateID = wrongID
			files.labels[0].Grades[0].CandidateID = wrongID
			files.recordings[0].Scores[0].CandidateID = wrongID
		}},
		{name: "orphan recording", mutate: func(files *testCorpusFiles) { files.recordings[0].CaseID = "orphan" }},
		{name: "missing score", mutate: func(files *testCorpusFiles) { files.recordings[0].Scores = files.recordings[0].Scores[:9] }},
		{name: "orphan grade", mutate: func(files *testCorpusFiles) { files.labels[0].Grades[0].CandidateID = "orphan" }},
		{name: "invalid grade", mutate: func(files *testCorpusFiles) { files.labels[0].Grades[0].Grade = 4 }},
		{name: "hard label lie", mutate: func(files *testCorpusFiles) { files.labels[0].Hard = false }},
		{name: "rank gap", mutate: func(files *testCorpusFiles) { files.documents[0].Candidates[9].ProviderRank = 11 }},
		{name: "duplicate query", mutate: func(files *testCorpusFiles) {
			files.documents[1].Query = files.documents[0].Query
			refreshTestRecording(t, files, 1)
		}},
		{name: "non finite score", raw: func(t *testing.T, files testCorpusFiles) (string, string, string) {
			docs, labels, recordings := marshalTestCorpus(t, files)
			recordings = strings.Replace(recordings, `"relevance_score":0`, `"relevance_score":1e999`, 1)
			return docs, labels, recordings
		}},
		{name: "bom", raw: func(t *testing.T, files testCorpusFiles) (string, string, string) {
			docs, labels, recordings := marshalTestCorpus(t, files)
			return "\ufeff" + docs, labels, recordings
		}},
		{name: "empty line", raw: func(t *testing.T, files testCorpusFiles) (string, string, string) {
			docs, labels, recordings := marshalTestCorpus(t, files)
			return strings.Replace(docs, "\n", "\n\n", 1), labels, recordings
		}},
		{name: "unicode duplicate", raw: func(t *testing.T, files testCorpusFiles) (string, string, string) {
			docs, labels, recordings := marshalTestCorpus(t, files)
			docs = strings.Replace(docs, `"case_id":"case-00"`, `"case_id":"case-00","case\u005fid":"case-00"`, 1)
			return docs, labels, recordings
		}},
		{name: "case smuggle", raw: func(t *testing.T, files testCorpusFiles) (string, string, string) {
			docs, labels, recordings := marshalTestCorpus(t, files)
			docs = strings.Replace(docs, `"query":"query 00"`, `"Query":"query 00"`, 1)
			return docs, labels, recordings
		}},
		{name: "unknown field", raw: func(t *testing.T, files testCorpusFiles) (string, string, string) {
			docs, labels, recordings := marshalTestCorpus(t, files)
			docs = strings.Replace(docs, `"query":"query 00"`, `"query":"query 00","extra":true`, 1)
			return docs, labels, recordings
		}},
		{name: "trailing value", raw: func(t *testing.T, files testCorpusFiles) (string, string, string) {
			docs, labels, recordings := marshalTestCorpus(t, files)
			return strings.Replace(docs, "\n", " {}\n", 1), labels, recordings
		}},
		{name: "required null", raw: func(t *testing.T, files testCorpusFiles) (string, string, string) {
			docs, labels, recordings := marshalTestCorpus(t, files)
			docs = strings.Replace(docs, `"query":"query 00"`, `"query":null`, 1)
			return docs, labels, recordings
		}},
		{name: "invalid utf8", raw: func(t *testing.T, files testCorpusFiles) (string, string, string) {
			docs, labels, recordings := marshalTestCorpus(t, files)
			return docs[:1] + string([]byte{0xff}) + docs[1:], labels, recordings
		}},
		{name: "lone high surrogate", raw: func(t *testing.T, files testCorpusFiles) (string, string, string) {
			docs, labels, recordings := marshalTestCorpus(t, files)
			labels = strings.Replace(labels, `"note":"independently judged"`, `"note":"bad\uD800"`, 1)
			return docs, labels, recordings
		}},
		{name: "lone low surrogate", raw: func(t *testing.T, files testCorpusFiles) (string, string, string) {
			docs, labels, recordings := marshalTestCorpus(t, files)
			labels = strings.Replace(labels, `"note":"independently judged"`, `"note":"bad\uDC00"`, 1)
			return docs, labels, recordings
		}},
		{name: "depth", raw: func(t *testing.T, files testCorpusFiles) (string, string, string) {
			docs, labels, recordings := marshalTestCorpus(t, files)
			deep := strings.Repeat("[", MaxJSONDepth+1) + "0" + strings.Repeat("]", MaxJSONDepth+1)
			docs = strings.Replace(docs, `"query":"query 00"`, `"query":`+deep, 1)
			return docs, labels, recordings
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			files := cloneTestCorpus(t, base)
			if test.mutate != nil {
				test.mutate(&files)
			}
			var docs, labels, recordings string
			if test.raw != nil {
				docs, labels, recordings = test.raw(t, files)
			} else {
				docs, labels, recordings = marshalTestCorpus(t, files)
			}
			docsPath, labelsPath, recordingsPath := writeRawTestCorpus(t, docs, labels, recordings)
			if _, err := LoadCorpus(docsPath, labelsPath, recordingsPath, LoadOptions{ExpectedManifestID: testManifestID}); err == nil {
				t.Fatal("LoadCorpus() succeeded")
			}
		})
	}
}

func TestLoadCorpusAcceptsPairedUTF16SurrogateEscape(t *testing.T) {
	files := validTestCorpus(t)
	docs, labels, recordings := marshalTestCorpus(t, files)
	labels = strings.Replace(labels, `"note":"independently judged"`, `"note":"paired \uD83D\uDE00"`, 1)
	docsPath, labelsPath, recordingsPath := writeRawTestCorpus(t, docs, labels, recordings)
	if _, err := LoadCorpus(docsPath, labelsPath, recordingsPath, LoadOptions{ExpectedManifestID: testManifestID}); err != nil {
		t.Fatalf("LoadCorpus(paired surrogate) error = %v", err)
	}
}

func TestLoadCorpusRejectsBudgets(t *testing.T) {
	files := validTestCorpus(t)
	docs, labels, recordings := marshalTestCorpus(t, files)
	tests := []struct {
		name string
		docs string
	}{
		{name: "line", docs: strings.Repeat(" ", MaxJSONLLineBytes+1) + "\n"},
		{name: "file", docs: strings.Repeat(" ", MaxJSONLFileBytes+1)},
		{name: "string", docs: strings.Replace(docs, `"query":"query 00"`, `"query":"`+strings.Repeat("a", MaxJSONStringBytes+1)+`"`, 1)},
		{name: "array", docs: strings.Replace(docs, `"candidates":[`, `"candidates":[`+strings.Repeat(`null,`, MaxJSONArrayElements), 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			docsPath, labelsPath, recordingsPath := writeRawTestCorpus(t, test.docs, labels, recordings)
			if _, err := LoadCorpus(docsPath, labelsPath, recordingsPath, LoadOptions{ExpectedManifestID: testManifestID}); err == nil {
				t.Fatal("LoadCorpus() succeeded")
			}
		})
	}
}

func TestStrictJSONBoundsAcceptNRejectNPlusOne(t *testing.T) {
	maxString := `"` + strings.Repeat("a", MaxJSONStringBytes) + `"`
	maxArray := `[` + strings.Repeat(`0,`, MaxJSONArrayElements-1) + `0]`
	maxDepth := strings.Repeat("[", MaxJSONDepth) + "0" + strings.Repeat("]", MaxJSONDepth)
	tests := []struct {
		name    string
		atN     string
		atNPlus string
	}{
		{name: "string", atN: maxString, atNPlus: `"` + strings.Repeat("a", MaxJSONStringBytes+1) + `"`},
		{name: "array", atN: maxArray, atNPlus: `[` + strings.Repeat(`0,`, MaxJSONArrayElements) + `0]`},
		{name: "depth", atN: maxDepth, atNPlus: `[` + maxDepth + `]`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateJSON([]byte(test.atN)); err != nil {
				t.Fatalf("validateJSON(N) error = %v", err)
			}
			if err := validateJSON([]byte(test.atNPlus)); err == nil {
				t.Fatal("validateJSON(N+1) succeeded")
			}
		})
	}
}

func TestJSONLLineAndFileBoundsAcceptNRejectNPlusOne(t *testing.T) {
	lineAtN := fixedJSONLine(t, MaxJSONLLineBytes)
	path := filepath.Join(t.TempDir(), "line.jsonl")
	if err := os.WriteFile(path, []byte(lineAtN), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readJSONLines(path); err != nil {
		t.Fatalf("readJSONLines(line N) error = %v", err)
	}
	if err := os.WriteFile(path, []byte(lineAtN+" "), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readJSONLines(path); err == nil {
		t.Fatal("readJSONLines(line N+1) succeeded")
	}

	fileLine := fixedJSONLine(t, MaxJSONLLineBytes-1)
	fileAtN := strings.Repeat(fileLine+"\n", MaxJSONLFileBytes/MaxJSONLLineBytes)
	if len(fileAtN) != MaxJSONLFileBytes {
		t.Fatalf("file fixture size = %d", len(fileAtN))
	}
	if err := os.WriteFile(path, []byte(fileAtN), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readJSONLines(path); err != nil {
		t.Fatalf("readJSONLines(file N) error = %v", err)
	}
	if err := os.WriteFile(path, []byte(fileAtN+" "), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readJSONLines(path); err == nil {
		t.Fatal("readJSONLines(file N+1) succeeded")
	}
}

func fixedJSONLine(t *testing.T, size int) string {
	t.Helper()
	if size < 2+MaxJSONArrayElements*2+(MaxJSONArrayElements-1) {
		t.Fatalf("line size %d is too small", size)
	}
	payloadBytes := size - 2 - MaxJSONArrayElements*2 - (MaxJSONArrayElements - 1)
	base := payloadBytes / MaxJSONArrayElements
	remainder := payloadBytes % MaxJSONArrayElements
	var line strings.Builder
	line.Grow(size)
	line.WriteByte('[')
	for index := 0; index < MaxJSONArrayElements; index++ {
		if index > 0 {
			line.WriteByte(',')
		}
		line.WriteByte('"')
		length := base
		if index < remainder {
			length++
		}
		if length > MaxJSONStringBytes {
			t.Fatalf("element length = %d", length)
		}
		line.WriteString(strings.Repeat("a", length))
		line.WriteByte('"')
	}
	line.WriteByte(']')
	if line.Len() != size {
		t.Fatalf("line size = %d, want %d", line.Len(), size)
	}
	return line.String()
}

func validTestCorpus(t *testing.T) testCorpusFiles {
	return testCorpusWithCases(t, MinimumCases)
}

func testCorpusWithCases(t *testing.T, caseCount int) testCorpusFiles {
	t.Helper()
	files := testCorpusFiles{}
	buckets := Buckets()
	languages := Languages()
	grades := []int{0, 3, 2, 1, 0, 3, 2, 1, 0, 0}
	for index := 0; index < caseCount; index++ {
		caseID := fmt.Sprintf("case-%02d", index)
		document := testDocumentCase{CaseID: caseID, Query: fmt.Sprintf("query %02d", index)}
		label := testLabelCase{CaseID: caseID, Language: languages[(index/len(buckets))%len(languages)], Bucket: buckets[index%len(buckets)], Hard: true, Note: "independently judged"}
		recording := testRecordingCase{CaseID: caseID, ManifestID: testManifestID, Usage: testUsage{PromptTokens: 10, TotalTokens: 10}, LatencyUS: 1_000}
		for rank, grade := range grades {
			candidateURL := fmt.Sprintf("https://example-%02d.test/page-%02d", index, rank+1)
			candidateID, err := rerank.CandidateID(candidateURL)
			if err != nil {
				t.Fatalf("CandidateID(%q): %v", candidateURL, err)
			}
			document.Candidates = append(document.Candidates, testDocumentCandidate{
				CandidateID:  candidateID,
				URL:          candidateURL,
				ProviderRank: rank + 1,
				Title:        fmt.Sprintf("title %d", rank+1),
				Snippet:      fmt.Sprintf("snippet %d", rank+1),
			})
			label.Grades = append(label.Grades, testGrade{CandidateID: candidateID, Grade: grade})
			recording.Scores = append(recording.Scores, testScore{CandidateID: candidateID, RelevanceScore: float64(grade) / 3})
		}
		request := buildTestRequest(t, document)
		digest, err := rerank.ReferenceInputDigest(request)
		if err != nil {
			t.Fatalf("ReferenceInputDigest(%s): %v", caseID, err)
		}
		recording.InputDigest = digest
		candidateDigest, err := rerank.ReferenceCandidateDigest(request)
		if err != nil {
			t.Fatalf("ReferenceCandidateDigest(%s): %v", caseID, err)
		}
		recording.CandidateDigest = candidateDigest
		files.documents = append(files.documents, document)
		files.labels = append(files.labels, label)
		files.recordings = append(files.recordings, recording)
	}
	return files
}

func refreshTestRecording(t *testing.T, files *testCorpusFiles, index int) {
	t.Helper()
	request := buildTestRequest(t, files.documents[index])
	digest, err := rerank.ReferenceInputDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	files.recordings[index].InputDigest = digest
	candidateDigest, err := rerank.ReferenceCandidateDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	files.recordings[index].CandidateDigest = candidateDigest
}

func buildTestRequest(t *testing.T, document testDocumentCase) rerank.ScoreRequest {
	t.Helper()
	candidates := make([]rerank.Candidate, len(document.Candidates))
	for index, candidate := range document.Candidates {
		candidates[index] = rerank.Candidate{CanonicalURL: candidate.URL, ProviderRank: candidate.ProviderRank, Title: candidate.Title, Snippet: candidate.Snippet}
	}
	request, err := rerank.BuildRequest(document.Query, candidates)
	if err != nil {
		t.Fatalf("BuildRequest(%s): %v", document.CaseID, err)
	}
	return request
}

func cloneTestCorpus(t *testing.T, input testCorpusFiles) testCorpusFiles {
	t.Helper()
	output := testCorpusFiles{
		documents:  append([]testDocumentCase(nil), input.documents...),
		labels:     append([]testLabelCase(nil), input.labels...),
		recordings: append([]testRecordingCase(nil), input.recordings...),
	}
	for index := range output.documents {
		output.documents[index].Candidates = append([]testDocumentCandidate(nil), input.documents[index].Candidates...)
	}
	for index := range output.labels {
		output.labels[index].Grades = append([]testGrade(nil), input.labels[index].Grades...)
	}
	for index := range output.recordings {
		output.recordings[index].Scores = append([]testScore(nil), input.recordings[index].Scores...)
	}
	return output
}

func writeTestCorpus(t *testing.T, files testCorpusFiles) (string, string, string) {
	t.Helper()
	docs, labels, recordings := marshalTestCorpus(t, files)
	return writeRawTestCorpus(t, docs, labels, recordings)
}

func marshalTestCorpus(t *testing.T, files testCorpusFiles) (string, string, string) {
	t.Helper()
	return marshalJSONLines(t, files.documents), marshalJSONLines(t, files.labels), marshalJSONLines(t, files.recordings)
}

func marshalJSONLines[T any](t *testing.T, rows []T) string {
	t.Helper()
	var output strings.Builder
	for _, row := range rows {
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		output.Write(encoded)
		output.WriteByte('\n')
	}
	return output.String()
}

func reverse[T any](values []T) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func writeRawTestCorpus(t *testing.T, docs, labels, recordings string) (string, string, string) {
	t.Helper()
	directory := t.TempDir()
	paths := []string{filepath.Join(directory, "docs.jsonl"), filepath.Join(directory, "labels.jsonl"), filepath.Join(directory, "recordings.jsonl")}
	for index, body := range []string{docs, labels, recordings} {
		if err := os.WriteFile(paths[index], []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return paths[0], paths[1], paths[2]
}

func cloneCorpus(input Corpus) Corpus {
	output := Corpus{Cases: make([]Case, len(input.Cases))}
	copy(output.Cases, input.Cases)
	for index := range output.Cases {
		output.Cases[index].Candidates = append([]Candidate(nil), input.Cases[index].Candidates...)
	}
	return output
}

func corporaEqual(left, right Corpus) bool {
	return reflect.DeepEqual(left, right)
}
