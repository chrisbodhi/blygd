package blyg

import (
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
	el(&b, 4, "blyg:manifest", ch.Origin+"blyg.json")
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
