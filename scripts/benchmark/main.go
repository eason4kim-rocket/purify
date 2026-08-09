package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/use-agent/purify/compiler"
	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/extract"
	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/llm"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/scrape"
	"github.com/use-agent/purify/scraper"
)

// CLI flags
var (
	apiURL = flag.String("api-url", "http://localhost:8080", "Purify API base URL")
	apiKey = flag.String("api-key", "", "API key for authenticated requests")
	runs   = flag.Int("runs", 3, "Number of runs per URL for averaging")
	output = flag.String("output", "benchmark-results.json", "JSON output file path")
)

// Test URLs covering 5 site types.
var testURLs = []struct {
	Label string
	URL   string
}{
	{"Static", "https://example.com"},
	{"Blog", "https://go.dev/blog/go1.21"},
	{"Docs", "https://go.dev/doc/effective_go"},
	{"News", "https://www.bbc.com/news"},
	{"Complex", "https://github.com/go-rod/rod"},
}

// --- Request / Response types (mirrors models package) ---

type scrapeRequest struct {
	URL          string `json:"url"`
	OutputFormat string `json:"output_format"`
	Timeout      int    `json:"timeout"`
}

type scrapeResponse struct {
	Success    bool         `json:"success"`
	StatusCode int          `json:"status_code"`
	Content    string       `json:"content"`
	Metadata   metadata     `json:"metadata"`
	Links      links        `json:"links"`
	Tokens     tokenInfo    `json:"tokens"`
	Timing     timingInfo   `json:"timing"`
	Error      *errorDetail `json:"error,omitempty"`
}

type metadata struct {
	Title string `json:"title"`
}

type links struct {
	Internal []link `json:"internal"`
	External []link `json:"external"`
}

type link struct {
	Href string `json:"href"`
}

type tokenInfo struct {
	OriginalEstimate int     `json:"original_estimate"`
	CleanedEstimate  int     `json:"cleaned_estimate"`
	SavingsPercent   float64 `json:"savings_percent"`
}

