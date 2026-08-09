package evidence

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAlignValueContextMatchesAlignValue(t *testing.T) {
	tests := []struct {
		name, value, cleaned, html string
	}{
		{
			name:    "exact",
			value:   "Pro Plan",
			cleaned: "Choose the Pro Plan today",
			html:    `<main><h2 id="pro">Pro Plan</h2></main>`,
		},
		{
			name:    "normalized",
			value:   "Fast reliable search",
			cleaned: "Fast\n\t reliable   search",
			html:    `<p class="tagline">Fast reliable search</p>`,
		},
		{
			name:    "fuzzy",
			value:   "Acme new model launches today",
			cleaned: "Breaking: Acme launches new model today worldwide.",
			html:    `<article><p class="lead">Acme launches new model today worldwide.</p></article>`,
		},
		{
			name:    "unlocated",
			value:   "invented fact",
			cleaned: "Only sourced facts live here",
			html:    `<p>Only sourced facts live here</p>`,
		},
		{name: "empty value", cleaned: "content", html: `<p>content</p>`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			legacy := AlignValue(test.value, test.cleaned, test.html)
			contextual, err := AlignValueContext(context.Background(), test.value, test.cleaned, test.html)
			if err != nil {
				t.Fatalf("AlignValueContext() error = %v", err)
			}
			if !reflect.DeepEqual(contextual, legacy) {
				t.Fatalf("AlignValueContext() = %#v, AlignValue() = %#v", contextual, legacy)
			}
		})
	}
}

func TestAlignValueContextRejectsNilAndPreCanceledContext(t *testing.T) {
	if _, err := AlignValueContext(nil, "needle", "needle", `<p>needle</p>`); err == nil {
		t.Fatal("AlignValueContext() accepted a nil context")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	anchor, err := alignValueContext(ctx, "needle", "needle", func(context.Context, string) (string, error) {
		called = true
		return "#needle", nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("alignValueContext() error = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("pre-canceled alignment invoked the selector path")
	}
	if !reflect.DeepEqual(anchor, Anchor{}) {
		t.Fatalf("pre-canceled anchor = %#v, want zero value", anchor)
	}
}

func TestAlignmentScansObserveCancellation(t *testing.T) {
	t.Run("exact", func(t *testing.T) {
		ctx := newCancelAfterChecksContext(2, context.Canceled)
		if _, err := indexStringContext(ctx, strings.Repeat("a", 32<<10), "b"); !errors.Is(err, context.Canceled) {
			t.Fatalf("indexStringContext() error = %v, want context.Canceled", err)
		}
	})

	t.Run("normalized", func(t *testing.T) {
		ctx := newCancelAfterChecksContext(2, context.DeadlineExceeded)
		if _, err := normalizeWithOffsetsContext(ctx, strings.Repeat("A ", 8<<10)); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("normalizeWithOffsetsContext() error = %v, want context.DeadlineExceeded", err)
		}
	})

	t.Run("fuzzy inner loop", func(t *testing.T) {
		ctx := newCancelAfterChecksContext(20, context.Canceled)
		_, _, _, _, err := fuzzyWindowContext(
			ctx,
			"alpha beta gamma delta epsilon",
			strings.Repeat("noise ", 100),
		)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("fuzzyWindowContext() error = %v, want context.Canceled", err)
		}
	})

	t.Run("HTML parser", func(t *testing.T) {
		ctx := newCancelAfterChecksContext(4, context.DeadlineExceeded)
		_, err := newSelectorDocumentContext(ctx, `<main>`+strings.Repeat("x", 16<<10)+`</main>`)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("newSelectorDocumentContext() error = %v, want context.DeadlineExceeded", err)
		}
	})

	t.Run("HTML DOM text", func(t *testing.T) {
		document, err := newSelectorDocumentContext(
			context.Background(),
			`<main>`+strings.Repeat(`<p>noise</p>`, 1_000)+`<p>needle</p></main>`,
		)
		if err != nil {
			t.Fatalf("newSelectorDocumentContext() error = %v", err)
		}
		ctx := newCancelAfterChecksContext(6, context.Canceled)
		if _, err := document.findContext(ctx, "needle"); !errors.Is(err, context.Canceled) {
			t.Fatalf("findContext() error = %v, want context.Canceled", err)
		}
	})

	t.Run("HTML selector uniqueness", func(t *testing.T) {
		document, err := newSelectorDocumentContext(
			context.Background(),
			`<main>`+strings.Repeat(`<p class="same">noise</p>`, 100)+`<p id="needle">needle</p></main>`,
		)
		if err != nil {
			t.Fatalf("newSelectorDocumentContext() error = %v", err)
		}
		target := document.doc.Find("#needle")
		ctx := newCancelAfterChecksContext(10, context.DeadlineExceeded)
		if _, err := selectorForContext(ctx, document.doc.Selection.Nodes, target.Nodes[0]); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("selectorForContext() error = %v, want context.DeadlineExceeded", err)
		}
	})

}

func TestAlignValueContextObservesLateCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	anchor, err := alignValueContext(ctx, "needle", "needle", func(context.Context, string) (string, error) {
		cancel()
		return "#needle", nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("alignValueContext() error = %v, want context.Canceled", err)
	}
	if !reflect.DeepEqual(anchor, Anchor{}) {
		t.Fatalf("late-canceled anchor = %#v, want zero value", anchor)
	}
}

func TestFindUniqueSelectorPreservesCascadiaCandidateSemantics(t *testing.T) {
	tests := []struct {
		name string
		html string
		want string
	}{
		{name: "lone hyphen class", html: `<p class="-">needle</p>`, want: ""},
		{name: "hyphen digit class", html: `<p class="-1">needle</p>`, want: ""},
		{name: "hyphen id", html: `<p id="-">needle</p>`, want: "#-"},
		{name: "invalid then valid class", html: `<p class="- ok">needle</p>`, want: "p.ok"},
		{name: "leading digit id", html: `<p id="1lead">needle</p>`, want: `#\31 lead`},
		{name: "special id", html: `<p id="a:b">needle</p>`, want: `#a\3a b`},
		{name: "non ASCII id", html: `<p id="名:id">needle</p>`, want: `#\540d \3a id`},
		{name: "non ASCII class", html: `<p class="café">needle</p>`, want: `p.caf\e9 `},
		{
			name: "escaped combined classes",
			html: `<main><p class="a:b café">needle</p><p class="a:b">other</p><p class="café">other</p></main>`,
			want: `p.a\3a b.caf\e9 `,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selector := FindUniqueSelector(test.html, "needle")
			if selector != test.want {
				t.Fatalf("FindUniqueSelector() = %q, want %q", selector, test.want)
			}
			if selector == "" {
				return
			}
			document := newSelectorDocument(test.html)
			matched := document.doc.Find(selector)
			if matched.Length() != 1 || normalizeText(matched.Text()) != "needle" {
				t.Fatalf("published selector %q is not uniquely replayable", selector)
			}
		})
	}
}

