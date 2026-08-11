package consensus

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/simhash"
)

const (
	neffGoldenMaxSourceLineBytes = 8 << 20
	neffGoldenMaxCaseLineBytes   = 1 << 20
	neffGoldenMaxSources         = 512
	neffGoldenMaxCases           = 1_024
	neffGoldenMaxIDBytes         = 256
	neffGoldenMaxNoteBytes       = 4 << 10
	neffGoldenRandomPermutations = 32
)

var (
	neffGoldenClasses   = []string{"core_mirror", "lineage", "independent", "mixed", "compat", "bridge"}
	neffGoldenLanguages = []string{"en", "zh", "mixed"}
)

type neffGoldenAnchorRow struct {
	Quote     string          `json:"quote"`
	TextRange []int           `json:"text_range"`
	Method    evidence.Method `json:"method"`
}

type neffGoldenSourceRow struct {
	ID      string                         `json:"id"`
	URL     string                         `json:"url"`
	Data    json.RawMessage                `json:"data"`
	Cleaned string                         `json:"cleaned"`
	Basis   map[string]neffGoldenAnchorRow `json:"basis"`
}

type neffGoldenOptionalInt struct {
	Present bool
	Value   int
}

func (value *neffGoldenOptionalInt) UnmarshalJSON(raw []byte) error {
	value.Present = true
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("must be an integer, not null")
	}
	return json.Unmarshal(raw, &value.Value)
}

type neffGoldenExpectationRow struct {
	Path                   string                `json:"path"`
	Value                  json.RawMessage       `json:"value"`
	Pages                  int                   `json:"pages"`
	IndependentRoots       int                   `json:"independent_roots"`
	WantV0IndependentRoots neffGoldenOptionalInt `json:"want_v0_independent_roots"`
	SupportReasons         map[string]FoldReason `json:"support_reasons"`
	AgreementReason        json.RawMessage       `json:"agreement_reason"`
}

type neffGoldenCaseRow struct {
	ID             string                     `json:"id"`
	Class          string                     `json:"class"`
	Language       string                     `json:"language"`
	Sources        []string                   `json:"sources"`
	UseCleanedText *bool                      `json:"use_cleaned_text"`
	Hard           *bool                      `json:"hard"`
	Note           string                     `json:"note"`
	Expect         []neffGoldenExpectationRow `json:"expect"`
}

type neffGoldenSource struct {
	id     string
	result SourceResult
	line   int
}

type neffGoldenExpectation struct {
	path                   string
	value                  scalarValue
	pages                  int
	independentRoots       int
	wantV0IndependentRoots neffGoldenOptionalInt
	supportReasons         map[string]FoldReason
	agreementReason        FoldReason
}

type neffGoldenCase struct {
	id             string
	class          string
	language       string
	sourceIDs      []string
	useCleanedText bool
	hard           bool
	note           string
	expectations   []neffGoldenExpectation
	line           int
}

type neffGoldenCorpus struct {
	sources map[string]neffGoldenSource
	cases   []neffGoldenCase
}

type neffGoldenSlice struct {
	cases        int
	expectations int
	passedCases  int
	passedExpect int
}

type neffGoldenCoverage struct {
	originAndSevenMirrors bool
	eightIndependent      bool
	shortLineageBoundary  bool
	longLineage           bool
	mixedThreePlusSingles bool
	compat                bool
	bridge                bool
}

type neffGoldenActualCandidate struct {
	value     json.RawMessage
	agreement Agreement
	supports  []Support
}

func TestNEFFGolden(t *testing.T) {
	corpus := loadNEFFGoldenCorpus(t)
	slices := make(map[string]*neffGoldenSlice, len(neffGoldenClasses)*len(neffGoldenLanguages)*2)
	for _, class := range neffGoldenClasses {
		for _, language := range neffGoldenLanguages {
			for _, hard := range []bool{false, true} {
				slices[neffGoldenSliceKey(class, language, hard)] = &neffGoldenSlice{}
			}
		}
	}

	coverage := neffGoldenCoverage{}
	for _, goldenCase := range corpus.cases {
		goldenCase := goldenCase
		slice := slices[neffGoldenSliceKey(goldenCase.class, goldenCase.language, goldenCase.hard)]
		slice.cases++
		slice.expectations += len(goldenCase.expectations)
		var caseCoverage neffGoldenCoverage
		passed := t.Run("case/"+goldenCase.id, func(t *testing.T) {
			caseCoverage = runNEFFGoldenCase(t, corpus, goldenCase)
		})
		if passed {
			slice.passedCases++
			slice.passedExpect += len(goldenCase.expectations)
		}
		coverage.merge(caseCoverage)
	}

	logNEFFGoldenSlices(t, slices)
	for name, present := range map[string]bool{
		"one origin plus seven mirrors (8 pages -> 1 component)":        coverage.originAndSevenMirrors,
		"eight genuinely independent sources (8 pages -> 8 components)": coverage.eightIndependent,
		"shared 95-rune lineage boundary rejection":                     coverage.shortLineageBoundary,
		"long quote lineage":                         coverage.longLineage,
		"mixed 3-copy cluster plus three singletons": coverage.mixedThreePlusSingles,
		"missing-cleaned compatibility":              coverage.compat,
		"cross-value mixed-signal bridge":            coverage.bridge,
	} {
		if !present {
			t.Errorf("N_eff golden corpus is missing machine-proven bucket %q", name)
		}
	}
}

func TestNEFFGoldenStrictSchemaRejectsFieldSmuggling(t *testing.T) {
	validSource := `{"id":"s","url":"https://alpha.com/","data":{"claim":"Ada"},"cleaned":"Ada","basis":{"claim":{"quote":"Ada","text_range":[0,3],"method":"exact"}}}`
	validCase := `{"id":"c","class":"compat","language":"en","sources":["s"],"use_cleaned_text":false,"hard":false,"note":"compat","expect":[{"path":"claim","value":"Ada","pages":1,"independent_roots":1,"support_reasons":{},"agreement_reason":null}]}`
	tests := []struct {
		name    string
		raw     string
		output  func() any
		wantErr bool
	}{
		{name: "valid source", raw: validSource, output: func() any { return &neffGoldenSourceRow{} }},
		{name: "valid case", raw: validCase, output: func() any { return &neffGoldenCaseRow{} }},
		{name: "source case alias", raw: strings.Replace(validSource, `"id":"s"`, `"ID":"s"`, 1), output: func() any { return &neffGoldenSourceRow{} }, wantErr: true},
		{name: "source case double write", raw: strings.Replace(validSource, `"id":"s"`, `"id":"s","ID":"override"`, 1), output: func() any { return &neffGoldenSourceRow{} }, wantErr: true},
		{name: "anchor case alias", raw: strings.Replace(validSource, `"quote":"Ada"`, `"Quote":"Ada"`, 1), output: func() any { return &neffGoldenSourceRow{} }, wantErr: true},
		{name: "case underscore alias", raw: strings.Replace(validCase, `"use_cleaned_text":false`, `"Use_Cleaned_Text":false`, 1), output: func() any { return &neffGoldenCaseRow{} }, wantErr: true},
		{name: "expectation case alias", raw: strings.Replace(validCase, `"pages":1`, `"Pages":1`, 1), output: func() any { return &neffGoldenCaseRow{} }, wantErr: true},
		{name: "duplicate exact field", raw: strings.Replace(validCase, `"hard":false`, `"hard":false,"hard":true`, 1), output: func() any { return &neffGoldenCaseRow{} }, wantErr: true},
		{name: "trailing value", raw: validCase + ` {}`, output: func() any { return &neffGoldenCaseRow{} }, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := decodeNEFFGoldenRow([]byte(test.raw), test.output())
			if (err != nil) != test.wantErr {
				t.Fatalf("decodeNEFFGoldenRow() error = %v, wantErr=%t", err, test.wantErr)
			}
		})
	}
}

