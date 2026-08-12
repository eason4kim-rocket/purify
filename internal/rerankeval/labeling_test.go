package rerankeval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

type testLabelingCase struct {
	CaseID   string `json:"case_id"`
	Query    string `json:"query"`
	Language string `json:"language"`
	Bucket   string `json:"bucket"`
}

type testBlindPacketCase struct {
	CaseID       string               `json:"case_id"`
	PacketDigest string               `json:"packet_digest"`
	Query        string               `json:"query"`
	Candidates   []testBlindCandidate `json:"candidates"`
}

type testBlindCandidate struct {
	CandidateID string `json:"candidate_id"`
	Title       string `json:"title"`
	Snippet     string `json:"snippet"`
}

type testJudgmentCase struct {
	CaseID        string      `json:"case_id"`
	PacketDigest  string      `json:"packet_digest"`
	RubricVersion string      `json:"rubric_version"`
	Grades        []testGrade `json:"grades"`
	Note          string      `json:"note,omitempty"`
}

type testCompiledLabelCase struct {
	CaseID        string      `json:"case_id"`
	Language      string      `json:"language"`
	Bucket        string      `json:"bucket"`
	Hard          bool        `json:"hard"`
	RubricVersion string      `json:"rubric_version"`
	Grades        []testGrade `json:"grades"`
	Note          string      `json:"note,omitempty"`
}

func TestBuildBlindLabelPacketBlindsAndCanonicalizes(t *testing.T) {
	files := testCorpusWithCases(t, 2)
	cases := testLabelingCases(files)
	reverse(cases)
	for index := range files.documents {
		reverse(files.documents[index].Candidates)
	}
	reverse(files.documents)

	casesPath, docsPath, _ := writeLabelingInputs(t, marshalJSONLines(t, cases), marshalJSONLines(t, files.documents), "unused\n")
	packet, err := BuildBlindLabelPacket(casesPath, docsPath)
	if err != nil {
		t.Fatalf("BuildBlindLabelPacket() error = %v", err)
	}
	rows := decodeBlindPacket(t, packet)
	if got := []string{rows[0].CaseID, rows[1].CaseID}; !reflect.DeepEqual(got, []string{"case-00", "case-01"}) {
		t.Fatalf("packet case order = %v", got)
	}
	for _, row := range rows {
		if !validDigest(row.PacketDigest) {
			t.Fatalf("packet digest = %q", row.PacketDigest)
		}
		gotIDs := make([]string, len(row.Candidates))
		for index, candidate := range row.Candidates {
			gotIDs[index] = candidate.CandidateID
		}
		wantIDs := append([]string(nil), gotIDs...)
		sort.Strings(wantIDs)
		if !reflect.DeepEqual(gotIDs, wantIDs) {
			t.Fatalf("case %q candidate order = %v, want byte order %v", row.CaseID, gotIDs, wantIDs)
		}
	}
	assertBlindPacketKeys(t, packet)

	// Provider position, source array order, and construction metadata are not
	// observable in the packet or its change-detection digest.
	mutated := cloneTestCorpus(t, files)
	for index := range mutated.documents {
		mutated.documents[index].Candidates[0].ProviderRank, mutated.documents[index].Candidates[9].ProviderRank =
			mutated.documents[index].Candidates[9].ProviderRank, mutated.documents[index].Candidates[0].ProviderRank
		reverse(mutated.documents[index].Candidates)
	}
	mutatedCases := testLabelingCases(mutated)
	for index := range mutatedCases {
		mutatedCases[index].Language = LanguageChinese
		mutatedCases[index].Bucket = BucketPromptInjection
	}
	mutatedCasesPath, mutatedDocsPath, _ := writeLabelingInputs(t, marshalJSONLines(t, mutatedCases), marshalJSONLines(t, mutated.documents), "unused\n")
	mutatedPacket, err := BuildBlindLabelPacket(mutatedCasesPath, mutatedDocsPath)
	if err != nil {
		t.Fatalf("BuildBlindLabelPacket(mutated ranks) error = %v", err)
	}
	if !bytes.Equal(packet, mutatedPacket) {
		t.Fatalf("blind packet changed with provider order or hidden metadata:\n%s\n%s", packet, mutatedPacket)
	}
}

