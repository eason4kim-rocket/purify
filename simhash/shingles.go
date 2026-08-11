package simhash

import "hash/fnv"

const minimumValidShingles = 24

// ShingleFingerprint describes a SimHash computed from retained atomic
// shingles. Valid is independent of the fingerprint value.
type ShingleFingerprint struct {
	Fingerprint          uint64
	RetainedShingles     uint64
	NormalizedAlnumRunes uint64
	Valid                bool
}

// FingerprintShingles computes a 64-bit SimHash from already-selected atomic
// shingle encodings. Each string is hashed as one indivisible atom; callers are
// responsible for bounded selection and uniqueness.
func FingerprintShingles(retained []string, normalizedAlnumRunes uint64) ShingleFingerprint {
	result := ShingleFingerprint{
		RetainedShingles:     uint64(len(retained)),
		NormalizedAlnumRunes: normalizedAlnumRunes,
		Valid:                len(retained) >= minimumValidShingles,
	}

	var vector [64]int
	for _, shingle := range retained {
		h := fnv.New64a()
		_, _ = h.Write([]byte(shingle))
		hash := h.Sum64()

		for i := range vector {
			if hash&(uint64(1)<<uint(i)) != 0 {
				vector[i]++
			} else {
				vector[i]--
			}
		}
	}

	for i, weight := range vector {
		if weight > 0 {
			result.Fingerprint |= uint64(1) << uint(i)
		}
	}

	return result
}
