package compiler

import "testing"

func TestBuiltInTransforms(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "trim", input: "  Hello \n", want: "Hello"},
		{name: "collapse_ws", input: " A\n\tB\u00a0 C ", want: "A B C"},
		{name: "lower", input: "Straße ACME", want: "straße acme"},
		{name: "parse_number", input: "1,299.50", want: "1299.50"},
		{name: "parse_date", input: "August 9, 2026 14:30 +08:00", want: "2026-08-09T06:30:00Z"},
		{name: "currency_amount", input: "USD $1,299.50", want: "1299.50"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := transformRegistry[test.name](test.input)
			if err != nil {
				t.Fatalf("transform: %v", err)
			}
			if got != test.want {
				t.Fatalf("transform(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestNumberNormalization(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "1 299,50", want: "1299.50"},
		{input: "1.299,50", want: "1299.50"},
		{input: "(1,299.50)", want: "-1299.50"},
		{input: "00012.30", want: "12.30"},
		{input: "0.125", want: "0.125"},
		{input: "0,125", want: "0.125"},
	}
	for _, test := range tests {
		got, err := canonicalNumber(test.input)
		if err != nil {
			t.Fatalf("canonicalNumber(%q): %v", test.input, err)
		}
		if got != test.want {
			t.Errorf("canonicalNumber(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}
