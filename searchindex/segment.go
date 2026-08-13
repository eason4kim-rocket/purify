package searchindex

import (
	"fmt"
	"strings"
	"sync"

	"github.com/go-ego/gse"
)

// The Chinese dictionary is compiled into the binary, but loading it costs
// about 130 MB and a third of a second, so an English-only deployment never
// pays for it: the segmenter is built on the first Chinese page or query.
var (
	segmenterOnce sync.Once
	segmenter     gse.Segmenter
	segmenterErr  error
)

func chineseSegmenter() (*gse.Segmenter, error) {
	segmenterOnce.Do(func() {
		segmenter, segmenterErr = gse.NewEmbed("zh_s")
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
