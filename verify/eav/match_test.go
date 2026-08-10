package eav

import (
	"strings"
	"testing"
)

func TestMatchLadder(t *testing.T) {
	cases := []struct {
		name        string
		subject     Subject
		entity      Entity
		wantVerdict Verdict
		wantTier    MatchTier
	}{
		// Tier 1: equal normalized forms.
		{
			"exact with punctuation", Subject{Name: "Apple Inc."},
			Entity{Name: "Apple, Inc."}, VerdictMatch, TierExact,
		},
		{
			"exact cjk", Subject{Name: "小米科技有限公司"},
			Entity{Name: "小米科技有限公司"}, VerdictMatch, TierExact,
		},
		{
			"exact fullwidth", Subject{Name: "Ａｐｐｌｅ"},
			Entity{Name: "apple"}, VerdictMatch, TierExact,
		},
		{
			"exact person initial period", Subject{Name: "Timothy D. Cook"},
			Entity{Name: "Timothy D Cook", Kind: KindPerson}, VerdictMatch, TierExact,
		},
		{
			"exact hyphenation", Subject{Name: "Mercedes-Benz"},
			Entity{Name: "Mercedes Benz"}, VerdictMatch, TierExact,
		},
		{
			// Documented v1 limit: a homonym subject (the state and the
			// country share the surface "Georgia") matches on the surface.
			"homonym matches on surface", Subject{Name: "Georgia"},
			Entity{Name: "Georgia", Kind: KindPlace}, VerdictMatch, TierExact,
		},

		// Tier 2: equal to a same-document alias.
		{
			"alias initialism", Subject{Name: "IBM"},
			Entity{Name: "International Business Machines", Aliases: []string{"IBM", "Big Blue"}},
			VerdictMatch, TierAlias,
		},
		{
			"alias ticker", Subject{Name: "AAPL"},
			Entity{Name: "Apple Inc", Aliases: []string{"AAPL"}}, VerdictMatch, TierAlias,
		},
		{
			"alias cross language", Subject{Name: "谷歌"},
			Entity{Name: "Alphabet Inc", Aliases: []string{"Google", "谷歌"}}, VerdictMatch, TierAlias,
		},
		{
			"alias brand", Subject{Name: "红米"},
			Entity{Name: "Redmi", Aliases: []string{"红米", "Redmi Note"}}, VerdictMatch, TierAlias,
		},

		// Tier 3: equal after stripping legal wrappers.
		{
			"suffix inc", Subject{Name: "Apple"},
			Entity{Name: "Apple Inc."}, VerdictMatch, TierSuffix,
		},
		{
			"suffix article", Subject{Name: "The Home Depot"},
			Entity{Name: "Home Depot Inc"}, VerdictMatch, TierSuffix,
		},
		{
			"suffix corporation", Subject{Name: "Toyota Motor"},
			Entity{Name: "Toyota Motor Corporation"}, VerdictMatch, TierSuffix,
		},
		{
			"suffix ja prefix", Subject{Name: "任天堂"},
			Entity{Name: "株式会社任天堂"}, VerdictMatch, TierSuffix,
		},
		{
			"suffix zh joint stock", Subject{Name: "贵州茅台"},
			Entity{Name: "贵州茅台股份有限公司"}, VerdictMatch, TierSuffix,
		},
		{
			"suffix via alias base", Subject{Name: "IBM"},
			Entity{Name: "International Business Machines", Aliases: []string{"IBM Corp"}},
			VerdictMatch, TierSuffix,
		},

		// Tier 4: every similarity component below the floor -> the only
		// deterministic alarm.
		{
			"floor different companies", Subject{Name: "Apple Inc"},
			Entity{Name: "Samsung Electronics"}, VerdictMismatch, TierFloor,
		},
		{
			"floor different drugs", Subject{Name: "Metformin", Hint: "dosage"},
			Entity{Name: "Ibuprofen", Kind: KindSubstance}, VerdictMismatch, TierFloor,
		},
		{
			"floor different zh companies", Subject{Name: "宁德时代"},
			Entity{Name: "比亚迪"}, VerdictMismatch, TierFloor,
		},
		{
			"floor different pharma", Subject{Name: "Pfizer"},
			Entity{Name: "Moderna"}, VerdictMismatch, TierFloor,
		},
		{
			"floor different telecom", Subject{Name: "华为"},
			Entity{Name: "中兴通讯"}, VerdictMismatch, TierFloor,
		},

		// Gray zone: confusable pairs stay uncertain for a referee.
		{
			"gray shared token", Subject{Name: "Apple"},
			Entity{Name: "Apple Bank"}, VerdictUncertain, TierNone,
		},
		{
			"gray drug variant", Subject{Name: "Metformin"},
			Entity{Name: "Metformin HCl ER", Kind: KindSubstance}, VerdictUncertain, TierNone,
		},
		{
			"gray near typo", Subject{Name: "AMD"},
			Entity{Name: "ARM"}, VerdictUncertain, TierNone,
		},
		{
			"gray product line", Subject{Name: "iPhone 15"},
			Entity{Name: "iPhone 15 Pro", Kind: KindProduct}, VerdictUncertain, TierNone,
		},
		{
			"gray initialism without alias", Subject{Name: "IBM"},
			Entity{Name: "International Business Machines"}, VerdictUncertain, TierNone,
		},
		{
			"gray short initialism", Subject{Name: "GE"},
			Entity{Name: "General Electric"}, VerdictUncertain, TierNone,
		},
		{
			"gray confusable provinces", Subject{Name: "湖南"},
			Entity{Name: "湖北"}, VerdictUncertain, TierNone,
		},
		{
			"gray containment", Subject{Name: "Harvard"},
			Entity{Name: "Harvard John A Paulson School of Engineering"}, VerdictUncertain, TierNone,
		},
		{
			"gray cjk near typo", Subject{Name: "小米"},
			Entity{Name: "红米"}, VerdictUncertain, TierNone,
		},
		{
			"gray cjk containment", Subject{Name: "小米"},
			Entity{Name: "小米科技"}, VerdictUncertain, TierNone,
		},

		// Degenerate and bounded input is refused as uncertain.
		{
			"empty subject", Subject{}, Entity{Name: "Apple"}, VerdictUncertain, TierNone,
		},
		{
			"decoration only subject", Subject{Name: "..."},
			Entity{Name: "Apple"}, VerdictUncertain, TierNone,
		},
		{
			"empty entity", Subject{Name: "Apple"}, Entity{}, VerdictUncertain, TierNone,
		},
		{
			"oversized subject", Subject{Name: strings.Repeat("a", MaxSubjectBytes+1)},
			Entity{Name: "Apple"}, VerdictUncertain, TierNone,
		},
		{
			"oversized entity", Subject{Name: "Apple"},
			Entity{Name: strings.Repeat("a", MaxEntityBytes+1)}, VerdictUncertain, TierNone,
		},
		{
			"oversized alias", Subject{Name: "Apple"},
			Entity{Name: "Apple", Aliases: []string{strings.Repeat("a", MaxEntityBytes+1)}},
			VerdictUncertain, TierNone,
		},
		{
			"too many aliases", Subject{Name: "Apple"},
			Entity{Name: "Apple", Aliases: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}},
			VerdictUncertain, TierNone,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := Match(testCase.subject, testCase.entity)
			if got.Verdict != testCase.wantVerdict || got.Tier != testCase.wantTier {
				t.Fatalf(
					"Match(%q, %q) = (%s, %q, %.3f), want (%s, %q)",
					testCase.subject.Name, testCase.entity.Name,
					got.Verdict, got.Tier, got.Similarity,
					testCase.wantVerdict, testCase.wantTier,
				)
			}
			if got.Verdict == VerdictMatch && got.Similarity != 1 {
				t.Fatalf("match verdicts must report similarity 1, got %.3f", got.Similarity)
			}
			if got.Verdict == VerdictMismatch && got.Similarity >= similarityFloor {
				t.Fatalf(
					"mismatch verdicts must sit below the similarity floor, got %.3f",
					got.Similarity,
				)
			}
		})
	}

	// False-positive discipline: the deterministic ladder can alarm only from
	// the similarity floor. Any other tier on a mismatch is a bug.
	for _, testCase := range cases {
		got := Match(testCase.subject, testCase.entity)
		if got.Verdict == VerdictMismatch && got.Tier != TierFloor {
			t.Fatalf(
				"deterministic mismatch for %q carried tier %q, only %q may alarm",
				testCase.name, got.Tier, TierFloor,
			)
		}
	}
}

func TestMatchIgnoresHint(t *testing.T) {
	with := Match(Subject{Name: "Apple", Hint: "chief executive officer"}, Entity{Name: "Apple Bank"})
	without := Match(Subject{Name: "Apple"}, Entity{Name: "Apple Bank"})
	if with != without {
		t.Fatalf("Match must ignore Subject.Hint: %+v != %+v", with, without)
	}
}
