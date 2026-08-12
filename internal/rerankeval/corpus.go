// Package rerankeval loads and evaluates the pinned, offline relevance replay
// corpus. It deliberately has no recorder or network dependency: ordinary
// tests can only consume committed inputs and scores.
package rerankeval

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/use-agent/purify/search/rerank"
	"github.com/use-agent/purify/search/rerank/replay"
)

const (
	MaxJSONLLineBytes               = 256 << 10
	MaxJSONLFileBytes               = 16 << 20
	MaxJSONStringBytes              = 64 << 10
	MaxJSONDepth                    = 64
	MaxJSONArrayElements            = 1_024
	MaxJSONNodes                    = 20_000
	MinimumCases                    = 24
	MinimumCandidates               = 240
	MinimumCaseCandidates           = 10
	MaximumRecordingLatencyUS int64 = 3_600_000_000
	maxTokenCount                   = 1_000_000
)

const (
	LanguageEnglish = "en"
	LanguageChinese = "zh"

	BucketLexical         = "lexical"
	BucketSemantic        = "semantic"
	BucketEntityCollision = "entity_collision"
	BucketNumericRecency  = "numeric_recency"
	BucketLongNoisy       = "long_noisy"
	BucketPromptInjection = "prompt_injection"
)

var (
	ErrInvalidCorpus = errors.New("rerankeval: invalid corpus")
	ErrResourceLimit = errors.New("rerankeval: resource limit")

	languages = [...]string{LanguageEnglish, LanguageChinese}
	buckets   = [...]string{BucketLexical, BucketSemantic, BucketEntityCollision, BucketNumericRecency, BucketLongNoisy, BucketPromptInjection}
)

// Languages returns the closed language enumeration in scorecard order.
func Languages() []string {
	return append([]string(nil), languages[:]...)
}

// Buckets returns the closed construction-bucket enumeration in scorecard
// order.
func Buckets() []string {
	return append([]string(nil), buckets[:]...)
}

// LoadOptions binds recordings to the already-reviewed manifest. The loader
// never accepts a manifest identifier from a recording as authority.
type LoadOptions struct {
	ExpectedManifestID string
}

type Usage struct {
	PromptTokens int64
	TotalTokens  int64
}

type Candidate struct {
	ID             string
	CanonicalURL   string
	ProviderRank   int
	Title          string
	Snippet        string
	Grade          int
	RelevanceScore float64
}

type Case struct {
	ID              string
	Query           string
	Language        string
	Bucket          string
	Hard            bool
	Note            string
	ManifestID      string
	InputDigest     string
	CandidateDigest string
	Usage           Usage
	LatencyUS       int64
	Candidates      []Candidate
}

type Corpus struct {
	Cases []Case
}

// RecordingInput is the label-free input consumed by the explicit live
// recorder. Gold grades never cross this boundary.
type RecordingInput struct {
	CaseID  string
	Request rerank.ScoreRequest
}

// LoadRecordingInputs strictly loads only docs.jsonl and constructs each exact
// production request. It deliberately never reads labels.jsonl, preventing a
// recorder from conditioning model calls on gold grades.
func LoadRecordingInputs(docsPath string) ([]RecordingInput, error) {
	documentLines, err := readJSONLines(docsPath)
	if err != nil {
		return nil, fmt.Errorf("docs.jsonl: %w", err)
	}
	if len(documentLines) < MinimumCases {
		return nil, fmt.Errorf("%w: need at least %d recording cases", ErrInvalidCorpus, MinimumCases)
	}
	documents := make(map[string]documentCase, len(documentLines))
	seenQueries := make(map[string]struct{}, len(documentLines))
	totalCandidates := 0
	for index, line := range documentLines {
		document, parseErr := parseDocumentCase(line)
		if parseErr != nil {
			return nil, fmt.Errorf("docs.jsonl line %d: %w", index+1, parseErr)
		}
		if _, duplicate := documents[document.id]; duplicate {
			return nil, fmt.Errorf("%w: duplicate document case %q", ErrInvalidCorpus, document.id)
		}
		if _, duplicate := seenQueries[document.query]; duplicate {
			return nil, fmt.Errorf("%w: duplicate recording query", ErrInvalidCorpus)
		}
		documents[document.id] = document
		seenQueries[document.query] = struct{}{}
		totalCandidates += len(document.candidates)
	}
	if totalCandidates < MinimumCandidates {
		return nil, fmt.Errorf("%w: need at least %d recording candidates", ErrInvalidCorpus, MinimumCandidates)
	}
	caseIDs := make([]string, 0, len(documents))
	for caseID := range documents {
		caseIDs = append(caseIDs, caseID)
	}
	sort.Strings(caseIDs)
	inputs := make([]RecordingInput, len(caseIDs))
	for index, caseID := range caseIDs {
		document := documents[caseID]
		candidates := make([]rerank.Candidate, len(document.candidates))
		for candidateIndex, candidate := range document.candidates {
			candidates[candidateIndex] = rerank.Candidate{
				CanonicalURL: candidate.canonicalURL,
				ProviderRank: candidate.providerRank,
				Title:        strings.Clone(candidate.title),
				Snippet:      strings.Clone(candidate.snippet),
			}
		}
		request, buildErr := rerank.BuildRequest(document.query, candidates)
		if buildErr != nil {
			return nil, fmt.Errorf("%w: case %q production request", ErrInvalidCorpus, caseID)
		}
		inputs[index] = RecordingInput{CaseID: strings.Clone(caseID), Request: cloneRecordingRequest(request)}
	}
	return inputs, nil
}

