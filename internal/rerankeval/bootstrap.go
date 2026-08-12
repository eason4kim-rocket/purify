package rerankeval

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

const (
	MinimumSignificanceCases = 50
	BootstrapSeed            = uint64(0x5055524946595231)
	BootstrapIterations      = 10_000
	BootstrapLower95Index    = 249
)

var ErrInsufficientSample = errors.New("rerankeval: insufficient bootstrap sample")

type PairedDelta struct {
	CaseID     string
	DeltaNDCG5 float64
}

// RequireCorpusSignificance is the end-to-end significance gate. It derives
// every paired delta from a corpus that passes the same manifest, production
// input, candidate-join, and metric validation as the construction scorecard;
// callers cannot substitute an independently asserted delta list.
func RequireCorpusSignificance(corpus Corpus) (float64, error) {
	scorecard, err := Evaluate(corpus)
	if err != nil {
		return 0, err
	}
	deltas := make([]PairedDelta, len(scorecard.Cases))
	for index, metric := range scorecard.Cases {
		deltas[index] = PairedDelta{CaseID: metric.CaseID, DeltaNDCG5: metric.DeltaNDCG5}
	}
	return RequirePositivePairedBootstrap(deltas)
}

// PairedBootstrapLower95 is the locked low-level bootstrap primitive used by
// RequireCorpusSignificance. Release decisions must use the corpus entrypoint,
// not a caller-assembled delta slice. Case IDs are byte-lexically ordered
// before a single continuous SplitMix64 stream is consumed.
func PairedBootstrapLower95(input []PairedDelta) (float64, error) {
	if len(input) < MinimumSignificanceCases {
		return 0, fmt.Errorf("%w: got %d, need %d", ErrInsufficientSample, len(input), MinimumSignificanceCases)
	}
	deltas := append([]PairedDelta(nil), input...)
	seen := make(map[string]struct{}, len(deltas))
	for _, delta := range deltas {
		if !validPlainID(delta.CaseID) || math.IsNaN(delta.DeltaNDCG5) || math.IsInf(delta.DeltaNDCG5, 0) ||
			delta.DeltaNDCG5 < -1 || delta.DeltaNDCG5 > 1 {
			return 0, fmt.Errorf("%w: invalid paired delta", ErrInvalidCorpus)
		}
		if _, duplicate := seen[delta.CaseID]; duplicate {
			return 0, fmt.Errorf("%w: duplicate case %q", ErrInvalidCorpus, delta.CaseID)
		}
		seen[delta.CaseID] = struct{}{}
	}
	sort.Slice(deltas, func(left, right int) bool { return strings.Compare(deltas[left].CaseID, deltas[right].CaseID) < 0 })
	means := make([]float64, BootstrapIterations)
	state := BootstrapSeed
	for iteration := range means {
		total := 0.0
		for range deltas {
			index := splitMix64(&state) % uint64(len(deltas))
			total += deltas[index].DeltaNDCG5
		}
		means[iteration] = total / float64(len(deltas))
	}
	sort.Float64s(means)
	return means[BootstrapLower95Index], nil
}

// RequirePositivePairedBootstrap applies the significance gate after computing
// the auditable lower bound. Construction corpora smaller than fifty cases can
// never pass this function.
func RequirePositivePairedBootstrap(input []PairedDelta) (float64, error) {
	lower, err := PairedBootstrapLower95(input)
	if err != nil {
		return 0, err
	}
	if lower <= 0 {
		return lower, fmt.Errorf("%w: paired-bootstrap lower bound %.17g is not positive", ErrGateFailed, lower)
	}
	return lower, nil
}

func splitMix64(state *uint64) uint64 {
	*state += 0x9e3779b97f4a7c15
	z := *state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}