func TestContextSelectorPreservesLegacyTagMatching(t *testing.T) {
	tests := []struct {
		name string
		html string
	}{
		{name: "SVG adjusted name", html: `<svg><linearGradient>needle</linearGradient></svg>`},
		{name: "foreign object", html: `<svg><foreignObject>needle</foreignObject></svg>`},
		{name: "custom colon tag", html: `<x:y>needle</x:y>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if selector := FindUniqueSelector(test.html, "needle"); selector != "" {
				t.Fatalf("legacy selector = %q, want empty", selector)
			}
			anchor, err := AlignValueContext(context.Background(), "needle", "needle", test.html)
			if err != nil {
				t.Fatalf("AlignValueContext() error = %v", err)
			}
			if anchor.Method != MethodExact || anchor.Selector != "" {
				t.Fatalf("AlignValueContext() = %#v, want exact anchor without selector", anchor)
			}
		})
	}
}

func TestContextSelectorComplexityBudgetsOmitOnlySelector(t *testing.T) {
	t.Run("HTML bytes N and N plus one", func(t *testing.T) {
		prefix := `<p>needle</p><!--`
		suffix := `-->`
		atLimit := prefix + strings.Repeat("x", maxContextSelectorHTMLBytes-len(prefix)-len(suffix)) + suffix
		if len(atLimit) != maxContextSelectorHTMLBytes {
			t.Fatalf("fixture length = %d, want %d", len(atLimit), maxContextSelectorHTMLBytes)
		}
		anchor, err := AlignValueContext(context.Background(), "needle", "needle", atLimit)
		if err != nil || anchor.Method != MethodExact || anchor.Selector == "" {
			t.Fatalf("at-limit AlignValueContext() = %#v, %v", anchor, err)
		}
		overLimit := atLimit + " "
		anchor, err = AlignValueContext(context.Background(), "needle", "needle", overLimit)
		if err != nil || anchor.Method != MethodExact || anchor.Selector != "" {
			t.Fatalf("over-limit AlignValueContext() = %#v, %v", anchor, err)
		}
		if selector := FindUniqueSelector(overLimit, "needle"); selector == "" {
			t.Fatal("legacy selector unexpectedly inherited the context-only HTML budget")
		}
	})

	t.Run("class bytes N and N plus one", func(t *testing.T) {
		for _, size := range []int{maxContextSelectorClassBytes, maxContextSelectorClassBytes + 1} {
			html := `<p class="` + strings.Repeat("a", size) + `">needle</p>`
			document, err := newSelectorDocumentContext(context.Background(), html)
			if err != nil {
				t.Fatalf("size %d parse error = %v", size, err)
			}
			node := document.doc.Find("p").Nodes[0]
			classes, err := classNamesNodeContext(context.Background(), node)
			if size == maxContextSelectorClassBytes {
				if err != nil || len(classes) != 1 || len(classes[0]) != size {
					t.Fatalf("at-limit classes = %d, %v", len(classes), err)
				}
			} else if !errors.Is(err, errContextSelectorBudget) {
				t.Fatalf("over-limit error = %v, want selector budget", err)
			}
		}
	})

	t.Run("class token N and N plus one", func(t *testing.T) {
		for _, count := range []int{maxContextSelectorClasses, maxContextSelectorClasses + 1} {
			html := `<p class="` + strings.TrimSpace(strings.Repeat("x ", count)) + `">needle</p>`
			document, err := newSelectorDocumentContext(context.Background(), html)
			if err != nil {
				t.Fatalf("count %d parse error = %v", count, err)
			}
			classes, err := classNamesNodeContext(context.Background(), document.doc.Find("p").Nodes[0])
			if count == maxContextSelectorClasses {
				if err != nil || len(classes) != count {
					t.Fatalf("at-limit classes = %d, %v", len(classes), err)
				}
			} else if !errors.Is(err, errContextSelectorBudget) {
				t.Fatalf("over-limit error = %v, want selector budget", err)
			}
		}
	})

	t.Run("non-target class budget", func(t *testing.T) {
		html := `<main><p class="a">needle</p><p class="` + strings.Repeat("x ", maxContextSelectorClassBytes) + `">other</p></main>`
		anchor, err := AlignValueContext(context.Background(), "needle", "needle", html)
		if err != nil || anchor.Method != MethodExact || anchor.Selector != "" {
			t.Fatalf("AlignValueContext() = %#v, %v", anchor, err)
		}
	})

	t.Run("DOM depth", func(t *testing.T) {
		html := strings.Repeat("<div>", maxContextSelectorDepth+1) + "needle" + strings.Repeat("</div>", maxContextSelectorDepth+1)
		anchor, err := AlignValueContext(context.Background(), "needle", "needle", html)
		if err != nil || anchor.Method != MethodExact || anchor.Selector != "" {
			t.Fatalf("AlignValueContext() = %#v, %v", anchor, err)
		}
	})

	t.Run("DOM elements", func(t *testing.T) {
		html := `<main><p>needle</p>` + strings.Repeat(`<i></i>`, maxContextSelectorDOMElements) + `</main>`
		anchor, err := AlignValueContext(context.Background(), "needle", "needle", html)
		if err != nil || anchor.Method != MethodExact || anchor.Selector != "" {
			t.Fatalf("AlignValueContext() = %#v, %v", anchor, err)
		}
	})

	t.Run("cumulative text work", func(t *testing.T) {
		text := strings.Repeat("x", 2<<20) + "needle"
		html := strings.Repeat("<div>", 10) + text + strings.Repeat("</div>", 10)
		anchor, err := AlignValueContext(context.Background(), "needle", "needle", html)
		if err != nil || anchor.Method != MethodExact || anchor.Selector != "" {
			t.Fatalf("AlignValueContext() = %#v, %v", anchor, err)
		}
	})

	t.Run("candidate bytes N and N plus one", func(t *testing.T) {
		atLimitID := strings.Repeat("a", maxContextSelectorCandidateBytes-1)
		anchor, err := AlignValueContext(
			context.Background(),
			"needle",
			"needle",
			`<p id="`+atLimitID+`">needle</p>`,
		)
		if err != nil || len(anchor.Selector) != maxContextSelectorCandidateBytes || anchor.Selector[0] != '#' {
			t.Fatalf("at-limit AlignValueContext() = selector bytes %d, %v", len(anchor.Selector), err)
		}
		overLimitID := strings.Repeat("a", maxContextSelectorCandidateBytes)
		anchor, err = AlignValueContext(
			context.Background(),
			"needle",
			"needle",
			`<p id="`+overLimitID+`">needle</p>`,
		)
		if err != nil || anchor.Selector != "p" {
			t.Fatalf("over-limit AlignValueContext() = %#v, %v", anchor, err)
		}
	})

	t.Run("budget error yields to cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := contextSelectorResult(ctx, errContextSelectorBudget); !errors.Is(err, context.Canceled) {
			t.Fatalf("contextSelectorResult() error = %v, want context.Canceled", err)
		}
	})
}

func TestSortStringsContextMatchesStandardSortAndCancels(t *testing.T) {
	values := []string{"z", "", "é", "aa", "a", "界", "-1", "-", "aa", "A"}
	want := append([]string(nil), values...)
	sort.Strings(want)
	got := append([]string(nil), values...)
	if err := sortStringsContext(context.Background(), got); err != nil {
		t.Fatalf("sortStringsContext() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sortStringsContext() = %#v, want %#v", got, want)
	}

	large := make([]string, 20_000)
	for index := range large {
		large[index] = strings.Repeat("a", 16) + string(rune(0x1000+index%1_000))
	}
	ctx := newCancelAfterChecksContext(2, context.Canceled)
	if err := sortStringsContext(ctx, large); !errors.Is(err, context.Canceled) {
		t.Fatalf("sortStringsContext() cancellation error = %v, want context.Canceled", err)
	}
}

func TestIndexStringContextMatchesStringsIndex(t *testing.T) {
	values := []string{"", "a", "b", "aa", "ab", "ba", "aba", "bbb", "é", "界", "a界"}
	texts := []string{"", "a", "bbb", "abababa", "baaab", "café", "边界a界尾"}
	values = append(values, stringsOverAlphabet("abc", 4)...)
	texts = append(texts, stringsOverAlphabet("abc", 5)...)
	for _, text := range texts {
		for _, value := range values {
			got, err := indexStringContext(context.Background(), text, value)
			if err != nil {
				t.Fatalf("indexStringContext(%q, %q) error = %v", text, value, err)
			}
			if want := strings.Index(text, value); got != want {
				t.Fatalf("indexStringContext(%q, %q) = %d, want %d", text, value, got, want)
			}
		}
	}
}

func stringsOverAlphabet(alphabet string, maximumLength int) []string {
	result := []string{""}
	level := []string{""}
	for range maximumLength {
		next := make([]string, 0, len(level)*len(alphabet))
		for _, prefix := range level {
			for _, character := range alphabet {
				next = append(next, prefix+string(character))
			}
		}
		result = append(result, next...)
		level = next
	}
	return result
}

type cancelAfterChecksContext struct {
	remaining atomic.Int64
	err       error
	done      chan struct{}
	once      sync.Once
}

func newCancelAfterChecksContext(checks int64, err error) *cancelAfterChecksContext {
	ctx := &cancelAfterChecksContext{err: err, done: make(chan struct{})}
	ctx.remaining.Store(checks)
	return ctx
}

func (*cancelAfterChecksContext) Deadline() (time.Time, bool) { return time.Time{}, false }

func (ctx *cancelAfterChecksContext) Done() <-chan struct{} { return ctx.done }

func (ctx *cancelAfterChecksContext) Err() error {
	if ctx.remaining.Add(-1) <= 0 {
		ctx.once.Do(func() { close(ctx.done) })
		return ctx.err
	}
	return nil
}

func (*cancelAfterChecksContext) Value(any) any { return nil }
