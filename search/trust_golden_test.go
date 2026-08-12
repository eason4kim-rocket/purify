package search

import (
	"math"
	"sort"
	"testing"

	"github.com/use-agent/purify/consensus"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/verify/eav"
)

type trustGoldenCase struct {
	id       string
	language string
	bucket   string
	axis     string
	pages    []trustGoldenPage
}

type trustGoldenPage struct {
	url      string
	score    float64
	rank     int
	grade    int
	verdict  eav.Verdict
	group    []string
	analyzed bool
}

func TestIndependentTrustGoldenGates(t *testing.T) {
	cases := independentTrustGoldenCases()
	if err := validateTrustGoldenCoverage(cases); err != nil {
		t.Fatal(err)
	}
	var deltaUnique, deltaMirror, recallBase, recallTrust, deltaNDCG float64
	for _, fixture := range cases {
		relevance := relevanceOrder(fixture.pages)
		fused := fuseOracleCase(t, fixture)
		if fixture.bucket == "neutral" {
			if !sameURLRankOrder(relevance, fused) {
				t.Fatalf("%s neutral order drifted", fixture.id)
			}
		}
		if isSignalBucket(fixture.bucket) {
			assertOraclePartition(t, fixture, fused)
		}
		deltaUnique += uniqueOracleComponents(fixture, fused, 5) - uniqueOracleComponents(fixture, relevance, 5)
		deltaMirror += mirrorOracleShare(fixture, fused, 5) - mirrorOracleShare(fixture, relevance, 5)
		recallBase += relevantComponentRecall(fixture, relevance, 5)
		recallTrust += relevantComponentRecall(fixture, fused, 5)
		deltaNDCG += ndcgAt5(fixture, fused) - ndcgAt5(fixture, relevance)
		if relevantNonMismatchComponents(fixture) >= 5 && uniqueOracleComponents(fixture, fused, 5) != 5 {
			t.Fatalf("%s unique@5 = %v, want 5", fixture.id, uniqueOracleComponents(fixture, fused, 5))
		}
	}
	n := float64(len(cases))
	if deltaUnique/n < 0.25 || deltaMirror/n > -0.05 || recallTrust < recallBase-1e-9 || deltaNDCG/n < -0.02 {
		t.Fatalf("aggregates unique=%v mirror=%v recall=%v->%v ndcg=%v",
			deltaUnique/n, deltaMirror/n, recallBase/n, recallTrust/n, deltaNDCG/n)
	}
}

