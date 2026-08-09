package search

import (
	"errors"
	"strings"
	"unicode"
)

// Freshness is a provider-neutral recency filter. Provider adapters own the
// final mapping to their private query syntax.
type Freshness uint8

const (
	FreshnessAny Freshness = iota
	FreshnessDay
	FreshnessWeek
	FreshnessMonth
	FreshnessYear
)

// ErrInvalidFreshness is intentionally stable and never echoes untrusted input.
var ErrInvalidFreshness = errors.New("search: freshness must be day, week, month, or year")

// ParseFreshness accepts only provider-neutral public words and compact
// duration aliases while returning a neutral enum. Provider wire codes are
// deliberately confined to their adapters.
func ParseFreshness(raw string) (Freshness, error) {
	for _, character := range raw {
		if unicode.IsControl(character) {
			return FreshnessAny, ErrInvalidFreshness
		}
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return FreshnessAny, nil
	case "day", "1d":
		return FreshnessDay, nil
	case "week", "7d":
		return FreshnessWeek, nil
	case "month":
		return FreshnessMonth, nil
	case "year":
		return FreshnessYear, nil
	default:
		return FreshnessAny, ErrInvalidFreshness
	}
}
