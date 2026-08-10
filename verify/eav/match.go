package eav

import (
	"strings"
	"unicode"
)

// Similarity constants for the deterministic ladder. The initial values are
// set by construction; the E-5 golden set recalibrates them, and any change
// must keep the golden gates green.
const (
	// similarityFloor separates a confident mismatch from the gray zone. A
	// deterministic alarm requires every similarity component to sit below
	// this floor.
	similarityFloor = 0.30
	// nearTypoMaxEdits is the absolute edit budget under which two forms
	// count as confusably close (AMD/ARM, 湖南/湖北, single-character drug
	// variants). Beyond it, edit distance stops contributing similarity, so
	// long unrelated names cannot ride a diluted ratio into the gray zone.
	nearTypoMaxEdits = 2
	// minContainmentRunes keeps the substring guard away from degenerate
	// short forms. CJK forms carry roughly one word per rune, so an
	// all-CJK shorter side needs minContainmentRunesCJK instead ("小米"
	// inside "小米科技" is a real containment relation, "ge" inside
	// "general" is noise).
	minContainmentRunes    = 3
	minContainmentRunesCJK = 2
	// minInitialismRunes keeps the initialism guard away from one-letter
	// coincidences.
	minInitialismRunes = 2
)

// Match runs the deterministic attribution ladder for one subject against
// one extracted document entity:
//
//  1. equal normalized forms                            -> match / exact
//  2. equal to any normalized alias                     -> match / alias
//  3. equal after stripping legal wrappers (name/alias) -> match / suffix
//  4. every similarity component below the floor        -> mismatch / floor
//  5. anything else                                     -> uncertain
//
// The gray zone (5) includes two protective guards that can never alarm:
// initialism pairs ("IBM" vs "International Business Machines") and
// substring containment ("Apple" vs "Applebees"), both of which need a
// referee rather than a deterministic verdict. Oversized input is refused as
// uncertain instead of being partially judged. Subject.Hint never
// participates: it is context for a referee, not for matching.
func Match(subject Subject, entity Entity) MatchResult {
	uncertain := MatchResult{Verdict: VerdictUncertain, Tier: TierNone}
	if len(subject.Name) > MaxSubjectBytes || len(entity.Name) > MaxEntityBytes ||
		len(entity.Aliases) > MaxAliases {
		return uncertain
	}
	for _, alias := range entity.Aliases {
		if len(alias) > MaxEntityBytes {
			return uncertain
		}
	}
	subjectNorm := Normalize(subject.Name)
	entityNorm := Normalize(entity.Name)
	if subjectNorm == "" || entityNorm == "" {
		return uncertain
	}
	if subjectNorm == entityNorm {
		return MatchResult{Verdict: VerdictMatch, Tier: TierExact, Similarity: 1}
	}
	aliasNorms := make([]string, 0, len(entity.Aliases))
	for _, alias := range entity.Aliases {
		aliasNorm := Normalize(alias)
		if aliasNorm == "" {
			continue
		}
		if aliasNorm == subjectNorm {
			return MatchResult{Verdict: VerdictMatch, Tier: TierAlias, Similarity: 1}
		}
		aliasNorms = append(aliasNorms, aliasNorm)
	}

	subjectBase, _ := StripLegalSuffix(subjectNorm)
	entityBase, _ := StripLegalSuffix(entityNorm)
	if subjectBase == entityBase {
		return MatchResult{Verdict: VerdictMatch, Tier: TierSuffix, Similarity: 1}
	}
	aliasBases := make([]string, 0, len(aliasNorms))
	for _, aliasNorm := range aliasNorms {
		aliasBase, _ := StripLegalSuffix(aliasNorm)
		if aliasBase == subjectBase {
			return MatchResult{Verdict: VerdictMatch, Tier: TierSuffix, Similarity: 1}
		}
		aliasBases = append(aliasBases, aliasBase)
	}

	best := similarity(subjectNorm, entityNorm)
	for _, aliasNorm := range aliasNorms {
		if score := similarity(subjectNorm, aliasNorm); score > best {
			best = score
		}
	}
	guarded := initialismEqual(subjectBase, entityBase) || compactContains(subjectNorm, entityNorm)
	for index := 0; index < len(aliasBases) && !guarded; index++ {
		guarded = initialismEqual(subjectBase, aliasBases[index]) ||
			compactContains(subjectNorm, aliasNorms[index])
	}
	if !guarded && best < similarityFloor {
		return MatchResult{Verdict: VerdictMismatch, Tier: TierFloor, Similarity: best}
	}
	return MatchResult{Verdict: VerdictUncertain, Tier: TierNone, Similarity: best}
}

// similarity is the maximum of three complementary comparisons on normalized
// forms: token-set Jaccard (space-separated scripts), character n-gram
// Jaccard with n shrunk to the shorter form (covers CJK, floor 2 via
// minContainmentRunes-independent logic), and a near-typo score that only
// counts within nearTypoMaxEdits.
func similarity(first, second string) float64 {
	best := tokenJaccard(first, second)
	if score := gramJaccard(first, second); score > best {
		best = score
	}
	if score := nearTypoSimilarity(first, second); score > best {
		best = score
	}
	return best
}

