package main

import (
	"bytes"
	"os"
	"testing"
)

func TestRunRejectsUnknownModeAndMissingFlags(t *testing.T) {
	if err := run(nil, os.Stdout, os.Geteuid()); err == nil {
		t.Fatal("run accepted empty arguments")
	}
	if err := run([]string{"delete"}, os.Stdout, os.Geteuid()); err == nil {
		t.Fatal("run accepted an unknown mode")
	}
	if err := run([]string{"list"}, os.Stdout, os.Geteuid()); err == nil {
		t.Fatal("run accepted list without root")
	}
	if err := run([]string{"adopt", "-root", "/tmp/x", "-run-id", "aa", "-container-id", "bb"}, os.Stdout, os.Geteuid()); err == nil {
		t.Fatal("run accepted adopt without confirm")
	}
	var stdout bytes.Buffer
	if err := run([]string{"list", "-root", t.TempDir()}, &stdout, os.Geteuid()); err == nil {
		t.Fatal("run listed a directory that is not a recovery journal")
	}
}
