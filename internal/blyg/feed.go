package blyg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"strconv"
	"strings"
	"time"
)

// Namespace is the permanent blyg: namespace URI (§7). The "0.1" in it is
// spelling, not a version claim.
const Namespace = "https://blygger.org/ns/0.1"

// FeedWindow is the RECOMMENDED number of feed entries (§7).
const FeedWindow = 50

// FeedChannel is the channel-level data of feed.xml.
type FeedChannel struct {
	Origin      string
	Title       string
	Description string
	Level       int
	BuiltAt     time.Time
}

// FeedEntry is one publish event. The caller has already applied §7's
// selection rules (window, withdrawn items contribute only their endcap).
type FeedEntry struct {
	ID         string
	Kind       string // this event's kind: fragment, thread or withdrawn
	Version    int
	At         time.Time
	Created    time.Time
	Page       string // origin-relative
	Note       *string
	LatestHTML string // the item's latest content_html, whatever the event (§7)
	AuthorName string // for dc:creator; empty for none
}

// FeedTitle derives the RSS <title> for a publish event (§7): the note, an
// em-dash, and an excerpt of the latest content; the excerpt alone when
// there is no note; the literal "withdrawn" for a withdrawal.
func FeedTitle(e FeedEntry) string {
	if e.Kind == "withdrawn" {
		return "withdrawn"
	}
	ex := Excerpt(e.LatestHTML, 60)
	if e.Note != nil && strings.TrimSpace(*e.Note) != "" {
		if ex == "" {
			return *e.Note
		}
		return *e.Note + " — " + ex
	}
	return ex
}