func tokenJaccard(first, second string) float64 {
	firstSet := tokenSet(strings.Fields(first))
	secondSet := tokenSet(strings.Fields(second))
	return jaccard(firstSet, secondSet)
}

func tokenSet(tokens []string) map[string]struct{} {
	set := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		set[token] = struct{}{}
	}
	return set
}

// gramJaccard compares character n-grams over space-stripped runes. The gram
// size starts at 3 and shrinks to the shorter input so two-rune CJK names
// still produce comparable sets; zero-length input yields zero similarity.
func gramJaccard(first, second string) float64 {
	firstRunes := compactRunes(first)
	secondRunes := compactRunes(second)
	size := 3
	if len(firstRunes) < size {
		size = len(firstRunes)
	}
	if len(secondRunes) < size {
		size = len(secondRunes)
	}
	if size == 0 {
		return 0
	}
	return jaccard(gramSet(firstRunes, size), gramSet(secondRunes, size))
}

func gramSet(runes []rune, size int) map[string]struct{} {
	set := make(map[string]struct{}, len(runes))
	for index := 0; index+size <= len(runes); index++ {
		set[string(runes[index:index+size])] = struct{}{}
	}
	return set
}

func jaccard(first, second map[string]struct{}) float64 {
	if len(first) == 0 || len(second) == 0 {
		return 0
	}
	intersection := 0
	for item := range first {
		if _, ok := second[item]; ok {
			intersection++
		}
	}
	union := len(first) + len(second) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// nearTypoSimilarity reports 1 - distance/longestLength only when the edit
// distance stays within nearTypoMaxEdits, and zero otherwise.
func nearTypoSimilarity(first, second string) float64 {
	firstRunes := compactRunes(first)
	secondRunes := compactRunes(second)
	longest := len(firstRunes)
	if len(secondRunes) > longest {
		longest = len(secondRunes)
	}
	if longest == 0 {
		return 0
	}
	if difference(len(firstRunes), len(secondRunes)) > nearTypoMaxEdits {
		return 0
	}
	distance := editDistance(firstRunes, secondRunes)
	if distance > nearTypoMaxEdits {
		return 0
	}
	return 1 - float64(distance)/float64(longest)
}

func editDistance(first, second []rune) int {
	if len(first) == 0 {
		return len(second)
	}
	if len(second) == 0 {
		return len(first)
	}
	previous := make([]int, len(second)+1)
	current := make([]int, len(second)+1)
	for column := range previous {
		previous[column] = column
	}
	for row := 1; row <= len(first); row++ {
		current[0] = row
		for column := 1; column <= len(second); column++ {
			cost := 1
			if first[row-1] == second[column-1] {
				cost = 0
			}
			current[column] = min(previous[column]+1, current[column-1]+1, previous[column-1]+cost)
		}
		previous, current = current, previous
	}
	return previous[len(second)]
}

func difference(first, second int) int {
	if first > second {
		return first - second
	}
	return second - first
}

func compactRunes(value string) []rune {
	runes := make([]rune, 0, len(value))
	for _, r := range value {
		if r != ' ' {
			runes = append(runes, r)
		}
	}
	return runes
}

// initialismEqual reports whether one side is exactly the initials of the
// other's tokens ("ibm" vs "international business machines"). Both
// directions are checked on stripped base forms; the initialism side must be
// a single token of at least minInitialismRunes runes.
func initialismEqual(first, second string) bool {
	return isInitialismOf(first, second) || isInitialismOf(second, first)
}

func isInitialismOf(short, long string) bool {
	if strings.ContainsRune(short, ' ') {
		return false
	}
	shortRunes := []rune(short)
	if len(shortRunes) < minInitialismRunes {
		return false
	}
	tokens := strings.Fields(long)
	if len(tokens) != len(shortRunes) {
		return false
	}
	for index, token := range tokens {
		if []rune(token)[0] != shortRunes[index] {
			return false
		}
	}
	return true
}

// compactContains reports whether either space-stripped form contains the
// other, with the shorter side long enough for its script (see
// minContainmentRunes). Substring relations (sub-brands, campus units,
// product lines) are confusable by construction and must reach a referee
// instead of a deterministic alarm.
func compactContains(first, second string) bool {
	shorter := compactRunes(first)
	longer := compactRunes(second)
	if len(shorter) > len(longer) {
		shorter, longer = longer, shorter
	}
	minimum := minContainmentRunes
	if allCJK(shorter) {
		minimum = minContainmentRunesCJK
	}
	if len(shorter) < minimum {
		return false
	}
	return strings.Contains(string(longer), string(shorter))
}

func allCJK(runes []rune) bool {
	if len(runes) == 0 {
		return false
	}
	for _, r := range runes {
		if !unicode.Is(unicode.Han, r) && !unicode.Is(unicode.Hiragana, r) &&
			!unicode.Is(unicode.Katakana, r) && !unicode.Is(unicode.Hangul, r) {
			return false
		}
	}
	return true
}
