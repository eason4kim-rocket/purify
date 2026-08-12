package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/use-agent/purify/internal/rerankeval"
)

func TestRunEmitsPrivateSeedOrderCorpus(t *testing.T) {
	directory := t.TempDir()
	seedsPath := filepath.Join(directory, "seeds.json")
	if err := os.WriteFile(seedsPath, validCLISeeds(t), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	outputDir := filepath.Join(directory, "corpus")
	if err := run(context.Background(), []string{"-seeds", seedsPath, "-out", outputDir}); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	assertPrivateFile(t, filepath.Join(outputDir, casesOutputName))
	assertPrivateFile(t, filepath.Join(outputDir, docsOutputName))
	casesBody, err := os.ReadFile(filepath.Join(outputDir, casesOutputName))
	if err != nil || !bytes.Contains(casesBody, []byte(`"case_id":"en-lexical-cli"`)) {
		t.Fatalf("cases.jsonl = %q err=%v", casesBody, err)
	}

	if err := run(context.Background(), []string{"-seeds", seedsPath, "-out", outputDir}); err == nil {
		t.Fatal("run replaced an existing corpus directory")
	}
}

func TestRunRejectsSearchProviderSeedsAndMissingFlags(t *testing.T) {
	if err := run(context.Background(), nil); err == nil {
		t.Fatal("run accepted missing flags")
	}
	directory := t.TempDir()
	seedsPath := filepath.Join(directory, "seeds.json")
	if err := os.WriteFile(seedsPath, []byte(`{"schema_version":"rerank-harvest-seeds-v1","baseline":"seed_order","license":"x","cases":[]}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := run(context.Background(), []string{"-seeds", seedsPath, "-out", filepath.Join(directory, "out")}); err == nil {
		t.Fatal("run accepted empty cases")
	}
}

func validCLISeeds(t *testing.T) []byte {
	t.Helper()
	pages := make([]map[string]string, 10)
	for index := range pages {
		pages[index] = map[string]string{
			"url":     "https://en.wikipedia.org/wiki/CLIHarvest" + twoDigits(index),
			"title":   "title " + twoDigits(index),
			"snippet": "public page snippet " + twoDigits(index),
		}
	}
	body, err := json.Marshal(map[string]any{
		"schema_version": "rerank-harvest-seeds-v1",
		"baseline":       rerankeval.HarvestBaselineSeedOrder,
		"license":        "operator-curated public pages; not search-provider results",
		"cases": []map[string]any{{
			"case_id":  "en-lexical-cli",
			"query":    "What is harvest",
			"language": "en",
			"bucket":   "lexical",
			"pages":    pages,
		}},
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return body
}

func assertPrivateFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat(%s) error = %v", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("%s mode = %v", path, info.Mode())
	}
}

func twoDigits(value int) string {
	return string(rune('0'+value/10)) + string(rune('0'+value%10))
}
