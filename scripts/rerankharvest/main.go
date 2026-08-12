// Command rerankharvest emits a provider-free seed-order construction corpus.
// It never calls a search API, never persists Search Results, and never
// enables the production reranker.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/use-agent/purify/cleaner"
	"github.com/use-agent/purify/internal/rerankeval"
)

const (
	commandTemporaryPattern = ".rerankharvest-partial-*"
	casesOutputName         = "cases.jsonl"
	docsOutputName          = "docs.jsonl"
)

var errUsage = errors.New("rerankharvest: invalid invocation")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "rerankharvest: command failed")
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string) error {
	if ctx == nil {
		return errUsage
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	flags := flag.NewFlagSet("rerankharvest", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	seedsPath := flags.String("seeds", "", "strict harvest seed JSON")
	outputDir := flags.String("out", "", "new directory for cases.jsonl and docs.jsonl")
	fetchPages := flags.Bool("fetch", false, "replace seed snapshots with a public HTTPS L1 fetch")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 || *seedsPath == "" || *outputDir == "" {
		return errUsage
	}

	seeds, err := rerankeval.LoadHarvestSeeds(*seedsPath)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var fetcher rerankeval.PageFetcher
	if *fetchPages {
		fetcher = newPublicPageFetcher()
	}
	casesOutput, docsOutput, err := rerankeval.EmitHarvest(ctx, seeds, fetcher)
	if err != nil {
		return err
	}
	if err := requireAbsentDir(*outputDir); err != nil {
		return err
	}
	if err := os.Mkdir(*outputDir, 0o700); err != nil {
		return err
	}
	if err := installNewOutput(ctx, filepath.Join(*outputDir, casesOutputName), casesOutput); err != nil {
		return err
	}
	return installNewOutput(ctx, filepath.Join(*outputDir, docsOutputName), docsOutput)
}

func requireAbsentDir(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return &os.PathError{Op: "create", Path: path, Err: os.ErrExist}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func installNewOutput(ctx context.Context, outputPath string, contents []byte) error {
	if _, err := os.Lstat(outputPath); err == nil {
		return &os.PathError{Op: "create", Path: outputPath, Err: os.ErrExist}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	output, err := os.CreateTemp(filepath.Dir(outputPath), commandTemporaryPattern)
	if err != nil {
		return err
	}
	temporaryPath := output.Name()
	defer func() {
		_ = output.Close()
		_ = os.Remove(temporaryPath)
	}()

	buffered := bufio.NewWriterSize(output, 64<<10)
	if _, err := buffered.Write(contents); err != nil {
		return err
	}
	if err := buffered.Flush(); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if chmodErr := output.Chmod(0o600); chmodErr != nil {
		return chmodErr
	}
	if err := output.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Link(temporaryPath, outputPath)
}

const (
	fetchUserAgent      = "purify-rerankharvest/1.0 (+licensed corpus builder)"
	maximumFetchBytes   = 4 << 20
	maximumSnippetRunes = 240
)

func newPublicPageFetcher() rerankeval.PageFetcher {
	client := &http.Client{Timeout: 30 * time.Second}
	pipeline := cleaner.NewCleaner()
	return func(ctx context.Context, rawURL string) (rerankeval.PageSnapshot, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return rerankeval.PageSnapshot{}, err
		}
		request.Header.Set("User-Agent", fetchUserAgent)
		response, err := client.Do(request)
		if err != nil {
			return rerankeval.PageSnapshot{}, err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return rerankeval.PageSnapshot{}, fmt.Errorf("http status %d", response.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, maximumFetchBytes+1))
		if err != nil {
			return rerankeval.PageSnapshot{}, err
		}
		if len(body) > maximumFetchBytes {
			return rerankeval.PageSnapshot{}, errUsage
		}
		cleaned, err := pipeline.Clean(string(body), rawURL, "text", "auto")
		if err != nil || cleaned == nil {
			return rerankeval.PageSnapshot{}, errUsage
		}
		title := strings.TrimSpace(cleaned.Metadata.Title)
		snippet := firstSnippet(cleaned.Content)
		if title == "" || snippet == "" {
			return rerankeval.PageSnapshot{}, errUsage
		}
		return rerankeval.PageSnapshot{Title: title, Snippet: snippet}, nil
	}
}

func firstSnippet(content string) string {
	trimmed := strings.Join(strings.Fields(content), " ")
	if trimmed == "" {
		return ""
	}
	runes := []rune(trimmed)
	if len(runes) > maximumSnippetRunes {
		runes = runes[:maximumSnippetRunes]
		for len(runes) > 0 && unicode.IsSpace(runes[len(runes)-1]) {
			runes = runes[:len(runes)-1]
		}
	}
	snippet := string(runes)
	if !utf8.ValidString(snippet) {
		return ""
	}
	return snippet
}
