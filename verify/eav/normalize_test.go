package eav

import "testing"

func TestNormalize(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"legal punctuation dropped", "Apple, Inc.", "apple inc"},
		{"apostrophe dropped", "McDonald's", "mcdonalds"},
		{"curly apostrophe dropped", "McDonald’s", "mcdonalds"},
		{"hyphen separates", "Mercedes-Benz", "mercedes benz"},
		{"period between letters dropped", "Node.js", "nodejs"},
		{"period between digits kept", "Python 3.10", "python 3.10"},
		{"measurement keeps decimal", "3.5mm", "3.5mm"},
		{"thousands separator dropped", "1,000", "1000"},
		{"plus kept", "C++", "c++"},
		{"hash kept", "C#", "c#"},
		{"trailing plus kept", "Disney+", "disney+"},
		{"ampersand separates", "AT&T", "at t"},
		{"acronym periods dropped", "U.S. Steel", "us steel"},
		{"interpunct separates", "史蒂夫·乔布斯", "史蒂夫 乔布斯"},
		{"katakana middle dot separates", "ソニー・グループ", "ソニー グループ"},
		{"whitespace collapsed", " The  Home   Depot ", "the home depot"},
		{"fullwidth folded", "Ａｐｐｌｅ", "apple"},
		{"ideographic space folded", "東京　タワー", "東京 タワー"},
		{"cjk untouched", "小米科技有限公司", "小米科技有限公司"},
		{"case folded", "iPhone 15", "iphone 15"},
		{"diacritics preserved", "L'Oréal", "loréal"},
		{"empty", "", ""},
		{"only decoration", "...", ""},
		{"only separators", " -/- ", ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := Normalize(testCase.input)
			if got != testCase.want {
				t.Fatalf("Normalize(%q) = %q, want %q", testCase.input, got, testCase.want)
			}
			if again := Normalize(got); again != got {
				t.Fatalf("Normalize is not idempotent: %q -> %q -> %q", testCase.input, got, again)
			}
		})
	}
}

func TestStripLegalSuffix(t *testing.T) {
	cases := []struct {
		name        string
		input       string
		want        string
		wantChanged bool
	}{
		{"inc", "apple inc", "apple", true},
		{"incorporated", "apple incorporated", "apple", true},
		{"stacked co ltd", "samsung electronics co ltd", "samsung electronics", true},
		{"co", "tiffany co", "tiffany", true},
		{"article and company", "the coca cola company", "coca cola", true},
		{"article alone", "the home depot", "home depot", true},
		{"identity token kept", "apple bank", "apple bank", false},
		{"identity word kept", "harvard university", "harvard university", false},
		{"unrelated untouched", "microsoft", "microsoft", false},
		{"wrapper alone kept", "inc", "inc", false},
		{"short wrapper alone kept", "公司", "公司", false},
		{"zh limited company", "小米科技有限公司", "小米科技", true},
		{"zh joint stock", "贵州茅台股份有限公司", "贵州茅台", true},
		{"zh group", "阿里巴巴集团", "阿里巴巴", true},
		{"zh keeps qualifier", "腾讯控股有限公司", "腾讯控股", true},
		{"ja suffix", "トヨタ自動車株式会社", "トヨタ自動車", true},
		{"ja prefix", "株式会社任天堂", "任天堂", true},
		{"en spelled out only strips wrapper", "alibaba group holding limited", "alibaba group holding", true},
		{"empty", "", "", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, changed := StripLegalSuffix(testCase.input)
			if got != testCase.want || changed != testCase.wantChanged {
				t.Fatalf(
					"StripLegalSuffix(%q) = (%q, %t), want (%q, %t)",
					testCase.input, got, changed, testCase.want, testCase.wantChanged,
				)
			}
		})
	}
}
