package web

import (
	_ "embed"
	"log/slog"
	"net/http"
	"time"
)

// llmsTxt says what this service is and where to read more (llmstxt.org).
// It lives at the host root beside /api/ and /studio/: it describes the
// service, never one origin, and lists no users. Handles cannot contain a
// dot, so no origin can claim the path.
//
//go:embed llms.txt
var llmsTxt []byte

// Router joins the host-rooted API with the origin surfaces.
func Router(api *API, surface *Surface, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/", api.Handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := api.Pool.Ping(r.Context()); err != nil {
			http.Error(w, "database unreachable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n")) //nolint:errcheck
	})
	mux.Handle("GET /studio/", studioHandler())
	mux.Handle("GET /studio", http.RedirectHandler("/studio/", http.StatusMovedPermanently))
	mux.HandleFunc("GET /llms.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Write(llmsTxt) //nolint:errcheck
	})
	mux.Handle("/", surface)
	return logRequests(mux, log)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

// Unwrap lets http.ResponseController reach the real writer, to flush an
// event stream and lift its write deadline.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func logRequests(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		log.Info("request", "method", r.Method, "host", r.Host, "path", r.URL.Path, "status", sw.status, "ms", time.Since(start).Milliseconds())
	})
}
