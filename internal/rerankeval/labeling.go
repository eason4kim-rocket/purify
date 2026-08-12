package rerankeval

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/use-agent/purify/search/rerank"
)

const (
	// JudgmentRubricVersion binds every judgment and compiled label to the
	// relevance rubric below. A judge sees only query, title, and snippet.
	JudgmentRubricVersion = "rerank-relevance-judgment-v1"

	// JudgmentGradeDirect means the candidate directly and sufficiently answers
	// the query.
	JudgmentGradeDirect = 3
	// JudgmentGradeSubstantial means the candidate contains the core answer but
	// is incomplete or indirect.
	JudgmentGradeSubstantial = 2
	// JudgmentGradeBackground means the candidate is topical background only.
	JudgmentGradeBackground = 1
	// JudgmentGradeIrrelevant means the candidate is irrelevant, the wrong
	// entity, conflicts on a numeric or recency constraint, or is prompt-
	// injection noise.
	JudgmentGradeIrrelevant = 0

	blindPacketDigestDomain = "rerankeval-blind-packet-v1\x00"
)

type labelingCase struct {
	id       string
	query    string
	language string
	bucket   string
}

type blindJudgment struct {
	id            string
	packetDigest  string
	rubricVersion string
	grades        map[string]int
	note          string
	notePresent   bool
}

type blindPacketRow struct {
	CaseID       string                 `json:"case_id"`
	PacketDigest string                 `json:"packet_digest"`
	Query        string                 `json:"query"`
	Candidates   []blindPacketCandidate `json:"candidates"`
}

type blindPacketCandidate struct {
	CandidateID string `json:"candidate_id"`
	Title       string `json:"title"`
	Snippet     string `json:"snippet"`
}

type compiledLabelRow struct {
	CaseID        string               `json:"case_id"`
	Language      string               `json:"language"`
	Bucket        string               `json:"bucket"`
	Hard          bool                 `json:"hard"`
	RubricVersion string               `json:"rubric_version"`
	Grades        []compiledLabelGrade `json:"grades"`
	Note          *string              `json:"note,omitempty"`
}

type compiledLabelGrade struct {
	CandidateID string `json:"candidate_id"`
	Grade       int    `json:"grade"`
}

// JudgedInputsSummary reports strict docs/labels join counts without exposing
// mutable package state. It is intended for the later CLI construction gate.
type JudgedInputsSummary struct {
	Cases               int
	Candidates          int
	LanguageBucketCases map[string]int
	HardCasesByLanguage map[string]int
	HardCasesByBucket   map[string]int
}

// BuildBlindLabelPacket constructs canonical JSONL for an independent judge.
// Provider rank, URL, language, construction bucket, and hard-case status are
// deliberately absent. Cases and candidates use UTF-8 byte order.
func BuildBlindLabelPacket(casesPath, docsPath string) ([]byte, error) {
	cases, documents, caseIDs, err := loadLabelingInputs(casesPath, docsPath)
	if err != nil {
		return nil, err
	}
	encoded := make([][]byte, 0, len(caseIDs))
	for _, caseID := range caseIDs {
		row := makeBlindPacketRow(cases[caseID], documents[caseID])
		line, marshalErr := json.Marshal(row)
		if marshalErr != nil {
			return nil, fmt.Errorf("%w: encode blind packet", ErrInvalidCorpus)
		}
		encoded = append(encoded, line)
	}
	return encodeStrictJSONLines(encoded)
}

