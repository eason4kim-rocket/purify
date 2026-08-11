// Command eavcorpus builds, pairs, and records the EAV golden corpus.
//
//	eavcorpus -mode fetch  -seeds seeds.json -out verify/eav/testdata/golden
//	eavcorpus -mode pair   -seeds seeds.json -out verify/eav/testdata/golden
//	eavcorpus -mode record -docs docs.jsonl -labels labels.jsonl -out .../recordings
//
// fetch downloads each seed URL and stores harvested snapshots (title,
// bounded cleaned text, candidate slate) as docs.jsonl. pair emits an
// UNAUDITED labels draft by sibling-category perturbation: every entity
// yields match and alias rows against its own document and mismatch rows
// against sibling documents, with hard flags for lexically confusable pairs.
// record replays the full judge against a live LLM (EAVCORPUS_API_KEY,
// EAVCORPUS_MODEL, EAVCORPUS_BASE_URL) and captures raw extraction and
// referee replies for offline replay in the golden tests. Drafts must be
// human-audited before they replace the committed corpus.
//
// The audited real-fetched corpus is staged under scripts/eavcorpus/corpus/
// (docs.jsonl + labels.jsonl). Promotion into verify/eav/testdata/golden/
// happens only together with fresh recordings:
//
//	EAVCORPUS_API_KEY=... eavcorpus -mode record \
//	  -docs scripts/eavcorpus/corpus/docs.jsonl \
//	  -labels scripts/eavcorpus/corpus/labels.jsonl \
//	  -out scripts/eavcorpus/corpus/recordings
//
// then copy docs.jsonl, labels.jsonl, and recordings/ over testdata/golden/
// and run the golden gates.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/use-agent/purify/cleaner"
	"github.com/use-agent/purify/llm"
	"github.com/use-agent/purify/verify/eav"
)

type seedEntity struct {
	ID      string   `json:"id"`
	Subject string   `json:"subject"`
	Aliases []string `json:"aliases,omitempty"`
	URL     string   `json:"url"`
}

type seedDomain struct {
	Domain   string       `json:"domain"`
	Entities []seedEntity `json:"entities"`
}

type corpusDoc struct {
	ID      string          `json:"id"`
	URL     string          `json:"url"`
	Title   string          `json:"title"`
	Cleaned string          `json:"cleaned"`
	Slate   []eav.Candidate `json:"slate,omitempty"`
}

type corpusLabel struct {
	Subject string `json:"subject"`
	Hint    string `json:"hint,omitempty"`
	Doc     string `json:"doc"`
	Label   string `json:"label"`
	Domain  string `json:"domain"`
	Hard    bool   `json:"hard,omitempty"`
	Waived  bool   `json:"waived,omitempty"`
	Note    string `json:"note,omitempty"`
}

func main() {
	mode := flag.String("mode", "", "fetch | pair | record")
	seedsPath := flag.String("seeds", "", "seeds JSON file (fetch, pair)")
	docsPath := flag.String("docs", "", "docs.jsonl produced by fetch (record)")
	labelsPath := flag.String("labels", "", "labels JSONL (record)")
	outDir := flag.String("out", "", "output directory")
	flag.Parse()

	var err error
	switch *mode {
	case "fetch":
		err = runFetch(*seedsPath, *outDir)
	case "pair":
		err = runPair(*seedsPath, *docsPath, *outDir)
	case "record":
		err = runRecord(*docsPath, *labelsPath, *outDir)
	default:
		err = fmt.Errorf("unknown -mode %q (want fetch, pair, or record)", *mode)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "eavcorpus: %v\n", err)
		os.Exit(1)
	}
}