// EncodeRecordingLine emits the exact recordings.jsonl row for one strict
// observation. Candidate scores are canonicalized back to production input
// order; response ordering and caller-provided fixture hashes are ignored.
func EncodeRecordingLine(input RecordingInput, recording rerank.ReferenceRecording) ([]byte, error) {
	if !validPlainID(input.CaseID) || len(input.Request.Candidates) < MinimumCaseCandidates ||
		len(input.Request.Candidates) > rerank.MaxCandidates || recording.LatencyUS <= 0 ||
		recording.LatencyUS > MaximumRecordingLatencyUS {
		return nil, ErrInvalidCorpus
	}
	inputDigest, err := rerank.ReferenceInputDigest(input.Request)
	candidateDigest, candidateErr := rerank.ReferenceCandidateDigest(input.Request)
	if err != nil || candidateErr != nil || recording.InputDigest != inputDigest || recording.CandidateDigest != candidateDigest ||
		recording.Observation.Usage.PromptTokens < 0 || recording.Observation.Usage.PromptTokens > maxTokenCount ||
		recording.Observation.Usage.TotalTokens != recording.Observation.Usage.PromptTokens ||
		len(recording.Observation.Scores) != len(input.Request.Candidates) {
		return nil, ErrInvalidCorpus
	}
	scoreByID := make(map[string]float64, len(recording.Observation.Scores))
	for _, score := range recording.Observation.Scores {
		if !validPlainID(score.StableID) || math.IsNaN(score.RelevanceScore) || math.IsInf(score.RelevanceScore, 0) ||
			score.RelevanceScore < 0 || score.RelevanceScore > 1 {
			return nil, ErrInvalidCorpus
		}
		if _, duplicate := scoreByID[score.StableID]; duplicate {
			return nil, ErrInvalidCorpus
		}
		scoreByID[score.StableID] = score.RelevanceScore
	}
	type encodedScore struct {
		CandidateID string  `json:"candidate_id"`
		Score       float64 `json:"relevance_score"`
	}
	type encodedUsage struct {
		PromptTokens int64 `json:"prompt_tokens"`
		TotalTokens  int64 `json:"total_tokens"`
	}
	type encodedRecording struct {
		CaseID          string         `json:"case_id"`
		ManifestID      string         `json:"manifest_id"`
		InputDigest     string         `json:"input_digest"`
		CandidateDigest string         `json:"candidate_digest"`
		Scores          []encodedScore `json:"scores"`
		Usage           encodedUsage   `json:"usage"`
		LatencyUS       int64          `json:"latency_us"`
	}
	manifest, manifestErr := replay.ReferenceManifest()
	if manifestErr != nil {
		return nil, ErrInvalidCorpus
	}
	row := encodedRecording{
		CaseID: strings.Clone(input.CaseID), ManifestID: manifest.ManifestID, InputDigest: strings.Clone(inputDigest),
		CandidateDigest: strings.Clone(candidateDigest),
		Scores:          make([]encodedScore, len(input.Request.Candidates)),
		Usage:           encodedUsage{PromptTokens: recording.Observation.Usage.PromptTokens, TotalTokens: recording.Observation.Usage.TotalTokens},
		LatencyUS:       recording.LatencyUS,
	}
	for index, candidate := range input.Request.Candidates {
		score, present := scoreByID[candidate.StableID]
		if !present {
			return nil, ErrInvalidCorpus
		}
		if score == 0 {
			score = 0
		}
		row.Scores[index] = encodedScore{CandidateID: strings.Clone(candidate.StableID), Score: score}
	}
	encoded, err := json.Marshal(row)
	if err != nil || len(encoded) > MaxJSONLLineBytes {
		return nil, ErrResourceLimit
	}
	return encoded, nil
}