// CompileBlindLabels strictly joins cases, source documents, and independently
// supplied judgments into the labels schema. Hard is derived only from source
// provider ranks and grades; judgments cannot assert it.
func CompileBlindLabels(casesPath, docsPath, judgmentsPath string) ([]byte, error) {
	cases, documents, caseIDs, err := loadLabelingInputs(casesPath, docsPath)
	if err != nil {
		return nil, err
	}
	judgments, err := loadBlindJudgments(judgmentsPath)
	if err != nil {
		return nil, err
	}
	if len(judgments) != len(documents) {
		return nil, fmt.Errorf("%w: judgment and document case counts differ", ErrInvalidCorpus)
	}
	for caseID := range judgments {
		if _, present := documents[caseID]; !present {
			return nil, fmt.Errorf("%w: orphan judgment case %q", ErrInvalidCorpus, caseID)
		}
	}

	encoded := make([][]byte, 0, len(caseIDs))
	for _, caseID := range caseIDs {
		metadata := cases[caseID]
		document := documents[caseID]
		judgment, present := judgments[caseID]
		if !present {
			return nil, fmt.Errorf("%w: case %q has no judgment", ErrInvalidCorpus, caseID)
		}
		packet := makeBlindPacketRow(metadata, document)
		if judgment.packetDigest != packet.PacketDigest || judgment.rubricVersion != JudgmentRubricVersion {
			return nil, fmt.Errorf("%w: case %q judgment does not match blind packet and rubric", ErrInvalidCorpus, caseID)
		}
		row, joinErr := compileLabelRow(metadata, document, judgment)
		if joinErr != nil {
			return nil, fmt.Errorf("case %q: %w", caseID, joinErr)
		}
		line, marshalErr := json.Marshal(row)
		if marshalErr != nil {
			return nil, fmt.Errorf("%w: encode compiled labels", ErrInvalidCorpus)
		}
		encoded = append(encoded, line)
	}
	return encodeStrictJSONLines(encoded)
}

// ValidateJudgedInputs strictly validates and exactly joins source documents
// with compiled labels and applies every GPU-independent construction gate.
// Model recordings and scores never cross this boundary.
func ValidateJudgedInputs(docsPath, labelsPath string) (JudgedInputsSummary, error) {
	documents, caseIDs, err := loadLabelingDocuments(docsPath)
	if err != nil {
		return JudgedInputsSummary{}, err
	}
	labels, err := loadCompiledLabels(labelsPath)
	if err != nil {
		return JudgedInputsSummary{}, err
	}
	if len(documents) != len(labels) {
		return JudgedInputsSummary{}, fmt.Errorf("%w: document and label case counts differ", ErrInvalidCorpus)
	}
	summary := JudgedInputsSummary{
		Cases: len(caseIDs), LanguageBucketCases: make(map[string]int, len(languages)*len(buckets)),
		HardCasesByLanguage: make(map[string]int, len(languages)), HardCasesByBucket: make(map[string]int, len(buckets)),
	}
	for _, caseID := range caseIDs {
		document := documents[caseID]
		label, present := labels[caseID]
		if !present {
			return JudgedInputsSummary{}, fmt.Errorf("%w: case %q has no compiled label", ErrInvalidCorpus, caseID)
		}
		if err := validateCompiledJoin(document, label); err != nil {
			return JudgedInputsSummary{}, fmt.Errorf("case %q: %w", caseID, err)
		}
		summary.Candidates += len(document.candidates)
		grades := make([]int, len(document.candidates))
		for index, candidate := range document.candidates {
			grades[index] = label.grades[candidate.id]
		}
		if _, err := rerank.NDCGAt5(grades); err != nil {
			return JudgedInputsSummary{}, fmt.Errorf("%w: case %q has zero or invalid IDCG@5", ErrInvalidCorpus, caseID)
		}
		summary.LanguageBucketCases[label.language+"/"+label.bucket]++
		if label.hard {
			summary.HardCasesByLanguage[label.language]++
			summary.HardCasesByBucket[label.bucket]++
		}
	}
	for caseID := range labels {
		if _, present := documents[caseID]; !present {
			return JudgedInputsSummary{}, fmt.Errorf("%w: orphan compiled label case %q", ErrInvalidCorpus, caseID)
		}
	}
	if summary.Cases < MinimumCases || summary.Candidates < MinimumCandidates {
		return JudgedInputsSummary{}, fmt.Errorf("%w: judged construction corpus is too small", ErrInvalidCorpus)
	}
	for _, language := range languages {
		if summary.HardCasesByLanguage[language] < 2 {
			return JudgedInputsSummary{}, fmt.Errorf("%w: language %q needs at least two hard cases", ErrInvalidCorpus, language)
		}
		for _, bucket := range buckets {
			if summary.LanguageBucketCases[language+"/"+bucket] < 1 {
				return JudgedInputsSummary{}, fmt.Errorf("%w: missing language/bucket slice", ErrInvalidCorpus)
			}
		}
	}
	for _, bucket := range buckets {
		if summary.HardCasesByBucket[bucket] < 1 {
			return JudgedInputsSummary{}, fmt.Errorf("%w: bucket %q needs a hard case", ErrInvalidCorpus, bucket)
		}
	}
	return summary, nil
}