func independentTrustGoldenCases() []trustGoldenCase {
	shared := []string{"https://a.example/1", "https://b.example/2", "https://c.example/3", "https://d.example/4", "https://e.example/5", "https://f.example/6", "https://g.example/7", "https://h.example/8"}
	independent := []string{"https://one.example/1", "https://two.example/2", "https://three.example/3", "https://four.example/4", "https://five.example/5", "https://six.example/6", "https://seven.example/7", "https://eight.example/8"}
	return []trustGoldenCase{
		goldenCase("en-neutral", "en", "neutral", "N_eff_only", independentPages(independent, eav.VerdictUncertain)),
		goldenCase("zh-neutral", "zh", "neutral", "N_eff_only", independentPages(prefixURLs(independent, "zh"), eav.VerdictUncertain)),
		goldenCase("en-mirror", "en", "mirror_1_7", "N_eff_only", mirroredPages(shared, eav.VerdictUncertain)),
		goldenCase("zh-mirror", "zh", "all_mirror", "N_eff_only", mirroredPages(prefixURLs(shared, "zh"), eav.VerdictUncertain)),
		goldenCase("en-indep", "en", "independent_8", "N_eff_only", independentPages(independent, eav.VerdictMatch)),
		goldenCase("en-same-root", "en", "same_root", "N_eff_only", []trustGoldenPage{
			page("https://one.alpha.com/a", 0.9, 1, 3, eav.VerdictUncertain, []string{"https://one.alpha.com/a", "https://two.alpha.com/b"}),
			page("https://two.alpha.com/b", 0.2, 2, 1, eav.VerdictUncertain, []string{"https://one.alpha.com/a", "https://two.alpha.com/b"}),
			page("https://bravo.net/c", 0.8, 3, 2, eav.VerdictUncertain, []string{"https://bravo.net/c"}),
		}),
		goldenCase("en-near", "en", "near_duplicate", "N_eff_only", []trustGoldenPage{
			page("https://alpha.net/a", 0.7, 1, 3, eav.VerdictUncertain, []string{"https://alpha.net/a", "https://bravo.org/b"}),
			page("https://bravo.org/b", 0.4, 2, 2, eav.VerdictUncertain, []string{"https://alpha.net/a", "https://bravo.org/b"}),
			page("https://charlie.com/c", 0.6, 3, 2, eav.VerdictUncertain, []string{"https://charlie.com/c"}),
		}),
		goldenCase("en-lineage", "en", "quote_lineage", "N_eff_only", []trustGoldenPage{
			page("https://source.net/a", 0.85, 1, 3, eav.VerdictUncertain, []string{"https://source.net/a", "https://copy.org/b"}),
			page("https://copy.org/b", 0.3, 2, 1, eav.VerdictUncertain, []string{"https://source.net/a", "https://copy.org/b"}),
			page("https://other.com/c", 0.7, 3, 2, eav.VerdictUncertain, []string{"https://other.com/c"}),
		}),
		goldenCase("en-mixed", "en", "mixed", "combined", append(
			highScoreMirrors(shared[:7], 0.99),
			lowScoreIndependents(independent[:5], 0.40)...,
		)),
		goldenCase("en-mismatch", "en", "mismatch", "EAV_only", []trustGoldenPage{
			page("https://right.example/a", 0.4, 2, 3, eav.VerdictMatch, []string{"https://right.example/a"}),
			page("https://wrong.example/b", 0.9, 1, 0, eav.VerdictMismatch, []string{"https://wrong.example/b"}),
			page("https://also.example/c", 0.5, 3, 2, eav.VerdictMatch, []string{"https://also.example/c"}),
		}),
		goldenCase("zh-uncertain", "zh", "uncertain", "EAV_only", []trustGoldenPage{
			page("https://zh-a.example/a", 0.8, 1, 3, eav.VerdictUncertain, []string{"https://zh-a.example/a"}),
			page("https://zh-b.example/b", 0.7, 2, 2, eav.VerdictMatch, []string{"https://zh-b.example/b"}),
			page("https://zh-c.example/c", 0.6, 3, 2, eav.VerdictUncertain, []string{"https://zh-c.example/c"}),
		}),
		goldenCase("en-combined", "en", "combined", "combined", []trustGoldenPage{
			page("https://one.alpha.com/x", 0.95, 1, 3, eav.VerdictMatch, []string{"https://one.alpha.com/x", "https://two.alpha.com/y"}),
			page("https://two.alpha.com/y", 0.2, 4, 1, eav.VerdictMatch, []string{"https://one.alpha.com/x", "https://two.alpha.com/y"}),
			page("https://wrong.example/z", 0.9, 2, 0, eav.VerdictMismatch, []string{"https://wrong.example/z"}),
			page("https://solo.example/w", 0.7, 3, 2, eav.VerdictUncertain, []string{"https://solo.example/w"}),
		}),
	}
}

func goldenCase(id, language, bucket, axis string, pages []trustGoldenPage) trustGoldenCase {
	return trustGoldenCase{id: id, language: language, bucket: bucket, axis: axis, pages: pages}
}

func page(url string, score float64, rank, grade int, verdict eav.Verdict, group []string) trustGoldenPage {
	return trustGoldenPage{url: url, score: score, rank: rank, grade: grade, verdict: verdict, group: append([]string(nil), group...), analyzed: true}
}

func independentPages(urls []string, verdict eav.Verdict) []trustGoldenPage {
	pages := make([]trustGoldenPage, len(urls))
	for index, url := range urls {
		pages[index] = page(url, 1-float64(index)*0.05, index+1, 3, verdict, []string{url})
	}
	return pages
}