// RenderFeed renders feed.xml.
func RenderFeed(ch FeedChannel, entries []FeedEntry) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<rss version="2.0" xmlns:blyg="` + Namespace + `" xmlns:dc="http://purl.org/dc/elements/1.1/">` + "\n")
	b.WriteString("  <channel>\n")
	el(&b, 4, "title", ch.Title)
	el(&b, 4, "link", ch.Origin)
	el(&b, 4, "description", ch.Description)
	el(&b, 4, "lastBuildDate", RFC822(ch.BuiltAt))
	el(&b, 4, "blyg:level", strconv.Itoa(ch.Level))
	el(&b, 4, "blyg:manifest", ch.Origin+ManifestFile)
	for _, e := range entries {
		b.WriteString("    <item>\n")
		b.WriteString(`      <guid isPermaLink="false">blyg:` + e.ID + ":v" + strconv.Itoa(e.Version) + "</guid>\n")
		el(&b, 6, "link", ch.Origin+e.Page)
		el(&b, 6, "title", FeedTitle(e))
		desc := e.LatestHTML
		if e.Kind == "withdrawn" {
			desc = ""
		}
		b.WriteString("      <description>" + cdata(desc) + "</description>\n")
		el(&b, 6, "pubDate", RFC822(e.At))
		if e.AuthorName != "" {
			el(&b, 6, "dc:creator", e.AuthorName)
		}
		el(&b, 6, "blyg:id", e.ID)
		el(&b, 6, "blyg:kind", e.Kind)
		el(&b, 6, "blyg:version", strconv.Itoa(e.Version))
		el(&b, 6, "blyg:created", Time(e.Created))
		el(&b, 6, "blyg:item", ItemDocURL(ch.Origin, e.ID))
		b.WriteString("    </item>\n")
	}
	b.WriteString("  </channel>\n</rss>\n")
	return []byte(b.String())
}

func el(b *strings.Builder, indent int, name, text string) {
	b.WriteString(strings.Repeat(" ", indent) + "<" + name + ">")
	_ = xml.EscapeText(b, []byte(text))
	b.WriteString("</" + name + ">\n")
}

// cdata wraps s in a CDATA section, splitting any "]]>" it contains.
func cdata(s string) string {
	if s == "" {
		return ""
	}
	return "<![CDATA[" + strings.ReplaceAll(s, "]]>", "]]]]><![CDATA[>") + "]]>"
}

// HopperFeed is a public hopper's feed (§13.5, decision #86): RSS 2.0, a
// curation display in feed form, never the publisher's feed.xml.
type HopperFeed struct {
	Title   string // the hopper's name
	Page    string // the hopper's page, absolute
	Origin  string // the publisher's origin, absolute
	Site    string // the publisher's title
	BuiltAt time.Time
}

// HopperFeedEntry is one membership (§13.5 rule (d)).
type HopperFeedEntry struct {
	Identity   string // the member's identity origin (an L0 entry's feed URL)
	ID         string // the member's id
	Link       string // its page, else its item document; an L0 entry's own link
	Excerpt    string // plain text, never the held snapshot
	Own        bool
	SourceFeed string // the member origin's feed; ignored when Own
	Source     string // that origin's title
	Author     string // the member's author.name, when it has one (§5.5)
	Note       string // the display's own remark, e.g. "plain RSS"
	AddedAt    time.Time
}

// HopperGUID names one membership for the life of the membership: derived
// from the hopper and the member's origin and id, so it is stable, and
// opaque, since it is not a link (§13.5 rule (d)).
func HopperGUID(hopperPage, identity, id string) string {
	sum := sha256.Sum256([]byte(hopperPage + "\n" + identity + "\n" + id))
	return "hopper:" + hex.EncodeToString(sum[:16])
}

// RenderHopperFeed writes a hopper feed. It uses no blyg: namespace
// anywhere, own items included (§13.5 rule (b)): own items already reach
// blyg-aware readers through feed.xml. It carries every member it is given,
// in the order given, which is newest-joined first (rule (c)). Every string
// goes through XML escaping, which replaces any character XML cannot hold.
func RenderHopperFeed(ch HopperFeed, entries []HopperFeedEntry) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<rss version="2.0" xmlns:dc="http://purl.org/dc/elements/1.1/">` + "\n")
	b.WriteString("  <channel>\n")
	el(&b, 4, "title", ch.Title+" · "+ch.Site)
	el(&b, 4, "link", ch.Page)
	el(&b, 4, "description", "A list curated at "+ch.Origin+". Each item belongs to its author and links to its origin.")
	el(&b, 4, "lastBuildDate", RFC822(ch.BuiltAt))
	for _, e := range entries {
		b.WriteString("    <item>\n")
		b.WriteString(`      <guid isPermaLink="false">`)
		_ = xml.EscapeText(&b, []byte(HopperGUID(ch.Page, e.Identity, e.ID)))
		b.WriteString("</guid>\n")
		if e.Link != "" {
			el(&b, 6, "link", e.Link)
		}
		title := e.Excerpt
		if cut, ok := Truncate(title, 79); ok {
			title = strings.TrimRight(cut, " ") + "…"
		}
		el(&b, 6, "title", title)
		el(&b, 6, "description", hopperDescription(e))
		el(&b, 6, "pubDate", RFC822(e.AddedAt))
		if e.Author != "" {
			el(&b, 6, "dc:creator", e.Author)
		}
		if !e.Own && e.SourceFeed != "" {
			b.WriteString(`      <source url="`)
			_ = xml.EscapeText(&b, []byte(e.SourceFeed))
			b.WriteString(`">`)
			_ = xml.EscapeText(&b, []byte(or(e.Source, e.Identity)))
			b.WriteString("</source>\n")
		}
		b.WriteString("    </item>\n")
	}
	b.WriteString("  </channel>\n</rss>\n")
	return []byte(b.String())
}

// hopperDescription is an excerpt with a link to the origin (§13.5 rule
// (d)), as escaped HTML: the excerpt is plain text, so nothing of the
// member's own HTML is carried.
func hopperDescription(e HopperFeedEntry) string {
	h := "<p>" + escapeHTML(e.Excerpt) + "</p>"
	if e.Note != "" {
		h += "\n<p><em>" + escapeHTML(e.Note) + "</em></p>"
	}
	at := e.Link
	if at == "" {
		at = e.Identity
	}
	return h + "\n<p><a href=\"" + escapeHTML(at) + "\">" + escapeHTML(or(e.Source, e.Identity)) + "</a></p>"
}