func loadLabelingInputs(casesPath, docsPath string) (map[string]labelingCase, map[string]documentCase, []string, error) {
	cases, err := loadLabelingCases(casesPath)
	if err != nil {
		return nil, nil, nil, err
	}
	documents, caseIDs, err := loadLabelingDocuments(docsPath)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(cases) != len(documents) {
		return nil, nil, nil, fmt.Errorf("%w: cases and documents counts differ", ErrInvalidCorpus)
	}
	for _, caseID := range caseIDs {
		metadata, present := cases[caseID]
		if !present {
			return nil, nil, nil, fmt.Errorf("%w: document case %q has no case metadata", ErrInvalidCorpus, caseID)
		}
		if metadata.query != documents[caseID].query {
			return nil, nil, nil, fmt.Errorf("%w: case %q query does not exactly match document", ErrInvalidCorpus, caseID)
		}
	}
	for caseID := range cases {
		if _, present := documents[caseID]; !present {
			return nil, nil, nil, fmt.Errorf("%w: orphan case metadata %q", ErrInvalidCorpus, caseID)
		}
	}
	return cases, documents, caseIDs, nil
}

func loadLabelingCases(path string) (map[string]labelingCase, error) {
	lines, err := readJSONLines(path)
	if err != nil {
		return nil, fmt.Errorf("cases.jsonl: %w", err)
	}
	rows := make(map[string]labelingCase, len(lines))
	seenQueries := make(map[string]string, len(lines))
	for index, line := range lines {
		row, parseErr := parseLabelingCase(line)
		if parseErr != nil {
			return nil, fmt.Errorf("cases.jsonl line %d: %w", index+1, parseErr)
		}
		if _, duplicate := rows[row.id]; duplicate {
			return nil, fmt.Errorf("%w: duplicate case metadata %q", ErrInvalidCorpus, row.id)
		}
		if prior, duplicate := seenQueries[row.query]; duplicate {
			return nil, fmt.Errorf("%w: cases %q and %q repeat a query", ErrInvalidCorpus, prior, row.id)
		}
		rows[row.id] = row
		seenQueries[row.query] = row.id
	}
	return rows, nil
}

func parseLabelingCase(raw []byte) (labelingCase, error) {
	root, err := exactObject(raw, []string{"case_id", "query", "language", "bucket"}, nil)
	if err != nil {
		return labelingCase{}, err
	}
	id, idErr := fixtureID(root["case_id"])
	query, queryErr := jsonString(root["query"])
	language, languageErr := jsonString(root["language"])
	bucket, bucketErr := jsonString(root["bucket"])
	if idErr != nil || queryErr != nil || languageErr != nil || bucketErr != nil || !validLanguage(language) || !validBucket(bucket) {
		return labelingCase{}, fmt.Errorf("%w: invalid case metadata", ErrInvalidCorpus)
	}
	return labelingCase{id: id, query: query, language: language, bucket: bucket}, nil
}

func loadLabelingDocuments(path string) (map[string]documentCase, []string, error) {
	lines, err := readJSONLines(path)
	if err != nil {
		return nil, nil, fmt.Errorf("docs.jsonl: %w", err)
	}
	documents := make(map[string]documentCase, len(lines))
	seenQueries := make(map[string]string, len(lines))
	for index, line := range lines {
		document, parseErr := parseDocumentCase(line)
		if parseErr != nil {
			return nil, nil, fmt.Errorf("docs.jsonl line %d: %w", index+1, parseErr)
		}
		if _, duplicate := documents[document.id]; duplicate {
			return nil, nil, fmt.Errorf("%w: duplicate document case %q", ErrInvalidCorpus, document.id)
		}
		if prior, duplicate := seenQueries[document.query]; duplicate {
			return nil, nil, fmt.Errorf("%w: document cases %q and %q repeat a query", ErrInvalidCorpus, prior, document.id)
		}
		if err := validateLabelingDocument(document); err != nil {
			return nil, nil, fmt.Errorf("docs.jsonl line %d: %w", index+1, err)
		}
		documents[document.id] = document
		seenQueries[document.query] = document.id
	}
	caseIDs := make([]string, 0, len(documents))
	for caseID := range documents {
		caseIDs = append(caseIDs, caseID)
	}
	sort.Strings(caseIDs)
	return documents, caseIDs, nil
}

