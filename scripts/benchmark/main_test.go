package main

import (
	"testing"
	"time"
)

func TestPercentileDurationNearestRank(t *testing.T) {
	durations := []time.Duration{
		1 * time.Millisecond,
		2 * time.Millisecond,
		3 * time.Millisecond,
		4 * time.Millisecond,
		5 * time.Millisecond,
	}
	for _, test := range []struct {
		name     string
		quantile float64
		want     time.Duration
	}{
		{name: "minimum", quantile: 0, want: time.Millisecond},
		{name: "median", quantile: 0.5, want: 3 * time.Millisecond},
		{name: "p95", quantile: 0.95, want: 5 * time.Millisecond},
		{name: "maximum", quantile: 1, want: 5 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := percentileDuration(durations, test.quantile); got != test.want {
				t.Fatalf("percentileDuration() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestPercentileDurationEmpty(t *testing.T) {
	if got := percentileDuration(nil, 0.95); got != 0 {
		t.Fatalf("percentileDuration(nil) = %v, want 0", got)
	}
}
