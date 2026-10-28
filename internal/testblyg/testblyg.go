// Package testblyg is a fake static blyg for tests: a map of paths to bytes
// on an httptest server, with ETags and per-path request counts. Tests
// mutate it to make an origin misbehave in ways a real studio's triggers
// forbid (regressions, stealth edits, missing indexes).
package testblyg

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chrisbodhi/blygd/internal/blyg"
)

// Response overrides what one path returns.
type Response struct {
	Status   int
	Header   map[string]string
	Body     string
	Redirect string // with Status 3xx
}

// Server is one fake origin host.
type Server struct {
	*httptest.Server
	mu        sync.Mutex
	files     map[string]Response
	hits      map[string]int
	condition map[string]string
	delay     time.Duration
}

// SetDelay makes every response wait d first, as a slow origin would. A
// request cancelled meanwhile gets nothing.
func (s *Server) SetDelay(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delay = d
}

// New starts a server and registers its shutdown with t.
func New(t *testing.T) *Server {
	s := &Server{files: map[string]Response{}, hits: map[string]int{}, condition: map[string]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Set serves body at path with 200. A path may carry a query string, which
// is then part of what it matches.
func (s *Server) Set(path, body string) { s.SetResponse(path, Response{Status: 200, Body: body}) }

// SetResponse serves r at path.
func (s *Server) SetResponse(path string, r Response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[path] = r
}

// Delete makes path 404.
func (s *Server) Delete(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.files, path)
}

// Hits is how many requests path has had.
func (s *Server) Hits(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

// LastCondition is the If-None-Match the last request to path sent.
func (s *Server) LastCondition(path string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.condition[path]
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Path
	if r.URL.RawQuery != "" {
		key += "?" + r.URL.RawQuery
	}
	s.mu.Lock()
	s.hits[key]++
	s.condition[key] = r.Header.Get("If-None-Match")
	f, ok := s.files[key]
	delay := s.delay
	s.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	for k, v := range f.Header {
		w.Header().Set(k, v)
	}
	if f.Redirect != "" {
		http.Redirect(w, r, f.Redirect, f.Status)
		return
	}
	if f.Status == 200 {
		sum := sha256.Sum256([]byte(f.Body))
		etag := `"` + hex.EncodeToString(sum[:8]) + `"`
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.WriteHeader(f.Status)
	w.Write([]byte(f.Body)) //nolint:errcheck
}

// Item is one item of a Blyg, as the tests describe it.
type Item struct {
	ID        string
	Kind      string // fragment, thread, withdrawn
	Version   int
	ContentMD string
	Hash      string // empty: the correct hash
	Page      string // empty: f/{id}/
	NoPage    bool   // omit page, as a blyg with no HTML pages does (§5.8)
	Pinned    []int
	Updated   time.Time
}

// Blyg writes a whole conformant-looking surface at a mount on a Server.
type Blyg struct {
	S     *Server
	Mount string // e.g. "/blyg/"
	Title string
	Site  string // manifest site; empty: the true origin
	Items []*Item
	// NoIndex leaves items/index.json unserved (§13.2 lossy publisher).
	NoIndex bool
	// FeedWindow caps the feed's entries; 0 means every item.
	FeedWindow int
	// Feed, Index, ItemTmpl and PinTmpl relocate the surface: the
	// manifest's feed, items, item and pin values (§6.1), origin-relative
	// or absolute, query strings allowed. Empty means the default path, and
	// the manifest names item and pin only when set.
	Feed, Index, ItemTmpl, PinTmpl string
	// ManifestPath serves the manifest somewhere other than
	// Mount+"blyg.json", as a rel="blyg" link may name it (§12.1 step 4).
	// The identity stays Mount, so ManifestPath should sit directly under
	// it.
	ManifestPath string
	// Webmention is the manifest's webmention value (§15.1), if any.
	Webmention string
}

// Origin is the blyg's identity origin.
func (b *Blyg) Origin() string { return b.S.URL + b.Mount }

// Surface is where this blyg's files are, as its manifest says.
func (b *Blyg) Surface() blyg.Surface {
	return blyg.Surface{Origin: b.Origin(), Feed: b.Feed, Items: b.Index, Item: b.ItemTmpl, Pin: b.PinTmpl}
}

// Path is the server key for an absolute URL on this blyg's server: its
// path and any query.
func (b *Blyg) Path(u string) string { return strings.TrimPrefix(u, b.S.URL) }

// ManifestURL is where the manifest is served.
func (b *Blyg) ManifestURL() string {
	if b.ManifestPath != "" {
		return b.S.URL + b.ManifestPath
	}
	return b.Origin() + "blyg.json"
}

// SetPin serves a pinned version document where the surface puts it.
func (b *Blyg) SetPin(it *Item, v int, contentMD string) {
	b.S.Set(b.Path(b.Surface().PinURL(it.ID, v)), b.Pin(it, v, contentMD))
}

// Doc renders an item document.
func (b *Blyg) Doc(it *Item) string {
	page := it.Page
	if page == "" {
		page = "f/" + it.ID + "/"
	}
	hash := it.Hash
	if hash == "" {
		hash = blyg.ContentHash(it.ContentMD)
	}
	html := ""
	if it.ContentMD != "" {
		html = "<p>" + it.ContentMD + "</p>"
	}
	var cl []string
	for v := 1; v <= it.Version; v++ {
		pinned := ""
		for _, p := range it.Pinned {
			if p == v {
				pinned = `, "pinned": true`
			}
		}
		cl = append(cl, fmt.Sprintf(`{"version": %d, "at": "%s", "note": null%s}`, v, blyg.Time(it.Updated), pinned))
	}
	pageMember := fmt.Sprintf(`"page": %q, `, page)
	if it.NoPage {
		pageMember = ""
	}
	return fmt.Sprintf(`{"blyg": "0.3", "id": %q, "kind": %q, "origin": %q, %s"created": "%s", "updated": "%s",
		"version": %d, "content_md": %q, "content_html": %q, "content_hash": %q, "media": [], "x-unknown": {"kept": true},
		"changelog": [%s]}`,
		it.ID, it.Kind, b.Origin(), pageMember, blyg.Time(it.Updated), blyg.Time(it.Updated),
		it.Version, it.ContentMD, html, hash, strings.Join(cl, ", "))
}

// Pin renders a pinned version document with the given content.
func (b *Blyg) Pin(it *Item, v int, contentMD string) string {
	return fmt.Sprintf(`{"blyg": "0.3", "id": %q, "kind": "fragment", "version": %d, "at": "%s", "note": null, "pinned": true,
		"origin": %q, "content_md": %q, "content_html": %q, "content_hash": %q}`,
		it.ID, v, blyg.Time(it.Updated), b.Origin(), contentMD, "<p>"+contentMD+"</p>", blyg.ContentHash(contentMD))
}

// Publish (re)writes the manifest, feed, index and every item document.
// Pin files are the caller's to write.
func (b *Blyg) Publish() {
	site := b.Site
	if site == "" {
		site = b.Origin()
	}
	sf := b.Surface()
	keys := fmt.Sprintf(`"feed": %q, "items": %q`, or(b.Feed, blyg.DefaultFeed), or(b.Index, blyg.DefaultItems))
	if b.ItemTmpl != "" {
		keys += fmt.Sprintf(`, "item": %q`, b.ItemTmpl)
	}
	if b.PinTmpl != "" {
		keys += fmt.Sprintf(`, "pin": %q`, b.PinTmpl)
	}
	if b.Webmention != "" {
		keys += fmt.Sprintf(`, "webmention": %q`, b.Webmention)
	}
	b.S.Set(b.Path(b.ManifestURL()), fmt.Sprintf(`{"blyg": "0.3", "level": 1, "site": %q, "title": %q, %s}`, site, b.Title, keys))
	var idx, entries []string
	for _, it := range b.Items {
		b.S.Set(b.Path(sf.ItemURL(it.ID)), b.Doc(it))
		idx = append(idx, fmt.Sprintf(`{"id": %q, "kind": %q, "created": "%s", "updated": "%s", "version": %d}`,
			it.ID, it.Kind, blyg.Time(it.Updated), blyg.Time(it.Updated), it.Version))
		entries = append(entries, `<item><guid isPermaLink="false">blyg:`+it.ID+":v"+strconv.Itoa(it.Version)+`</guid>`+
			`<link>`+b.Origin()+"f/"+it.ID+`/</link><title>x</title><description></description>`+
			`<blyg:id>`+it.ID+`</blyg:id><blyg:kind>`+it.Kind+`</blyg:kind><blyg:version>`+strconv.Itoa(it.Version)+`</blyg:version></item>`)
	}
	// Newest first, like a real feed.
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	if b.FeedWindow > 0 && len(entries) > b.FeedWindow {
		entries = entries[:b.FeedWindow]
	}
	if b.NoIndex {
		b.S.Delete(b.Path(sf.IndexURL()))
	} else {
		b.S.Set(b.Path(sf.IndexURL()), `{"updated": "2026-10-01T00:00:00Z", "items": [`+strings.Join(idx, ", ")+`]}`)
	}
	b.S.Set(b.Path(sf.FeedURL()), `<?xml version="1.0" encoding="UTF-8"?><rss version="2.0" xmlns:blyg="`+blyg.Namespace+`"><channel>`+
		`<title>`+b.Title+`</title><link>`+b.Origin()+`</link><blyg:manifest>`+html.EscapeString(b.ManifestURL())+`</blyg:manifest>`+
		strings.Join(entries, "")+`</channel></rss>`)
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
