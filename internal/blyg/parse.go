package blyg

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

// The import side reads other publishers' surfaces. Everything here is
// lenient in the §13.1 rule 4 sense: unknown members and elements are
// ignored, and one bad entry never rejects the document containing it.

// ParseManifest reports whether body is a manifest: a JSON object with a
// "blyg" version key (§12.1 step 2). Parse success is the test, never
// Content-Type. The returned map is the manifest as served.
func ParseManifest(body []byte) (map[string]json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil || m == nil {
		return nil, false
	}
	if _, ok := m["blyg"]; !ok {
		return nil, false
	}
	return m, true
}

// ManifestString returns a string member of a parsed manifest, or "".
func ManifestString(m map[string]json.RawMessage, key string) string {
	var s string
	if raw, ok := m[key]; ok && json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// ParsedFeed is an RSS 2.0 or Atom feed, reduced to what a reader uses.
type ParsedFeed struct {
	Title    string
	Link     string // channel link: the site, for a blogroll's htmlUrl (§11)
	Manifest string // <blyg:manifest>, the upgrade hook (§7, §12.1 step 3)
	Entries  []ParsedEntry
}

// ParsedEntry is one feed entry. The blyg fields are zero when absent.
type ParsedEntry struct {
	GUID        string
	Link        string
	Title       string
	Description string // HTML as served; never trusted (§14)
	Published   time.Time
	BlygID      string
	BlygKind    string
	BlygVersion int
}

// ErrNotFeed means the bytes are not RSS or Atom.
var ErrNotFeed = errors.New("not an RSS or Atom feed")

type rssDoc struct {
	XMLName xml.Name `xml:"rss"`
	Channel struct {
		Title    string `xml:"title"`
		Link     string `xml:"link"`
		Manifest string `xml:"https://blygger.org/ns/0.1 manifest"`
		Items    []struct {
			GUID        string `xml:"guid"`
			Link        string `xml:"link"`
			Title       string `xml:"title"`
			Description string `xml:"description"`
			Encoded     string `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
			PubDate     string `xml:"pubDate"`
			ID          string `xml:"https://blygger.org/ns/0.1 id"`
			Kind        string `xml:"https://blygger.org/ns/0.1 kind"`
			Version     string `xml:"https://blygger.org/ns/0.1 version"`
		} `xml:"item"`
	} `xml:"channel"`
}

type atomLink struct {
	Rel  string `xml:"rel,attr"`
	Href string `xml:"href,attr"`
}

type atomDoc struct {
	XMLName  xml.Name   `xml:"http://www.w3.org/2005/Atom feed"`
	Title    string     `xml:"http://www.w3.org/2005/Atom title"`
	Links    []atomLink `xml:"http://www.w3.org/2005/Atom link"`
	Manifest string     `xml:"https://blygger.org/ns/0.1 manifest"`
	Entries  []struct {
		ID        string     `xml:"http://www.w3.org/2005/Atom id"`
		Title     string     `xml:"http://www.w3.org/2005/Atom title"`
		Links     []atomLink `xml:"http://www.w3.org/2005/Atom link"`
		Summary   string     `xml:"http://www.w3.org/2005/Atom summary"`
		Content   string     `xml:"http://www.w3.org/2005/Atom content"`
		Updated   string     `xml:"http://www.w3.org/2005/Atom updated"`
		Published string     `xml:"http://www.w3.org/2005/Atom published"`
	} `xml:"http://www.w3.org/2005/Atom entry"`
}

func alternate(links []atomLink) string {
	for _, l := range links {
		if l.Rel == "" || l.Rel == "alternate" {
			return strings.TrimSpace(l.Href)
		}
	}
	return ""
}

func newXMLDecoder(body []byte) *xml.Decoder {
	d := xml.NewDecoder(bytes.NewReader(body))
	d.Strict = false
	d.Entity = xml.HTMLEntity
	d.CharsetReader = charset.NewReaderLabel
	return d
}

// ParseFeed parses RSS 2.0 or Atom. Relative entry links are resolved
// against base.
func ParseFeed(body []byte, base string) (*ParsedFeed, error) {
	root, err := rootElement(body)
	if err != nil {
		return nil, ErrNotFeed
	}
	switch {
	case root.Local == "rss":
		var d rssDoc
		if err := newXMLDecoder(body).Decode(&d); err != nil {
			return nil, ErrNotFeed
		}
		f := &ParsedFeed{
			Title:    strings.TrimSpace(d.Channel.Title),
			Link:     Resolve(base, strings.TrimSpace(d.Channel.Link)),
			Manifest: strings.TrimSpace(d.Channel.Manifest),
		}
		for _, it := range d.Channel.Items {
			e := ParsedEntry{
				GUID:        strings.TrimSpace(it.GUID),
				Title:       strings.TrimSpace(it.Title),
				Description: it.Description,
				BlygID:      strings.TrimSpace(it.ID),
				BlygKind:    strings.TrimSpace(it.Kind),
			}
			if e.Description == "" {
				e.Description = it.Encoded
			}
			if l := strings.TrimSpace(it.Link); l != "" {
				e.Link = Resolve(base, l)
			}
			e.Published = parseFeedTime(it.PubDate)
			if n, err := strconv.Atoi(strings.TrimSpace(it.Version)); err == nil && n > 0 {
				e.BlygVersion = n
			}
			f.Entries = append(f.Entries, e)
		}
		return f, nil
	case root.Local == "feed" && root.Space == "http://www.w3.org/2005/Atom":
		var d atomDoc
		if err := newXMLDecoder(body).Decode(&d); err != nil {
			return nil, ErrNotFeed
		}
		f := &ParsedFeed{
			Title:    strings.TrimSpace(d.Title),
			Link:     Resolve(base, alternate(d.Links)),
			Manifest: strings.TrimSpace(d.Manifest),
		}
		for _, it := range d.Entries {
			e := ParsedEntry{GUID: strings.TrimSpace(it.ID), Title: strings.TrimSpace(it.Title), Description: it.Summary}
			if e.Description == "" {
				e.Description = it.Content
			}
			if l := alternate(it.Links); l != "" {
				e.Link = Resolve(base, l)
			}
			e.Published = parseFeedTime(it.Published)
			if e.Published.IsZero() {
				e.Published = parseFeedTime(it.Updated)
			}
			f.Entries = append(f.Entries, e)
		}
		return f, nil
	}
	return nil, ErrNotFeed
}

// rootElement returns the name of the document element, skipping the
// prolog. It does not decode the rest.
func rootElement(body []byte) (xml.Name, error) {
	d := newXMLDecoder(body)
	for {
		tok, err := d.Token()
		if err != nil {
			return xml.Name{}, err
		}
		if se, ok := tok.(xml.StartElement); ok {
			return se.Name, nil
		}
	}
}

var feedTimeLayouts = []string{
	time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822, time.RFC3339, time.RFC3339Nano,
	"Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST", "2 Jan 2006 15:04:05 -0700",
	"Mon, 02 Jan 2006 15:04 -0700", "2006-01-02T15:04:05", "2006-01-02",
}

func parseFeedTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, l := range feedTimeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// HTMLLinks is what resolution reads from an HTML page (§12.1 steps 4, 6).
type HTMLLinks struct {
	Blyg  string   // first <link rel="blyg" href>, resolved: an origin base URL
	Feeds []string // rel="alternate" RSS or Atom links, resolved, in order
}

// FindHTMLLinks scans the <link> elements of an HTML page.
func FindHTMLLinks(body []byte, base string) HTMLLinks {
	var out HTMLLinks
	z := html.NewTokenizer(bytes.NewReader(body))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return out
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			if string(name) == "body" {
				return out // <link> belongs in <head>; stop at the content
			}
			if string(name) != "link" || !hasAttr {
				continue
			}
			var rel, href, typ string
			hasHref := false
			for {
				k, v, more := z.TagAttr()
				switch string(k) {
				case "rel":
					rel = strings.ToLower(string(v))
				case "href":
					href, hasHref = strings.TrimSpace(string(v)), true
				case "type":
					typ = strings.ToLower(strings.TrimSpace(string(v)))
				}
				if !more {
					break
				}
			}
			if !hasHref || href == "" {
				continue
			}
			rels := strings.Fields(rel)
			has := func(t string) bool {
				for _, r := range rels {
					if r == t {
						return true
					}
				}
				return false
			}
			if has("blyg") && out.Blyg == "" {
				out.Blyg = Resolve(base, href)
			}
			if has("alternate") && (typ == "application/rss+xml" || typ == "application/atom+xml") {
				out.Feeds = append(out.Feeds, Resolve(base, href))
			}
		}
	}
}

// L0ID derives the synthetic local id of a legacy RSS entry (§13.6): from
// its GUID, or its link absent one. The "l0:" prefix and hex alphabet make
// it impossible to mistake for a blyg id, which §13.6 requires.
func L0ID(guid, link string) string {
	key := strings.TrimSpace(guid)
	if key == "" {
		key = strings.TrimSpace(link)
	}
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return "l0:" + hex.EncodeToString(sum[:16])
}

// BlogrollEntry is one <outline> of blogroll.opml (§11).
type BlogrollEntry struct {
	Text    string
	XMLURL  string
	HTMLURL string
}

// RenderOPML renders blogroll.opml: standard OPML 2.0, no extensions (§11).
func RenderOPML(title string, entries []BlogrollEntry) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<opml version="2.0">` + "\n")
	b.WriteString("  <head><title>")
	_ = xml.EscapeText(&b, []byte(title))
	b.WriteString("</title></head>\n  <body>\n")
	for _, e := range entries {
		b.WriteString(`    <outline type="rss" text="` + attr(e.Text) + `" title="` + attr(e.Text) +
			`" xmlUrl="` + attr(e.XMLURL) + `" htmlUrl="` + attr(e.HTMLURL) + `" />` + "\n")
	}
	b.WriteString("  </body>\n</opml>\n")
	return []byte(b.String())
}

func attr(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
