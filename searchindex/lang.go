package searchindex

import (
	"strings"
	"unicode"
)

const cjkShareThreshold = 0.15

// DetectLanguage returns zh when CJK ideographs are a large enough share of
// letters, otherwise en. It does not import a language-detection library.
func DetectLanguage(text string) string {
	var letters, cjk int
	for _, r := range text {
		if !unicode.IsLetter(r) {
			continue
		}
		letters++
		if unicode.Is(unicode.Han, r) {
			cjk++
		}
	}
	if letters == 0 {
		return LangEnglish
	}
	if float64(cjk)/float64(letters) >= cjkShareThreshold {
		return LangChinese
	}
	return LangEnglish
}

// indexText rewrites text into the token stream stored in the language's FTS
// index. English passes through untouched. Chinese runs become overlapping
// character bigrams because a trigram tokenizer silently drops the
// two-character words that dominate real Chinese queries, and unicode61 alone
// treats a whole Han run as one token.
//
// The FTS tables are external-content, so this rewritten stream is only
// indexed, never stored: `pages.body` keeps the original text for callers.
func indexText(lang, text string) string {
	if lang != LangChinese {
		return text
	}
	return bigramCJK(text)
}

// bigramCJK space-separates every adjacent Han pair and leaves other runs as
// they are. A lone Han character survives as its own token.
func bigramCJK(text string) string {
	runes := []rune(text)
	var builder strings.Builder
	builder.Grow(len(text) * 2)
	write := func(token string) {
		if token == "" {
			return
		}
		if builder.Len() > 0 {
			builder.WriteByte(' ')
		}
		builder.WriteString(token)
	}
	for position := 0; position < len(runes); {
		if !unicode.Is(unicode.Han, runes[position]) {
			start := position
			for position < len(runes) && !unicode.Is(unicode.Han, runes[position]) {
				position++
			}
			write(strings.TrimSpace(string(runes[start:position])))
			continue
		}
		start := position
		for position < len(runes) && unicode.Is(unicode.Han, runes[position]) {
			position++
		}
		run := runes[start:position]
		if len(run) == 1 {
			write(string(run))
			continue
		}
		for offset := 0; offset+1 < len(run); offset++ {
			write(string(run[offset : offset+2]))
		}
	}
	return builder.String()
}
