package web

import (
	_ "embed"
	"log/slog"
	"net/http"
	"strings"
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
	return logRequests(publicBudget(mux, api.Limits), api.Limits, log)
}

// publicBudget charges requests outside /api/ to their address: origin
// files, hopper pages, the studio's files and webmention posts. /api/
// charges tokens instead (API.guard), and /healthz is free for probes.
func publicBudget(next http.Handler, l *Limits) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if ok, wait := l.public(r); !ok {
			retryAfter(w, wait)
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
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

// logRequests logs every request with its client address, and the token
// that made it when there was one, so 401s, 403s and 429s can be counted
// per token and per address.
func logRequests(next http.Handler, l *Limits, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		r, ri := withReqInfo(r)
		next.ServeHTTP(sw, r)
		attrs := []any{"method", r.Method, "host", r.Host, "path", r.URL.Path, "status", sw.status,
			"ms", time.Since(start).Milliseconds(), "addr", l.addr(r)}
		if ri.tokenID != 0 {
			attrs = append(attrs, "token", ri.tokenID)
		}
		log.Info("request", attrs...)
	})
}