func cloneRecordingRequest(source rerank.ScoreRequest) rerank.ScoreRequest {
	cloned := rerank.ScoreRequest{Query: strings.Clone(source.Query), Candidates: make([]rerank.ScoringCandidate, len(source.Candidates))}
	for index, candidate := range source.Candidates {
		cloned.Candidates[index] = rerank.ScoringCandidate{
			StableID: strings.Clone(candidate.StableID), ProviderRank: candidate.ProviderRank, Text: strings.Clone(candidate.Text),
		}
	}
	return cloned
}

type documentCase struct {
	id         string
	query      string
	candidates []documentCandidate
}

type documentCandidate struct {
	id           string
	canonicalURL string
	providerRank int
	title        string
	snippet      string
}

type labelCase struct {
	id       string
	language string
	bucket   string
	hard     bool
	note     string
	grades   map[string]int
}

type recordingCase struct {
	id              string
	manifestID      string
	inputDigest     string
	candidateDigest string
	scores          map[string]float64
	usage           Usage
	latencyUS       int64
}

// LoadCorpus performs a strict, bounded three-way JSONL join and reconstructs
// every production scorer input before accepting its digest.
func LoadCorpus(docsPath, labelsPath, recordingsPath string, options LoadOptions) (Corpus, error) {
	reference, referenceErr := replay.ReferenceManifest()
	if referenceErr != nil || options.ExpectedManifestID != reference.ManifestID {
		return Corpus{}, fmt.Errorf("%w: expected manifest id", ErrInvalidCorpus)
	}
	documentLines, err := readJSONLines(docsPath)
	if err != nil {
		return Corpus{}, fmt.Errorf("docs.jsonl: %w", err)
	}
	labelLines, err := readJSONLines(labelsPath)
	if err != nil {
		return Corpus{}, fmt.Errorf("labels.jsonl: %w", err)
	}
	recordingLines, err := readJSONLines(recordingsPath)
	if err != nil {
		return Corpus{}, fmt.Errorf("recordings.jsonl: %w", err)
	}

	documents := make(map[string]documentCase, len(documentLines))
	seenQueries := make(map[string]string, len(documentLines))
	for index, line := range documentLines {
		row, parseErr := parseDocumentCase(line)
		if parseErr != nil {
			return Corpus{}, fmt.Errorf("docs.jsonl line %d: %w", index+1, parseErr)
		}
		if _, duplicate := documents[row.id]; duplicate {
			return Corpus{}, fmt.Errorf("%w: duplicate document case %q", ErrInvalidCorpus, row.id)
		}
		if prior, duplicate := seenQueries[row.query]; duplicate {
			return Corpus{}, fmt.Errorf("%w: cases %q and %q repeat a query", ErrInvalidCorpus, prior, row.id)
		}
		documents[row.id] = row
		seenQueries[row.query] = row.id
	}

	labels := make(map[string]labelCase, len(labelLines))
	for index, line := range labelLines {
		row, parseErr := parseLabelCase(line)
		if parseErr != nil {
			return Corpus{}, fmt.Errorf("labels.jsonl line %d: %w", index+1, parseErr)
		}
		if _, duplicate := labels[row.id]; duplicate {
			return Corpus{}, fmt.Errorf("%w: duplicate label case %q", ErrInvalidCorpus, row.id)
		}
		labels[row.id] = row
	}

	recordings := make(map[string]recordingCase, len(recordingLines))
	for index, line := range recordingLines {
		row, parseErr := parseRecordingCase(line)
		if parseErr != nil {
			return Corpus{}, fmt.Errorf("recordings.jsonl line %d: %w", index+1, parseErr)
		}
		if row.manifestID != options.ExpectedManifestID {
			return Corpus{}, fmt.Errorf("%w: case %q manifest does not match expected manifest", ErrInvalidCorpus, row.id)
		}
		if _, duplicate := recordings[row.id]; duplicate {
			return Corpus{}, fmt.Errorf("%w: duplicate recording case %q", ErrInvalidCorpus, row.id)
		}
		recordings[row.id] = row
	}

	if len(documents) != len(labels) || len(documents) != len(recordings) {
		return Corpus{}, fmt.Errorf("%w: three-way case counts differ", ErrInvalidCorpus)
	}
	caseIDs := make([]string, 0, len(documents))
	for id := range documents {
		if _, present := labels[id]; !present {
			return Corpus{}, fmt.Errorf("%w: case %q has no labels", ErrInvalidCorpus, id)
		}
		if _, present := recordings[id]; !present {
			return Corpus{}, fmt.Errorf("%w: case %q has no recording", ErrInvalidCorpus, id)
		}
		caseIDs = append(caseIDs, id)
	}
	for id := range labels {
		if _, present := documents[id]; !present {
			return Corpus{}, fmt.Errorf("%w: orphan label case %q", ErrInvalidCorpus, id)
		}
	}
	for id := range recordings {
		if _, present := documents[id]; !present {
			return Corpus{}, fmt.Errorf("%w: orphan recording case %q", ErrInvalidCorpus, id)
		}
	}
	sort.Strings(caseIDs)

	corpus := Corpus{Cases: make([]Case, 0, len(caseIDs))}
	for _, id := range caseIDs {
		joined, joinErr := joinCase(documents[id], labels[id], recordings[id])
		if joinErr != nil {
			return Corpus{}, fmt.Errorf("case %q: %w", id, joinErr)
		}
		corpus.Cases = append(corpus.Cases, joined)
	}
	if err := validateConstructionCorpus(corpus); err != nil {
		return Corpus{}, err
	}
	return corpus, nil
}