func loadSeeds(path string) ([]seedDomain, error) {
	if path == "" {
		return nil, fmt.Errorf("-seeds is required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var seeds []seedDomain
	if err := json.Unmarshal(raw, &seeds); err != nil {
		return nil, fmt.Errorf("decode seeds: %w", err)
	}
	if len(seeds) == 0 {
		return nil, fmt.Errorf("seeds file has no domains")
	}
	return seeds, nil
}

func runFetch(seedsPath, outDir string) error {
	seeds, err := loadSeeds(seedsPath)
	if err != nil {
		return err
	}
	if outDir == "" {
		return fmt.Errorf("-out is required")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	pipeline := cleaner.NewCleaner()
	docs := make([]corpusDoc, 0, 64)
	failures := 0
	for _, domain := range seeds {
		for _, entity := range domain.Entities {
			doc, fetchErr := fetchOne(client, pipeline, domain.Domain, entity)
			if fetchErr != nil {
				failures++
				fmt.Fprintf(os.Stderr, "fetch %s/%s: %v\n", domain.Domain, entity.ID, fetchErr)
				continue
			}
			docs = append(docs, doc)
			fmt.Printf("fetched %s (%d bytes cleaned, %d candidates)\n", doc.ID, len(doc.Cleaned), len(doc.Slate))
		}
	}
	if len(docs) == 0 {
		return fmt.Errorf("every fetch failed (%d attempts)", failures)
	}
	return writeJSONL(filepath.Join(outDir, "docs.jsonl"), len(docs), func(index int) (any, error) {
		return docs[index], nil
	})
}

func fetchOne(client *http.Client, pipeline *cleaner.Cleaner, domain string, entity seedEntity) (corpusDoc, error) {
	request, err := http.NewRequest(http.MethodGet, entity.URL, nil)
	if err != nil {
		return corpusDoc{}, err
	}
	request.Header.Set("User-Agent", "purify-eavcorpus/1.0 (+corpus builder)")
	response, err := client.Do(request)
	if err != nil {
		return corpusDoc{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return corpusDoc{}, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	rawHTML, err := io.ReadAll(io.LimitReader(response.Body, int64(eav.MaxDocumentBytes)+1))
	if err != nil {
		return corpusDoc{}, err
	}
	if len(rawHTML) > eav.MaxDocumentBytes {
		return corpusDoc{}, fmt.Errorf("page exceeds %d bytes", eav.MaxDocumentBytes)
	}
	cleanedResponse, err := pipeline.Clean(string(rawHTML), entity.URL, "text", "auto")
	if err != nil {
		return corpusDoc{}, fmt.Errorf("clean: %w", err)
	}
	title := strings.TrimSpace(cleanedResponse.Metadata.Title)
	cleaned := strings.TrimSpace(cleanedResponse.Content)
	if title == "" || cleaned == "" {
		return corpusDoc{}, fmt.Errorf("empty title or cleaned text")
	}
	slate := eav.HarvestCandidates(eav.Document{
		URL:     entity.URL,
		Title:   title,
		Cleaned: cleaned,
		RawHTML: string(rawHTML),
	})
	return corpusDoc{
		ID:      domain + "/" + entity.ID,
		URL:     entity.URL,
		Title:   title,
		Cleaned: runeSafePrefix(cleaned, eav.MaxHeadWindowBytes),
		Slate:   slate,
	}, nil
}

// hardConfusabilityFloor separates a hard (lexically confusable) mismatch
// draft row from an ordinary sibling row.
const hardConfusabilityFloor = 0.34

func runPair(seedsPath, docsPath, outDir string) error {
	seeds, err := loadSeeds(seedsPath)
	if err != nil {
		return err
	}
	if outDir == "" {
		return fmt.Errorf("-out is required")
	}
	fetched := map[string]struct{}{}
	if docsPath != "" {
		if err := readJSONL(docsPath, func(line []byte) error {
			var doc corpusDoc
			if err := json.Unmarshal(line, &doc); err != nil {
				return err
			}
			fetched[doc.ID] = struct{}{}
			return nil
		}); err != nil {
			return fmt.Errorf("read docs: %w", err)
		}
	}
	hasDoc := func(docID string) bool {
		if docsPath == "" {
			return true
		}
		_, ok := fetched[docID]
		return ok
	}

	labels := make([]corpusLabel, 0, 512)
	for _, domain := range seeds {
		for index, entity := range domain.Entities {
			docID := domain.Domain + "/" + entity.ID
			if !hasDoc(docID) {
				continue
			}
			labels = append(labels, corpusLabel{
				Subject: entity.Subject, Doc: docID, Label: "match",
				Domain: domain.Domain, Note: "UNAUDITED DRAFT",
			})
			for _, alias := range entity.Aliases {
				labels = append(labels, corpusLabel{
					Subject: alias, Doc: docID, Label: "match",
					Domain: domain.Domain, Note: "UNAUDITED DRAFT: alias",
				})
			}
			for _, pick := range siblingPicks(domain.Entities, index) {
				note := "UNAUDITED DRAFT: sibling"
				if pick.hard {
					note = "UNAUDITED DRAFT: confusable sibling"
				}
				labels = append(labels, corpusLabel{
					Subject: domain.Entities[pick.index].Subject, Doc: docID, Label: "mismatch",
					Domain: domain.Domain, Hard: pick.hard, Note: note,
				})
			}
		}
	}
	if len(labels) == 0 {
		return fmt.Errorf("no labels generated")
	}
	return writeJSONL(filepath.Join(outDir, "labels-draft.jsonl"), len(labels), func(index int) (any, error) {
		return labels[index], nil
	})
}

type siblingPick struct {
	index int
	hard  bool
}

// siblingPicks returns up to three mismatch partners for the entity at
// index: the two most lexically confusable siblings and the least similar
// one. Hardness is decided by the confusability score itself, not by rank,
// so a domain of fully distinct names yields no false hard rows.
func siblingPicks(entities []seedEntity, index int) []siblingPick {
	subject := eav.Normalize(entities[index].Subject)
	type scored struct {
		index int
		score float64
	}
	siblings := make([]scored, 0, len(entities)-1)
	for sibling := range entities {
		if sibling == index {
			continue
		}
		siblings = append(siblings, scored{
			index: sibling,
			score: confusability(subject, eav.Normalize(entities[sibling].Subject)),
		})
	}
	sort.Slice(siblings, func(first, second int) bool {
		if siblings[first].score != siblings[second].score {
			return siblings[first].score > siblings[second].score
		}
		return entities[siblings[first].index].Subject < entities[siblings[second].index].Subject
	})
	picks := make([]siblingPick, 0, 3)
	for _, candidate := range siblings {
		if len(picks) == 2 {
			break
		}
		picks = append(picks, siblingPick{index: candidate.index, hard: candidate.score >= hardConfusabilityFloor})
	}
	if len(siblings) > len(picks) {
		last := siblings[len(siblings)-1]
		duplicate := false
		for _, pick := range picks {
			if pick.index == last.index {
				duplicate = true
				break
			}
		}
		if !duplicate {
			picks = append(picks, siblingPick{index: last.index, hard: last.score >= hardConfusabilityFloor})
		}
	}
	return picks
}

// confusability is a draft-labeling heuristic only: token overlap plus a
// bounded edit-distance bonus. The real ladder's similarity stays private to
// the eav package; drafts are audited by a human anyway.
func confusability(first, second string) float64 {
	firstTokens := strings.Fields(first)
	secondTokens := strings.Fields(second)
	set := make(map[string]struct{}, len(firstTokens))
	for _, token := range firstTokens {
		set[token] = struct{}{}
	}
	shared := 0
	for _, token := range secondTokens {
		if _, ok := set[token]; ok {
			shared++
		}
	}
	union := len(firstTokens) + len(secondTokens) - shared
	score := 0.0
	if union > 0 {
		score = float64(shared) / float64(union)
	}
	if distance := editDistance([]rune(strings.ReplaceAll(first, " ", "")), []rune(strings.ReplaceAll(second, " ", ""))); distance <= 2 {
		score += 0.5
	}
	return score
}

func editDistance(first, second []rune) int {
	if len(first) == 0 {
		return len(second)
	}
	if len(second) == 0 {
		return len(first)
	}
	previous := make([]int, len(second)+1)
	current := make([]int, len(second)+1)
	for column := range previous {
		previous[column] = column
	}
	for row := 1; row <= len(first); row++ {
		current[0] = row
		for column := 1; column <= len(second); column++ {
			cost := 1
			if first[row-1] == second[column-1] {
				cost = 0
			}
			current[column] = min(previous[column]+1, current[column-1]+1, previous[column-1]+cost)
		}
		previous, current = current, previous
	}
	return previous[len(second)]
}

type extractRecordingRow struct {
	Doc   string          `json:"doc"`
	Reply json.RawMessage `json:"reply"`
}

type refereeRecordingRow struct {
	Doc     string          `json:"doc"`
	Subject string          `json:"subject"`
	Reply   json.RawMessage `json:"reply"`
}

// recordingExtractor calls the live model once per document and captures the
// raw reply. llm.Client owns the system prompt, so the eav instruction is
// prepended to the user content — the recording is the reply, which replays
// identically either way.
type recordingExtractor struct {
	client     *llm.Client
	params     llm.ExtractParams
	urlToDocID map[string]string
	replies    map[string]json.RawMessage
}

func (recorder *recordingExtractor) ExtractEntities(
	ctx context.Context,
	doc eav.Document,
	slate []eav.Candidate,
) (eav.DocumentEntities, error) {
	docID := recorder.urlToDocID[doc.URL]
	if raw, ok := recorder.replies[docID]; ok {
		return eav.DecodeExtractionReply(raw)
	}
	content := eav.ExtractionSystemPrompt + "\n\n" + eav.BuildExtractionInput(doc, slate)
	result, err := recorder.client.Extract(ctx, content, json.RawMessage(eav.ExtractionReplySchema), recorder.params)
	if err != nil {
		return eav.DocumentEntities{}, err
	}
	recorder.replies[docID] = result.Data
	return eav.DecodeExtractionReply(result.Data)
}

type recordingReferee struct {
	client     *llm.Client
	params     llm.ExtractParams
	urlToDocID map[string]string
	replies    map[string]json.RawMessage
}

func (recorder *recordingReferee) SameReferent(
	ctx context.Context,
	subject eav.Subject,
	entity eav.Entity,
	doc eav.Document,
) (eav.RefereeVerdict, error) {
	key := recorder.urlToDocID[doc.URL] + "\x00" + eav.Normalize(subject.Name)
	if raw, ok := recorder.replies[key]; ok {
		return eav.DecodeRefereeReply(raw)
	}
	content := eav.RefereeSystemPrompt + "\n\n" + eav.BuildRefereeInput(subject, entity, doc)
	result, err := recorder.client.Extract(ctx, content, json.RawMessage(eav.RefereeReplySchema), recorder.params)
	if err != nil {
		return eav.RefereeVerdict{}, err
	}
	recorder.replies[key] = result.Data
	return eav.DecodeRefereeReply(result.Data)
}

func runRecord(docsPath, labelsPath, outDir string) error {
	if docsPath == "" || labelsPath == "" || outDir == "" {
		return fmt.Errorf("-docs, -labels, and -out are required")
	}
	apiKey := os.Getenv("EAVCORPUS_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("EAVCORPUS_API_KEY is required")
	}
	params := llm.ExtractParams{
		APIKey:  apiKey,
		Model:   envOr("EAVCORPUS_MODEL", "gpt-4o-mini"),
		BaseURL: envOr("EAVCORPUS_BASE_URL", "https://api.openai.com/v1"),
	}

	docs := make(map[string]corpusDoc)
	urlToDocID := make(map[string]string)
	if err := readJSONL(docsPath, func(line []byte) error {
		var doc corpusDoc
		if err := json.Unmarshal(line, &doc); err != nil {
			return err
		}
		docs[doc.ID] = doc
		urlToDocID[doc.URL] = doc.ID
		return nil
	}); err != nil {
		return fmt.Errorf("read docs: %w", err)
	}
	labels := make([]corpusLabel, 0, 256)
	if err := readJSONL(labelsPath, func(line []byte) error {
		var label corpusLabel
		if err := json.Unmarshal(line, &label); err != nil {
			return err
		}
		if _, ok := docs[label.Doc]; !ok {
			return fmt.Errorf("label references unknown doc %q", label.Doc)
		}
		labels = append(labels, label)
		return nil
	}); err != nil {
		return fmt.Errorf("read labels: %w", err)
	}

	extractor := &recordingExtractor{
		client: llm.NewClient(nil), params: params,
		urlToDocID: urlToDocID, replies: make(map[string]json.RawMessage),
	}
	referee := &recordingReferee{
		client: llm.NewClient(nil), params: params,
		urlToDocID: urlToDocID, replies: make(map[string]json.RawMessage),
	}
	judge, err := eav.NewJudge(eav.Config{Extractor: extractor, Referee: referee})
	if err != nil {
		return err
	}

	ctx := context.Background()
	agreements, disagreements := 0, 0
	for _, label := range labels {
		doc := docs[label.Doc]
		judgment, judgeErr := judge.JudgeDocument(
			ctx,
			eav.Subject{Name: label.Subject, Hint: label.Hint},
			eav.Document{URL: doc.URL, Title: doc.Title, Cleaned: doc.Cleaned},
		)
		if judgeErr != nil {
			return fmt.Errorf("judge %q vs %s: %w", label.Subject, label.Doc, judgeErr)
		}
		agreed := (label.Label == "match" && judgment.Verdict == eav.VerdictMatch) ||
			(label.Label == "mismatch" && judgment.Verdict == eav.VerdictMismatch)
		marker := "AGREE   "
		if !agreed {
			marker = "DISAGREE"
			disagreements++
		} else {
			agreements++
		}
		fmt.Printf("%s %-28q vs %-28s labeled %-8s judged %s/%s\n",
			marker, label.Subject, label.Doc, label.Label, judgment.Verdict, judgment.Tier)
	}
	fmt.Printf("preview: %d agree, %d disagree across %d rows\n", agreements, disagreements, len(labels))

	extractRows := make([]extractRecordingRow, 0, len(extractor.replies))
	for docID, reply := range extractor.replies {
		extractRows = append(extractRows, extractRecordingRow{Doc: docID, Reply: reply})
	}
	sort.Slice(extractRows, func(first, second int) bool {
		return extractRows[first].Doc < extractRows[second].Doc
	})
	refereeRows := make([]refereeRecordingRow, 0, len(referee.replies))
	for key, reply := range referee.replies {
		docID, subject, _ := strings.Cut(key, "\x00")
		refereeRows = append(refereeRows, refereeRecordingRow{Doc: docID, Subject: subject, Reply: reply})
	}
	sort.Slice(refereeRows, func(first, second int) bool {
		if refereeRows[first].Doc != refereeRows[second].Doc {
			return refereeRows[first].Doc < refereeRows[second].Doc
		}
		return refereeRows[first].Subject < refereeRows[second].Subject
	})

	if err := writeJSONL(filepath.Join(outDir, "extract.jsonl"), len(extractRows), func(index int) (any, error) {
		return extractRows[index], nil
	}); err != nil {
		return err
	}
	return writeJSONL(filepath.Join(outDir, "referee.jsonl"), len(refereeRows), func(index int) (any, error) {
		return refereeRows[index], nil
	})
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func readJSONL(path string, handle func(line []byte) error) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if err := handle([]byte(trimmed)); err != nil {
			return err
		}
	}
	return nil
}

func writeJSONL(path string, count int, row func(index int) (any, error)) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var output strings.Builder
	for index := 0; index < count; index++ {
		value, err := row(index)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		output.Write(encoded)
		output.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(output.String()), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%d rows)\n", path, count)
	return nil
}

func runeSafePrefix(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}