type timingInfo struct {
	TotalMs      int64 `json:"total_ms"`
	NavigationMs int64 `json:"navigation_ms"`
	CleaningMs   int64 `json:"cleaning_ms"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// --- Benchmark result types ---

type runResult struct {
	Run            int     `json:"run"`
	TotalMs        int64   `json:"total_ms"`
	NavigationMs   int64   `json:"navigation_ms"`
	CleaningMs     int64   `json:"cleaning_ms"`
	OriginalTokens int     `json:"original_tokens"`
	CleanedTokens  int     `json:"cleaned_tokens"`
	SavingsPercent float64 `json:"savings_percent"`
	ContentLength  int     `json:"content_length"`
	StatusCode     int     `json:"status_code"`
	HasTitle       bool    `json:"has_title"`
	HasLinks       bool    `json:"has_links"`
	Success        bool    `json:"success"`
	Error          string  `json:"error,omitempty"`
}

type urlAverages struct {
	TotalMs        float64 `json:"total_ms"`
	NavigationMs   float64 `json:"navigation_ms"`
	CleaningMs     float64 `json:"cleaning_ms"`
	SavingsPercent float64 `json:"savings_percent"`
	ContentLength  float64 `json:"content_length"`
}

type urlResult struct {
	URL      string       `json:"url"`
	Label    string       `json:"label"`
	Runs     []runResult  `json:"runs"`
	Averages *urlAverages `json:"averages,omitempty"`
}

type benchmarkReport struct {
	Timestamp  string      `json:"timestamp"`
	APIURL     string      `json:"api_url"`
	RunsPerURL int         `json:"runs_per_url"`
	Results    []urlResult `json:"results"`
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "evidence" {
		if err := runEvidenceBenchmark(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "evidence benchmark failed: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "compiled" {
		if err := runCompiledBenchmark(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "compiled benchmark failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	flag.Parse()

	fmt.Println("=== Purify Benchmark Suite ===")
	fmt.Printf("API URL:   %s\n", *apiURL)
	fmt.Printf("Runs/URL:  %d\n", *runs)
	fmt.Printf("Output:    %s\n", *output)
	fmt.Println()

	// Quick connectivity check.
	if err := checkAPI(*apiURL); err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot reach API at %s: %v\n", *apiURL, err)
		fmt.Fprintf(os.Stderr, "Make sure Purify is running (e.g. make run)\n")
		os.Exit(1)
	}

	report := benchmarkReport{
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		APIURL:     *apiURL,
		RunsPerURL: *runs,
	}

	for _, t := range testURLs {
		fmt.Printf("Benchmarking [%s] %s ...\n", t.Label, t.URL)
		ur := urlResult{URL: t.URL, Label: t.Label}

		for i := 1; i <= *runs; i++ {
			fmt.Printf("  Run %d/%d ... ", i, *runs)
			rr := benchmarkURL(t.URL, i)
			if rr.Success {
				fmt.Printf("OK  %dms  %.1f%% saved\n", rr.TotalMs, rr.SavingsPercent)
			} else {
				fmt.Printf("FAILED: %s\n", rr.Error)
			}
			ur.Runs = append(ur.Runs, rr)
		}

		ur.Averages = computeAverages(ur.Runs)
		report.Results = append(report.Results, ur)
		fmt.Println()
	}

	// Print summary table.
	printTable(report.Results)

	// Write JSON report.
	if err := writeJSON(*output, report); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing JSON output: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\nDetailed results written to %s\n", *output)
}

const compiledP95ThresholdMilliseconds = 50.0

type compiledBenchmarkReport struct {
	Runs            int     `json:"runs"`
	WarmupRuns      int     `json:"warmup_runs"`
	P50Ms           float64 `json:"p50_ms"`
	P95Ms           float64 `json:"p95_ms"`
	P99Ms           float64 `json:"p99_ms"`
	LLMCalls        int64   `json:"llm_calls"`
	ThresholdMs     float64 `json:"threshold_ms"`
	ThresholdPassed bool    `json:"threshold_passed"`
}

func runCompiledBenchmark(args []string) error {
	flags := flag.NewFlagSet("compiled", flag.ContinueOnError)
	runCount := flags.Int("runs", 1000, "number of measured full compiled extraction runs")
	warmupCount := flags.Int("warmup", 100, "number of full compiled extraction warmup runs")
	outputPath := flags.String("output", "", "optional JSON report path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}

	dataDir, err := os.MkdirTemp("", "purify-compiled-benchmark-")
	if err != nil {
		return fmt.Errorf("create benchmark data directory: %w", err)
	}
	defer os.RemoveAll(dataDir)

	report, err := measureCompiledBenchmark(context.Background(), dataDir, *runCount, *warmupCount)
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	fmt.Println(string(encoded))
	if *outputPath != "" {
		if err := os.WriteFile(*outputPath, append(encoded, '\n'), 0644); err != nil {
			return fmt.Errorf("write report: %w", err)
		}
	}
	if !report.ThresholdPassed {
		return fmt.Errorf("P95 %.3fms is not below %.0fms", report.P95Ms, compiledP95ThresholdMilliseconds)
	}
	return nil
}

func measureCompiledBenchmark(
	ctx context.Context,
	dataDir string,
	runCount, warmupCount int,
) (compiledBenchmarkReport, error) {
	if ctx == nil {
		return compiledBenchmarkReport{}, errors.New("compiled benchmark context is nil")
	}
	if err := ctx.Err(); err != nil {
		return compiledBenchmarkReport{}, err
	}
	if strings.TrimSpace(dataDir) == "" {
		return compiledBenchmarkReport{}, errors.New("compiled benchmark data directory is required")
	}
	if runCount < 1 {
		return compiledBenchmarkReport{}, errors.New("runs must be at least 1")
	}
	if warmupCount < 0 {
		return compiledBenchmarkReport{}, errors.New("warmup must be non-negative")
	}

	durable, err := ledger.Open(dataDir)
	if err != nil {
		return compiledBenchmarkReport{}, fmt.Errorf("open benchmark ledger: %w", err)
	}
	defer durable.Close()
	repository, err := compiler.NewStore(durable)
	if err != nil {
		return compiledBenchmarkReport{}, fmt.Errorf("open compiled extractor store: %w", err)
	}

	const sourceURL = "https://shop.example.com/products/purify-pro"
	const rawHTML = `<html><body><main class="product"><h1 class="product-title">Purify Pro</h1><span class="price" data-value="1299.50">$1,299.50</span><span class="stock"> TRUE </span></main></body></html>`
	schema := json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"},"price":{"type":"number"},"available":{"type":"boolean"}},"required":["title","price","available"],"additionalProperties":false}`)
	ir := compiler.IR{Version: compiler.CurrentIRVersion, Fields: []compiler.FieldRule{
		{Name: "title", Selector: ".product-title", Transforms: []string{"trim"}, Type: compiler.TypeString, Required: true},
		{Name: "price", Selector: ".price", Attr: "data-value", Type: compiler.TypeNumber, Required: true},
		{Name: "available", Selector: ".stock", Transforms: []string{"trim", "lower"}, Type: compiler.TypeBoolean, Required: true},
	}}
	validation := compiler.ValidationReport{
		PerField: []compiler.FieldValidation{
			{Name: "title", Matches: 3, Samples: 3, Score: 1},
			{Name: "price", Matches: 3, Samples: 3, Score: 1},
			{Name: "available", Matches: 3, Samples: 3, Score: 1},
		},
		Overall: 1, Samples: 3, ValidSamples: 3,
		Threshold: compiler.ValidationThreshold, CanEnable: true,
	}
	key, err := compiler.BuildPageKey(sourceURL, schema, rawHTML)
	if err != nil {
		return compiledBenchmarkReport{}, fmt.Errorf("build benchmark page key: %w", err)
	}
	revision, err := repository.Save(ctx, key, ir, validation)
	if err != nil {
		return compiledBenchmarkReport{}, fmt.Errorf("seed active compiled extractor: %w", err)
	}

	llmGuard := &countingBenchmarkExtractor{}
	service, err := extract.NewService(unusedBenchmarkRunner{}, llmGuard, nil, extract.Config{
		CompiledRepository: repository,
	})
	if err != nil {
		return compiledBenchmarkReport{}, fmt.Errorf("create extraction service: %w", err)
	}
	artifact := &extract.Artifact{
		Public: &models.ScrapeResponse{
			Success: true, StatusCode: http.StatusOK, FinalURL: sourceURL,
			Content: "Purify Pro $1,299.50 TRUE",
		},
		Source: &scraper.ScrapeResult{
			RawHTML: rawHTML, StatusCode: http.StatusOK, FinalURL: sourceURL,
			FetchedAt: time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC),
		},
	}
	request := &models.ExtractRequest{
		URL: sourceURL, Schema: append(json.RawMessage(nil), schema...), Engine: "auto",
		LLMAPIKey:    "compiled-benchmark-must-not-call-llm",
		OutputFormat: "markdown", ExtractMode: "readability",
	}
	wantData := json.RawMessage(`{"available":true,"price":1299.50,"title":"Purify Pro"}`)
	runOnce := func() error {
		response, err := service.ExtractArtifact(ctx, artifact, request)
		if err != nil {
			return err
		}
		if response == nil || !response.Success || response.Extractor == nil ||
			response.Extractor.Mode != "compiled" || response.Extractor.ID != revision.ID ||
			response.Extractor.Version != revision.Version || !bytes.Equal(response.Data, wantData) {
			return fmt.Errorf("unexpected compiled response: %#v", response)
		}
		if calls := llmGuard.calls.Load(); calls != 0 {
			return fmt.Errorf("compiled path invoked structured extractor %d times", calls)
		}
		return nil
	}

	for index := 0; index < warmupCount; index++ {
		if err := runOnce(); err != nil {
			return compiledBenchmarkReport{}, fmt.Errorf("warmup %d: %w", index+1, err)
		}
	}
	durations := make([]time.Duration, runCount)
	for index := range durations {
		started := time.Now()
		if err := runOnce(); err != nil {
			return compiledBenchmarkReport{}, fmt.Errorf("measured run %d: %w", index+1, err)
		}
		durations[index] = time.Since(started)
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	report := compiledBenchmarkReport{
		Runs:        runCount,
		WarmupRuns:  warmupCount,
		P50Ms:       durationMilliseconds(percentileDuration(durations, 0.50)),
		P95Ms:       durationMilliseconds(percentileDuration(durations, 0.95)),
		P99Ms:       durationMilliseconds(percentileDuration(durations, 0.99)),
		LLMCalls:    llmGuard.calls.Load(),
		ThresholdMs: compiledP95ThresholdMilliseconds,
	}
	report.ThresholdPassed = report.P95Ms < report.ThresholdMs
	return report, nil
}

