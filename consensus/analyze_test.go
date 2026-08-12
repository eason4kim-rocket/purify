package consensus

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestAnalyzeIndependenceMatchesMergeComponentPlan(t *testing.T) {
	inputs := []SourceResult{
		testSource("https://one.alpha.com/a", `{"claim":"yes"}`, 0x01),
		testSource("https://two.alpha.com/b", `{"claim":"yes"}`, 0xff00),
		testSource("https://bravo.net/c", `{"claim":"yes"}`, 0x07),
		testSource("https://charlie.org/d", `{"claim":"yes"}`, 0x3f),
	}
	merged, err := Merge(inputs)
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	analysis, err := AnalyzeIndependence(context.Background(), inputs)
	if err != nil {
		t.Fatalf("AnalyzeIndependence() error = %v", err)
	}
	if analysis.EffectiveSources != merged.Fields["claim"].Agreement.IndependentRoots {
		t.Fatalf("effective = %d, merge independent_roots = %d", analysis.EffectiveSources, merged.Fields["claim"].Agreement.IndependentRoots)
	}
	if len(analysis.Members) != 4 {
		t.Fatalf("members = %d", len(analysis.Members))
	}
	prepared := prepareSourcesForTest(t, inputs)
	plan := buildIndependencePlan(prepared)
	for index, member := range analysis.Members {
		if member.URL != prepared[index].url {
			t.Fatalf("member %d URL = %q", index, member.URL)
		}
		if member.Representative != prepared[plan.components[index]].url {
			t.Fatalf("member %d representative = %q", index, member.Representative)
		}
		if parent := plan.parent[index]; parent >= 0 {
			if member.ParentURL != prepared[parent].url || member.ParentReason != plan.parentReason[index] {
				t.Fatalf("member %d parent = %#v", index, member)
			}
		} else if member.ParentURL != "" || member.ParentReason != "" {
			t.Fatalf("root member %d has parent %#v", index, member)
		}
	}
	reversed, err := AnalyzeIndependence(context.Background(), []SourceResult{inputs[3], inputs[1], inputs[2], inputs[0]})
	if err != nil {
		t.Fatalf("reversed AnalyzeIndependence() error = %v", err)
	}
	if !reflect.DeepEqual(analysis, reversed) {
		t.Fatalf("analysis depends on input order")
	}
}

func TestAnalyzeIndependenceHonorsCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := AnalyzeIndependence(ctx, []SourceResult{testSource("https://alpha.com/a", `{"claim":true}`, 1)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("AnalyzeIndependence() error = %v, want context.Canceled", err)
	}
	if _, err := AnalyzeIndependence(nil, []SourceResult{testSource("https://alpha.com/a", `{"claim":true}`, 1)}); err == nil {
		t.Fatal("AnalyzeIndependence accepted a nil context")
	}

	deadline, cancelDeadline := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelDeadline()
	if _, err := AnalyzeIndependence(deadline, []SourceResult{testSource("https://alpha.com/a", `{"claim":true}`, 1)}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline error = %v", err)
	}
}

func TestAnalyzeIndependenceDoesNotChangeMergeBytes(t *testing.T) {
	inputs := []SourceResult{
		testSource("https://alpha.com/report", `{"price":1}`, 0),
		testSource("https://bravo.net/report", `{"price":1}`, 0),
	}
	before, err := Merge(inputs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AnalyzeIndependence(context.Background(), inputs); err != nil {
		t.Fatal(err)
	}
	after, err := Merge(inputs)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("AnalyzeIndependence changed Merge output")
	}
}
