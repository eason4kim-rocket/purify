package rerank

import (
	"math"
	"sort"
)

func ndcgAtK(orderedGrades []int, k int) (float64, error) {
	if len(orderedGrades) == 0 || len(orderedGrades) > MaxCandidates || k < 1 || k > MaxCandidates {
		return 0, ErrInvalidGrades
	}
	ideal := append([]int(nil), orderedGrades...)
	for _, grade := range ideal {
		if grade < 0 || grade > 3 {
			return 0, ErrInvalidGrades
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ideal)))

	cutoff := min(k, len(orderedGrades))
	dcg := discountedGain(orderedGrades[:cutoff])
	idcg := discountedGain(ideal[:cutoff])
	if idcg == 0 || math.IsNaN(idcg) || math.IsInf(idcg, 0) {
		return 0, ErrInvalidGrades
	}
	result := dcg / idcg
	if math.IsNaN(result) || math.IsInf(result, 0) {
		return 0, ErrInvalidGrades
	}
	return result, nil
}

// NDCGAt5 is the fixed product metric used by the rerank evaluation cards.
func NDCGAt5(orderedGrades []int) (float64, error) {
	return ndcgAtK(orderedGrades, 5)
}

func discountedGain(grades []int) float64 {
	total := 0.0
	for index, grade := range grades {
		gain := float64((uint64(1) << uint(grade)) - 1)
		total += gain / math.Log2(float64(index+2))
	}
	return total
}