type unusedBenchmarkRunner struct{}

func (unusedBenchmarkRunner) Run(context.Context, *models.ScrapeRequest, scrape.Observer) (*scrape.Result, error) {
	return nil, errors.New("compiled benchmark unexpectedly invoked the scrape runner")
}

type countingBenchmarkExtractor struct {
	calls atomic.Int64
}

func (extractor *countingBenchmarkExtractor) Extract(
	context.Context,
	string,
	json.RawMessage,
	llm.ExtractParams,
) (*llm.ExtractResult, error) {
	extractor.calls.Add(1)
	return nil, errors.New("compiled benchmark unexpectedly invoked the LLM extractor")
}

func (extractor *countingBenchmarkExtractor) ExtractWithRepair(
	context.Context,
	string,
	json.RawMessage,
	json.RawMessage,
	[]llm.Violation,
	llm.ExtractParams,
) (*llm.ExtractResult, error) {
	extractor.calls.Add(1)
	return nil, errors.New("compiled benchmark unexpectedly invoked the LLM repair extractor")
}

type evidenceBenchmarkReport struct {
	Timestamp       string  `json:"timestamp"`
	Runs            int     `json:"runs"`
	FieldsPerRun    int     `json:"fields_per_run"`
	P50Ms           float64 `json:"p50_ms"`
	P95Ms           float64 `json:"p95_ms"`
	P99Ms           float64 `json:"p99_ms"`
	ThresholdMs     float64 `json:"threshold_ms"`
	UnlocatedRate   float64 `json:"unlocated_rate"`
	ThresholdPassed bool    `json:"threshold_passed"`
}