func joinCase(document documentCase, label labelCase, recording recordingCase) (Case, error) {
	if len(label.grades) != len(document.candidates) || len(recording.scores) != len(document.candidates) {
		return Case{}, fmt.Errorf("%w: candidate join counts differ", ErrInvalidCorpus)
	}
	input := make([]rerank.Candidate, len(document.candidates))
	joined := Case{
		ID:              strings.Clone(document.id),
		Query:           strings.Clone(document.query),
		Language:        strings.Clone(label.language),
		Bucket:          strings.Clone(label.bucket),
		Hard:            label.hard,
		Note:            strings.Clone(label.note),
		ManifestID:      strings.Clone(recording.manifestID),
		InputDigest:     strings.Clone(recording.inputDigest),
		CandidateDigest: strings.Clone(recording.candidateDigest),
		Usage:           recording.usage,
		LatencyUS:       recording.latencyUS,
		Candidates:      make([]Candidate, len(document.candidates)),
	}
	for index, candidate := range document.candidates {
		grade, labelled := label.grades[candidate.id]
		score, recorded := recording.scores[candidate.id]
		if !labelled || !recorded {
			return Case{}, fmt.Errorf("%w: candidate %q is not exactly joined", ErrInvalidCorpus, candidate.id)
		}
		input[index] = rerank.Candidate{
			CanonicalURL: candidate.canonicalURL,
			ProviderRank: candidate.providerRank,
			Title:        candidate.title,
			Snippet:      candidate.snippet,
		}
		joined.Candidates[index] = Candidate{
			ID:             strings.Clone(candidate.id),
			CanonicalURL:   strings.Clone(candidate.canonicalURL),
			ProviderRank:   candidate.providerRank,
			Title:          strings.Clone(candidate.title),
			Snippet:        strings.Clone(candidate.snippet),
			Grade:          grade,
			RelevanceScore: score,
		}
	}
	request, err := rerank.BuildRequest(document.query, input)
	if err != nil {
		return Case{}, fmt.Errorf("%w: production request builder rejected candidates", ErrInvalidCorpus)
	}
	digest, err := rerank.ReferenceInputDigest(request)
	if err != nil || digest != recording.inputDigest {
		return Case{}, fmt.Errorf("%w: production input digest mismatch", ErrInvalidCorpus)
	}
	candidateDigest, err := rerank.ReferenceCandidateDigest(request)
	if err != nil || candidateDigest != recording.candidateDigest {
		return Case{}, fmt.Errorf("%w: production candidate digest mismatch", ErrInvalidCorpus)
	}
	if objectiveHard(joined.Candidates) != joined.Hard {
		return Case{}, fmt.Errorf("%w: hard label does not equal objective hard predicate", ErrInvalidCorpus)
	}
	return joined, nil
}