func (coverage *neffGoldenCoverage) merge(other neffGoldenCoverage) {
	coverage.originAndSevenMirrors = coverage.originAndSevenMirrors || other.originAndSevenMirrors
	coverage.eightIndependent = coverage.eightIndependent || other.eightIndependent
	coverage.shortLineageBoundary = coverage.shortLineageBoundary || other.shortLineageBoundary
	coverage.longLineage = coverage.longLineage || other.longLineage
	coverage.mixedThreePlusSingles = coverage.mixedThreePlusSingles || other.mixedThreePlusSingles
	coverage.compat = coverage.compat || other.compat
	coverage.bridge = coverage.bridge || other.bridge
}

func loadNEFFGoldenCorpus(t *testing.T) *neffGoldenCorpus {
	t.Helper()
	root := filepath.Join("testdata", "neff")
	corpus := &neffGoldenCorpus{sources: make(map[string]neffGoldenSource)}
	canonicalURLs := make(map[string]string)

	readNEFFGoldenLines(t, filepath.Join(root, "sources.jsonl"), neffGoldenMaxSourceLineBytes, func(lineNumber int, raw []byte) {
		if len(corpus.sources) >= neffGoldenMaxSources {
			t.Fatalf("%s:%d: source rows exceed %d", filepath.Join(root, "sources.jsonl"), lineNumber, neffGoldenMaxSources)
		}
		var row neffGoldenSourceRow
		decodeNEFFGoldenLine(t, filepath.Join(root, "sources.jsonl"), lineNumber, raw, &row)
		source := validateNEFFGoldenSource(t, filepath.Join(root, "sources.jsonl"), lineNumber, row)
		if _, duplicate := corpus.sources[source.id]; duplicate {
			t.Fatalf("%s:%d: duplicate source id %q", filepath.Join(root, "sources.jsonl"), lineNumber, source.id)
		}
		if prior, duplicate := canonicalURLs[source.result.URL]; duplicate {
			t.Fatalf("%s:%d: source %q repeats canonical URL owned by %q", filepath.Join(root, "sources.jsonl"), lineNumber, source.id, prior)
		}
		corpus.sources[source.id] = source
		canonicalURLs[source.result.URL] = source.id
	})
	if len(corpus.sources) == 0 {
		t.Fatal("N_eff golden corpus has no source rows")
	}

	caseIDs := make(map[string]struct{})
	sourceUses := make(map[string]int, len(corpus.sources))
	classCounts := make(map[string]int, len(neffGoldenClasses))
	coreLanguages := make(map[string]bool)
	hardNegative := false
	readNEFFGoldenLines(t, filepath.Join(root, "cases.jsonl"), neffGoldenMaxCaseLineBytes, func(lineNumber int, raw []byte) {
		if len(corpus.cases) >= neffGoldenMaxCases {
			t.Fatalf("%s:%d: case rows exceed %d", filepath.Join(root, "cases.jsonl"), lineNumber, neffGoldenMaxCases)
		}
		var row neffGoldenCaseRow
		decodeNEFFGoldenLine(t, filepath.Join(root, "cases.jsonl"), lineNumber, raw, &row)
		goldenCase := validateNEFFGoldenCase(t, filepath.Join(root, "cases.jsonl"), lineNumber, row, corpus.sources)
		if _, duplicate := caseIDs[goldenCase.id]; duplicate {
			t.Fatalf("%s:%d: duplicate case id %q", filepath.Join(root, "cases.jsonl"), lineNumber, goldenCase.id)
		}
		caseIDs[goldenCase.id] = struct{}{}
		for _, sourceID := range goldenCase.sourceIDs {
			sourceUses[sourceID]++
		}
		classCounts[goldenCase.class]++
		if goldenCase.class == "core_mirror" {
			coreLanguages[goldenCase.language] = true
		}
		if goldenCase.class == "independent" && goldenCase.hard && len(goldenCase.sourceIDs) >= 2 {
			qualifies := false
			valid := true
			for _, expectation := range goldenCase.expectations {
				qualifies = qualifies || expectation.pages >= 2
				valid = valid && expectation.independentRoots == expectation.pages && len(expectation.supportReasons) == 0 && expectation.agreementReason == ""
			}
			hardNegative = hardNegative || qualifies && valid
		}
		corpus.cases = append(corpus.cases, goldenCase)
	})

	if len(corpus.cases) < 12 {
		t.Fatalf("N_eff golden corpus has %d cases, want at least 12", len(corpus.cases))
	}
	for _, class := range neffGoldenClasses {
		if classCounts[class] == 0 {
			t.Fatalf("N_eff golden corpus has no required class %q", class)
		}
	}
	if !coreLanguages["en"] || !coreLanguages["zh"] {
		t.Fatalf("N_eff core_mirror bucket must include en and zh; got %#v", coreLanguages)
	}
	if !hardNegative {
		t.Fatal("N_eff golden corpus has no valid hard independent negative bucket")
	}
	for sourceID := range corpus.sources {
		if sourceUses[sourceID] == 0 {
			source := corpus.sources[sourceID]
			t.Fatalf("%s:%d: N_eff golden source %q is not referenced by any case", filepath.Join(root, "sources.jsonl"), source.line, sourceID)
		}
	}
	return corpus
}

func readNEFFGoldenLines(t *testing.T, path string, maxLineBytes int, visit func(lineNumber int, raw []byte)) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), maxLineBytes+1)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		raw := scanner.Bytes()
		if len(raw) > maxLineBytes {
			t.Fatalf("%s:%d: JSONL row exceeds %d bytes", path, lineNumber, maxLineBytes)
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			t.Fatalf("%s:%d: empty JSONL row is forbidden", path, lineNumber)
		}
		if bytes.HasPrefix(raw, []byte{0xef, 0xbb, 0xbf}) {
			t.Fatalf("%s:%d: UTF-8 BOM is forbidden", path, lineNumber)
		}
		if !utf8.Valid(raw) {
			t.Fatalf("%s:%d: row is not valid UTF-8", path, lineNumber)
		}
		visit(lineNumber, append([]byte(nil), raw...))
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan %s near line %d (line budget %d): %v", path, lineNumber+1, maxLineBytes, err)
	}
}