func runEvidenceBenchmark(args []string) error {
	flags := flag.NewFlagSet("evidence", flag.ContinueOnError)
	runCount := flags.Int("runs", 1000, "number of measured AlignAll runs")
	outputPath := flags.String("output", "", "optional JSON report path")
	threshold := flags.Float64("threshold-ms", 30, "maximum accepted P95 in milliseconds")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *runCount < 1 {
		return fmt.Errorf("runs must be at least 1")
	}
	if *threshold <= 0 {
		return fmt.Errorf("threshold-ms must be greater than 0")
	}

	data := json.RawMessage(`{"title":"Pro Plan","price":"1299","features":["Fast reliable search","Evidence receipts"],"available":true}`)
	cleaned := "Pro Plan costs $1,299. Fast reliable search with Evidence receipts. Available: true."
	rawHTML := `<main><article id="pro"><h1>Pro Plan</h1><p>Pro Plan costs $1,299.</p><ul><li>Fast reliable search</li><li>Evidence receipts</li></ul><p>Available: true.</p></article></main>`

	// Warm parser and allocator paths before collecting latency samples.
	for i := 0; i < 100; i++ {
		evidence.AlignAll(data, cleaned, rawHTML, "sha256:benchmark")
	}

	durations := make([]time.Duration, *runCount)
	var unlocatedRate float64
	fields := 0
	for i := range durations {
		started := time.Now()
		basis, rate := evidence.AlignAll(data, cleaned, rawHTML, "sha256:benchmark")
		durations[i] = time.Since(started)
		fields = len(basis)
		unlocatedRate = rate
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })

	report := evidenceBenchmarkReport{
		Timestamp:     time.Now().UTC().Format(time.RFC3339),
		Runs:          *runCount,
		FieldsPerRun:  fields,
		P50Ms:         durationMilliseconds(percentileDuration(durations, 0.50)),
		P95Ms:         durationMilliseconds(percentileDuration(durations, 0.95)),
		P99Ms:         durationMilliseconds(percentileDuration(durations, 0.99)),
		ThresholdMs:   *threshold,
		UnlocatedRate: unlocatedRate,
	}
	report.ThresholdPassed = report.P95Ms < report.ThresholdMs

	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	if *outputPath != "" {
		if err := os.WriteFile(*outputPath, append(encoded, '\n'), 0644); err != nil {
			return fmt.Errorf("write report: %w", err)
		}
	}
	if !report.ThresholdPassed {
		return fmt.Errorf("P95 %.3fms is not below %.3fms", report.P95Ms, report.ThresholdMs)
	}
	return nil
}