func validateLabelingDocument(document documentCase) error {
	candidates := make([]rerank.Candidate, len(document.candidates))
	for index, candidate := range document.candidates {
		candidates[index] = rerank.Candidate{
			CanonicalURL: candidate.canonicalURL, ProviderRank: candidate.providerRank,
			Title: candidate.title, Snippet: candidate.snippet,
		}
	}
	if _, err := rerank.BuildRequest(document.query, candidates); err != nil {
		return fmt.Errorf("%w: production request builder rejected case %q", ErrInvalidCorpus, document.id)
	}
	return nil
}

func loadBlindJudgments(path string) (map[string]blindJudgment, error) {
	lines, err := readJSONLines(path)
	if err != nil {
		return nil, fmt.Errorf("judgments.jsonl: %w", err)
	}
	judgments := make(map[string]blindJudgment, len(lines))
	for index, line := range lines {
		row, parseErr := parseBlindJudgment(line)
		if parseErr != nil {
			return nil, fmt.Errorf("judgments.jsonl line %d: %w", index+1, parseErr)
		}
		if _, duplicate := judgments[row.id]; duplicate {
			return nil, fmt.Errorf("%w: duplicate judgment case %q", ErrInvalidCorpus, row.id)
		}
		judgments[row.id] = row
	}
	return judgments, nil
}

func parseBlindJudgment(raw []byte) (blindJudgment, error) {
	root, err := exactObject(raw, []string{"case_id", "packet_digest", "rubric_version", "grades"}, []string{"note"})
	if err != nil {
		return blindJudgment{}, err
	}
	id, idErr := fixtureID(root["case_id"])
	digest, digestErr := jsonString(root["packet_digest"])
	rubric, rubricErr := jsonString(root["rubric_version"])
	if idErr != nil || digestErr != nil || rubricErr != nil || !validDigest(digest) || rubric != JudgmentRubricVersion {
		return blindJudgment{}, fmt.Errorf("%w: invalid judgment header", ErrInvalidCorpus)
	}
	rawGrades, err := rawArray(root["grades"], MinimumCaseCandidates, rerank.MaxCandidates)
	if err != nil {
		return blindJudgment{}, fmt.Errorf("grades: %w", err)
	}
	row := blindJudgment{id: id, packetDigest: digest, rubricVersion: rubric, grades: make(map[string]int, len(rawGrades))}
	if rawNote, present := root["note"]; present {
		row.note, err = jsonString(rawNote)
		if err != nil {
			return blindJudgment{}, fmt.Errorf("note: %w", err)
		}
		row.notePresent = true
	}
	for index, rawGrade := range rawGrades {
		fields, objectErr := exactObject(rawGrade, []string{"candidate_id", "grade"}, nil)
		if objectErr != nil {
			return blindJudgment{}, fmt.Errorf("grade %d: %w", index, objectErr)
		}
		candidateID, candidateErr := fixtureID(fields["candidate_id"])
		grade, gradeErr := jsonInt(fields["grade"])
		if candidateErr != nil || gradeErr != nil || grade < JudgmentGradeIrrelevant || grade > JudgmentGradeDirect {
			return blindJudgment{}, fmt.Errorf("%w: invalid grade %d", ErrInvalidCorpus, index)
		}
		if _, duplicate := row.grades[candidateID]; duplicate {
			return blindJudgment{}, fmt.Errorf("%w: duplicate grade candidate %q", ErrInvalidCorpus, candidateID)
		}
		row.grades[candidateID] = grade
	}
	return row, nil
}

func makeBlindPacketRow(metadata labelingCase, document documentCase) blindPacketRow {
	candidates := make([]blindPacketCandidate, len(document.candidates))
	for index, candidate := range document.candidates {
		candidates[index] = blindPacketCandidate{
			CandidateID: strings.Clone(candidate.id), Title: strings.Clone(candidate.title), Snippet: strings.Clone(candidate.snippet),
		}
	}
	sort.Slice(candidates, func(left, right int) bool { return candidates[left].CandidateID < candidates[right].CandidateID })
	row := blindPacketRow{CaseID: strings.Clone(metadata.id), Query: strings.Clone(metadata.query), Candidates: candidates}
	row.PacketDigest = blindPacketDigest(row)
	return row
}

