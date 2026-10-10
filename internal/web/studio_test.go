package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestStudioIsServed(t *testing.T) {
	h := Router(&API{}, &Surface{}, discardLog())
	for path, ctype := range map[string]string{"/studio/": "text/html", "/studio/app.js": "javascript", "/studio/app.css": "text/css"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), ctype) {
			t.Errorf("%s: %d %q", path, rec.Code, rec.Header().Get("Content-Type"))
		}
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s: CSP %q", path, csp)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/studio", nil))
	if rec.Code != http.StatusMovedPermanently {
		t.Errorf("/studio: %d", rec.Code)
	}
}

// The studio never turns text into markup: imported content is someone
// else's HTML (§14), and the studio only ever reads it as text.
func TestStudioNeverWritesHTML(t *testing.T) {
	f, err := studioFiles.Open("studio/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src, _ := io.ReadAll(f)
	// An empty or truncated file passes every check below, so first make
	// sure it is the studio.
	for _, want := range []string{"function route(", "function viewEditor(", "function startStream(", "route();"} {
		if !strings.Contains(string(src), want) {
			t.Fatalf("app.js lacks %q: is it truncated?", want)
		}
	}
	if m := regexp.MustCompile(`innerHTML|outerHTML|insertAdjacentHTML|document\.write|eval\(|new Function`).Find(src); m != nil {
		t.Errorf("app.js uses %q: build nodes with textContent instead", m)
	}
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