func decodeNEFFGoldenLine(t *testing.T, path string, lineNumber int, raw []byte, output any) {
	t.Helper()
	if err := decodeNEFFGoldenRow(raw, output); err != nil {
		t.Fatalf("%s:%d: %v", path, lineNumber, err)
	}
}

func decodeNEFFGoldenRow(raw []byte, output any) error {
	if err := rejectNEFFGoldenDuplicateKeys(raw); err != nil {
		return err
	}
	if err := validateNEFFGoldenRawSchema(raw, output); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode row: %w", err)
	}
	if err := requireNEFFGoldenEOF(decoder); err != nil {
		return err
	}
	return nil
}

func validateNEFFGoldenRawSchema(raw []byte, output any) error {
	fields, err := decodeNEFFGoldenRawObject(raw, "row")
	if err != nil {
		return err
	}
	switch output.(type) {
	case *neffGoldenSourceRow:
		if err := requireNEFFGoldenRawFields(fields, "source row", []string{"id", "url", "data", "cleaned", "basis"}, nil); err != nil {
			return err
		}
		var basis map[string]json.RawMessage
		if err := json.Unmarshal(fields["basis"], &basis); err != nil || basis == nil {
			return fmt.Errorf("source basis must be a JSON object")
		}
		for path, anchorRaw := range basis {
			anchorFields, err := decodeNEFFGoldenRawObject(anchorRaw, fmt.Sprintf("source basis %q", path))
			if err != nil {
				return err
			}
			if err := requireNEFFGoldenRawFields(anchorFields, fmt.Sprintf("source basis %q", path), []string{"quote", "text_range", "method"}, nil); err != nil {
				return err
			}
		}
		return nil
	case *neffGoldenCaseRow:
		if err := requireNEFFGoldenRawFields(fields, "case row", []string{"id", "class", "language", "sources", "use_cleaned_text", "hard", "note", "expect"}, nil); err != nil {
			return err
		}
		var expectations []json.RawMessage
		if err := json.Unmarshal(fields["expect"], &expectations); err != nil || expectations == nil {
			return fmt.Errorf("case expect must be a JSON array")
		}
		for index, expectedRaw := range expectations {
			expectedFields, err := decodeNEFFGoldenRawObject(expectedRaw, fmt.Sprintf("case expectation %d", index))
			if err != nil {
				return err
			}
			if err := requireNEFFGoldenRawFields(
				expectedFields,
				fmt.Sprintf("case expectation %d", index),
				[]string{"path", "value", "pages", "independent_roots", "support_reasons", "agreement_reason"},
				[]string{"want_v0_independent_roots"},
			); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported N_eff golden row type %T", output)
	}
}

func decodeNEFFGoldenRawObject(raw []byte, name string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("%s must be a JSON object", name)
	}
	return fields, nil
}

func requireNEFFGoldenRawFields(fields map[string]json.RawMessage, name string, required, optional []string) error {
	allowed := make(map[string]struct{}, len(required)+len(optional))
	for _, field := range required {
		allowed[field] = struct{}{}
		if _, present := fields[field]; !present {
			return fmt.Errorf("%s is missing required field %q", name, field)
		}
	}
	for _, field := range optional {
		allowed[field] = struct{}{}
	}
	for field := range fields {
		if _, admitted := allowed[field]; !admitted {
			return fmt.Errorf("%s contains unsupported field %q", name, field)
		}
	}
	return nil
}

func rejectNEFFGoldenDuplicateKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := walkNEFFGoldenJSON(decoder, "$"); err != nil {
		return err
	}
	if token, err := decoder.Token(); err != io.EOF {
		if err != nil {
			return fmt.Errorf("decode JSON token: %w", err)
		}
		return fmt.Errorf("trailing JSON value begins with %v", token)
	}
	return nil
}

func walkNEFFGoldenJSON(decoder *json.Decoder, path string) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("decode JSON at %s: %w", path, err)
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, keyErr := decoder.Token()
			if keyErr != nil {
				return fmt.Errorf("decode object key at %s: %w", path, keyErr)
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key at %s is not a string", path)
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON field %q at %s", key, path)
			}
			seen[key] = struct{}{}
			if err := walkNEFFGoldenJSON(decoder, path+"."+key); err != nil {
				return err
			}
		}
		closing, closeErr := decoder.Token()
		if closeErr != nil || closing != json.Delim('}') {
			return fmt.Errorf("decode object close at %s: %v", path, closeErr)
		}
	case '[':
		index := 0
		for decoder.More() {
			if err := walkNEFFGoldenJSON(decoder, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
			index++
		}
		closing, closeErr := decoder.Token()
		if closeErr != nil || closing != json.Delim(']') {
			return fmt.Errorf("decode array close at %s: %v", path, closeErr)
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q at %s", delimiter, path)
	}
	return nil
}

func requireNEFFGoldenEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("row contains a trailing JSON value")
		}
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return nil
}

func validateNEFFGoldenSource(t *testing.T, path string, lineNumber int, row neffGoldenSourceRow) neffGoldenSource {
	t.Helper()
	where := fmt.Sprintf("%s:%d", path, lineNumber)
	if row.ID == "" || row.ID != strings.TrimSpace(row.ID) || !utf8.ValidString(row.ID) || len(row.ID) > neffGoldenMaxIDBytes {
		t.Fatalf("%s: source id is empty, invalid, or exceeds %d bytes", where, neffGoldenMaxIDBytes)
	}
	if row.URL == "" || row.Cleaned == "" || len(row.Data) == 0 || len(row.Basis) == 0 {
		t.Fatalf("%s: source %q requires non-empty url, data, cleaned, and basis", where, row.ID)
	}
	if len(row.Cleaned) > MaxCleanedTextBytes || !utf8.ValidString(row.Cleaned) {
		t.Fatalf("%s: source %q cleaned text exceeds %d bytes or is invalid UTF-8", where, row.ID, MaxCleanedTextBytes)
	}

	basis := make(map[string]evidence.Anchor, len(row.Basis))
	for evidencePath, anchor := range row.Basis {
		if evidencePath == "" || len(anchor.TextRange) != 2 || anchor.Quote == "" || !validNEFFGoldenMethod(anchor.Method) {
			t.Fatalf("%s: source %q basis path %q is incomplete or uses an ineligible method", where, row.ID, evidencePath)
		}
		start, end := anchor.TextRange[0], anchor.TextRange[1]
		if start < 0 || end <= start || end > len(row.Cleaned) || !utf8.RuneStart(row.Cleaned[start]) ||
			(end < len(row.Cleaned) && !utf8.RuneStart(row.Cleaned[end])) || row.Cleaned[start:end] != anchor.Quote {
			t.Fatalf("%s: source %q basis path %q range is not an exact UTF-8 boundary match", where, row.ID, evidencePath)
		}
		basis[evidencePath] = evidence.Anchor{Quote: anchor.Quote, TextRange: [2]int{start, end}, Method: anchor.Method}
	}

	result := SourceResult{
		URL:         row.URL,
		Data:        append(json.RawMessage(nil), row.Data...),
		Basis:       basis,
		SimText:     simhash.Fingerprint(row.Cleaned),
		CleanedText: row.Cleaned,
	}
	prepared, _, _, err := prepareSource(0, result)
	if err != nil {
		t.Fatalf("%s: source %q fails production admission: %v", where, row.ID, err)
	}
	if result.URL != prepared.url {
		t.Fatalf("%s: source %q URL %q is not canonical; want %q", where, row.ID, result.URL, prepared.url)
	}
	if _, object := prepared.document.(map[string]any); !object {
		t.Fatalf("%s: source %q data must be a JSON object", where, row.ID)
	}
	if _, err := Merge([]SourceResult{result}); err != nil {
		t.Fatalf("%s: source %q fails singleton Merge admission: %v", where, row.ID, err)
	}
	return neffGoldenSource{id: row.ID, result: result, line: lineNumber}
}

