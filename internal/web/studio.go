package web

import (
	"embed"
	"io/fs"
	"net/http"
)

// The studio is a plain client for phones and laptops, served by blygd at
// the host-rooted /studio/ (a reserved handle, so no origin collides). It is
// static: it talks to /api/ with a pasted token and never renders imported
// HTML (it reads text only).
//
//go:embed studio
var studioFiles embed.FS

func studioHandler() http.Handler {
	sub, _ := fs.Sub(studioFiles, "studio")
	files := http.StripPrefix("/studio/", http.FileServerFS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// Scripts and styles from here only; it calls only this host's
		// API; nobody may frame it.
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}
