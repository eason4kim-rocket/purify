package searchindex

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"

	"github.com/go-ego/gse"
)

// The Chinese dictionary is compiled into the binary, but loading it costs
// about 130 MB and a third of a second, so an English-only deployment never
// pays for it: the segmenter is built on the first Chinese page or query.
var (
	segmenterOnce sync.Once
	segmenter     gse.Segmenter
	segmenterErr  error

	segmenterLoadedFlag atomic.Bool
)

func chineseSegmenter() (*gse.Segmenter, error) {
	segmenterOnce.Do(func() {
		segmenter, segmenterErr = gse.NewEmbed("zh_s")
		segmenterLoadedFlag.Store(segmenterErr == nil)
	})
	if segmenterErr != nil {
		return nil, fmt.Errorf("searchindex: load chinese dictionary: %w", segmenterErr)
	}
	return &segmenter, nil
}

// indexText produces the token stream written to a language's FTS table.
// English passes through. Chinese uses search mode, which additionally emits
// the parts of a compound word ("内存容量" also yields "内存" and "容量") so a
// query for either half still reaches the page.
func indexText(lang, text string) (string, error) {
	return segmentFor(lang, text, true)
}

// queryText produces the token stream a query is matched with. Chinese uses
// precise mode here: the index already carries the sub-words, so re-splitting
// the query would only add noise.
func queryText(lang, text string) (string, error) {
	return segmentFor(lang, text, false)
}

func segmentFor(lang, text string, search bool) (string, error) {
	if lang != LangChinese || strings.TrimSpace(text) == "" {
		return text, nil
	}
	// Every query probes both language tables, so English text reaches this
	// path routinely. Text with no Han characters has nothing to segment, and
	// short-circuiting it here is what keeps an English-only deployment from
	// ever loading the dictionary.
	if !containsHan(text) {
		return text, nil
	}
	seg, err := chineseSegmenter()
	if err != nil {
		// Falling back to another tokenizer here would leave the index holding
		// two incompatible token streams, so Chinese fails closed instead.
		return "", err
	}
	var tokens []string
	if search {
		tokens = seg.CutSearch(text, true)
	} else {
		tokens = seg.Cut(text, true)
	}
	kept := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if token = strings.TrimSpace(token); token != "" {
			kept = append(kept, token)
		}
	}
	return strings.Join(kept, " "), nil
}

func containsHan(text string) bool {
	for _, r := range text {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// segmenterLoaded reports whether the dictionary has been built. Reading the
// flag must not touch segmenterOnce: calling Do would consume it and stop the
// real loader from ever running.
func segmenterLoaded() bool { return segmenterLoadedFlag.Load() }
