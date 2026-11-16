package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"html/template"
	"net/http"
	"regexp"
	"strings"

	"github.com/chrisbodhi/blygd/internal/blyg"
	"github.com/chrisbodhi/blygd/internal/hopper"
	"github.com/chrisbodhi/blygd/internal/studio"
)

// Public hoppers (§13.5) are served live at {origin}h/{slug}/, never stored
// in surface_files: they are a reader's public face, not part of the blyg,
// so the protocol surface (and a static export of it) never holds imported
// content. Every value on the page is plain text, escaped by the template.

var hopperSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

var hopperTmpl = template.Must(template.New("hopper").Funcs(template.FuncMap{"time": blyg.Time}).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.View.Name}} · {{.Title}}</title>
<link rel="blyg" href="{{.Origin}}">
{{- if .Feed}}
<link rel="alternate" type="application/rss+xml" title="{{.View.Name}}" href="{{.Feed}}">
{{- end}}
<style>body{max-width:40rem;margin:2rem auto;padding:0 1rem;font:1rem/1.5 system-ui,sans-serif}
.meta{color:#666;font-size:.875rem}li{margin:0 0 1.25rem}</style>
</head>
<body>
<p class="meta"><a href="{{.Origin}}">{{.Title}}</a> · a list, curated here; each item belongs to its author and links to its origin.</p>
<h1>{{.View.Name}}</h1>
{{- if .View.Entries}}
<ol>
{{- range .View.Entries}}
<li><p dir="auto">{{.Excerpt}}</p>
<p class="meta">{{if .Author}}<bdi>{{.Author}}</bdi> · {{end}}{{if .Origin}}<a href="{{.Origin}}">{{if .Source}}<bdi>{{.Source}}</bdi>{{else}}{{.Origin}}{{end}}</a>{{end}}
{{- if .URL}} · <a href="{{.URL}}">read it there</a>{{end}}{{if .Note}} · {{.Note}}{{end}} · added {{time .AddedAt}}</p></li>
{{- end}}
</ol>
{{- else}}
<p class="meta">Nothing here yet.</p>
{{- end}}
</body>
</html>
`))

// serveHopper serves a public hopper, or its feed, or 404.
func (h *Surface) serveHopper(w http.ResponseWriter, r *http.Request, o *studio.Origin, rel string) {
	if slug, ok := strings.CutSuffix(strings.TrimPrefix(rel, "h/"), "/feed.xml"); ok {
		h.serveHopperFeed(w, r, o, slug)
		return
	}
	slug, ok := strings.CutSuffix(strings.TrimPrefix(rel, "h/"), "/")
	if !hopperSlug.MatchString(slug) || h.Hoppers == nil {
		http.NotFound(w, r)
		return
	}
	listed, err := h.Studio.HopperListed(r.Context(), o.ID, slug)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	v, err := h.Hoppers.Public(r.Context(), o.OwnerID, slug)
	if errors.Is(err, hopper.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if !ok {
		http.Redirect(w, r, o.BaseURL+"h/"+slug+"/", http.StatusMovedPermanently)
		return
	}
	title := o.Title
	if title == "" {
		title = o.BaseURL
	}
	var buf bytes.Buffer
	feed := ""
	if listed {
		feed = o.BaseURL + blyg.HopperFeedPath(slug)
	}
	if err := hopperTmpl.Execute(&buf, map[string]any{"View": v, "Origin": o.BaseURL, "Title": title, "Feed": feed}); err != nil {
		h.fail(w, r, err)
		return
	}
	sum := sha256.Sum256(buf.Bytes())
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	hd := w.Header()
	hd.Set("Content-Type", "text/html; charset=utf-8")
	hd.Set("ETag", etag)
	hd.Set("Cache-Control", "no-cache") // live: a withdrawal at the source shows on the next request
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Security-Policy", hopperCSP)
	hd.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	if etagMatch(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.Method == http.MethodHead {
		return
	}
	w.Write(buf.Bytes()) //nolint:errcheck
}

// serveHopperFeed serves a public hopper's feed (§13.5, decision #86),
// live like its page: only while the hopper is public and the manifest
// names it (rule (a)), and 404 otherwise, as for a hopper taken private.
// Every current member is in it, newest-joined first (rule (c)).
func (h *Surface) serveHopperFeed(w http.ResponseWriter, r *http.Request, o *studio.Origin, slug string) {
	if !hopperSlug.MatchString(slug) || h.Hoppers == nil {
		http.NotFound(w, r)
		return
	}
	listed, err := h.Studio.HopperListed(r.Context(), o.ID, slug)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if !listed {
		http.NotFound(w, r)
		return
	}
	v, err := h.Hoppers.Public(r.Context(), o.OwnerID, slug)
	if errors.Is(err, hopper.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	title := o.Title
	if title == "" {
		title = o.BaseURL
	}
	built := o.UpdatedAt
	entries := make([]blyg.HopperFeedEntry, 0, len(v.Entries))
	for _, e := range v.Entries {
		if e.AddedAt.After(built) {
			built = e.AddedAt
		}
		entries = append(entries, blyg.HopperFeedEntry{Identity: e.Identity, ID: e.ItemID, Link: e.URL, Excerpt: e.Excerpt,
			Own: e.Own, SourceFeed: e.SourceFeed, Source: e.Source, Author: e.Author, Note: e.Note, AddedAt: e.AddedAt})
	}
	body := blyg.RenderHopperFeed(blyg.HopperFeed{Title: v.Name, Page: o.BaseURL + blyg.HopperPage(slug),
		Origin: o.BaseURL, Site: title, BuiltAt: built}, entries)
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	hd := w.Header()
	hd.Set("Content-Type", "application/rss+xml; charset=utf-8")
	hd.Set("ETag", etag)
	hd.Set("Cache-Control", "no-cache") // live, like the page
	hd.Set("X-Content-Type-Options", "nosniff")
	if etagMatch(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.Method == http.MethodHead {
		return
	}
	w.Write(body) //nolint:errcheck
}
