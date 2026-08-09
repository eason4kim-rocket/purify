package search

import (
	"errors"
	"strings"
	"testing"
)

func TestParseFreshnessAliases(t *testing.T) {
	tests := []struct {
		raw  string
		want Freshness
	}{
		{raw: "", want: FreshnessAny},
		{raw: "   ", want: FreshnessAny},
		{raw: "day", want: FreshnessDay},
		{raw: "1d", want: FreshnessDay},
		{raw: " DAY ", want: FreshnessDay},
		{raw: "week", want: FreshnessWeek},
		{raw: "7d", want: FreshnessWeek},
		{raw: "month", want: FreshnessMonth},
		{raw: "year", want: FreshnessYear},
	}
	for _, test := range tests {
		t.Run(strings.ReplaceAll(test.raw, " ", "_"), func(t *testing.T) {
			got, err := ParseFreshness(test.raw)
			if err != nil || got != test.want {
				t.Fatalf("ParseFreshness(%q) = %d, %v; want %d", test.raw, got, err, test.want)
			}
		})
	}
}

func TestParseFreshnessRejectsUndocumentedOrUnsafeValuesWithoutEcho(t *testing.T) {
	for _, raw := range []string{
		"all", "any", "30d", "365d", "hour", "day\n", "\tweek", "pd", "pw", "pm", "py", "provider-secret-value",
	} {
		t.Run(strings.ReplaceAll(raw, " ", "_"), func(t *testing.T) {
			got, err := ParseFreshness(raw)
			if got != FreshnessAny || !errors.Is(err, ErrInvalidFreshness) {
				t.Fatalf("ParseFreshness(%q) = %d, %v", raw, got, err)
			}
			if strings.Contains(err.Error(), raw) {
				t.Fatalf("error %q echoed untrusted value %q", err, raw)
			}
		})
	}
}

func TestFreshnessValuesAreStableAndDistinct(t *testing.T) {
	values := []Freshness{FreshnessAny, FreshnessDay, FreshnessWeek, FreshnessMonth, FreshnessYear}
	seen := make(map[Freshness]struct{}, len(values))
	for _, value := range values {
		if _, duplicate := seen[value]; duplicate {
			t.Fatalf("duplicate Freshness value %d", value)
		}
		seen[value] = struct{}{}
	}
}