func validNEFFGoldenMethod(method evidence.Method) bool {
	switch method {
	case evidence.MethodExact, evidence.MethodNormalized, evidence.MethodFuzzy, evidence.MethodCompiled:
		return true
	default:
		return false
	}
}

func validateNEFFGoldenCase(
	t *testing.T,
	path string,
	lineNumber int,
	row neffGoldenCaseRow,
	sources map[string]neffGoldenSource,
) neffGoldenCase {
	t.Helper()
	where := fmt.Sprintf("%s:%d", path, lineNumber)
	if row.ID == "" || row.ID != strings.TrimSpace(row.ID) || !utf8.ValidString(row.ID) || len(row.ID) > neffGoldenMaxIDBytes {
		t.Fatalf("%s: case id is empty, invalid, or exceeds %d bytes", where, neffGoldenMaxIDBytes)
	}
	if !containsNEFFGoldenString(neffGoldenClasses, row.Class) || !containsNEFFGoldenString(neffGoldenLanguages, row.Language) {
		t.Fatalf("%s: case %q has invalid class/language %q/%q", where, row.ID, row.Class, row.Language)
	}
	if row.UseCleanedText == nil || row.Hard == nil {
		t.Fatalf("%s: case %q must explicitly set use_cleaned_text and hard", where, row.ID)
	}
	if row.Class == "compat" && *row.UseCleanedText || row.Class != "compat" && !*row.UseCleanedText {
		t.Fatalf("%s: case %q must use cleaned text iff class is not compat", where, row.ID)
	}
	if strings.TrimSpace(row.Note) == "" || !utf8.ValidString(row.Note) || len(row.Note) > neffGoldenMaxNoteBytes {
		t.Fatalf("%s: case %q note is empty, invalid, or exceeds %d bytes", where, row.ID, neffGoldenMaxNoteBytes)
	}
	if len(row.Sources) == 0 || len(row.Sources) > MaxSources {
		t.Fatalf("%s: case %q source count = %d, want 1..%d", where, row.ID, len(row.Sources), MaxSources)
	}
	if len(row.Expect) == 0 {
		t.Fatalf("%s: case %q has no expectations", where, row.ID)
	}

	caseURLs := make(map[string]struct{}, len(row.Sources))
	seenSourceIDs := make(map[string]struct{}, len(row.Sources))
	for _, sourceID := range row.Sources {
		source, exists := sources[sourceID]
		if sourceID == "" || !exists {
			t.Fatalf("%s: case %q references missing source %q", where, row.ID, sourceID)
		}
		if _, duplicate := seenSourceIDs[sourceID]; duplicate {
			t.Fatalf("%s: case %q repeats source id %q", where, row.ID, sourceID)
		}
		seenSourceIDs[sourceID] = struct{}{}
		caseURLs[source.result.URL] = struct{}{}
	}

	expectations := make([]neffGoldenExpectation, 0, len(row.Expect))
	seenExpectations := make(map[string]struct{}, len(row.Expect))
	for index, expected := range row.Expect {
		if err := validatePath(expected.Path); err != nil {
			t.Fatalf("%s: case %q expectation %d path: %v", where, row.ID, index, err)
		}
		if len(expected.Value) == 0 {
			t.Fatalf("%s: case %q expectation %d is missing value", where, row.ID, index)
		}
		value, err := canonicalScalar(expected.Value)
		if err != nil {
			t.Fatalf("%s: case %q expectation %d value: %v", where, row.ID, index, err)
		}
		if !bytes.Equal(expected.Value, value.raw) {
			t.Fatalf("%s: case %q expectation %d value must use canonical scalar JSON; got %s want %s", where, row.ID, index, expected.Value, value.raw)
		}
		key := neffGoldenCandidateKey(expected.Path, value)
		if _, duplicate := seenExpectations[key]; duplicate {
			t.Fatalf("%s: case %q repeats expectation for path/value %q/%s", where, row.ID, expected.Path, value.raw)
		}
		seenExpectations[key] = struct{}{}
		if expected.Pages < 1 || expected.Pages > len(row.Sources) || expected.IndependentRoots < 1 || expected.IndependentRoots > expected.Pages {
			t.Fatalf("%s: case %q expectation %d has invalid pages/roots %d/%d", where, row.ID, index, expected.Pages, expected.IndependentRoots)
		}
		if expected.SupportReasons == nil {
			t.Fatalf("%s: case %q expectation %d must explicitly provide support_reasons object", where, row.ID, index)
		}
		for rawURL, reason := range expected.SupportReasons {
			if _, exists := caseURLs[rawURL]; !exists {
				t.Fatalf("%s: case %q expectation %d support reason URL %q is not a case source canonical URL", where, row.ID, index, rawURL)
			}
			if !validNEFFGoldenFoldReason(reason) {
				t.Fatalf("%s: case %q expectation %d has invalid support reason %q", where, row.ID, index, reason)
			}
		}
		agreementReason, err := decodeNEFFGoldenAgreementReason(expected.AgreementReason)
		if err != nil {
			t.Fatalf("%s: case %q expectation %d agreement_reason: %v", where, row.ID, index, err)
		}
		if agreementReason != "" && expected.Pages == expected.IndependentRoots {
			t.Fatalf("%s: case %q expectation %d claims a fold reason without an actual fold", where, row.ID, index)
		}
		if expected.WantV0IndependentRoots.Present &&
			(expected.WantV0IndependentRoots.Value < 1 || expected.WantV0IndependentRoots.Value > expected.Pages) {
			t.Fatalf("%s: case %q expectation %d has invalid v0 roots %d", where, row.ID, index, expected.WantV0IndependentRoots.Value)
		}

		if row.Class == "core_mirror" || row.Class == "lineage" {
			wantReason := FoldReasonNearDuplicate
			if row.Class == "lineage" {
				wantReason = FoldReasonQuoteLineage
			}
			if expected.IndependentRoots != 1 || agreementReason != wantReason || len(expected.SupportReasons) == 0 {
				t.Fatalf("%s: %s case %q expectation %d must fold to one with non-empty %q reasons", where, row.Class, row.ID, index, wantReason)
			}
			for _, reason := range expected.SupportReasons {
				if reason != wantReason {
					t.Fatalf("%s: %s case %q expectation %d contains support reason %q, want %q", where, row.Class, row.ID, index, reason, wantReason)
				}
			}
			if !expected.WantV0IndependentRoots.Present || expected.WantV0IndependentRoots.Value <= expected.IndependentRoots {
				t.Fatalf("%s: %s case %q expectation %d must provide a strictly larger v0 root count", where, row.Class, row.ID, index)
			}
		}
		if row.Class == "independent" &&
			(expected.IndependentRoots != expected.Pages || len(expected.SupportReasons) != 0 || agreementReason != "") {
			t.Fatalf("%s: independent case %q expectation %d must remain fully independent with no reasons", where, row.ID, index)
		}

		expectations = append(expectations, neffGoldenExpectation{
			path:                   expected.Path,
			value:                  value,
			pages:                  expected.Pages,
			independentRoots:       expected.IndependentRoots,
			wantV0IndependentRoots: expected.WantV0IndependentRoots,
			supportReasons:         cloneNEFFGoldenReasons(expected.SupportReasons),
			agreementReason:        agreementReason,
		})
	}
	return neffGoldenCase{
		id:             row.ID,
		class:          row.Class,
		language:       row.Language,
		sourceIDs:      append([]string(nil), row.Sources...),
		useCleanedText: *row.UseCleanedText,
		hard:           *row.Hard,
		note:           row.Note,
		expectations:   expectations,
		line:           lineNumber,
	}
}