func TestCompileBlindLabelsJoinsExactlyAndComputesHard(t *testing.T) {
	files := testCorpusWithCases(t, 2)
	cases := testLabelingCases(files)
	casesBody := marshalJSONLines(t, cases)
	docsBody := marshalJSONLines(t, files.documents)
	casesPath, docsPath, _ := writeLabelingInputs(t, casesBody, docsBody, "unused\n")
	packet, err := BuildBlindLabelPacket(casesPath, docsPath)
	if err != nil {
		t.Fatal(err)
	}
	judgments := testJudgments(t, packet, files.documents)
	judgments[0].Note = "independent blind judgment"
	for index := range judgments[1].Grades {
		judgments[1].Grades[index].Grade = 1
	}
	reverse(judgments)
	for index := range judgments {
		reverse(judgments[index].Grades)
	}
	_, _, judgmentsPath := writeLabelingInputs(t, casesBody, docsBody, marshalJSONLines(t, judgments))

	labels, err := CompileBlindLabels(casesPath, docsPath, judgmentsPath)
	if err != nil {
		t.Fatalf("CompileBlindLabels() error = %v", err)
	}
	lines := bytes.Split(bytes.TrimSuffix(labels, []byte{'\n'}), []byte{'\n'})
	if len(lines) != 2 {
		t.Fatalf("compiled lines = %d", len(lines))
	}
	first, err := parseCompiledLabelCase(lines[0])
	if err != nil {
		t.Fatalf("compiled label is not strict judged schema: %v\n%s", err, lines[0])
	}
	if first.id != "case-00" || first.language != cases[0].Language || first.bucket != cases[0].Bucket || !first.hard || first.note != "independent blind judgment" {
		t.Fatalf("first compiled label = %#v", first)
	}
	second, err := parseCompiledLabelCase(lines[1])
	if err != nil || second.id != "case-01" || second.hard {
		t.Fatalf("second compiled label = %#v / %v", second, err)
	}
	var encoded testCompiledLabelCase
	if err := json.Unmarshal(lines[0], &encoded); err != nil {
		t.Fatal(err)
	}
	if encoded.RubricVersion != JudgmentRubricVersion {
		t.Fatalf("rubric_version = %q", encoded.RubricVersion)
	}
	gotIDs := make([]string, len(encoded.Grades))
	for index, grade := range encoded.Grades {
		gotIDs[index] = grade.CandidateID
	}
	wantIDs := append([]string(nil), gotIDs...)
	sort.Strings(wantIDs)
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("compiled grade order = %v, want %v", gotIDs, wantIDs)
	}
}

func TestCompileBlindLabelsBindsJudgmentsToCanonicalPacket(t *testing.T) {
	files := testCorpusWithCases(t, 1)
	cases := testLabelingCases(files)
	casesBody, docsBody := marshalJSONLines(t, cases), marshalJSONLines(t, files.documents)
	casesPath, docsPath, _ := writeLabelingInputs(t, casesBody, docsBody, "unused\n")
	packet, err := BuildBlindLabelPacket(casesPath, docsPath)
	if err != nil {
		t.Fatal(err)
	}
	judgments := testJudgments(t, packet, files.documents)

	files.documents[0].Query = "changed query"
	cases[0].Query = "changed query"
	changedCasesPath, changedDocsPath, changedJudgmentsPath := writeLabelingInputs(
		t, marshalJSONLines(t, cases), marshalJSONLines(t, files.documents), marshalJSONLines(t, judgments),
	)
	if labels, err := CompileBlindLabels(changedCasesPath, changedDocsPath, changedJudgmentsPath); err == nil || labels != nil {
		t.Fatalf("CompileBlindLabels(stale packet digest) = %s, %v", labels, err)
	}
}

