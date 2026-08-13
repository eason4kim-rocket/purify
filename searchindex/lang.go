package searchindex

import "unicode"

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
