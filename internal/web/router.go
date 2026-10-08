package web

import (
	"log/slog"
	"net/http"
	"time"
)

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
	mux.Handle("/", surface)
	return logRequests(mux, log)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

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
