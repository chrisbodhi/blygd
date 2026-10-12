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
<style>body{max-width:40rem;margin:2rem auto;padding:0 1rem;font:1rem/1.5 system-ui,sans-serif}
.meta{color:#666;font-size:.875rem}li{margin:0 0 1.25rem}</style>
</head>
<body>
<p class="meta"><a href="{{.Origin}}">{{.Title}}</a> · a list, curated here; each item belongs to its author and links to its origin.</p>
<h1>{{.View.Name}}</h1>
{{- if .View.Entries}}
<ol>
{{- range .View.Entries}}
<li><p>{{.Excerpt}}</p>
<p class="meta">{{if .Author}}{{.Author}} · {{end}}{{if .Origin}}<a href="{{.Origin}}">{{if .Source}}{{.Source}}{{else}}{{.Origin}}{{end}}</a>{{end}}
{{- if .URL}} · <a href="{{.URL}}">read it there</a>{{end}}{{if .Note}} · {{.Note}}{{end}} · added {{time .AddedAt}}</p></li>
{{- end}}
</ol>
{{- else}}
<p class="meta">Nothing here yet.</p>
{{- end}}
</body>
</html>
`))

// serveHopper serves a public hopper, or 404.
func (h *Surface) serveHopper(w http.ResponseWriter, r *http.Request, o *studio.Origin, rel string) {
	slug, ok := strings.CutSuffix(strings.TrimPrefix(rel, "h/"), "/")
	if !hopperSlug.MatchString(slug) || h.Hoppers == nil {
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
	if !ok {
		http.Redirect(w, r, o.BaseURL+"h/"+slug+"/", http.StatusMovedPermanently)
		return
	}
	title := o.Title
	if title == "" {
		title = o.BaseURL
	}
	var buf bytes.Buffer
	if err := hopperTmpl.Execute(&buf, map[string]any{"View": v, "Origin": o.BaseURL, "Title": title}); err != nil {
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
