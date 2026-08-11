package simhash

import (
	"hash/fnv"
	"slices"
	"testing"
)

func TestFingerprintShinglesTreatsEncodedShingleAsAtomic(t *testing.T) {
	const encoded = "word\x00shingle with embedded whitespace"

	h := fnv.New64a()
	_, _ = h.Write([]byte(encoded))
	wantFingerprint := h.Sum64()

	got := FingerprintShingles([]string{encoded}, 31)
	if got.Fingerprint != wantFingerprint {
		t.Fatalf("Fingerprint = %016x, want atomic FNV-64a %016x", got.Fingerprint, wantFingerprint)
	}
	if got.RetainedShingles != 1 {
		t.Errorf("RetainedShingles = %d, want 1", got.RetainedShingles)
	}
	if got.NormalizedAlnumRunes != 31 {
		t.Errorf("NormalizedAlnumRunes = %d, want 31", got.NormalizedAlnumRunes)
	}
	if got.Valid {
		t.Error("Valid = true with fewer than 24 retained shingles")
	}
}

func TestFingerprintShinglesValidBoundary(t *testing.T) {
	shingles := make([]string, 24)
	for i := range shingles {
		shingles[i] = string(rune('a' + i))
	}

	below := FingerprintShingles(shingles[:23], 23)
	if below.Valid {
		t.Error("Valid = true with 23 retained shingles")
	}

	at := FingerprintShingles(shingles, 24)
	if !at.Valid {
		t.Error("Valid = false with 24 retained shingles")
	}
	if at.RetainedShingles != 24 {
		t.Errorf("RetainedShingles = %d, want 24", at.RetainedShingles)
	}
}

func TestFingerprintShinglesZeroFingerprintCanBeValid(t *testing.T) {
	// These 24 distinct atoms have at most 12 FNV-64a one bits in every
	// position, so SimHash's strict-majority tie rule produces zero.
	shingles := []string{
		"zero-valid-492",
		"zero-valid-722",
		"zero-valid-743",
		"zero-valid-744",
		"zero-valid-1312",
		"zero-valid-2787",
		"zero-valid-2910",
		"zero-valid-2911",
		"zero-valid-2915",
		"zero-valid-2916",
		"zero-valid-2919",
		"zero-valid-3158",
		"zero-valid-8020",
		"zero-valid-8210",
		"zero-valid-8213",
		"zero-valid-8214",
		"zero-valid-8812",
		"zero-valid-10801",
		"zero-valid-14877",
		"zero-valid-14938",
		"zero-valid-17591",
		"zero-valid-17596",
		"zero-valid-21139",
		"zero-valid-33125",
	}

	got := FingerprintShingles(shingles, 505)
	if got.Fingerprint != 0 {
		t.Fatalf("Fingerprint = %016x, want 0", got.Fingerprint)
	}
	if !got.Valid {
		t.Error("Valid = false for 24 retained shingles with fingerprint zero")
	}
	if got.NormalizedAlnumRunes != 505 {
		t.Errorf("NormalizedAlnumRunes = %d, want 505", got.NormalizedAlnumRunes)
	}
}

func TestFingerprintShinglesStableAcrossInputOrder(t *testing.T) {
	shingles := []string{
		"w:alpha|beta|gamma",
		"w:beta|gamma|delta",
		"r:\xe4\xb8\xad\xe6\x96\x87\xe6\xb5\x8b\xe8\xaf\x95",
		"r:mixed123",
	}
	want := FingerprintShingles(shingles, 42)

	reversed := slices.Clone(shingles)
	slices.Reverse(reversed)
	got := FingerprintShingles(reversed, 42)
	if got != want {
		t.Fatalf("reordered result = %#v, want %#v", got, want)
	}

	again := FingerprintShingles(shingles, 42)
	if again != want {
		t.Fatalf("repeated result = %#v, want %#v", again, want)
	}
}
