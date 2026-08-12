// Command reranklabels builds blind relevance-judgment packets, compiles their
// independent judgments, and validates the GPU-independent judged-input gates.
// It never records model output or enables the production reranker.
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

	"github.com/use-agent/purify/internal/rerankeval"
)

const commandTemporaryPattern = ".reranklabels-partial-*.jsonl"

var errUsage = errors.New("reranklabels: invalid invocation")

type commandOperations struct {
	buildPacket    func(string, string) ([]byte, error)
	compileLabels  func(string, string, string) ([]byte, error)
	validateInputs func(string, string) (rerankeval.JudgedInputsSummary, error)

	// beforeLink is nil in production. Tests use it to hold the exact
	// no-replace installation window after the temporary file is durable.
	beforeLink func(string) error
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, productionOperations()); err != nil {
		// Input paths, queries, candidate metadata, and validation details are
		// deliberately absent from the command's diagnostic surface.
		_, _ = fmt.Fprintln(os.Stderr, "reranklabels: command failed")
		os.Exit(1)
	}
}

func productionOperations() commandOperations {
	return commandOperations{
		buildPacket:    rerankeval.BuildBlindLabelPacket,
		compileLabels:  rerankeval.CompileBlindLabels,
		validateInputs: rerankeval.ValidateJudgedInputs,
	}
}

func run(ctx context.Context, arguments []string, stdout io.Writer, operations commandOperations) error {
	if ctx == nil || stdout == nil || operations.buildPacket == nil || operations.compileLabels == nil || operations.validateInputs == nil {
		return errUsage
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(arguments) == 0 {
		return errUsage
	}

	switch arguments[0] {
	case "packet":
		casesPath, docsPath, outputPath, err := parsePacketFlags(arguments[1:])
		if err != nil {
			return err
		}
		return generateNewOutput(ctx, outputPath, operations, func() ([]byte, error) {
			return operations.buildPacket(casesPath, docsPath)
		})
	case "compile":
		casesPath, docsPath, judgmentsPath, outputPath, err := parseCompileFlags(arguments[1:])
		if err != nil {
			return err
		}
		return generateNewOutput(ctx, outputPath, operations, func() ([]byte, error) {
			return operations.compileLabels(casesPath, docsPath, judgmentsPath)
		})
	case "validate":
		docsPath, labelsPath, err := parseValidateFlags(arguments[1:])
		if err != nil {
			return err
		}
		summary, err := operations.validateInputs(docsPath, labelsPath)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "valid cases=%d candidates=%d\n", summary.Cases, summary.Candidates)
		return err
	default:
		return errUsage
	}
}

func parsePacketFlags(arguments []string) (string, string, string, error) {
	flags := newCommandFlagSet("reranklabels packet")
	casesPath := flags.String("cases", "", "strict cases.jsonl input")
	docsPath := flags.String("docs", "", "strict docs.jsonl input")
	outputPath := flags.String("out", "", "new blind packet JSONL output")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 || *casesPath == "" || *docsPath == "" || *outputPath == "" {
		return "", "", "", errUsage
	}
	return *casesPath, *docsPath, *outputPath, nil
}

func parseCompileFlags(arguments []string) (string, string, string, string, error) {
	flags := newCommandFlagSet("reranklabels compile")
	casesPath := flags.String("cases", "", "strict cases.jsonl input")
	docsPath := flags.String("docs", "", "strict docs.jsonl input")
	judgmentsPath := flags.String("judgments", "", "strict blind judgments JSONL input")
	outputPath := flags.String("out", "", "new compiled labels.jsonl output")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 || *casesPath == "" || *docsPath == "" || *judgmentsPath == "" || *outputPath == "" {
		return "", "", "", "", errUsage
	}
	return *casesPath, *docsPath, *judgmentsPath, *outputPath, nil
}

func parseValidateFlags(arguments []string) (string, string, error) {
	flags := newCommandFlagSet("reranklabels validate")
	docsPath := flags.String("docs", "", "strict docs.jsonl input")
	labelsPath := flags.String("labels", "", "strict compiled labels.jsonl input")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 || *docsPath == "" || *labelsPath == "" {
		return "", "", errUsage
	}
	return *docsPath, *labelsPath, nil
}

func newCommandFlagSet(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags
}

func generateNewOutput(ctx context.Context, outputPath string, operations commandOperations, generate func() ([]byte, error)) error {
	if err := requireAbsent(outputPath); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	contents, err := generate()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return installNewOutput(ctx, outputPath, contents, operations.beforeLink)
}

func requireAbsent(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return &os.PathError{Op: "create", Path: path, Err: os.ErrExist}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func installNewOutput(ctx context.Context, outputPath string, contents []byte, beforeLink func(string) error) error {
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
	if err := output.Close(); err != nil {
		return err
	}
	if beforeLink != nil {
		if err := beforeLink(temporaryPath); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// The hard link atomically installs the finished same-directory temporary
	// without replacement. Deferred cleanup only ever names our random source.
	return os.Link(temporaryPath, outputPath)
}
