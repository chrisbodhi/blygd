package blyg

import (
	"bytes"
	"html/template"
	"time"
)

// Pages are presentation, not protocol (§4) — except for the few links
// §5.8, §12.1 and §15.1 ask permalink and feed pages to carry. These
// templates are deliberately plain; styling is a later concern.

// PageItem is what a permalink page or feed page shows for one item.
type PageItem struct {
	ID          string
	Kind        string
	Version     int
	Updated     time.Time
	Page        string
	ContentHTML template.HTML // sanitized by PageHTML: threads carry other origins' bytes
	Pins        []int
	Withdrawn   bool
	ForkedFrom  *Reference // §5.6 rule 7: a fork's page SHOULD show its lineage
	// ForkedFromURL is where the lineage line links: the pinned version's
	// page where the publisher knows one is served, else its pinned file.
	ForkedFromURL string
	// Responses are approved, verified mentions (§15.5): curation by the
	// owner, never automatic.
	Responses []PageResponse
}

// PageResponse is one shown response: no words from the source.
type PageResponse struct {
	Verb   string // responded, quoted this, forked this
	Author string // the source's own author.name, as asserted (§5.5)
	Host   string
	URL    string
}

// PinnedItem is what a pinned version page shows (§8.4).
type PinnedItem struct {
	ID          string
	Kind        string
	Version     int
	At          time.Time // when this version was published
	Note        *string
	Page        string        // the live permalink, origin-relative
	ContentHTML template.HTML // sanitized by PageHTML, like every page
	Latest      int           // the item's current version
	Withdrawn   bool          // the item has since been withdrawn
}

// PageSite is the origin-level data every page needs.
type PageSite struct {
	Origin     string
	Title      string
	Webmention bool
	Blogroll   bool // §11: the feed page SHOULD link a non-empty blogroll
}

var pageFuncs = template.FuncMap{
	"time":       Time,
	"pinURL":     PinURL,
	"itemURL":    ItemDocURL,
	"pinnedPage": PinnedPage,
}