func highScoreMirrors(urls []string, start float64) []trustGoldenPage {
	pages := mirroredPages(urls, eav.VerdictMatch)
	for index := range pages {
		pages[index].score = start - float64(index)*0.001
		pages[index].rank = index + 1
	}
	return pages
}

func lowScoreIndependents(urls []string, start float64) []trustGoldenPage {
	pages := independentPages(urls, eav.VerdictMatch)
	for index := range pages {
		pages[index].score = start - float64(index)*0.001
		pages[index].rank = 20 + index
	}
	return pages
}

func mirroredPages(urls []string, verdict eav.Verdict) []trustGoldenPage {
	pages := make([]trustGoldenPage, len(urls))
	for index, url := range urls {
		pages[index] = page(url, 1-float64(index)*0.05, index+1, 3, verdict, append([]string(nil), urls...))
	}
	return pages
}

func prefixURLs(urls []string, prefix string) []string {
	out := make([]string, len(urls))
	for index, url := range urls {
		out[index] = "https://" + prefix + "." + url[len("https://"):]
	}
	return out
}

func fuseOracleCase(t *testing.T, fixture trustGoldenCase) []trustPage {
	t.Helper()
	pages := make([]trustPage, len(fixture.pages))
	members := make([]consensus.IndependenceMember, 0, len(fixture.pages))
	for index, item := range fixture.pages {
		pages[index] = trustPage{
			canonicalURL: item.url, providerRank: item.rank, relevanceScore: item.score,
			mismatch: item.verdict == eav.VerdictMismatch, verdict: item.verdict,
			result: models.SearchResult{URL: item.url, Rank: item.rank},
		}
		if !item.analyzed {
			continue
		}
		rep := oracleRepresentative(item.group)
		members = append(members, consensus.IndependenceMember{
			URL: item.url, Representative: rep, ComponentSize: len(item.group),
		})
	}
	analysis := consensus.IndependenceAnalysis{Members: members, EffectiveSources: countOracleComponents(fixture.pages)}
	return fuseTrustPages(pages, analysis).pages
}

func relevanceOrder(pages []trustGoldenPage) []trustPage {
	cloned := make([]trustPage, len(pages))
	for index, item := range pages {
		cloned[index] = trustPage{canonicalURL: item.url, providerRank: item.rank, relevanceScore: item.score, verdict: item.verdict, result: models.SearchResult{URL: item.url}}
	}
	sortTrustPages(cloned, func(trustPage) int { return 0 })
	return cloned
}

func assertOraclePartition(t *testing.T, fixture trustGoldenCase, fused []trustPage) {
	t.Helper()
	byURL := map[string]trustGoldenPage{}
	for _, item := range fixture.pages {
		byURL[item.url] = item
	}
	for _, page := range fused {
		item := byURL[page.canonicalURL]
		if !item.analyzed || !page.analyzed {
			continue
		}
		wantID := searchComponentID(item.group)
		if page.componentID != wantID {
			t.Fatalf("%s %s component %q != oracle %q", fixture.id, page.canonicalURL, page.componentID, wantID)
		}
	}
}

func uniqueOracleComponents(fixture trustGoldenCase, pages []trustPage, k int) float64 {
	if k > len(pages) {
		k = len(pages)
	}
	seen := map[string]struct{}{}
	for _, page := range pages[:k] {
		seen[searchComponentID(lookupGoldenPage(fixture, page.canonicalURL).group)] = struct{}{}
	}
	return float64(len(seen))
}

func mirrorOracleShare(fixture trustGoldenCase, pages []trustPage, k int) float64 {
	if k > len(pages) {
		k = len(pages)
	}
	if k == 0 {
		return 0
	}
	return (float64(k) - uniqueOracleComponents(fixture, pages, k)) / float64(k)
}