func TestValidateJudgedInputsAppliesGPUIndependentConstructionGates(t *testing.T) {
	files := validTestCorpus(t)
	docsPath, labelsPath, _ := writeTestCorpus(t, files)
	summary, err := ValidateJudgedInputs(docsPath, labelsPath)
	if err != nil {
		t.Fatalf("ValidateJudgedInputs() error = %v", err)
	}
	if summary.Cases != MinimumCases || summary.Candidates != MinimumCandidates {
		t.Fatalf("summary counts = %#v", summary)
	}
	for _, language := range Languages() {
		if summary.HardCasesByLanguage[language] < 2 {
			t.Fatalf("hard language coverage = %#v", summary.HardCasesByLanguage)
		}
		for _, bucket := range Buckets() {
			if summary.LanguageBucketCases[language+"/"+bucket] < 1 {
				t.Fatalf("language/bucket coverage = %#v", summary.LanguageBucketCases)
			}
		}
	}
	for _, bucket := range Buckets() {
		if summary.HardCasesByBucket[bucket] < 1 {
			t.Fatalf("hard bucket coverage = %#v", summary.HardCasesByBucket)
		}
	}

	t.Run("minimum cases", func(t *testing.T) {
		small := testCorpusWithCases(t, 2)
		docs, labels, _ := marshalTestCorpus(t, small)
		docsPath, labelsPath, _ := writeRawTestCorpus(t, docs, labels, "unused")
		if _, err := ValidateJudgedInputs(docsPath, labelsPath); err == nil {
			t.Fatal("ValidateJudgedInputs accepted a two-case corpus")
		}
	})

	t.Run("zero IDCG", func(t *testing.T) {
		mutated := cloneTestCorpus(t, files)
		for index := range mutated.labels[0].Grades {
			mutated.labels[0].Grades[index].Grade = 0
		}
		mutated.labels[0].Hard = false
		docs, labels, _ := marshalTestCorpus(t, mutated)
		docsPath, labelsPath, _ := writeRawTestCorpus(t, docs, labels, "unused")
		if _, err := ValidateJudgedInputs(docsPath, labelsPath); err == nil {
			t.Fatal("ValidateJudgedInputs accepted zero IDCG")
		}
	})

	t.Run("missing slice", func(t *testing.T) {
		mutated := cloneTestCorpus(t, files)
		for index := range mutated.labels {
			mutated.labels[index].Language = LanguageEnglish
		}
		docs, labels, _ := marshalTestCorpus(t, mutated)
		docsPath, labelsPath, _ := writeRawTestCorpus(t, docs, labels, "unused")
		if _, err := ValidateJudgedInputs(docsPath, labelsPath); err == nil {
			t.Fatal("ValidateJudgedInputs accepted missing language slices")
		}
	})

	t.Run("language needs two hard cases", func(t *testing.T) {
		mutated := cloneTestCorpus(t, files)
		kept := false
		for index := range mutated.labels {
			if mutated.labels[index].Language != LanguageEnglish {
				continue
			}
			if !kept {
				kept = true
				continue
			}
			for gradeIndex := range mutated.labels[index].Grades {
				if mutated.documents[index].Candidates[gradeIndex].ProviderRank <= 5 && mutated.labels[index].Grades[gradeIndex].Grade == 0 {
					mutated.labels[index].Grades[gradeIndex].Grade = 1
				}
			}
			mutated.labels[index].Hard = false
		}
		docs, labels, _ := marshalTestCorpus(t, mutated)
		docsPath, labelsPath, _ := writeRawTestCorpus(t, docs, labels, "unused")
		if _, err := ValidateJudgedInputs(docsPath, labelsPath); err == nil {
			t.Fatal("ValidateJudgedInputs accepted one hard English case")
		}
	})

	t.Run("bucket needs hard case", func(t *testing.T) {
		mutated := cloneTestCorpus(t, files)
		for index := range mutated.labels {
			if mutated.labels[index].Bucket == BucketLexical {
				for gradeIndex := range mutated.labels[index].Grades {
					if mutated.documents[index].Candidates[gradeIndex].ProviderRank <= 5 && mutated.labels[index].Grades[gradeIndex].Grade == 0 {
						mutated.labels[index].Grades[gradeIndex].Grade = 1
					}
				}
				mutated.labels[index].Hard = false
			}
		}
		docs, labels, _ := marshalTestCorpus(t, mutated)
		docsPath, labelsPath, _ := writeRawTestCorpus(t, docs, labels, "unused")
		if _, err := ValidateJudgedInputs(docsPath, labelsPath); err == nil {
			t.Fatal("ValidateJudgedInputs accepted a bucket without a hard case")
		}
	})
}