func parseDocumentCase(raw []byte) (documentCase, error) {
	root, err := exactObject(raw, []string{"case_id", "query", "candidates"}, nil)
	if err != nil {
		return documentCase{}, err
	}
	id, err := fixtureID(root["case_id"])
	if err != nil {
		return documentCase{}, fmt.Errorf("case_id: %w", err)
	}
	query, err := jsonString(root["query"])
	if err != nil {
		return documentCase{}, fmt.Errorf("query: %w", err)
	}
	rawCandidates, err := rawArray(root["candidates"], MinimumCaseCandidates, rerank.MaxCandidates)
	if err != nil {
		return documentCase{}, fmt.Errorf("candidates: %w", err)
	}
	row := documentCase{id: id, query: query, candidates: make([]documentCandidate, len(rawCandidates))}
	seenIDs := make(map[string]struct{}, len(rawCandidates))
	seenRanks := make([]bool, len(rawCandidates))
	for index, rawCandidate := range rawCandidates {
		fields, objectErr := exactObject(rawCandidate, []string{"candidate_id", "url", "provider_rank", "title", "snippet"}, nil)
		if objectErr != nil {
			return documentCase{}, fmt.Errorf("candidate %d: %w", index, objectErr)
		}
		candidateID, idErr := fixtureID(fields["candidate_id"])
		canonicalURL, urlErr := jsonString(fields["url"])
		providerRank, rankErr := jsonInt(fields["provider_rank"])
		title, titleErr := jsonString(fields["title"])
		snippet, snippetErr := jsonString(fields["snippet"])
		if idErr != nil || urlErr != nil || rankErr != nil || titleErr != nil || snippetErr != nil {
			return documentCase{}, fmt.Errorf("%w: candidate %d has an invalid field", ErrInvalidCorpus, index)
		}
		stableID, stableErr := rerank.CandidateID(canonicalURL)
		if stableErr != nil || candidateID != stableID {
			return documentCase{}, fmt.Errorf("%w: candidate %d id does not match canonical URL", ErrInvalidCorpus, index)
		}
		if _, duplicate := seenIDs[candidateID]; duplicate {
			return documentCase{}, fmt.Errorf("%w: duplicate candidate id %q", ErrInvalidCorpus, candidateID)
		}
		if providerRank < 1 || providerRank > len(rawCandidates) || seenRanks[providerRank-1] {
			return documentCase{}, fmt.Errorf("%w: provider ranks must be exactly 1..N", ErrInvalidCorpus)
		}
		seenIDs[candidateID] = struct{}{}
		seenRanks[providerRank-1] = true
		row.candidates[index] = documentCandidate{id: candidateID, canonicalURL: canonicalURL, providerRank: providerRank, title: title, snippet: snippet}
	}
	for _, present := range seenRanks {
		if !present {
			return documentCase{}, fmt.Errorf("%w: provider ranks must be exactly 1..N", ErrInvalidCorpus)
		}
	}
	sort.Slice(row.candidates, func(left, right int) bool {
		return row.candidates[left].providerRank < row.candidates[right].providerRank
	})
	return row, nil
}

func parseLabelCase(raw []byte) (labelCase, error) {
	root, err := exactObject(raw, []string{"case_id", "language", "bucket", "hard", "rubric_version", "grades"}, []string{"note"})
	if err != nil {
		return labelCase{}, err
	}
	id, idErr := fixtureID(root["case_id"])
	language, languageErr := jsonString(root["language"])
	bucket, bucketErr := jsonString(root["bucket"])
	hard, hardErr := jsonBool(root["hard"])
	rubric, rubricErr := jsonString(root["rubric_version"])
	if idErr != nil || languageErr != nil || bucketErr != nil || hardErr != nil || rubricErr != nil ||
		rubric != JudgmentRubricVersion || !validLanguage(language) || !validBucket(bucket) {
		return labelCase{}, fmt.Errorf("%w: invalid label header", ErrInvalidCorpus)
	}
	note := ""
	if rawNote, present := root["note"]; present {
		note, err = jsonString(rawNote)
		if err != nil {
			return labelCase{}, fmt.Errorf("note: %w", err)
		}
	}
	rawGrades, err := rawArray(root["grades"], MinimumCaseCandidates, rerank.MaxCandidates)
	if err != nil {
		return labelCase{}, fmt.Errorf("grades: %w", err)
	}
	row := labelCase{id: id, language: language, bucket: bucket, hard: hard, note: note, grades: make(map[string]int, len(rawGrades))}
	for index, rawGrade := range rawGrades {
		fields, objectErr := exactObject(rawGrade, []string{"candidate_id", "grade"}, nil)
		if objectErr != nil {
			return labelCase{}, fmt.Errorf("grade %d: %w", index, objectErr)
		}
		candidateID, candidateErr := fixtureID(fields["candidate_id"])
		grade, gradeErr := jsonInt(fields["grade"])
		if candidateErr != nil || gradeErr != nil || grade < 0 || grade > 3 {
			return labelCase{}, fmt.Errorf("%w: invalid grade %d", ErrInvalidCorpus, index)
		}
		if _, duplicate := row.grades[candidateID]; duplicate {
			return labelCase{}, fmt.Errorf("%w: duplicate grade candidate %q", ErrInvalidCorpus, candidateID)
		}
		row.grades[candidateID] = grade
	}
	return row, nil
}