func percentileDuration(sorted []time.Duration, quantile float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	index := int(math.Ceil(quantile*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func durationMilliseconds(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}

func checkAPI(baseURL string) error {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(baseURL + "/api/v1/health")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func benchmarkURL(url string, run int) runResult {
	rr := runResult{Run: run}

	reqBody := scrapeRequest{
		URL:          url,
		OutputFormat: "markdown",
		Timeout:      60,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		rr.Error = fmt.Sprintf("marshal error: %v", err)
		return rr
	}

	req, err := http.NewRequest("POST", *apiURL+"/api/v1/scrape", bytes.NewReader(bodyBytes))
	if err != nil {
		rr.Error = fmt.Sprintf("request error: %v", err)
		return rr
	}
	req.Header.Set("Content-Type", "application/json")
	if *apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+*apiKey)
	}

	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		rr.Error = fmt.Sprintf("request failed: %v", err)
		return rr
	}
	defer resp.Body.Close()

	var sr scrapeResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		rr.Error = fmt.Sprintf("decode error: %v", err)
		return rr
	}

	rr.Success = sr.Success
	rr.StatusCode = sr.StatusCode
	rr.TotalMs = sr.Timing.TotalMs
	rr.NavigationMs = sr.Timing.NavigationMs
	rr.CleaningMs = sr.Timing.CleaningMs
	rr.OriginalTokens = sr.Tokens.OriginalEstimate
	rr.CleanedTokens = sr.Tokens.CleanedEstimate
	rr.SavingsPercent = sr.Tokens.SavingsPercent
	rr.ContentLength = len(sr.Content)
	rr.HasTitle = sr.Metadata.Title != ""
	rr.HasLinks = len(sr.Links.Internal)+len(sr.Links.External) > 0

	if sr.Error != nil {
		rr.Error = sr.Error.Message
	}

	return rr
}

func computeAverages(runs []runResult) *urlAverages {
	var successCount int
	var avg urlAverages

	for _, r := range runs {
		if !r.Success {
			continue
		}
		successCount++
		avg.TotalMs += float64(r.TotalMs)
		avg.NavigationMs += float64(r.NavigationMs)
		avg.CleaningMs += float64(r.CleaningMs)
		avg.SavingsPercent += r.SavingsPercent
		avg.ContentLength += float64(r.ContentLength)
	}

	if successCount == 0 {
		return nil
	}

	n := float64(successCount)
	avg.TotalMs /= n
	avg.NavigationMs /= n
	avg.CleaningMs /= n
	avg.SavingsPercent /= n
	avg.ContentLength /= n
	return &avg
}

func printTable(results []urlResult) {
	fmt.Println(strings.Repeat("─", 85))
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "URL\tAvg Latency\tTokens Saved\tContent Len\tStatus\n")
	fmt.Fprintf(w, "───\t───────────\t────────────\t───────────\t──────\n")

	for _, r := range results {
		if r.Averages == nil {
			fmt.Fprintf(w, "%s\tFAILED\t-\t-\t-\n", truncateURL(r.URL, 40))
			continue
		}

		// Determine dominant status code from runs.
		status := dominantStatus(r.Runs)

		fmt.Fprintf(w, "%s\t%dms\t%.1f%%\t%s\t%d\n",
			truncateURL(r.URL, 40),
			int64(r.Averages.TotalMs),
			r.Averages.SavingsPercent,
			formatInt(int(r.Averages.ContentLength)),
			status,
		)
	}

	w.Flush()
	fmt.Println(strings.Repeat("─", 85))
}

func dominantStatus(runs []runResult) int {
	counts := map[int]int{}
	for _, r := range runs {
		if r.Success {
			counts[r.StatusCode]++
		}
	}
	best, bestCount := 0, 0
	for code, count := range counts {
		if count > bestCount {
			best = code
			bestCount = count
		}
	}
	return best
}

func truncateURL(u string, max int) string {
	if len(u) <= max {
		return u
	}
	return u[:max-3] + "..."
}

func formatInt(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var result []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return string(result)
}

func writeJSON(path string, report benchmarkReport) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