func TestLoadCorpusRequiresJudgmentRubricVersion(t *testing.T) {
	files := validTestCorpus(t)
	docs, labels, recordings := marshalTestCorpus(t, files)
	labels = strings.Replace(labels, `"rubric_version":"`+JudgmentRubricVersion+`",`, "", 1)
	docsPath, labelsPath, recordingsPath := writeRawTestCorpus(t, docs, labels, recordings)
	if corpus, err := LoadCorpus(docsPath, labelsPath, recordingsPath, LoadOptions{ExpectedManifestID: testManifestID}); err == nil || len(corpus.Cases) != 0 {
		t.Fatalf("LoadCorpus(missing rubric) = %#v, %v", corpus, err)
	}
}

func TestBlindLabelingRejectsStrictCaseInputViolations(t *testing.T) {
	files := testCorpusWithCases(t, 1)
	validCases := marshalJSONLines(t, testLabelingCases(files))
	validDocs := marshalJSONLines(t, files.documents)
	base := `{"case_id":"case-00","query":"query 00","language":"en","bucket":"lexical"}` + "\n"
	tests := []struct {
		name         string
		cases        string
		docs         string
		wantResource bool
	}{
		{name: "duplicate case", cases: validCases + validCases, docs: validDocs},
		{name: "duplicate document", cases: validCases, docs: validDocs + validDocs},
		{name: "unknown field", cases: strings.Replace(base, `}`, `,"extra":false}`, 1), docs: validDocs},
		{name: "null", cases: strings.Replace(base, `"language":"en"`, `"language":null`, 1), docs: validDocs},
		{name: "case-sensitive field", cases: strings.Replace(base, `"case_id"`, `"Case_ID"`, 1), docs: validDocs},
		{name: "unicode escaped duplicate", cases: strings.Replace(base, `"query"`, `"c\u0061se_id":"case-00","query"`, 1), docs: validDocs},
		{name: "BOM", cases: "\ufeff" + base, docs: validDocs},
		{name: "empty line", cases: base + "\n", docs: validDocs},
		{name: "oversize line", cases: strings.Repeat("x", MaxJSONLLineBytes+1) + "\n", docs: validDocs, wantResource: true},
		{name: "query mismatch", cases: strings.Replace(base, "query 00", "different query", 1), docs: validDocs},
		{name: "orphan case", cases: validCases + strings.Replace(base, "case-00", "case-orphan", 1), docs: validDocs},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			casesPath, docsPath, _ := writeLabelingInputs(t, test.cases, test.docs, "unused\n")
			packet, err := BuildBlindLabelPacket(casesPath, docsPath)
			if err == nil || packet != nil {
				t.Fatalf("BuildBlindLabelPacket() = %s, %v", packet, err)
			}
			if test.wantResource && !errors.Is(err, ErrResourceLimit) {
				t.Fatalf("error = %v, want ErrResourceLimit", err)
			}
		})
	}
}

