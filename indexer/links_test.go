package indexer

import (
	"fmt"
	"strings"
	"testing"
)

// TestCollectLinksKeepsSameRootDocumentPages locks the BFS admission rules:
// only same-root, en/zh, query-free document URLs may grow the frontier, so a
// fetched page cannot pull the crawl off its curated sites or into asset and
// faceted-URL traps.
func TestCollectLinksKeepsSameRootDocumentPages(t *testing.T) {
	html := `<html><body>
		<a href="/tutorial/datastructures.html">relative</a>
		<a href="https://docs.a.example/reference/">subdomain, same root</a>
		<a href="https://b.example/page">other root</a>
		<a href="/tutorial/datastructures.html#list-comprehensions">fragment duplicate</a>
		<a href="/search?q=term">query string</a>
		<a href="/logo.png">asset</a>
		<a href="/styles.css">asset</a>
		<a href="/de/tutorial/">foreign locale</a>
		<a href="/zh-cn/tutorial/">chinese locale</a>
		<a href="mailto:x@a.example">mail</a>
		<a href="/current/">self after normalize is fine</a>
		<a href="https://a.example/current/">base itself</a>
	</body></html>`
	links := collectLinks(html, "https://a.example/current/", "a.example", false)

	got := make([]string, 0, len(links))
	for _, link := range links {
		if link.Root != "a.example" {
			t.Fatalf("link %q carries root %q, want a.example", link.URL, link.Root)
		}
		got = append(got, link.URL)
	}
	// Same-host links surface before cross-subdomain ones; both stay in play.
	want := []string{
		"https://a.example/tutorial/datastructures.html",
		"https://a.example/zh-cn/tutorial/",
		"https://docs.a.example/reference/",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("collectLinks() =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestCollectLinksCapsOnePage(t *testing.T) {
	var builder strings.Builder
	builder.WriteString("<html><body>")
	for index := range maxLinksPerPage + 50 {
		fmt.Fprintf(&builder, `<a href="/page-%d.html">p</a>`, index)
	}
	builder.WriteString("</body></html>")
	links := collectLinks(builder.String(), "https://a.example/", "a.example", false)
	if len(links) != maxLinksPerPage {
		t.Fatalf("collectLinks() returned %d links, want the %d cap", len(links), maxLinksPerPage)
	}
}

func TestCollectLinksSurvivesUncleanableHTML(t *testing.T) {
	if links := collectLinks("", "https://a.example/", "a.example", false); len(links) != 0 {
		t.Fatalf("empty page produced links: %#v", links)
	}
	if links := collectLinks("<a href='::bad::'>x</a><a href", "https://a.example/", "a.example", false); len(links) != 0 {
		t.Fatalf("broken page produced links: %#v", links)
	}
}