func decodeNEFFGoldenAgreementReason(raw json.RawMessage) (FoldReason, error) {
	if len(raw) == 0 {
		return "", fmt.Errorf("field is required and must be enum or null")
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil
	}
	var reason FoldReason
	if err := json.Unmarshal(raw, &reason); err != nil || !validNEFFGoldenFoldReason(reason) {
		return "", fmt.Errorf("invalid fold reason %s", raw)
	}
	return reason, nil
}

func validNEFFGoldenFoldReason(reason FoldReason) bool {
	switch reason {
	case FoldReasonSameRoot, FoldReasonNearDuplicate, FoldReasonQuoteLineage:
		return true
	default:
		return false
	}
}

func runNEFFGoldenCase(t *testing.T, corpus *neffGoldenCorpus, goldenCase neffGoldenCase) neffGoldenCoverage {
	t.Helper()
	sources := make([]SourceResult, 0, len(goldenCase.sourceIDs))
	for _, sourceID := range goldenCase.sourceIDs {
		result := corpus.sources[sourceID].result
		if !goldenCase.useCleanedText {
			result.CleanedText = ""
		}
		sources = append(sources, result)
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].URL < sources[j].URL })

	result, err := Merge(sources)
	if err != nil {
		t.Fatalf("Merge(v1) error: %v", err)
	}
	actual, err := collectNEFFGoldenCandidates(result)
	if err != nil {
		t.Fatal(err)
	}
	assertNEFFGoldenExpectations(t, goldenCase, actual, sources)

	needsV0 := false
	for _, expectation := range goldenCase.expectations {
		needsV0 = needsV0 || expectation.wantV0IndependentRoots.Present
	}
	var v0Baseline []byte
	if needsV0 {
		v0Sources := append([]SourceResult(nil), sources...)
		for index := range v0Sources {
			v0Sources[index].CleanedText = ""
		}
		v0Result, mergeErr := Merge(v0Sources)
		if mergeErr != nil {
			t.Fatalf("Merge(v0) error: %v", mergeErr)
		}
		v0Candidates, collectErr := collectNEFFGoldenCandidates(v0Result)
		if collectErr != nil {
			t.Fatal(collectErr)
		}
		assertNEFFGoldenV0Expectations(t, goldenCase, v0Candidates)
		v0Baseline, err = json.Marshal(v0Result)
		if err != nil {
			t.Fatalf("Marshal(v0 baseline) error: %v", err)
		}
	}

	prepared := prepareNEFFGoldenSources(t, sources)
	plan := buildIndependencePlan(prepared)
	coverage := neffGoldenCoverage{}
	switch goldenCase.class {
	case "core_mirror":
		assertNEFFGoldenCoreIsolation(t, prepared)
	case "lineage":
		assertNEFFGoldenLineageIsolation(t, prepared)
		coverage.longLineage = true
	case "independent":
		for first := range prepared {
			for second := first + 1; second < len(prepared); second++ {
				if reason := sourcePairFoldReason(prepared[first], prepared[second]); reason != "" {
					t.Fatalf("independent pair %s/%s has fold reason %q", prepared[first].url, prepared[second].url, reason)
				}
			}
		}
		if goldenCase.hard && hasNEFFGoldenShared95RuneFragment(prepared) {
			coverage.shortLineageBoundary = true
		}
	case "compat":
		coverage.compat = !goldenCase.useCleanedText
	case "bridge":
		for _, expectation := range goldenCase.expectations {
			if provesNEFFGoldenBridge(plan, prepared, expectation) {
				coverage.bridge = true
				break
			}
		}
		if !coverage.bridge {
			t.Fatal("bridge case lacks a cross-value, group-external witness subtree with at least two fold reasons")
		}
	}

	for _, expectation := range goldenCase.expectations {
		if goldenCase.class == "core_mirror" && len(sources) == 8 && expectation.pages == 8 &&
			expectation.independentRoots == 1 && len(expectation.supportReasons) == 7 {
			coverage.originAndSevenMirrors = true
		}
		if goldenCase.class == "independent" && len(sources) == 8 && expectation.pages == 8 && expectation.independentRoots == 8 {
			coverage.eightIndependent = true
		}
		if goldenCase.class == "mixed" && expectation.pages == 6 && expectation.independentRoots == 4 &&
			equalNEFFGoldenInts(neffGoldenComponentSizes(plan.components), []int{1, 1, 1, 3}) {
			coverage.mixedThreePlusSingles = true
		}
	}

	baseline, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Marshal(baseline) error: %v", err)
	}
	permutations := boundedNEFFGoldenPermutations(sources)
	for index, permutation := range permutations {
		permuted, mergeErr := Merge(permutation)
		if mergeErr != nil {
			t.Fatalf("Merge(permutation %d/%d) error: %v", index+1, len(permutations), mergeErr)
		}
		encoded, marshalErr := json.Marshal(permuted)
		if marshalErr != nil {
			t.Fatalf("Marshal(permutation %d/%d) error: %v", index+1, len(permutations), marshalErr)
		}
		if !bytes.Equal(encoded, baseline) {
			t.Fatalf("permutation %d/%d changed output:\n got %s\nwant %s", index+1, len(permutations), encoded, baseline)
		}
		if needsV0 {
			v0Permutation := append([]SourceResult(nil), permutation...)
			for sourceID := range v0Permutation {
				v0Permutation[sourceID].CleanedText = ""
			}
			v0Permuted, v0MergeErr := Merge(v0Permutation)
			if v0MergeErr != nil {
				t.Fatalf("Merge(v0 permutation %d/%d) error: %v", index+1, len(permutations), v0MergeErr)
			}
			v0Candidates, collectErr := collectNEFFGoldenCandidates(v0Permuted)
			if collectErr != nil {
				t.Fatalf("collect v0 permutation %d/%d: %v", index+1, len(permutations), collectErr)
			}
			assertNEFFGoldenV0Expectations(t, goldenCase, v0Candidates)
			v0Encoded, v0MarshalErr := json.Marshal(v0Permuted)
			if v0MarshalErr != nil {
				t.Fatalf("Marshal(v0 permutation %d/%d) error: %v", index+1, len(permutations), v0MarshalErr)
			}
			if !bytes.Equal(v0Encoded, v0Baseline) {
				t.Fatalf("v0 permutation %d/%d changed output:\n got %s\nwant %s", index+1, len(permutations), v0Encoded, v0Baseline)
			}
		}
	}
	return coverage
}