func TestCompileBlindLabelsRejectsStrictJudgmentAndJoinViolations(t *testing.T) {
	files := testCorpusWithCases(t, 2)
	cases := testLabelingCases(files)
	casesBody, docsBody := marshalJSONLines(t, cases), marshalJSONLines(t, files.documents)
	casesPath, docsPath, _ := writeLabelingInputs(t, casesBody, docsBody, "unused\n")
	packet, err := BuildBlindLabelPacket(casesPath, docsPath)
	if err != nil {
		t.Fatal(err)
	}
	valid := testJudgments(t, packet, files.documents)

	t.Run("caller asserted hard", func(t *testing.T) {
		body := strings.Replace(marshalJSONLines(t, valid), `"grades":`, `"hard":true,"grades":`, 1)
		assertCompileRejected(t, casesBody, docsBody, body)
	})
	t.Run("null note", func(t *testing.T) {
		body := strings.Replace(marshalJSONLines(t, valid), `"grades":`, `"note":null,"grades":`, 1)
		assertCompileRejected(t, casesBody, docsBody, body)
	})
	t.Run("duplicate judgment", func(t *testing.T) {
		body := marshalJSONLines(t, append(valid, valid[0]))
		assertCompileRejected(t, casesBody, docsBody, body)
	})
	t.Run("unknown judgment case", func(t *testing.T) {
		mutated := append([]testJudgmentCase(nil), valid...)
		mutated[0].CaseID = "case-orphan"
		assertCompileRejected(t, casesBody, docsBody, marshalJSONLines(t, mutated))
	})
	t.Run("missing judgment", func(t *testing.T) {
		assertCompileRejected(t, casesBody, docsBody, marshalJSONLines(t, valid[:1]))
	})
	t.Run("digest case", func(t *testing.T) {
		mutated := cloneJudgments(valid)
		mutated[0].PacketDigest = strings.ToUpper(mutated[0].PacketDigest)
		assertCompileRejected(t, casesBody, docsBody, marshalJSONLines(t, mutated))
	})
	t.Run("wrong digest", func(t *testing.T) {
		mutated := cloneJudgments(valid)
		mutated[0].PacketDigest = strings.Repeat("0", 64)
		assertCompileRejected(t, casesBody, docsBody, marshalJSONLines(t, mutated))
	})
	t.Run("wrong rubric", func(t *testing.T) {
		mutated := cloneJudgments(valid)
		mutated[0].RubricVersion = "rerank-relevance-judgment-v0"
		assertCompileRejected(t, casesBody, docsBody, marshalJSONLines(t, mutated))
	})
	t.Run("duplicate grade", func(t *testing.T) {
		mutated := cloneJudgments(valid)
		mutated[0].Grades = append(mutated[0].Grades, mutated[0].Grades[0])
		assertCompileRejected(t, casesBody, docsBody, marshalJSONLines(t, mutated))
	})
	for _, grade := range []int{-1, 4} {
		t.Run(fmt.Sprintf("grade %d", grade), func(t *testing.T) {
			mutated := cloneJudgments(valid)
			mutated[0].Grades[0].Grade = grade
			assertCompileRejected(t, casesBody, docsBody, marshalJSONLines(t, mutated))
		})
	}
	t.Run("orphan grade", func(t *testing.T) {
		mutated := cloneJudgments(valid)
		mutated[0].Grades[0].CandidateID = strings.Repeat("f", 64)
		assertCompileRejected(t, casesBody, docsBody, marshalJSONLines(t, mutated))
	})
	t.Run("unicode duplicate judgment key", func(t *testing.T) {
		body := strings.Replace(marshalJSONLines(t, valid), `"grades"`, `"c\u0061se_id":"case-00","grades"`, 1)
		assertCompileRejected(t, casesBody, docsBody, body)
	})
}