func blindPacketDigest(row blindPacketRow) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte(blindPacketDigestDomain))
	writeDigestString(digest, row.CaseID)
	writeDigestString(digest, row.Query)
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(len(row.Candidates)))
	_, _ = digest.Write(count[:])
	for _, candidate := range row.Candidates {
		writeDigestString(digest, candidate.CandidateID)
		writeDigestString(digest, candidate.Title)
		writeDigestString(digest, candidate.Snippet)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

type digestWriter interface {
	Write([]byte) (int, error)
}

func writeDigestString(destination digestWriter, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = destination.Write(length[:])
	_, _ = destination.Write([]byte(value))
}

func compileLabelRow(metadata labelingCase, document documentCase, judgment blindJudgment) (compiledLabelRow, error) {
	if len(document.candidates) != len(judgment.grades) {
		return compiledLabelRow{}, fmt.Errorf("%w: candidate and grade counts differ", ErrInvalidCorpus)
	}
	candidates := make([]Candidate, len(document.candidates))
	grades := make([]compiledLabelGrade, len(document.candidates))
	for index, candidate := range document.candidates {
		grade, present := judgment.grades[candidate.id]
		if !present {
			return compiledLabelRow{}, fmt.Errorf("%w: candidate %q has no exact grade", ErrInvalidCorpus, candidate.id)
		}
		candidates[index] = Candidate{ID: candidate.id, ProviderRank: candidate.providerRank, Grade: grade}
		grades[index] = compiledLabelGrade{CandidateID: strings.Clone(candidate.id), Grade: grade}
	}
	sort.Slice(grades, func(left, right int) bool { return grades[left].CandidateID < grades[right].CandidateID })
	row := compiledLabelRow{
		CaseID: strings.Clone(metadata.id), Language: strings.Clone(metadata.language), Bucket: strings.Clone(metadata.bucket),
		Hard: objectiveHard(candidates), RubricVersion: JudgmentRubricVersion, Grades: grades,
	}
	if judgment.notePresent {
		note := strings.Clone(judgment.note)
		row.Note = &note
	}
	return row, nil
}

func encodeStrictJSONLines(lines [][]byte) ([]byte, error) {
	total := 0
	for _, line := range lines {
		if len(line) > MaxJSONLLineBytes {
			return nil, fmt.Errorf("%w: encoded line exceeds %d bytes", ErrResourceLimit, MaxJSONLLineBytes)
		}
		if total > MaxJSONLFileBytes-len(line)-1 {
			return nil, fmt.Errorf("%w: encoded file exceeds %d bytes", ErrResourceLimit, MaxJSONLFileBytes)
		}
		total += len(line) + 1
	}
	output := make([]byte, 0, total)
	for _, line := range lines {
		output = append(output, line...)
		output = append(output, '\n')
	}
	return output, nil
}

func loadCompiledLabels(path string) (map[string]labelCase, error) {
	lines, err := readJSONLines(path)
	if err != nil {
		return nil, fmt.Errorf("labels.jsonl: %w", err)
	}
	labels := make(map[string]labelCase, len(lines))
	for index, line := range lines {
		label, parseErr := parseCompiledLabelCase(line)
		if parseErr != nil {
			return nil, fmt.Errorf("labels.jsonl line %d: %w", index+1, parseErr)
		}
		if _, duplicate := labels[label.id]; duplicate {
			return nil, fmt.Errorf("%w: duplicate compiled label case %q", ErrInvalidCorpus, label.id)
		}
		labels[label.id] = label
	}
	return labels, nil
}

func parseCompiledLabelCase(raw []byte) (labelCase, error) {
	return parseLabelCase(raw)
}

func validateCompiledJoin(document documentCase, label labelCase) error {
	if len(document.candidates) != len(label.grades) {
		return fmt.Errorf("%w: candidate and grade counts differ", ErrInvalidCorpus)
	}
	candidates := make([]Candidate, len(document.candidates))
	for index, candidate := range document.candidates {
		grade, present := label.grades[candidate.id]
		if !present {
			return fmt.Errorf("%w: candidate %q has no exact grade", ErrInvalidCorpus, candidate.id)
		}
		candidates[index] = Candidate{ID: candidate.id, ProviderRank: candidate.providerRank, Grade: grade}
	}
	if objectiveHard(candidates) != label.hard {
		return fmt.Errorf("%w: hard label does not equal objective hard predicate", ErrInvalidCorpus)
	}
	return nil
}
