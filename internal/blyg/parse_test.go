package blyg

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseFeedReadsOwnFeed(t *testing.T) {
	note := "sharpened"
	body := RenderFeed(FeedChannel{Origin: "https://ex.com/blyg/", Title: "T", Level: 1, BuiltAt: time.Unix(0, 0)},
		[]FeedEntry{{ID: "7c9wk2mhq0v3xj8tn5rzfd41bg", Kind: "fragment", Version: 3, At: time.Unix(1700000000, 0),
			Created: time.Unix(1700000000, 0), Page: "f/7c9wk2mhq0v3xj8tn5rzfd41bg/", Note: &note, LatestHTML: "<p>hi ]]> there</p>"}})
	f, err := ParseFeed(body, "https://ex.com/blyg/feed.xml")
	if err != nil {
		t.Fatal(err)
	}
	if f.Manifest != "https://ex.com/blyg/blyg.json" || f.Title != "T" || f.Link != "https://ex.com/blyg/" || len(f.Entries) != 1 {
		t.Fatalf("channel: %+v", f)
	}
	e := f.Entries[0]
	if e.BlygID != "7c9wk2mhq0v3xj8tn5rzfd41bg" || e.BlygKind != "fragment" || e.BlygVersion != 3 ||
		e.GUID != "blyg:7c9wk2mhq0v3xj8tn5rzfd41bg:v3" || e.Description != "<p>hi ]]> there</p>" ||
		!e.Published.Equal(time.Unix(1700000000, 0)) {
		t.Fatalf("entry: %+v", e)
	}
}

func TestParseFeedLenient(t *testing.T) {
	// An entry with a junk version is kept with BlygVersion 0, so the caller
	// can skip it alone (§13.2); unknown elements are ignored (§13.1).
	rss := `<rss version="2.0" xmlns:blyg="https://blygger.org/ns/0.1" xmlns:x="urn:x"><channel><title>A &amp; B</title>
		<x:future>?</x:future>
		<item><guid>g1</guid><link>/p/1</link><blyg:version>nope</blyg:version><pubDate>Tue, 6 Oct 2026 10:00:00 +0200</pubDate></item>
		<item><guid>g2</guid><description><![CDATA[<b>x</b>]]></description></item>
		</channel></rss>`
	f, err := ParseFeed([]byte(rss), "https://h.example/feed")
	if err != nil {
		t.Fatal(err)
	}
	if f.Title != "A & B" || len(f.Entries) != 2 || f.Entries[0].BlygVersion != 0 || f.Entries[0].Link != "https://h.example/p/1" ||
		f.Entries[0].Published.Format(time.RFC3339) != "2026-10-06T08:00:00Z" || f.Entries[1].Description != "<b>x</b>" {
		t.Fatalf("%+v", f)
	}

	atom := `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"><title>At</title>
		<link rel="self" href="https://a.example/atom"/><link href="https://a.example/"/>
		<entry><id>tag:a,1</id><title>One</title><link href="https://a.example/1"/><updated>2026-10-01T00:00:00Z</updated><summary>s</summary></entry></feed>`
	f, err = ParseFeed([]byte(atom), "https://a.example/atom")
	if err != nil {
		t.Fatal(err)
	}
	if f.Title != "At" || f.Link != "https://a.example/" || len(f.Entries) != 1 || f.Entries[0].GUID != "tag:a,1" ||
		f.Entries[0].Link != "https://a.example/1" || f.Entries[0].Description != "s" || f.Entries[0].Published.IsZero() {
		t.Fatalf("atom: %+v", f)
	}

	for _, junk := range []string{`{"blyg": "0.3"}`, `<html><body>hi</body></html>`, `<feed><entry/></feed>`, ``} {
		if _, err := ParseFeed([]byte(junk), "https://x/"); err == nil {
			t.Errorf("parsed as a feed: %q", junk)
		}
	}
}

func TestParseManifest(t *testing.T) {
	if _, ok := ParseManifest([]byte(`{"blyg": "0.3", "site": "https://x/"}`)); !ok {
		t.Error("manifest refused")
	}
	for _, s := range []string{`{"site": "x"}`, `[]`, `null`, `<rss/>`, `{"blyg"`} {
		if _, ok := ParseManifest([]byte(s)); ok {
			t.Errorf("accepted %q", s)
		}
	}
}

func TestFindHTMLLinks(t *testing.T) {
	page := `<html><head><link rel="stylesheet" href="s.css">
		<link rel="alternate" type="application/rss+xml" href="/feed.xml">
		<link rel="Blyg me" href="../blyg/"><link rel="blyg" href="/second/">
		</head><body><link rel="blyg" href="/in-body/"></body></html>`
	l := FindHTMLLinks([]byte(page), "https://h.example/a/b")
	if l.Blyg != "https://h.example/blyg/" || len(l.Feeds) != 1 || l.Feeds[0] != "https://h.example/feed.xml" {
		t.Fatalf("%+v", l)
	}
}

func TestL0ID(t *testing.T) {
	a := L0ID("guid-1", "https://x/1")
	if a == "" || ValidID(a) || !strings.HasPrefix(a, "l0:") || a != L0ID("guid-1", "https://other/") {
		t.Errorf("L0ID by guid: %q", a)
	}
	if L0ID("", "https://x/1") == a || L0ID("", "") != "" {
		t.Error("L0ID fallback to link")
	}
}

func TestRenderOPML(t *testing.T) {
	out := string(RenderOPML(`A "B" & C`, []BlogrollEntry{{Text: `<x>`, XMLURL: "https://e/feed.xml?a=1&b=2", HTMLURL: "https://e/"}}))
	for _, want := range []string{`<opml version="2.0">`, `<title>A &#34;B&#34; &amp; C</title>`, `text="&lt;x&gt;"`,
		`xmlUrl="https://e/feed.xml?a=1&amp;b=2"`, `type="rss"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

// OPML from the wild: folders, entries with only a page, repeats, title
// without text, odd case, and a truncated tail. A feed is not OPML.
func TestParseOPML(t *testing.T) {
	got, err := ParseOPML([]byte(`<?xml version="1.0"?>
<opml version="1.0"><head><title>mine</title></head><body>
  <outline text="Friends">
    <outline type="rss" text="Alice" xmlUrl=" https://a.example/feed.xml " htmlUrl="https://a.example/"/>
    <outline title="Bob" htmlURL="https://b.example/"/>
  </outline>
  <outline type="rss" text="Alice again" xmlUrl="https://a.example/feed.xml"/>
  <OUTLINE text="Carol" XMLURL="https://c.example/rss"/>
  <outline text="Dan" xmlUrl="https://d.example/feed`))
	if err != nil {
		t.Fatal(err)
	}
	want := []BlogrollEntry{
		{Text: "Alice", XMLURL: "https://a.example/feed.xml", HTMLURL: "https://a.example/"},
		{Text: "Bob", HTMLURL: "https://b.example/"},
		{Text: "Carol", XMLURL: "https://c.example/rss"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if _, err := ParseOPML([]byte(`<rss version="2.0"><channel><title>x</title></channel></rss>`)); err == nil {
		t.Error("a feed parsed as OPML")
	}
	if _, err := ParseOPML([]byte(`not xml at all`)); err == nil {
		t.Error("text parsed as OPML")
	}
}
