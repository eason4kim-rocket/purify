package verify

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/use-agent/purify/evidence"
)

func TestQuoteCorpusNumberBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		quote      string
		value      string
		want       bool
		wantMethod evidence.Method
	}{
		{name: "formatted", text: "price $1,299.00 / mo", quote: "$1,299.00", value: `1299`, want: true, wantMethod: evidence.MethodExact},
		{name: "exponent", text: "price 1.299e3", quote: "1.299e3", value: `1299`, want: true, wantMethod: evidence.MethodExact},
		{name: "larger integer", text: "1000", quote: "100", value: `100`},
		{name: "negative", text: "-100", quote: "100", value: `100`},
		{name: "leading decimal", text: ".100", quote: "100", value: `100`},
		{name: "exponent suffix", text: "1e100", quote: "100", value: `100`},
		{name: "thousands suffix", text: "1,100", quote: "100", value: `100`},
		{name: "fraction suffix", text: "100.50", quote: "100", value: `100`},
		{name: "inside identifier", text: "v100", quote: "100", value: `100`},
		{name: "ambiguous", text: "100 100", quote: "100", value: `100`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scalar := mustDecodeScalar(t, test.value)
			match, ok := newQuoteCorpus(test.text).match(test.quote, scalar)
			if ok != test.want {
				t.Fatalf("match(%q in %q) ok = %v, want %v (match=%#v)", test.quote, test.text, ok, test.want, match)
			}
			if !ok {
				return
			}
			if match.method != test.wantMethod || test.text[match.start:match.end] != match.quote {
				t.Fatalf("match = %#v, resolved=%q", match, test.text[match.start:match.end])
			}
		})
	}
}

func TestQuoteCorpusBooleanBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		want       bool
		wantMethod evidence.Method
	}{
		{name: "normalized case", text: "TRUE", want: true, wantMethod: evidence.MethodNormalized},
		{name: "different value", text: "false"},
		{name: "word prefix", text: "untrue"},
		{name: "word suffix", text: "trueish"},
		{name: "underscore", text: "_true"},
		{name: "ambiguous", text: "true true"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			match, ok := newQuoteCorpus(test.text).match("true", mustDecodeScalar(t, `true`))
			if ok != test.want {
				t.Fatalf("match(true in %q) ok = %v, want %v (match=%#v)", test.text, ok, test.want, match)
			}
			if ok && (match.method != test.wantMethod || test.text[match.start:match.end] != match.quote) {
				t.Fatalf("match = %#v, resolved=%q", match, test.text[match.start:match.end])
			}
		})
	}
}

func TestQuoteCorpusStringBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		quote      string
		value      string
		want       bool
		wantMethod evidence.Method
	}{
		{name: "phrase", text: "Plan: Pro Plan.", quote: "Pro Plan", value: `"Pro Plan"`, want: true, wantMethod: evidence.MethodExact},
		{name: "normalized UTF-8 offsets", text: "前缀 PRO   PLAN 后缀", quote: "Pro Plan", value: `"Pro Plan"`, want: true, wantMethod: evidence.MethodNormalized},
		{name: "word suffix", text: "Professional", quote: "Pro", value: `"Pro"`},
		{name: "word prefix", text: "XPro", quote: "Pro", value: `"Pro"`},
		{name: "digit suffix", text: "Pro2", quote: "Pro", value: `"Pro"`},
		{name: "CJK suffix", text: "北京市", quote: "北京", value: `"北京"`},
		{name: "hyphen connector", text: "Pro-Max", quote: "Pro", value: `"Pro"`},
		{name: "plus connector", text: "C++17", quote: "C++", value: `"C++"`},
		{name: "connector chain", text: "on-call", quote: "on", value: `"on"`},
		{name: "domain connector", text: "foo.bar.com", quote: "foo.bar", value: `"foo.bar"`},
		{name: "terminal punctuation", text: "C++.", quote: "C++", value: `"C++"`, want: true, wantMethod: evidence.MethodExact},
		{name: "numeric decoration", text: "$100.00", quote: "100", value: `"100"`},
		{name: "ambiguous", text: "Pro Pro", quote: "Pro", value: `"Pro"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			match, ok := newQuoteCorpus(test.text).match(test.quote, mustDecodeScalar(t, test.value))
			if ok != test.want {
				t.Fatalf("match(%q in %q) ok = %v, want %v (match=%#v)", test.quote, test.text, ok, test.want, match)
			}
			if !ok {
				return
			}
			if match.method != test.wantMethod || test.text[match.start:match.end] != match.quote {
				t.Fatalf("match = %#v, resolved=%q", match, test.text[match.start:match.end])
			}
		})
	}
}

func TestCanonicalPagePreservesDOMTextSemantics(t *testing.T) {
	tests := []struct {
		name string
		html string
		want string
	}{
		{name: "inline number", html: `<span>10</span><span>0</span>`, want: "100"},
		{name: "inline word", html: `<b>Pro</b><i>fessional</i>`, want: "Professional"},
		{name: "source whitespace", html: `<b>Pro</b> Plan`, want: "Pro Plan"},
		{name: "block boundary", html: `<div>Pro</div><div>Plan</div>`, want: "Pro Plan"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			page, err := parseCanonicalPage(`<html><body>` + test.html + `</body></html>`)
			if err != nil {
				t.Fatalf("parseCanonicalPage() error = %v", err)
			}
			if page.text != test.want {
				t.Fatalf("canonical text = %q, want %q", page.text, test.want)
			}
		})
	}

	page, err := parseCanonicalPage(`<html><body><div id="value"><span>10</span><span>0</span></div></body></html>`)
	if err != nil {
		t.Fatalf("parseCanonicalPage() error = %v", err)
	}
	value, quote, span, state := page.scalarAtSelector("#value", scalarNumber)
	if state != selectorValue || !value.equal(mustDecodeScalar(t, `100`)) || quote != "100" || page.text[span.start:span.end] != "100" {
		t.Fatalf("selector = (%#v, %q, %#v, %d), page=%q", value, quote, span, state, page.text)
	}
}

func TestCanonicalPageExcludesNonRenderableText(t *testing.T) {
	tests := []struct {
		name      string
		hiddenDOM string
	}{
		{name: "hidden attribute", hiddenDOM: `<span hidden>secret</span>`},
		{name: "aria hidden", hiddenDOM: `<span aria-hidden="true">secret</span>`},
		{name: "display none", hiddenDOM: `<span style="display: none !IMPORTANT">secret</span>`},
		{name: "visibility hidden", hiddenDOM: `<span style="visibility:hidden">secret</span>`},
		{name: "content visibility", hiddenDOM: `<span style="content-visibility:hidden">secret</span>`},
		{name: "opacity zero", hiddenDOM: `<span style="opacity:0.00">secret</span>`},
		{name: "inert", hiddenDOM: `<span inert>secret</span>`},
		{name: "hidden input", hiddenDOM: `<input type="hidden" value="secret">`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			page, err := parseCanonicalPage(`<html><head><title>secret</title></head><body>` + test.hiddenDOM + `<span>public</span></body></html>`)
			if err != nil {
				t.Fatalf("parseCanonicalPage() error = %v", err)
			}
			if page.text != "public" {
				t.Fatalf("canonical text = %q, want public", page.text)
			}
			if _, ok := newQuoteCorpus(page.text).match("secret", mustDecodeScalar(t, `"secret"`)); ok {
				t.Fatal("non-renderable text entered quote evidence")
			}
		})
	}
}

func TestQuoteCorpusBoundsDenseAdversarialInput(t *testing.T) {
	dense := strings.Repeat("1", maximumCanonicalTextBytes)
	corpus := newQuoteCorpus(dense)
	if _, ok := corpus.match("1", mustDecodeScalar(t, `1`)); ok {
		t.Fatal("dense longer numeric token matched a one-digit claim")
	}
	if len(corpus.normalized.offsets) > maximumCanonicalTextBytes {
		t.Fatalf("offset table length = %d, want at most %d", len(corpus.normalized.offsets), maximumCanonicalTextBytes)
	}

	tooLongNumber := strings.Repeat("9", maximumNumberTokenBytes+1)
	if _, err := decodeScalar(json.RawMessage(tooLongNumber)); err == nil {
		t.Fatal("decodeScalar accepted an oversized arbitrary-precision number")
	}
}

func TestCanonicalPageRejectsTextOverLimit(t *testing.T) {
	_, err := parseCanonicalPage(`<html><body>` + strings.Repeat("x", maximumCanonicalTextBytes+1) + `</body></html>`)
	if err == nil {
		t.Fatal("parseCanonicalPage accepted text beyond the canonical limit")
	}
}

func BenchmarkQuoteCorpusDenseText(b *testing.B) {
	dense := strings.Repeat("1", maximumCanonicalTextBytes)
	scalar, err := decodeScalar(json.RawMessage(`1`))
	if err != nil {
		b.Fatalf("decodeScalar() error = %v", err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(dense)))
	for range b.N {
		corpus := newQuoteCorpus(dense)
		_, _ = corpus.match("1", scalar)
	}
}

func mustDecodeScalar(t *testing.T, raw string) scalarValue {
	t.Helper()
	value, err := decodeScalar(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("decodeScalar(%s) error = %v", raw, err)
	}
	return value
}