// pinnedTmpl is a pinned version page (§8.4). Gated like its JSON twin,
// linked to it, marked frozen, canonical to the live page, and absent from
// every feed and index. It advertises no Webmention endpoint: §15.3 takes
// a target only as an item's page or document.
var pinnedTmpl = template.Must(template.New("pinned").Funcs(pageFuncs).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Site.Title}} · v{{.Item.Version}} (pinned)</title>
<link rel="canonical" href="{{.Site.Origin}}{{.Item.Page}}">
<link rel="alternate" type="application/json" href="{{pinURL .Site.Origin .Item.ID .Item.Version}}">
<style>body{max-width:40rem;margin:2rem auto;padding:0 1rem;font:1rem/1.5 system-ui,sans-serif}
.meta{color:#666;font-size:.875rem}blockquote{border-left:3px solid #ccc;margin-left:0;padding-left:1rem}
.frozen{border:1px solid #ccc;border-radius:4px;padding:.5rem .75rem;background:#f6f6f6}</style>
</head>
<body>
<p class="meta"><a href="{{.Site.Origin}}">{{.Site.Title}}</a></p>
<p class="meta frozen">Pinned snapshot: version {{.Item.Version}} of this {{.Item.Kind}}, published {{time .Item.At}}{{with .Item.Note}} ({{.}}){{end}}, kept exactly as it was.
<a href="{{pinURL .Site.Origin .Item.ID .Item.Version}}">JSON</a> ·
{{- if .Item.Withdrawn}} the item has since been <a href="{{.Site.Origin}}{{.Item.Page}}">withdrawn</a>.
{{- else if eq .Item.Latest .Item.Version}} <a href="{{.Site.Origin}}{{.Item.Page}}">current page</a> (this is still the latest version).
{{- else}} <a href="{{.Site.Origin}}{{.Item.Page}}">current version</a> (v{{.Item.Latest}}).
{{- end}}</p>
<article>
{{.Item.ContentHTML}}
</article>
</body>
</html>
`))

// RenderPinnedPage renders a pinned version page (§8.4).
func RenderPinnedPage(site PageSite, item PinnedItem) ([]byte, error) {
	var buf bytes.Buffer
	err := pinnedTmpl.Execute(&buf, struct {
		Site PageSite
		Item PinnedItem
	}{site, item})
	return buf.Bytes(), err
}

var permalinkTmpl = template.Must(template.New("permalink").Funcs(pageFuncs).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Site.Title}}</title>
<link rel="alternate" type="application/json" href="{{itemURL .Site.Origin .Item.ID}}">
<link rel="alternate" type="application/rss+xml" href="{{.Site.Origin}}feed.xml">
<link rel="blyg" href="{{.Site.Origin}}">
{{- if .Site.Webmention}}
<link rel="webmention" href="{{.Site.Origin}}webmention">
{{- end}}
<style>body{max-width:40rem;margin:2rem auto;padding:0 1rem;font:1rem/1.5 system-ui,sans-serif}
.meta{color:#666;font-size:.875rem}blockquote{border-left:3px solid #ccc;margin-left:0;padding-left:1rem}</style>
</head>
<body>
<p class="meta"><a href="{{.Site.Origin}}">{{.Site.Title}}</a></p>
<article>
{{- if .Item.Withdrawn}}
<p><em>Withdrawn.</em></p>
{{- else}}
{{.Item.ContentHTML}}
{{- end}}
</article>
{{- with .Item.ForkedFrom}}
<p class="meta">Forked from <a href="{{$.Item.ForkedFromURL}}">{{.Origin}} · {{.ID}} v{{.Version}}</a></p>
{{- end}}
{{- if .Item.Responses}}
<section class="responses"><h2 class="meta">Responses</h2>
<ul class="meta">
{{- range .Item.Responses}}
<li>{{if .Author}}{{.Author}}, at {{end}}<a href="{{.URL}}" rel="nofollow ugc">{{.Host}}</a>, {{.Verb}}</li>
{{- end}}
</ul></section>
{{- end}}
<p class="meta">v{{.Item.Version}} · {{time .Item.Updated}}
{{- if .Item.Pins}} · pinned:{{range .Item.Pins}} <a href="{{$.Site.Origin}}{{pinnedPage $.Item.Page .}}">v{{.}}</a>{{end}}{{end}}</p>
</body>
</html>
`))

var feedPageTmpl = template.Must(template.New("feedpage").Funcs(pageFuncs).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Site.Title}}</title>
<link rel="alternate" type="application/rss+xml" href="{{.Site.Origin}}feed.xml">
<link rel="blyg" href="{{.Site.Origin}}">
{{- if .Site.Webmention}}
<link rel="webmention" href="{{.Site.Origin}}webmention">
{{- end}}
{{- if .Site.Blogroll}}
<link rel="blogroll" type="text/x-opml" href="{{.Site.Origin}}blogroll.opml">
{{- end}}
<style>body{max-width:40rem;margin:2rem auto;padding:0 1rem;font:1rem/1.5 system-ui,sans-serif}
.meta{color:#666;font-size:.875rem}article{border-bottom:1px solid #eee;padding:1rem 0}</style>
</head>
<body>
<h1>{{.Site.Title}}</h1>
{{- range .Items}}
<article>
{{.ContentHTML}}
<p class="meta"><a href="{{$.Site.Origin}}{{.Page}}">{{time .Updated}}</a></p>
</article>
{{- end}}
</body>
</html>
`))

// RenderPermalink renders an item's permalink page.
func RenderPermalink(site PageSite, item PageItem) ([]byte, error) {
	var buf bytes.Buffer
	err := permalinkTmpl.Execute(&buf, struct {
		Site PageSite
		Item PageItem
	}{site, item})
	return buf.Bytes(), err
}

// RenderFeedPage renders the origin's front page: live items, newest first.
func RenderFeedPage(site PageSite, items []PageItem) ([]byte, error) {
	var buf bytes.Buffer
	err := feedPageTmpl.Execute(&buf, struct {
		Site  PageSite
		Items []PageItem
	}{site, items})
	return buf.Bytes(), err
}