func parseRecordingCase(raw []byte) (recordingCase, error) {
	root, err := exactObject(raw, []string{"case_id", "manifest_id", "input_digest", "candidate_digest", "scores", "usage", "latency_us"}, nil)
	if err != nil {
		return recordingCase{}, err
	}
	id, idErr := fixtureID(root["case_id"])
	manifestID, manifestErr := jsonString(root["manifest_id"])
	inputDigest, digestErr := jsonString(root["input_digest"])
	candidateDigest, candidateDigestErr := jsonString(root["candidate_digest"])
	latencyUS, latencyErr := jsonInt64(root["latency_us"])
	if idErr != nil || manifestErr != nil || digestErr != nil || candidateDigestErr != nil || latencyErr != nil ||
		!validDigest(manifestID) || !validDigest(inputDigest) || !validDigest(candidateDigest) ||
		latencyUS <= 0 || latencyUS > MaximumRecordingLatencyUS {
		return recordingCase{}, fmt.Errorf("%w: invalid recording header", ErrInvalidCorpus)
	}
	usageFields, err := exactObject(root["usage"], []string{"prompt_tokens", "total_tokens"}, nil)
	if err != nil {
		return recordingCase{}, fmt.Errorf("usage: %w", err)
	}
	promptTokens, promptErr := jsonInt64(usageFields["prompt_tokens"])
	totalTokens, totalErr := jsonInt64(usageFields["total_tokens"])
	if promptErr != nil || totalErr != nil || promptTokens < 0 || promptTokens > maxTokenCount || totalTokens != promptTokens {
		return recordingCase{}, fmt.Errorf("%w: invalid usage", ErrInvalidCorpus)
	}
	rawScores, err := rawArray(root["scores"], MinimumCaseCandidates, rerank.MaxCandidates)
	if err != nil {
		return recordingCase{}, fmt.Errorf("scores: %w", err)
	}
	row := recordingCase{
		id: id, manifestID: manifestID, inputDigest: inputDigest, candidateDigest: candidateDigest,
		scores: make(map[string]float64, len(rawScores)),
		usage:  Usage{PromptTokens: promptTokens, TotalTokens: totalTokens}, latencyUS: latencyUS,
	}
	for index, rawScore := range rawScores {
		fields, objectErr := exactObject(rawScore, []string{"candidate_id", "relevance_score"}, nil)
		if objectErr != nil {
			return recordingCase{}, fmt.Errorf("score %d: %w", index, objectErr)
		}
		candidateID, candidateErr := fixtureID(fields["candidate_id"])
		score, scoreErr := jsonFloat(fields["relevance_score"])
		if candidateErr != nil || scoreErr != nil || math.IsNaN(score) || math.IsInf(score, 0) || score < 0 || score > 1 {
			return recordingCase{}, fmt.Errorf("%w: invalid score %d", ErrInvalidCorpus, index)
		}
		if _, duplicate := row.scores[candidateID]; duplicate {
			return recordingCase{}, fmt.Errorf("%w: duplicate score candidate %q", ErrInvalidCorpus, candidateID)
		}
		if score == 0 {
			score = 0
		}
		row.scores[candidateID] = score
	}
	return row, nil
}

