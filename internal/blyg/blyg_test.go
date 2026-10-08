package blyg

import (
	"strings"
	"testing"
	"time"
)

func TestEncodeID(t *testing.T) {
	var zero, ones [16]byte
	for i := range ones {
		ones[i] = 0xff
	}
	if got := EncodeID(zero); got != strings.Repeat("0", 26) {
		t.Errorf("zero id = %q", got)
	}
	// 128 one-bits under two zero pad bits: first char is 0b00111 = '7'.
	if got := EncodeID(ones); got != "7"+strings.Repeat("z", 25) {
		t.Errorf("ones id = %q", got)
	}
	var one [16]byte
	one[15] = 1
	if got := EncodeID(one); got != strings.Repeat("0", 25)+"1" {
		t.Errorf("one id = %q", got)
	}
	// 32 = 0b100000 → "10" at the tail.
	var thirtyTwo [16]byte
	thirtyTwo[15] = 32
	if got := EncodeID(thirtyTwo); got != strings.Repeat("0", 24)+"10" {
		t.Errorf("32 id = %q", got)
	}
}

func TestNewIDShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewID()
		if !ValidID(id) {
			t.Fatalf("invalid id %q", id)
		}
		if id[0] > '7' {
			t.Fatalf("pad bits set in %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
	for _, bad := range []string{"", "7c9wk2mhq0v3xj8tn5rzfd41b", "7c9wk2mhq0v3xj8tn5rzfd41bgi", "7C9WK2MHQ0V3XJ8TN5RZFD41BG", "7c9wk2mhq0v3xj8tn5rzfd41bu"} {
		if ValidID(bad) {
			t.Errorf("ValidID(%q) = true", bad)
		}
	}
}

func TestContentHash(t *testing.T) {
	// §5's example hash is SHA-256("test").
	if got := ContentHash("test"); got != "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" {
		t.Errorf("hash(test) = %s", got)
	}
	if got := ContentHash(""); got != "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("hash(empty) = %s", got)
	}
}

func TestNormalizeOrigin(t *testing.T) {
	cases := map[string]string{
		"HTTPS://Example.COM:443/blyg":     "https://example.com/blyg/",
		"https://example.com":              "https://example.com/",
		"http://example.com:80/a/b/?q=1#f": "http://example.com/a/b/",
		"https://example.com:8443/Chris/":  "https://example.com:8443/Chris/",
		"http://[::1]:8080/x/":             "http://[::1]:8080/x/",
	}
	for in, want := range cases {
		got, err := NormalizeOrigin(in)
		if err != nil || got != want {
			t.Errorf("NormalizeOrigin(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"ftp://example.com/", "/relative/", "https://user:pw@example.com/"} {
		if _, err := NormalizeOrigin(bad); err == nil {
			t.Errorf("NormalizeOrigin(%q) accepted", bad)
		}
	}
	if !SameOrigin("https://Example.com/blyg", "https://example.com:443/blyg/") {
		t.Error("SameOrigin should normalize")
	}
	if SameOrigin("https://example.com/a/", "https://example.com/b/") {
		t.Error("path-mounted origins on one host are different origins")
	}
}

func TestRenderMarkdown(t *testing.T) {
	page := "https://example.com/blyg/f/abc/"
	got, err := RenderMarkdown("Hi <b>there</b> [rel](../x/) ![img](/media/a.png) <script>x</script>\n\n<div>block</div>", page)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"&lt;b&gt;there&lt;/b&gt;",
		`href="https://example.com/blyg/f/x/"`,
		`src="https://example.com/media/a.png"`,
		"&lt;script&gt;",
		"&lt;div&gt;block&lt;/div&gt;",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered HTML missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "<script>") || strings.Contains(got, "<b>") || strings.Contains(got, "<div>") {
		t.Errorf("raw HTML passed through:\n%s", got)
	}
	js, _ := RenderMarkdown("[x](javascript:alert(1))", page)
	if strings.Contains(js, "javascript:") {
		t.Errorf("javascript: URL survived: %s", js)
	}
	link, _ := RenderMarkdown("see https://example.org/a", page)
	if !strings.Contains(link, `href="https://example.org/a"`) {
		t.Errorf("bare URL not linkified: %s", link)
	}
}

func TestFeedTitleAndExcerpt(t *testing.T) {
	note := "sharpened the claim"
	long := "<p>" + strings.Repeat("word ", 30) + "</p>"
	e := FeedEntry{Kind: "fragment", Note: &note, LatestHTML: long}
	got := FeedTitle(e)
	if !strings.HasPrefix(got, "sharpened the claim — word word") || !strings.HasSuffix(got, "…") {
		t.Errorf("title = %q", got)
	}
	e.Note = nil
	e.LatestHTML = "<p>short &amp; sweet</p>"
	if got := FeedTitle(e); got != "short & sweet" {
		t.Errorf("title = %q", got)
	}
	e.LatestHTML = "<p>from <strong>blygd</strong>.</p><p>Next</p>"
	if got := FeedTitle(e); got != "from blygd. Next" {
		t.Errorf("inline tags added space: %q", got)
	}
	e.Kind = "withdrawn"
	if got := FeedTitle(e); got != "withdrawn" {
		t.Errorf("withdrawal title = %q", got)
	}
}

func TestRenderFeed(t *testing.T) {
	at := time.Date(2026, 7, 18, 9, 30, 0, 0, time.UTC)
	out := string(RenderFeed(FeedChannel{Origin: "https://example.com/blyg/", Title: "T & co", Level: 2, BuiltAt: at},
		[]FeedEntry{{ID: "7c9wk2mhq0v3xj8tn5rzfd41bg", Kind: "fragment", Version: 3, At: at, Created: at, Page: "f/7c9wk2mhq0v3xj8tn5rzfd41bg/", LatestHTML: "<p>a ]]> b</p>", AuthorName: "V"}}))
	for _, want := range []string{
		`<guid isPermaLink="false">blyg:7c9wk2mhq0v3xj8tn5rzfd41bg:v3</guid>`,
		"<title>T &amp; co</title>",
		"<blyg:manifest>https://example.com/blyg/blyg.json</blyg:manifest>",
		"<pubDate>Sat, 18 Jul 2026 09:30:00 GMT</pubDate>",
		"<blyg:created>2026-07-18T09:30:00Z</blyg:created>",
		"<blyg:item>https://example.com/blyg/items/7c9wk2mhq0v3xj8tn5rzfd41bg.json</blyg:item>",
		"<![CDATA[<p>a ]]]]><![CDATA[> b</p>]]>",
		"<dc:creator>V</dc:creator>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("feed missing %q:\n%s", want, out)
		}
	}
}

func TestMarshalKeepsHTML(t *testing.T) {
	b, err := Marshal(map[string]string{"content_html": "<p>a & b</p>"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"<p>a & b</p>"`) {
		t.Errorf("HTML was escaped: %s", b)
	}
}
