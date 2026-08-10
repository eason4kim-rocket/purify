package eav

import (
	"strings"
	"unicode"
)

// Normalize returns the canonical comparison form of one entity surface form:
// fullwidth characters folded to their ASCII equivalents, letters lowercased,
// orthographic decoration (periods, commas, apostrophes, quotation marks)
// removed, every other symbol treated as a word separator, and whitespace
// collapsed to single spaces. '+' and '#' survive because they are semantic
// in product names (C++, C#, Disney+), and a period between two digits
// survives so version and measurement tokens stay distinct ("python 3.10"
// must not become "python 310"). Diacritics are deliberately not folded in
// v1; near-identical accented variants resolve through the gray zone.
// Normalize is idempotent: Normalize(Normalize(s)) == Normalize(s).
func Normalize(surface string) string {
	runes := []rune(surface)
	var normalized strings.Builder
	normalized.Grow(len(surface))
	pendingSpace := false
	empty := true
	for index, original := range runes {
		r := unicode.ToLower(foldWidth(original))
		keep := false
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '+' || r == '#':
			keep = true
		case r == '.':
			keep = index > 0 && index+1 < len(runes) &&
				unicode.IsDigit(runes[index-1]) && unicode.IsDigit(runes[index+1])
		case isDroppedDecoration(r):
			// Dropped entirely: "Apple, Inc." and "McDonald's" keep their
			// word shape instead of splitting on decoration.
		default:
			if !empty {
				pendingSpace = true
			}
		}
		if keep {
			if pendingSpace {
				normalized.WriteByte(' ')
				pendingSpace = false
			}
			normalized.WriteRune(r)
			empty = false
		}
	}
	return normalized.String()
}

// foldWidth mirrors the evidence package's width folding: the ideographic
// space and the fullwidth ASCII block fold to their halfwidth forms so CJK
// pages compare against ASCII surface forms.
func foldWidth(r rune) rune {
	switch {
	case r == '　':
		return ' '
	case r >= '！' && r <= '～':
		return r - 0xfee0
	default:
		return r
	}
}

func isDroppedDecoration(r rune) bool {
	switch r {
	case ',', '\'', '‘', '’', '`', '"', '“', '”', '„':
		return true
	default:
		return false
	}
}

// English legal-form tokens stripped from the end of a normalized form. The
// table is the MASTERPLAN §15 card E-1 list plus its spelled-out variants;
// widening it is a calibration change that must keep the golden gates green.
var legalSuffixTokens = map[string]struct{}{
	"inc": {}, "incorporated": {},
	"corp": {}, "corporation": {},
	"co": {}, "company": {},
	"ltd": {}, "limited": {},
	"llc": {}, "llp": {}, "plc": {},
	"ag": {}, "gmbh": {}, "sa": {}, "nv": {}, "oyj": {},
}

// CJK legal forms match as raw suffixes without token boundaries, longest
// first so 股份有限公司 wins over 有限公司 wins over 公司.
var cjkLegalSuffixes = []string{
	"股份有限公司", "有限责任公司", "有限公司", "公司", "集团",
	"株式会社", "合同会社", "有限会社",
}

// Japanese corporate forms also appear as prefixes (前株 style).
var cjkLegalPrefixes = []string{"株式会社", "合同会社", "有限会社"}

// StripLegalSuffix removes the ceremonial wrapper from one normalized entity
// form: a leading English article "the ", trailing English legal-form tokens
// (possibly several: "co ltd"), and CJK legal forms as suffixes or Japanese
// 前株-style prefixes. The input must already be in Normalize form; other
// input is not re-normalized here. The bool reports whether anything was
// removed. Stripping never produces an empty base: a form consisting only of
// wrapper tokens is returned unchanged.
func StripLegalSuffix(normalized string) (string, bool) {
	base := normalized
	changed := false
	if rest, ok := strings.CutPrefix(base, "the "); ok && rest != "" {
		base = rest
		changed = true
	}
	for {
		trimmed := false
		if space := strings.LastIndexByte(base, ' '); space >= 0 {
			if _, ok := legalSuffixTokens[base[space+1:]]; ok {
				base = strings.TrimRight(base[:space], " ")
				changed, trimmed = true, true
			}
		}
		if !trimmed {
			for _, suffix := range cjkLegalSuffixes {
				if rest, ok := strings.CutSuffix(base, suffix); ok && rest != "" {
					base = strings.TrimRight(rest, " ")
					changed, trimmed = true, true
					break
				}
			}
		}
		if !trimmed {
			for _, prefix := range cjkLegalPrefixes {
				if rest, ok := strings.CutPrefix(base, prefix); ok && rest != "" {
					base = strings.TrimLeft(rest, " ")
					changed, trimmed = true, true
					break
				}
			}
		}
		if !trimmed || base == "" {
			break
		}
	}
	if base == "" {
		return normalized, false
	}
	return base, changed
}
