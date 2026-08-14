package indexer

import "testing"

// TestJunkURLRejectsTrapsAssetsAndForeignLocales locks the frontier admission
// rules learned from a real crawl: foreign-locale subdomains, self-stacking
// path traps, content-addressed archive trees, and compiled-source assets
// each burned a root's crawl budget before they were filtered.
func TestJunkURLRejectsTrapsAssetsAndForeignLocales(t *testing.T) {
	junk := []string{
		"http://es.tldp.org/Manuales-LuCAS/AA_Linux_colegio-1.1/c84.htm",
		"https://bn.wikipedia.org/wiki/%E0%A7%AA",
		"https://ko.wikipedia.org/wiki/11%EC%9B%94",
		"http://vger.kernel.org/_sources/_sources/_sources/page.html",
		"http://archive.ubuntu.com/ubuntu/dists/bionic/multiverse/dep11/by-hash/SHA256/3a8d",
		"http://jigsaw.w3.org/css-validator/org/w3c/css/CssFontWidth.class",
		"http://jigsaw.w3.org/css-validator/org/w3c/css/CssFontWidth.java",
		"https://a.example/de/docs/page",
		"https://a.example/a/b/c/d/e/f/g/h/i/j/k/l/m/n/o/p/q/r",
		"https://a.example/logo.png",
		"::bad::",
	}
	for _, rawURL := range junk {
		if !junkURL(rawURL) {
			t.Errorf("junkURL(%q) = false, want junk", rawURL)
		}
	}
	legit := []string{
		"https://doc.rust-lang.org/book/ch04-01-what-is-ownership.html",
		"https://docs.python.org/zh-cn/3/tutorial/datastructures.html",
		"https://en.wikipedia.org/wiki/Ownership",
		"https://zh.wikipedia.org/wiki/%E6%89%80%E6%9C%89%E6%9D%83",
		"https://www.kernel.org/doc/html/latest/scheduler/index.html",
		"https://www.w3.org/TR/html52/",
		"https://www.rfc-editor.org/rfc/rfc9110.txt",
		"https://developer.mozilla.org/en-US/docs/Web/JavaScript",
		// A repeated segment below the trap threshold stays crawlable.
		"https://a.example/docs/docs/page.html",
	}
	for _, rawURL := range legit {
		if junkURL(rawURL) {
			t.Errorf("junkURL(%q) = true, want crawlable", rawURL)
		}
	}
}