func readJSONLines(path string) ([][]byte, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: empty path", ErrInvalidCorpus)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, MaxJSONLFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxJSONLFileBytes {
		return nil, fmt.Errorf("%w: file exceeds %d bytes", ErrResourceLimit, MaxJSONLFileBytes)
	}
	if len(raw) == 0 || !utf8.Valid(raw) || bytes.Contains(raw, []byte{0xef, 0xbb, 0xbf}) {
		return nil, fmt.Errorf("%w: empty, invalid UTF-8, or BOM-bearing file", ErrInvalidCorpus)
	}
	lines := bytes.Split(raw, []byte{'\n'})
	if len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("%w: no JSONL records", ErrInvalidCorpus)
	}
	for index, line := range lines {
		if len(line) == 0 || len(bytes.TrimSpace(line)) == 0 {
			return nil, fmt.Errorf("%w: empty line %d", ErrInvalidCorpus, index+1)
		}
		if len(line) > MaxJSONLLineBytes {
			return nil, fmt.Errorf("%w: line %d exceeds %d bytes", ErrResourceLimit, index+1, MaxJSONLLineBytes)
		}
		if err := validateJSON(line); err != nil {
			return nil, fmt.Errorf("line %d: %w", index+1, err)
		}
	}
	return lines, nil
}

func validateJSON(raw []byte) error {
	if err := validateUTF16Escapes(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	nodes := 0
	if err := consumeJSONValue(decoder, 0, &nodes); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing JSON value", ErrInvalidCorpus)
	}
	return nil
}

// encoding/json replaces an unpaired UTF-16 surrogate escape with U+FFFD.
// Fixtures must reject that lossy normalization while accepting exact pairs.
func validateUTF16Escapes(raw []byte) error {
	inString := false
	for index := 0; index < len(raw); index++ {
		switch raw[index] {
		case '"':
			inString = !inString
		case '\\':
			if !inString || index+1 >= len(raw) {
				continue
			}
			if raw[index+1] != 'u' {
				index++
				continue
			}
			unit, ok := parseHexQuad(raw, index+2)
			if !ok {
				return fmt.Errorf("%w: malformed UTF-16 escape", ErrInvalidCorpus)
			}
			if unit >= 0xd800 && unit <= 0xdbff {
				if index+11 >= len(raw) || raw[index+6] != '\\' || raw[index+7] != 'u' {
					return fmt.Errorf("%w: unpaired high UTF-16 surrogate", ErrInvalidCorpus)
				}
				low, lowOK := parseHexQuad(raw, index+8)
				if !lowOK || low < 0xdc00 || low > 0xdfff {
					return fmt.Errorf("%w: unpaired high UTF-16 surrogate", ErrInvalidCorpus)
				}
				index += 11
				continue
			}
			if unit >= 0xdc00 && unit <= 0xdfff {
				return fmt.Errorf("%w: unpaired low UTF-16 surrogate", ErrInvalidCorpus)
			}
			index += 5
		}
	}
	return nil
}

