// Command rerankrecover is the explicit human recovery surface for quarantined
// rerank creates. It never talks to Docker.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/use-agent/purify/search/rerank/deploy/dockerengine"
)

var errUsage = errors.New("rerankrecover: invalid invocation")

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Geteuid()); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "rerankrecover: command failed")
		os.Exit(1)
	}
}

func run(arguments []string, stdout io.Writer, uid int) error {
	if stdout == nil || uid < 0 || len(arguments) == 0 {
		return errUsage
	}
	switch arguments[0] {
	case "list":
		root, err := parseRootFlags("rerankrecover list", arguments[1:])
		if err != nil {
			return err
		}
		return withJournal(root, uid, func(journal *dockerengine.OperatorJournal) error {
			inventory, listErr := journal.List()
			if listErr != nil {
				return listErr
			}
			encoded, marshalErr := json.Marshal(inventory)
			if marshalErr != nil {
				return marshalErr
			}
			_, err := fmt.Fprintln(stdout, string(encoded))
			return err
		})
	case "adopt":
		root, runID, containerID, confirm, err := parseAdoptFlags(arguments[1:])
		if err != nil {
			return err
		}
		return withJournal(root, uid, func(journal *dockerengine.OperatorJournal) error {
			return journal.Adopt(runID, containerID, confirm)
		})
	case "abandon":
		root, runID, confirm, err := parseAbandonFlags(arguments[1:])
		if err != nil {
			return err
		}
		return withJournal(root, uid, func(journal *dockerengine.OperatorJournal) error {
			return journal.Abandon(runID, confirm)
		})
	default:
		return errUsage
	}
}

func withJournal(root string, uid int, use func(*dockerengine.OperatorJournal) error) error {
	journal, err := dockerengine.OpenOperatorJournal(root, uid)
	if err != nil {
		return err
	}
	defer func() { _ = journal.Close() }()
	return use(journal)
}

func parseRootFlags(name string, arguments []string) (string, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	root := flags.String("root", "", "recovery journal root")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 || *root == "" {
		return "", errUsage
	}
	return *root, nil
}

func parseAdoptFlags(arguments []string) (string, string, string, string, error) {
	flags := flag.NewFlagSet("rerankrecover adopt", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	root := flags.String("root", "", "recovery journal root")
	runID := flags.String("run-id", "", "quarantined run id")
	containerID := flags.String("container-id", "", "operator-inspected container id")
	confirm := flags.String("confirm", "", "typed confirmation")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 ||
		*root == "" || *runID == "" || *containerID == "" || *confirm == "" {
		return "", "", "", "", errUsage
	}
	return *root, *runID, *containerID, *confirm, nil
}

func parseAbandonFlags(arguments []string) (string, string, string, error) {
	flags := flag.NewFlagSet("rerankrecover abandon", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	root := flags.String("root", "", "recovery journal root")
	runID := flags.String("run-id", "", "quarantined run id")
	confirm := flags.String("confirm", "", "typed confirmation")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 ||
		*root == "" || *runID == "" || *confirm == "" {
		return "", "", "", errUsage
	}
	return *root, *runID, *confirm, nil
}