func assertNEFFGoldenV0Expectations(
	t *testing.T,
	goldenCase neffGoldenCase,
	candidates map[string]neffGoldenActualCandidate,
) {
	t.Helper()
	if len(candidates) != len(goldenCase.expectations) {
		t.Fatalf("v0 candidate count = %d, want %d", len(candidates), len(goldenCase.expectations))
	}
	expectedKeys := make(map[string]struct{}, len(goldenCase.expectations))
	for _, expectation := range goldenCase.expectations {
		key := neffGoldenCandidateKey(expectation.path, expectation.value)
		expectedKeys[key] = struct{}{}
		candidate, exists := candidates[key]
		if !exists || !bytes.Equal(candidate.value, expectation.value.raw) || candidate.agreement.Pages != expectation.pages {
			t.Fatalf("v0 candidate %q = %#v, want value=%s pages=%d", key, candidate, expectation.value.raw, expectation.pages)
		}
		if expectation.wantV0IndependentRoots.Present &&
			candidate.agreement.IndependentRoots != expectation.wantV0IndependentRoots.Value {
			t.Fatalf("v0 candidate %q = %#v, want pages=%d roots=%d", key, candidate, expectation.pages, expectation.wantV0IndependentRoots.Value)
		}
	}
	for key := range candidates {
		if _, expected := expectedKeys[key]; !expected {
			t.Fatalf("unexpected v0 candidate %q", key)
		}
	}
}

func assertNEFFGoldenExpectations(
	t *testing.T,
	goldenCase neffGoldenCase,
	actual map[string]neffGoldenActualCandidate,
	sources []SourceResult,
) {
	t.Helper()
	expected := make(map[string]neffGoldenExpectation, len(goldenCase.expectations))
	for _, expectation := range goldenCase.expectations {
		expected[neffGoldenCandidateKey(expectation.path, expectation.value)] = expectation
	}
	if len(actual) != len(expected) {
		t.Fatalf("candidate count = %d, want %d; actual=%v expected=%v", len(actual), len(expected), sortedNEFFGoldenKeys(actual), sortedNEFFGoldenKeys(expected))
	}
	caseURLs := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		caseURLs[source.URL] = struct{}{}
	}
	for key, expectation := range expected {
		candidate, exists := actual[key]
		if !exists {
			t.Fatalf("missing expected candidate %q; actual=%v", key, sortedNEFFGoldenKeys(actual))
		}
		if !bytes.Equal(candidate.value, expectation.value.raw) || candidate.agreement.Pages != expectation.pages ||
			candidate.agreement.IndependentRoots != expectation.independentRoots || candidate.agreement.FoldReason != expectation.agreementReason {
			t.Fatalf("candidate %q value/agreement = %s/%#v, want %s/pages=%d roots=%d reason=%q", key, candidate.value, candidate.agreement, expectation.value.raw, expectation.pages, expectation.independentRoots, expectation.agreementReason)
		}
		if len(candidate.supports) != expectation.pages {
			t.Fatalf("candidate %q support count = %d, want pages=%d", key, len(candidate.supports), expectation.pages)
		}
		reasons := make(map[string]FoldReason)
		seenURLs := make(map[string]struct{}, len(candidate.supports))
		for _, support := range candidate.supports {
			if _, admitted := caseURLs[support.URL]; !admitted {
				t.Fatalf("candidate %q contains support URL outside case: %q", key, support.URL)
			}
			if _, duplicate := seenURLs[support.URL]; duplicate {
				t.Fatalf("candidate %q repeats support URL %q", key, support.URL)
			}
			seenURLs[support.URL] = struct{}{}
			if support.FoldReason != "" {
				if !validNEFFGoldenFoldReason(support.FoldReason) {
					t.Fatalf("candidate %q has invalid support reason %q", key, support.FoldReason)
				}
				reasons[support.URL] = support.FoldReason
			}
		}
		if !equalNEFFGoldenReasons(reasons, expectation.supportReasons) {
			t.Fatalf("candidate %q support reasons = %#v, want %#v", key, reasons, expectation.supportReasons)
		}
	}
	for key := range actual {
		if _, exists := expected[key]; !exists {
			t.Fatalf("unexpected actual candidate %q", key)
		}
	}
}

func collectNEFFGoldenCandidates(result Result) (map[string]neffGoldenActualCandidate, error) {
	output := make(map[string]neffGoldenActualCandidate)
	add := func(path string, value json.RawMessage, agreement Agreement, supports []Support) error {
		scalar, err := canonicalScalar(value)
		if err != nil {
			return fmt.Errorf("candidate %q value: %w", path, err)
		}
		key := neffGoldenCandidateKey(path, scalar)
		if _, duplicate := output[key]; duplicate {
			return fmt.Errorf("duplicate actual candidate %q", key)
		}
		output[key] = neffGoldenActualCandidate{value: append(json.RawMessage(nil), value...), agreement: agreement, supports: supports}
		return nil
	}
	for path, field := range result.Fields {
		if field.Ambiguous {
			if field.Agreement != (Agreement{}) || len(field.Value) != 0 || len(field.Supports) != 0 || len(field.Conflicts) < 2 {
				return nil, fmt.Errorf("field %q has malformed ambiguous shape", path)
			}
		} else if err := add(path, field.Value, field.Agreement, field.Supports); err != nil {
			return nil, err
		}
		for _, conflict := range field.Conflicts {
			if err := add(path, conflict.Value, conflict.Agreement, conflict.Supports); err != nil {
				return nil, err
			}
		}
	}
	return output, nil
}