func testLabelingCases(files testCorpusFiles) []testLabelingCase {
	rows := make([]testLabelingCase, len(files.documents))
	metadata := make(map[string]testLabelCase, len(files.labels))
	for _, label := range files.labels {
		metadata[label.CaseID] = label
	}
	for index, document := range files.documents {
		label := metadata[document.CaseID]
		rows[index] = testLabelingCase{CaseID: document.CaseID, Query: document.Query, Language: label.Language, Bucket: label.Bucket}
	}
	return rows
}

func testJudgments(t *testing.T, packet []byte, documents []testDocumentCase) []testJudgmentCase {
	t.Helper()
	digests := make(map[string]string)
	for _, row := range decodeBlindPacket(t, packet) {
		digests[row.CaseID] = row.PacketDigest
	}
	rows := make([]testJudgmentCase, len(documents))
	for index, document := range documents {
		row := testJudgmentCase{CaseID: document.CaseID, PacketDigest: digests[document.CaseID], RubricVersion: JudgmentRubricVersion}
		for _, candidate := range document.Candidates {
			grade := 1
			if candidate.ProviderRank == 1 {
				grade = 0
			}
			if candidate.ProviderRank == 6 {
				grade = 2
			}
			row.Grades = append(row.Grades, testGrade{CandidateID: candidate.CandidateID, Grade: grade})
		}
		rows[index] = row
	}
	return rows
}

func cloneJudgments(input []testJudgmentCase) []testJudgmentCase {
	output := append([]testJudgmentCase(nil), input...)
	for index := range output {
		output[index].Grades = append([]testGrade(nil), input[index].Grades...)
	}
	return output
}

func decodeBlindPacket(t *testing.T, packet []byte) []testBlindPacketCase {
	t.Helper()
	trimmed := bytes.TrimSuffix(packet, []byte{'\n'})
	if len(trimmed) == 0 {
		t.Fatal("empty blind packet")
	}
	lines := bytes.Split(trimmed, []byte{'\n'})
	rows := make([]testBlindPacketCase, len(lines))
	for index, line := range lines {
		if err := json.Unmarshal(line, &rows[index]); err != nil {
			t.Fatalf("packet line %d: %v", index+1, err)
		}
	}
	return rows
}

func assertBlindPacketKeys(t *testing.T, packet []byte) {
	t.Helper()
	for lineIndex, line := range bytes.Split(bytes.TrimSuffix(packet, []byte{'\n'}), []byte{'\n'}) {
		var root map[string]json.RawMessage
		if err := json.Unmarshal(line, &root); err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"provider_rank", "url", "language", "bucket", "hard"} {
			if _, present := root[forbidden]; present {
				t.Fatalf("packet line %d exposes %q", lineIndex+1, forbidden)
			}
		}
		var candidates []map[string]json.RawMessage
		if err := json.Unmarshal(root["candidates"], &candidates); err != nil {
			t.Fatal(err)
		}
		for candidateIndex, candidate := range candidates {
			for _, forbidden := range []string{"provider_rank", "url", "language", "bucket", "hard"} {
				if _, present := candidate[forbidden]; present {
					t.Fatalf("packet line %d candidate %d exposes %s", lineIndex+1, candidateIndex, forbidden)
				}
			}
		}
	}
}

func assertCompileRejected(t *testing.T, cases, docs, judgments string) {
	t.Helper()
	casesPath, docsPath, judgmentsPath := writeLabelingInputs(t, cases, docs, judgments)
	labels, err := CompileBlindLabels(casesPath, docsPath, judgmentsPath)
	if err == nil || labels != nil {
		t.Fatalf("CompileBlindLabels() = %s, %v", labels, err)
	}
}

func writeLabelingInputs(t *testing.T, cases, docs, judgments string) (string, string, string) {
	t.Helper()
	directory := t.TempDir()
	paths := []string{
		filepath.Join(directory, "cases.jsonl"),
		filepath.Join(directory, "docs.jsonl"),
		filepath.Join(directory, "judgments.jsonl"),
	}
	for index, body := range []string{cases, docs, judgments} {
		if err := os.WriteFile(paths[index], []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return paths[0], paths[1], paths[2]
}
