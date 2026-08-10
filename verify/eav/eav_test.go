package eav

import (
	"strings"
	"testing"
)

// The verdict and tier literals become wire values in extract and answer
// responses; changing one is a public-contract break, not a rename.
func TestContractLiterals(t *testing.T) {
	verdicts := map[Verdict]string{
		VerdictMatch:     "entity_match",
		VerdictMismatch:  "entity_mismatch",
		VerdictUncertain: "entity_uncertain",
	}
	for verdict, want := range verdicts {
		if string(verdict) != want {
			t.Fatalf("verdict literal %q, want %q", verdict, want)
		}
	}
	tiers := map[MatchTier]string{
		TierExact:   "exact",
		TierAlias:   "alias",
		TierSuffix:  "suffix",
		TierFloor:   "floor",
		TierReferee: "referee",
		TierNone:    "",
	}
	for tier, want := range tiers {
		if string(tier) != want {
			t.Fatalf("tier literal %q, want %q", tier, want)
		}
	}
	kinds := []Kind{
		KindOrganization, KindPerson, KindProduct, KindPlace,
		KindEvent, KindWork, KindSubstance, KindOther,
	}
	seen := make(map[Kind]struct{}, len(kinds))
	for _, kind := range kinds {
		if kind == "" {
			t.Fatal("kind literals must not be empty")
		}
		if _, duplicate := seen[kind]; duplicate {
			t.Fatalf("duplicate kind literal %q", kind)
		}
		seen[kind] = struct{}{}
	}
}

func TestBoundsAndErrors(t *testing.T) {
	bounds := map[string]int{
		"MaxSubjectBytes":    MaxSubjectBytes,
		"MaxHintBytes":       MaxHintBytes,
		"MaxDocumentBytes":   MaxDocumentBytes,
		"MaxHeadWindowBytes": MaxHeadWindowBytes,
		"MaxCandidates":      MaxCandidates,
		"MaxAliases":         MaxAliases,
		"MaxSecondary":       MaxSecondary,
		"MaxEntityBytes":     MaxEntityBytes,
		"MaxQuoteBytes":      MaxQuoteBytes,
	}
	for name, value := range bounds {
		if value <= 0 {
			t.Fatalf("%s must be positive, got %d", name, value)
		}
	}
	if MaxHeadWindowBytes > MaxDocumentBytes {
		t.Fatal("the extraction head window cannot exceed the document budget")
	}
	if MaxEntityBytes > MaxSubjectBytes {
		t.Fatal("an entity surface form cannot outgrow the subject budget")
	}
	for _, err := range []error{ErrInvalidInput, ErrNotConfigured} {
		if !strings.HasPrefix(err.Error(), "eav: ") {
			t.Fatalf("error %q must carry the package prefix", err)
		}
	}
}