func prepareNEFFGoldenSources(t *testing.T, sources []SourceResult) []preparedSource {
	t.Helper()
	prepared := make([]preparedSource, 0, len(sources))
	for index, source := range sources {
		candidate, _, _, err := prepareSource(index, source)
		if err != nil {
			t.Fatalf("prepare source %d: %v", index, err)
		}
		prepared = append(prepared, candidate)
	}
	sort.Slice(prepared, func(i, j int) bool { return prepared[i].url < prepared[j].url })
	return prepared
}

func assertNEFFGoldenCoreIsolation(t *testing.T, sources []preparedSource) {
	t.Helper()
	coreEdge := false
	for _, source := range sources {
		for path, fragments := range source.lineage {
			if len(fragments) > 0 {
				t.Fatalf("core_mirror source %s has N-3 candidate fragments at %q: %#v", source.url, path, fragments)
			}
		}
	}
	for first := range sources {
		for second := first + 1; second < len(sources); second++ {
			if quoteLineageSimilar(sources[first].lineage, sources[first].fields, sources[second].lineage, sources[second].fields) {
				t.Fatalf("core_mirror pair %s/%s has a quote-lineage edge", sources[first].url, sources[second].url)
			}
			if sources[first].root != sources[second].root && contentCoresSimilar(sources[first].core, sources[second].core) {
				coreEdge = true
			}
		}
	}
	if !coreEdge {
		t.Fatal("core_mirror case has no cross-root production content-core edge")
	}
}

func assertNEFFGoldenLineageIsolation(t *testing.T, sources []preparedSource) {
	t.Helper()
	lineageSet := newDisjointSet(len(sources))
	lineageEdges := 0
	for first := range sources {
		for second := first + 1; second < len(sources); second++ {
			if sources[first].root == sources[second].root {
				t.Fatalf("lineage diagnostic pair shares root: %s/%s", sources[first].url, sources[second].url)
			}
			if sources[first].simText == 0 || sources[second].simText == 0 {
				t.Fatalf("lineage diagnostic pair has missing production legacy fingerprint: %s/%s", sources[first].url, sources[second].url)
			}
			legacyDistance := simhash.Distance(sources[first].simText, sources[second].simText)
			if legacyDistance <= independenceDistance {
				t.Fatalf("lineage diagnostic legacy distance %s/%s = %d, want >%d", sources[first].url, sources[second].url, legacyDistance, independenceDistance)
			}
			if !sources[first].core.valid || !sources[second].core.valid {
				t.Fatalf("lineage diagnostic pair has invalid content core: %s/%s", sources[first].url, sources[second].url)
			}
			coreDistanceGot := simhash.Distance(sources[first].core.fingerprint, sources[second].core.fingerprint)
			if coreDistanceGot <= coreDistance {
				t.Fatalf("lineage diagnostic core distance %s/%s = %d, want >%d", sources[first].url, sources[second].url, coreDistanceGot, coreDistance)
			}
			if quoteLineageSimilar(sources[first].lineage, sources[first].fields, sources[second].lineage, sources[second].fields) {
				lineageSet.union(first, second)
				lineageEdges++
			}
		}
	}
	if lineageEdges == 0 {
		t.Fatal("lineage case has no production quote-lineage edge")
	}
	representative := lineageSet.find(0)
	for sourceID := 1; sourceID < len(sources); sourceID++ {
		if lineageSet.find(sourceID) != representative {
			t.Fatal("lineage-only edge graph is not connected")
		}
	}
}

func hasNEFFGoldenShared95RuneFragment(sources []preparedSource) bool {
	fragments := make([]map[string][]string, len(sources))
	for index, source := range sources {
		fragments[index] = neffGolden95RuneFragments(source)
	}
	for first := range sources {
		for second := first + 1; second < len(sources); second++ {
			paths := make(map[string]struct{}, len(fragments[first])+len(fragments[second]))
			for path := range fragments[first] {
				paths[path] = struct{}{}
			}
			for path := range fragments[second] {
				paths[path] = struct{}{}
			}
			for path := range paths {
				firstValue, firstExists := sources[first].fields[path]
				secondValue, secondExists := sources[second].fields[path]
				if !firstExists || !secondExists || firstValue.groupKey != secondValue.groupKey {
					continue
				}
				// The boundary fixture is asymmetric by design: one page has an
				// otherwise eligible 95-rune fragment, while the other page may
				// contain it inside a production-eligible (>=96-rune) fragment.
				// Requiring both sides to be exactly 95 would miss that real
				// threshold refusal.
				if containsNEFFGoldenFragment(fragments[first][path], sources[second].lineage[path]) ||
					containsNEFFGoldenFragment(fragments[second][path], sources[first].lineage[path]) ||
					containsNEFFGoldenFragment(fragments[first][path], fragments[second][path]) {
					return true
				}
			}
		}
	}
	return false
}

func containsNEFFGoldenFragment(needles, haystacks []string) bool {
	for _, needle := range needles {
		for _, haystack := range haystacks {
			if strings.Contains(haystack, needle) {
				return true
			}
		}
	}
	return false
}

func neffGolden95RuneFragments(source preparedSource) map[string][]string {
	anchors, err := selectCoreAnchors(source.cleaned, source.basis)
	if err != nil || len(anchors) == 0 {
		return nil
	}
	bounds := make([]lineageAnchorBounds, len(anchors))
	for index, anchor := range anchors {
		bounds[index].anchor = anchor
		for kind := range lineageFragmentKinds {
			bounds[index].end[kind] = len(source.cleaned)
		}
	}
	sort.Slice(bounds, func(i, j int) bool {
		if bounds[i].anchor.start != bounds[j].anchor.start {
			return bounds[i].anchor.start < bounds[j].anchor.start
		}
		if bounds[i].anchor.end != bounds[j].anchor.end {
			return bounds[i].anchor.end < bounds[j].anchor.end
		}
		return bounds[i].anchor.path < bounds[j].anchor.path
	})
	locateLineageBounds(source.cleaned, bounds)
	output := make(map[string][]string)
	for _, bounded := range bounds {
		seen := make(map[string]struct{}, lineageFragmentKinds)
		for kind := range lineageFragmentKinds {
			start, end := bounded.start[kind], bounded.end[kind]
			if start < 0 || start > bounded.anchor.start || end < bounded.anchor.end || end > len(source.cleaned) || end < start {
				continue
			}
			candidate := source.cleaned[start:end]
			if len(candidate) > maxLineageFragmentBytes {
				continue
			}
			candidate = normalizeLineageFragment(candidate)
			if lineageAlnumRunes(candidate) != minLineageAlnumRunes-1 {
				continue
			}
			if _, duplicate := seen[candidate]; duplicate {
				continue
			}
			seen[candidate] = struct{}{}
			output[bounded.anchor.path] = append(output[bounded.anchor.path], candidate)
		}
	}
	return output
}

