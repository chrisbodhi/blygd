// Package web is blygd's HTTP layer: the public surface of every origin
// (static files plus the one dynamic endpoint, §4) and the private write API.
package web

import (
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"

	"github.com/chrisbodhi/blygd/internal/blyg"
	"github.com/chrisbodhi/blygd/internal/hopper"
	"github.com/chrisbodhi/blygd/internal/mention"
	"github.com/chrisbodhi/blygd/internal/studio"
)

// Policies for public HTML. Pages share a host with /studio/, whose token
// sits in localStorage, and carry other origins' bytes in baked quotes.
// bluemonday sanitizes them (blyg.PageHTML); these are the second wall: no
// script runs at all, and the sandbox gives the page an opaque origin, so
// even a script that got past the first could not reach the studio's
// storage. Pages need only their inline <style>, and images: the origin's
// media and whatever an author embeds over https. A hopper page shows
// strangers' excerpts, so it loads no images at all.
const (
	pageCSP   = "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' https:; base-uri 'none'; form-action 'none'; sandbox allow-popups allow-popups-to-escape-sandbox"
	hopperCSP = "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; sandbox allow-popups allow-popups-to-escape-sandbox"
)

// Surface serves every own origin's rendered files. It never renders: it
// serves the bytes the last publish committed, which is what makes the
// origin indistinguishable from a static host (§4).
type Surface struct {
	Studio   *studio.Studio
	Receiver *mention.Receiver
	Hoppers  *hopper.Store // public hoppers (§13.5), served live
	// Scheme is the public scheme. TLS normally terminates in front of
	// blygd, so the request itself cannot say.
	Scheme     string
	TrustProxy bool
	Log        *slog.Logger
}

func (h *Surface) requestURL(r *http.Request) (string, bool) {
	scheme := h.Scheme
	if h.TrustProxy {
		if p := r.Header.Get("X-Forwarded-Proto"); p == "http" || p == "https" {
			scheme = p
		}
	}
	u, err := blyg.NormalizeURL(scheme + "://" + r.Host + r.URL.EscapedPath())
	return u, err == nil
}

func (h *Surface) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u, ok := h.requestURL(r)
	if !ok {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	o, err := h.Studio.OriginForURL(ctx, u)
	if studio.IsCode(err, "not_found") {
		// An origin asked for without its trailing slash.
		if !strings.HasSuffix(u, "/") {
			if o2, err := h.Studio.OriginForURL(ctx, u+"/"); err == nil && o2.BaseURL == u+"/" {
				http.Redirect(w, r, o2.BaseURL, http.StatusMovedPermanently)
				return
			}
		}
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	rel, _ := studio.TrimOrigin(o.BaseURL, u)

	if rel == "webmention" {
		h.Receiver.ServeOrigin(w, r, o)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	case http.MethodOptions:
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD")
		w.Header().Set("Access-Control-Allow-Headers", "If-None-Match, If-Modified-Since")
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if strings.HasPrefix(rel, "media/") {
		h.serveMedia(w, r, o, rel)
		return
	}
	if strings.HasPrefix(rel, "h/") {
		h.serveHopper(w, r, o, rel)
		return
	}

	f, err := h.Studio.GetFile(ctx, o.ID, rel)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if f == nil {
		if rel != "" && !strings.HasSuffix(rel, "/") {
			if f2, err := h.Studio.GetFile(ctx, o.ID, rel+"/"); err == nil && f2 != nil {
				http.Redirect(w, r, o.BaseURL+rel+"/", http.StatusMovedPermanently)
				return
			}
		}
		// Drafts, unknown ids and unpinned versions all 404 (§4, §8).
		http.NotFound(w, r)
		return
	}

	hd := w.Header()
	hd.Set("Content-Type", f.ContentType)
	hd.Set("ETag", f.ETag)
	hd.Set("X-Content-Type-Options", "nosniff")
	if strings.HasPrefix(f.ContentType, "text/html") {
		hd.Set("Content-Security-Policy", pageCSP)
		hd.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	}
	if f.Immutable {
		hd.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		hd.Set("Cache-Control", "no-cache") // revalidate: conditional GETs are cheap (§12.3)
	}
	if !strings.HasPrefix(f.ContentType, "text/html") {
		// §4: public JSON and XML SHOULD carry permissive CORS.
		hd.Set("Access-Control-Allow-Origin", "*")
		hd.Set("Access-Control-Expose-Headers", "ETag")
	} else if o.AcceptMentions && !blyg.IsPinnedPage(rel) {
		// §15.1: announce the endpoint in a Link header too.
		hd.Set("Link", `<`+o.BaseURL+`webmention>; rel="webmention"`)
	}
	if etagMatch(r.Header.Get("If-None-Match"), f.ETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.Method == http.MethodHead {
		return
	}
	w.Write(f.Body) //nolint:errcheck
}

// serveMedia serves a media object once a published version references it
// (§5.4). Its bytes never change, so it is cached as immutable. An upload
// only drafts use 404s like the draft (§4).
func (h *Surface) serveMedia(w http.ResponseWriter, r *http.Request, o *studio.Origin, rel string) {
	mime, body, sum, err := h.Studio.PublicMedia(r.Context(), o.ID, rel)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if body == nil {
		http.NotFound(w, r)
		return
	}
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	hd := w.Header()
	hd.Set("Content-Type", mime)
	hd.Set("ETag", etag)
	hd.Set("Cache-Control", "public, max-age=31536000, immutable")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	hd.Set("Access-Control-Allow-Origin", "*")
	hd.Set("Access-Control-Expose-Headers", "ETag")
	if etagMatch(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.Method == http.MethodHead {
		return
	}
	w.Write(body) //nolint:errcheck
}

func etagMatch(header, etag string) bool {
	if header == "" {
		return false
	}
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimSpace(t)
		if t == "*" || strings.TrimPrefix(t, "W/") == etag {
			return true
		}
	}
	return false
}

func (h *Surface) fail(w http.ResponseWriter, r *http.Request, err error) {
	h.Log.Error("surface", "path", r.URL.Path, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