func parseHexQuad(raw []byte, start int) (uint16, bool) {
	if start < 0 || start+4 > len(raw) {
		return 0, false
	}
	var value uint16
	for _, digit := range raw[start : start+4] {
		value <<= 4
		switch {
		case digit >= '0' && digit <= '9':
			value |= uint16(digit - '0')
		case digit >= 'a' && digit <= 'f':
			value |= uint16(digit-'a') + 10
		case digit >= 'A' && digit <= 'F':
			value |= uint16(digit-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func consumeJSONValue(decoder *json.Decoder, depth int, nodes *int) error {
	if depth > MaxJSONDepth {
		return fmt.Errorf("%w: JSON depth exceeds %d", ErrResourceLimit, MaxJSONDepth)
	}
	(*nodes)++
	if *nodes > MaxJSONNodes {
		return fmt.Errorf("%w: JSON nodes exceed %d", ErrResourceLimit, MaxJSONNodes)
	}
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("%w: malformed JSON", ErrInvalidCorpus)
	}
	if text, ok := token.(string); ok && len(text) > MaxJSONStringBytes {
		return fmt.Errorf("%w: JSON string exceeds %d bytes", ErrResourceLimit, MaxJSONStringBytes)
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		fields := 0
		for decoder.More() {
			keyToken, keyErr := decoder.Token()
			key, ok := keyToken.(string)
			if keyErr != nil || !ok || len(key) > MaxJSONStringBytes {
				return fmt.Errorf("%w: invalid JSON object key", ErrInvalidCorpus)
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("%w: duplicate JSON object key", ErrInvalidCorpus)
			}
			seen[key] = struct{}{}
			fields++
			if fields > MaxJSONArrayElements {
				return fmt.Errorf("%w: object has too many fields", ErrResourceLimit)
			}
			if err := consumeJSONValue(decoder, depth+1, nodes); err != nil {
				return err
			}
		}
		closing, closeErr := decoder.Token()
		if closeErr != nil || closing != json.Delim('}') {
			return fmt.Errorf("%w: malformed JSON object", ErrInvalidCorpus)
		}
	case '[':
		elements := 0
		for decoder.More() {
			elements++
			if elements > MaxJSONArrayElements {
				return fmt.Errorf("%w: JSON array exceeds %d elements", ErrResourceLimit, MaxJSONArrayElements)
			}
			if err := consumeJSONValue(decoder, depth+1, nodes); err != nil {
				return err
			}
		}
		closing, closeErr := decoder.Token()
		if closeErr != nil || closing != json.Delim(']') {
			return fmt.Errorf("%w: malformed JSON array", ErrInvalidCorpus)
		}
	default:
		return fmt.Errorf("%w: unexpected JSON delimiter", ErrInvalidCorpus)
	}
	return nil
}

func exactObject(raw []byte, required, optional []string) (map[string]json.RawMessage, error) {
	if jsonNull(raw) {
		return nil, fmt.Errorf("%w: object is null", ErrInvalidCorpus)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, fmt.Errorf("%w: expected object", ErrInvalidCorpus)
	}
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, field := range required {
		allowed[field] = true
	}
	for _, field := range optional {
		allowed[field] = false
	}
	for field, value := range object {
		if _, ok := allowed[field]; !ok || len(value) == 0 || jsonNull(value) {
			return nil, fmt.Errorf("%w: unknown or null field %q", ErrInvalidCorpus, field)
		}
	}
	for _, field := range required {
		if _, present := object[field]; !present {
			return nil, fmt.Errorf("%w: missing field %q", ErrInvalidCorpus, field)
		}
	}
	return object, nil
}

func rawArray(raw []byte, minimum, maximum int) ([]json.RawMessage, error) {
	if jsonNull(raw) {
		return nil, fmt.Errorf("%w: array is null", ErrInvalidCorpus)
	}
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || values == nil || len(values) < minimum || len(values) > maximum {
		return nil, fmt.Errorf("%w: invalid array length", ErrInvalidCorpus)
	}
	return values, nil
}

func jsonString(raw []byte) (string, error) {
	if jsonNull(raw) {
		return "", ErrInvalidCorpus
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || !utf8.ValidString(value) || len(value) > MaxJSONStringBytes {
		return "", ErrInvalidCorpus
	}
	return strings.Clone(value), nil
}

func fixtureID(raw []byte) (string, error) {
	value, err := jsonString(raw)
	if err != nil || value == "" || strings.TrimSpace(value) != value {
		return "", ErrInvalidCorpus
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return "", ErrInvalidCorpus
		}
	}
	return value, nil
}

func jsonInt(raw []byte) (int, error) {
	if jsonNull(raw) {
		return 0, ErrInvalidCorpus
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, ErrInvalidCorpus
	}
	return value, nil
}

func jsonInt64(raw []byte) (int64, error) {
	if jsonNull(raw) {
		return 0, ErrInvalidCorpus
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, ErrInvalidCorpus
	}
	return value, nil
}

func jsonFloat(raw []byte) (float64, error) {
	if jsonNull(raw) {
		return 0, ErrInvalidCorpus
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, ErrInvalidCorpus
	}
	return value, nil
}

func jsonBool(raw []byte) (bool, error) {
	if jsonNull(raw) {
		return false, ErrInvalidCorpus
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, ErrInvalidCorpus
	}
	return value, nil
}

func jsonNull(raw []byte) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func validDigest(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}

func validLanguage(value string) bool {
	return value == LanguageEnglish || value == LanguageChinese
}

func validBucket(value string) bool {
	for _, candidate := range buckets {
		if value == candidate {
			return true
		}
	}
	return false
}

func objectiveHard(candidates []Candidate) bool {
	providerTopFiveDistractor := false
	laterRelevantCandidate := false
	for _, candidate := range candidates {
		if candidate.ProviderRank <= 5 && candidate.Grade == 0 {
			providerTopFiveDistractor = true
		}
		if candidate.ProviderRank >= 6 && candidate.Grade >= 2 {
			laterRelevantCandidate = true
		}
	}
	return providerTopFiveDistractor && laterRelevantCandidate
}