func provesNEFFGoldenBridge(plan independencePlan, sources []preparedSource, expectation neffGoldenExpectation) bool {
	group := make(map[int]struct{})
	byComponent := make(map[int][]int)
	for sourceID, source := range sources {
		value, exists := source.fields[expectation.path]
		if !exists || value.groupKey != expectation.value.groupKey {
			continue
		}
		group[sourceID] = struct{}{}
		byComponent[plan.components[sourceID]] = append(byComponent[plan.components[sourceID]], sourceID)
	}
	if len(group) != expectation.pages || expectation.pages <= expectation.independentRoots {
		return false
	}
	reasons := make(map[FoldReason]struct{})
	witnessNodes := make(map[int]struct{})
	for _, members := range byComponent {
		if len(members) < 2 {
			continue
		}
		for _, member := range members[1:] {
			if !addNEFFGoldenWitnessPath(plan, members[0], member, reasons, witnessNodes) {
				return false
			}
		}
	}
	crossValueExternal := false
	for sourceID := range witnessNodes {
		if _, terminal := group[sourceID]; terminal {
			continue
		}
		value, exists := sources[sourceID].fields[expectation.path]
		if exists && value.groupKey != expectation.value.groupKey {
			crossValueExternal = true
		}
	}
	return crossValueExternal && len(reasons) >= 2
}

func addNEFFGoldenWitnessPath(
	plan independencePlan,
	first int,
	second int,
	reasons map[FoldReason]struct{},
	nodes map[int]struct{},
) bool {
	ancestors := make(map[int]struct{}, len(plan.parent))
	for current := first; current >= 0; current = plan.parent[current] {
		ancestors[current] = struct{}{}
	}
	common := second
	for {
		if _, exists := ancestors[common]; exists {
			break
		}
		parent := plan.parent[common]
		reason := plan.parentReason[common]
		if parent < 0 || reason == "" {
			return false
		}
		nodes[common], nodes[parent] = struct{}{}, struct{}{}
		reasons[reason] = struct{}{}
		common = parent
	}
	for current := first; current != common; current = plan.parent[current] {
		parent := plan.parent[current]
		reason := plan.parentReason[current]
		if parent < 0 || reason == "" {
			return false
		}
		nodes[current], nodes[parent] = struct{}{}, struct{}{}
		reasons[reason] = struct{}{}
	}
	return true
}

func boundedNEFFGoldenPermutations(sources []SourceResult) [][]SourceResult {
	canonical := append([]SourceResult(nil), sources...)
	sort.Slice(canonical, func(i, j int) bool { return canonical[i].URL < canonical[j].URL })
	if len(canonical) <= 5 {
		output := make([][]SourceResult, 0)
		working := append([]SourceResult(nil), canonical...)
		var generate func(int)
		generate = func(index int) {
			if index == len(working) {
				output = append(output, append([]SourceResult(nil), working...))
				return
			}
			for candidate := index; candidate < len(working); candidate++ {
				working[index], working[candidate] = working[candidate], working[index]
				generate(index + 1)
				working[index], working[candidate] = working[candidate], working[index]
			}
		}
		generate(0)
		return output
	}

	output := make([][]SourceResult, 0, len(canonical)+neffGoldenRandomPermutations+2)
	seen := make(map[string]struct{})
	add := func(candidate []SourceResult) bool {
		key := neffGoldenPermutationKey(candidate)
		if _, duplicate := seen[key]; duplicate {
			return false
		}
		seen[key] = struct{}{}
		output = append(output, append([]SourceResult(nil), candidate...))
		return true
	}
	add(canonical)
	reversed := append([]SourceResult(nil), canonical...)
	for first, second := 0, len(reversed)-1; first < second; first, second = first+1, second-1 {
		reversed[first], reversed[second] = reversed[second], reversed[first]
	}
	add(reversed)
	for offset := 1; offset < len(canonical); offset++ {
		rotated := append([]SourceResult(nil), canonical[offset:]...)
		rotated = append(rotated, canonical[:offset]...)
		add(rotated)
	}

	random := rand.New(rand.NewSource(0x4e5ff))
	randomAdded := 0
	for attempts := 0; randomAdded < neffGoldenRandomPermutations && attempts < 4_096; attempts++ {
		candidate := append([]SourceResult(nil), canonical...)
		random.Shuffle(len(candidate), func(i, j int) { candidate[i], candidate[j] = candidate[j], candidate[i] })
		if add(candidate) {
			randomAdded++
		}
	}
	if randomAdded != neffGoldenRandomPermutations {
		panic(fmt.Sprintf("could not derive %d unique fixed-seed permutations", neffGoldenRandomPermutations))
	}
	return output
}

func neffGoldenPermutationKey(sources []SourceResult) string {
	var builder strings.Builder
	for _, source := range sources {
		builder.WriteString(source.URL)
		builder.WriteByte(0)
	}
	return builder.String()
}

func neffGoldenCandidateKey(path string, value scalarValue) string {
	return path + "\x00" + value.groupKey
}

func neffGoldenComponentSizes(components []int) []int {
	counts := make(map[int]int)
	for _, component := range components {
		counts[component]++
	}
	output := make([]int, 0, len(counts))
	for _, count := range counts {
		output = append(output, count)
	}
	sort.Ints(output)
	return output
}

func cloneNEFFGoldenReasons(input map[string]FoldReason) map[string]FoldReason {
	output := make(map[string]FoldReason, len(input))
	for rawURL, reason := range input {
		output[rawURL] = reason
	}
	return output
}

func equalNEFFGoldenReasons(first, second map[string]FoldReason) bool {
	if len(first) != len(second) {
		return false
	}
	for rawURL, reason := range first {
		if second[rawURL] != reason {
			return false
		}
	}
	return true
}

func equalNEFFGoldenInts(first, second []int) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}

func containsNEFFGoldenString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func neffGoldenSliceKey(class, language string, hard bool) string {
	return fmt.Sprintf("%s/%s/hard=%t", class, language, hard)
}

func logNEFFGoldenSlices(t *testing.T, slices map[string]*neffGoldenSlice) {
	t.Helper()
	for _, class := range neffGoldenClasses {
		for _, language := range neffGoldenLanguages {
			for _, hard := range []bool{false, true} {
				key := neffGoldenSliceKey(class, language, hard)
				slice := slices[key]
				if slice.cases == 0 {
					t.Logf("N_eff %-36s cases=0 expectations=0 result=N/A", key)
					continue
				}
				t.Logf("N_eff %-36s cases=%d/%d expectations=%d/%d", key, slice.passedCases, slice.cases, slice.passedExpect, slice.expectations)
			}
		}
	}
}

func sortedNEFFGoldenKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
