package searchindex

import "testing"

func TestDetectLanguageRoutesCJKAndLatin(t *testing.T) {
	if got := DetectLanguage("The Go programming language"); got != LangEnglish {
		t.Fatalf("english = %q", got)
	}
	if got := DetectLanguage("Go 编程语言是什么"); got != LangChinese {
		t.Fatalf("mixed chinese = %q", got)
	}
	if got := DetectLanguage("你好世界搜索引擎"); got != LangChinese {
		t.Fatalf("chinese = %q", got)
	}
	if got := DetectLanguage("12345 !!!"); got != LangEnglish {
		t.Fatalf("no letters = %q", got)
	}
}
