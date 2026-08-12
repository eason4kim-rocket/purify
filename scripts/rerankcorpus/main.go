// Command rerankcorpus records reference relevance observations into a new
// recordings.jsonl file. Output remains provisional until R-6a authenticates
// the launched runtime. The command never reads labels, retries a case,
// accepts a model/profile/manifest override, or enables production.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/use-agent/purify/internal/rerankeval"
	"github.com/use-agent/purify/search/rerank"
)

const (
	recorderAPIKeyEnvironment = "PURIFY_RERANK_API_KEY"
	recorderTemporaryPattern  = ".rerankcorpus-partial-*.jsonl"
)

var errUsage = errors.New("rerankcorpus: invalid invocation")

type referenceRecorder interface {
	Record(context.Context, rerank.ScoreRequest) (rerank.ReferenceRecording, error)
	Close()
}

type recorderFactory func(rerank.ReferenceRecorderConfig) (referenceRecorder, error)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv, func(config rerank.ReferenceRecorderConfig) (referenceRecorder, error) {
		return rerank.NewReferenceRecorder(config)
	}); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "rerankcorpus: recording failed")
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string, getenv func(string) string, factory recorderFactory) error {
	if ctx == nil || getenv == nil || factory == nil {
		return errUsage
	}
	flags := flag.NewFlagSet("rerankcorpus", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	docsPath := flags.String("docs", "", "strict docs.jsonl input")
	outputPath := flags.String("out", "", "new recordings.jsonl output")
	endpoint := flags.String("endpoint", "", "reference sidecar /v1/rerank endpoint")
	allowPrivate := flags.Bool("allow-private", false, "allow a private/loopback sidecar endpoint")
	timeoutSeconds := flags.Int("timeout-seconds", int(rerank.DefaultScorerTimeout/time.Second), "per-case timeout in seconds")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 || *docsPath == "" || *outputPath == "" || *endpoint == "" ||
		*timeoutSeconds < 1 || *timeoutSeconds > int(rerank.MaximumScorerTimeout/time.Second) {
		return errUsage
	}
	inputs, err := rerankeval.LoadRecordingInputs(*docsPath)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if _, err := os.Lstat(*outputPath); err == nil {
		return &os.PathError{Op: "create", Path: *outputPath, Err: os.ErrExist}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	output, err := os.CreateTemp(filepath.Dir(*outputPath), recorderTemporaryPattern)
	if err != nil {
		return err
	}
	temporaryPath := output.Name()
	defer func() {
		_ = output.Close()
		_ = os.Remove(temporaryPath)
	}()
	recorder, err := factory(rerank.ReferenceRecorderConfig{
		Endpoint: *endpoint, APIKey: getenv(recorderAPIKeyEnvironment), AllowPrivate: *allowPrivate,
		Timeout: time.Duration(*timeoutSeconds) * time.Second,
	})
	if err != nil {
		return err
	}
	defer recorder.Close()

	buffered := bufio.NewWriterSize(output, 64<<10)
	for _, input := range inputs {
		if err := ctx.Err(); err != nil {
			return err
		}
		recording, recordErr := recorder.Record(ctx, input.Request)
		if recordErr != nil {
			return recordErr
		}
		encoded, encodeErr := rerankeval.EncodeRecordingLine(input, recording)
		if encodeErr != nil {
			return encodeErr
		}
		if _, err := buffered.Write(encoded); err != nil {
			return err
		}
		if err := buffered.WriteByte('\n'); err != nil {
			return err
		}
	}
	if err := buffered.Flush(); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	// Linking a same-directory temporary file atomically installs the finished
	// recording while retaining O_EXCL semantics: an existing destination is
	// never replaced. The deferred cleanup only ever names our random temporary
	// path, so a destination created while recording cannot be removed on error.
	return os.Link(temporaryPath, *outputPath)
}