func relevantComponentRecall(fixture trustGoldenCase, pages []trustPage, k int) float64 {
	want := map[string]struct{}{}
	for _, item := range fixture.pages {
		if item.grade > 0 && item.verdict != eav.VerdictMismatch {
			want[searchComponentID(item.group)] = struct{}{}
		}
	}
	if len(want) == 0 {
		return 0
	}
	if k > len(pages) {
		k = len(pages)
	}
	got := 0
	seen := map[string]struct{}{}
	for _, page := range pages[:k] {
		item := lookupGoldenPage(fixture, page.canonicalURL)
		if item.grade > 0 && item.verdict != eav.VerdictMismatch {
			id := searchComponentID(item.group)
			if _, exists := seen[id]; !exists {
				if _, relevant := want[id]; relevant {
					got++
					seen[id] = struct{}{}
				}
			}
		}
	}
	return float64(got) / float64(len(want))
}

func ndcgAt5(fixture trustGoldenCase, pages []trustPage) float64 {
	grades := make([]int, 0, 5)
	for index, page := range pages {
		if index == 5 {
			break
		}
		grades = append(grades, lookupGoldenPage(fixture, page.canonicalURL).grade)
	}
	dcg := 0.0
	for index, grade := range grades {
		dcg += (math.Pow(2, float64(grade)) - 1) / math.Log2(float64(index+2))
	}
	ideal := append([]int(nil), grades...)
	sort.Slice(ideal, func(i, j int) bool { return ideal[i] > ideal[j] })
	idcg := 0.0
	for index, grade := range ideal {
		idcg += (math.Pow(2, float64(grade)) - 1) / math.Log2(float64(index+2))
	}
	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}

func relevantNonMismatchComponents(fixture trustGoldenCase) int {
	seen := map[string]struct{}{}
	for _, item := range fixture.pages {
		if item.grade > 0 && item.verdict != eav.VerdictMismatch {
			seen[searchComponentID(item.group)] = struct{}{}
		}
	}
	return len(seen)
}

func lookupGoldenPage(fixture trustGoldenCase, url string) trustGoldenPage {
	for _, item := range fixture.pages {
		if item.url == url {
			return item
		}
	}
	return trustGoldenPage{}
}

func sameURLRankOrder(left, right []trustPage) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].canonicalURL != right[index].canonicalURL {
			return false
		}
	}
	return true
}

func isSignalBucket(bucket string) bool {
	switch bucket {
	case "same_root", "near_duplicate", "quote_lineage", "mirror_1_7", "all_mirror", "combined":
		return true
	default:
		return false
	}
}

func oracleRepresentative(group []string) string {
	cloned := append([]string(nil), group...)
	sort.Strings(cloned)
	if len(cloned) == 0 {
		return ""
	}
	return cloned[0]
}

func countOracleComponents(pages []trustGoldenPage) int {
	seen := map[string]struct{}{}
	for _, page := range pages {
		seen[searchComponentID(page.group)] = struct{}{}
	}
	return len(seen)
}

func validateTrustGoldenCoverage(cases []trustGoldenCase) error {
	buckets := map[string]bool{"neutral": false, "mirror_1_7": false, "independent_8": false, "mixed": false, "same_root": false, "near_duplicate": false, "quote_lineage": false, "mismatch": false, "uncertain": false, "combined": false, "all_mirror": false}
	langs := map[string]bool{"en": false, "zh": false}
	axes := map[string]bool{"N_eff_only": false, "EAV_only": false, "combined": false}
	for _, fixture := range cases {
		buckets[fixture.bucket] = true
		langs[fixture.language] = true
		axes[fixture.axis] = true
		if len(fixture.pages) == 0 {
			return errTrustAnalysisInput
		}
	}
	for name, present := range buckets {
		if !present {
			return errCoverage("bucket " + name)
		}
	}
	for name, present := range langs {
		if !present {
			return errCoverage("language " + name)
		}
	}
	for name, present := range axes {
		if !present {
			return errCoverage("axis " + name)
		}
	}
	return nil
}

func errCoverage(name string) error {
	return errTrustAnalysisInput
}

func TestSearchComponentIDIsLengthPrefixed(t *testing.T) {
	left := searchComponentID([]string{"https://a.example/x", "https://b.example/y"})
	right := searchComponentID([]string{"https://b.example/y", "https://a.example/x"})
	if left != right || len(left) != 64 {
		t.Fatalf("component id = %s / %s", left, right)
	}
}
