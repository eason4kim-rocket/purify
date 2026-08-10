package eav

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestDecodeRefereeReply(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want RefereeVerdict
	}{
		{"same", `{"answer":"same","quote":null}`, RefereeVerdict{Answer: RefereeSame}},
		{
			"different with quote", `{"answer":"different","quote":"Apple Bank was founded in 1863."}`,
			RefereeVerdict{Answer: RefereeDifferent, Quote: "Apple Bank was founded in 1863."},
		},
		{"unsure", `{"answer":"unsure","quote":null}`, RefereeVerdict{Answer: RefereeUnsure}},
		{"case folded", `{"answer":" SAME ","quote":null}`, RefereeVerdict{Answer: RefereeSame}},
		{"unknown coerces to unsure", `{"answer":"maybe","quote":null}`, RefereeVerdict{Answer: RefereeUnsure}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := DecodeRefereeReply([]byte(testCase.raw))
			if err != nil {
				t.Fatalf("DecodeRefereeReply(%s) returned error: %v", testCase.raw, err)
			}
			if got != testCase.want {
				t.Fatalf("DecodeRefereeReply(%s) = %#v, want %#v", testCase.raw, got, testCase.want)
			}
		})
	}

	invalid := [][]byte{
		nil,
		[]byte(`{oops`),
		[]byte(`{"answer":"same","quote":null} trailing`),
		[]byte(strings.Repeat("x", MaxRefereeReplyBytes+1)),
	}
	for index, raw := range invalid {
		if _, err := DecodeRefereeReply(raw); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid reply %d must return ErrInvalidInput, got %v", index, err)
		}
	}
}

func TestRefereeReplySchemaMatchesContract(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(RefereeReplySchema), &schema); err != nil {
		t.Fatalf("schema constant must be valid JSON: %v", err)
	}
	enum := schema["properties"].(map[string]any)["answer"].(map[string]any)["enum"].([]any)
	wantAnswers := map[string]struct{}{
		string(RefereeSame): {}, string(RefereeDifferent): {}, string(RefereeUnsure): {},
	}
	if len(enum) != len(wantAnswers) {
		t.Fatalf("schema answer enum has %d entries, want %d", len(enum), len(wantAnswers))
	}
	for _, answer := range enum {
		if _, ok := wantAnswers[answer.(string)]; !ok {
			t.Fatalf("schema answer %q is not a RefereeAnswer literal", answer)
		}
	}
}

func TestBuildRefereeInput(t *testing.T) {
	subject := Subject{Name: "Apple", Hint: "chief executive officer"}
	entity := Entity{
		Name:    "Apple Bank",
		Kind:    KindOrganization,
		Aliases: []string{"ABNK"},
		Quote:   "Apple Bank was founded in 1863.",
	}
	doc := Document{
		URL:     "https://applebank.example/about",
		Title:   "Apple Bank - Personal Banking",
		Cleaned: "Apple Bank offers personal savings accounts.",
	}
	first := BuildRefereeInput(subject, entity, doc)
	if second := BuildRefereeInput(subject, entity, doc); second != first {
		t.Fatal("referee input must be deterministic")
	}
	for _, fragment := range []string{
		"SUBJECT: Apple",
		"HINT: chief executive officer",
		"DOCUMENT ENTITY: Apple Bank",
		"KIND: organization",
		"ALIASES: ABNK",
		"ENTITY QUOTE: Apple Bank was founded in 1863.",
		"URL: https://applebank.example/about",
		"TITLE: Apple Bank - Personal Banking",
		"CONTENT:\nApple Bank offers personal savings accounts.",
	} {
		if !strings.Contains(first, fragment) {
			t.Fatalf("referee input is missing %q in:\n%s", fragment, first)
		}
	}

	bare := BuildRefereeInput(Subject{Name: "Apple"}, Entity{Name: "Apple Bank"}, doc)
	for _, absent := range []string{"HINT:", "ALIASES:", "ENTITY QUOTE:"} {
		if strings.Contains(bare, absent) {
			t.Fatalf("referee input must omit empty section %q in:\n%s", absent, bare)
		}
	}
}

func TestRefereeFuncNilGuard(t *testing.T) {
	var fn RefereeFunc
	if _, err := fn.SameReferent(context.Background(), Subject{}, Entity{}, Document{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("nil referee function must return ErrNotConfigured, got %v", err)
	}
}
