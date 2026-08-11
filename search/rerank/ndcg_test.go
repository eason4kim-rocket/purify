package rerank

import (
	"math"
	"reflect"
	"testing"
)

func TestNDCGAtK(t *testing.T) {
	tests := []struct {
		name   string
		grades []int
		k      int
		want   float64
	}{
		{name: "ideal", grades: []int{3, 2, 1, 0, 0}, k: 5, want: 1},
		{name: "two item displacement", grades: []int{0, 3}, k: 5, want: 0.6309297535714575},
		{name: "two item inversion", grades: []int{2, 3}, k: 5, want: 0.8339912323981488},
		{name: "non ideal", grades: []int{0, 3, 2, 1, 0}, k: 5, want: 0.6757507974357403},
		{name: "relevant tail only", grades: []int{0, 0, 0, 0, 0, 3}, k: 5, want: 0},
		{name: "top one", grades: []int{2, 3, 1}, k: 1, want: 3.0 / 7.0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			original := append([]int(nil), test.grades...)
			got, err := ndcgAtK(test.grades, test.k)
			if err != nil {
				t.Fatalf("ndcgAtK() error = %v", err)
			}
			if math.Abs(got-test.want) > 1e-15 {
				t.Fatalf("ndcgAtK() = %.17g, want %.17g", got, test.want)
			}
			if !reflect.DeepEqual(test.grades, original) {
				t.Fatalf("NDCGAtK mutated grades: got %v want %v", test.grades, original)
			}
		})
	}
}

func TestNDCGAt5TwentyCandidates(t *testing.T) {
	tests := []struct {
		name          string
		relevantIndex int
		want          float64
	}{
		{name: "rank five included", relevantIndex: 4, want: 1 / math.Log2(6)},
		{name: "rank six excluded", relevantIndex: 5, want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			grades := make([]int, MaxCandidates)
			grades[test.relevantIndex] = 3
			got, err := NDCGAt5(grades)
			if err != nil {
				t.Fatalf("NDCGAt5() error = %v", err)
			}
			if math.Abs(got-test.want) > 1e-15 {
				t.Fatalf("NDCGAt5() = %.17g, want %.17g", got, test.want)
			}
		})
	}
}

func TestNDCGAtKRejectsInvalidGrades(t *testing.T) {
	tests := []struct {
		name   string
		grades []int
		k      int
	}{
		{name: "empty", k: 5},
		{name: "all zero", grades: []int{0, 0, 0}, k: 5},
		{name: "negative", grades: []int{-1, 2}, k: 5},
		{name: "over grade", grades: []int{4, 2}, k: 5},
		{name: "zero k", grades: []int{3}, k: 0},
		{name: "over k", grades: []int{3}, k: MaxCandidates + 1},
		{name: "too many candidates", grades: append([]int{3}, make([]int, MaxCandidates)...), k: 5},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			original := append([]int(nil), test.grades...)
			if _, err := ndcgAtK(test.grades, test.k); !errorsIs(err, ErrInvalidGrades) {
				t.Fatalf("ndcgAtK() error = %v, want ErrInvalidGrades", err)
			}
			if !reflect.DeepEqual(test.grades, original) {
				t.Fatalf("invalid ndcgAtK mutated grades: got %v want %v", test.grades, original)
			}
		})
	}
}
