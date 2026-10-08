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
	ContentHTML template.HTML // our own renderer's output: raw HTML already escaped
	Pins        []int
	Withdrawn   bool
}

// PageSite is the origin-level data every page needs.
type PageSite struct {
	Origin     string
	Title      string
	Webmention bool
}

var pageFuncs = template.FuncMap{
	"time":    Time,
	"pinURL":  PinURL,
	"itemURL": ItemDocURL,
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
<p class="meta">v{{.Item.Version}} · {{time .Item.Updated}}
{{- if .Item.Pins}} · pinned:{{range .Item.Pins}} <a href="{{pinURL $.Site.Origin $.Item.ID .}}">v{{.}}</a>{{end}}{{end}}</p>
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
