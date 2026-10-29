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
	for path, ctype := range map[string]string{"/studio/": "text/html", "/studio/app.js": "javascript", "/studio/app.css": "text/css",
		"/studio/api/": "text/html", "/studio/api/docs.js": "javascript", "/studio/api/docs.css": "text/css"} {
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
// else's HTML (§14). It is parsed inert and rebuilt from fresh nodes, so
// no foreign node or attribute (but a checked href) reaches the page.
func TestStudioNeverWritesHTML(t *testing.T) {
	// An empty or truncated file passes every check below, so first make
	// sure each is what it should be.
	for file, markers := range map[string][]string{
		"studio/app.js":      {"function route(", "function viewEditor(", "function startStream(", "route();"},
		"studio/api/docs.js": {"function render(", "function operation(", `fetch("/api/openapi.json")`},
	} {
		f, err := studioFiles.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		src, _ := io.ReadAll(f)
		for _, want := range markers {
			if !strings.Contains(string(src), want) {
				t.Fatalf("%s lacks %q: is it truncated?", file, want)
			}
		}
		if m := regexp.MustCompile(`innerHTML|outerHTML|insertAdjacentHTML|document\.write|eval\(|new Function`).Find(src); m != nil {
			t.Errorf("%s uses %q: build nodes with textContent instead", file, m)
		}
		if m := regexp.MustCompile(`cloneNode|importNode|adoptNode|\.attributes\b|getAttributeNames|getAttribute\("(?:[^h]|h[^r])`).Find(src); m != nil {
			t.Errorf("%s uses %q: rebuild foreign nodes, never copy them or their attributes", file, m)
		}
	}
}

// The API reference reads the public description and nothing else: it
// shares the studio's origin, so it must not touch the studio's token.
func TestAPIReferenceHoldsNoToken(t *testing.T) {
	f, err := studioFiles.Open("studio/api/docs.js")
	if err != nil {
		t.Fatal(err)
	}
	src, _ := io.ReadAll(f)
	if m := regexp.MustCompile(`(?i)localStorage|sessionStorage|indexedDB|document\.cookie|authorization|bearer|blygd\.token`).Find(src); m != nil {
		t.Errorf("docs.js mentions %q: the reference never reads or sends a token", m)
	}
}

// §16.6: a tool finds the write surface through an HTML rel link on the
// studio's page, and the link leads to the API description, readable
// before the tool holds a token.
func TestStudioPageLinksTheWriteSurface(t *testing.T) {
	h := Router(&API{}, &Surface{}, discardLog())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/studio/", nil))
	m := regexp.MustCompile(`<link rel="service-desc" type="application/openapi\+json" href="([^"]+)">`).FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("studio page has no service-desc link:\n%s", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", m[1], nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"openapi": "3.1.0"`) {
		t.Errorf("service-desc %s: %d %.80q", m[1], rec.Code, rec.Body.String())
	}
}

// llms.txt is served at the host root, points at the API description
// and the spec, and every host-relative link in it resolves.
func TestLLMSTxt(t *testing.T) {
	h := Router(&API{}, &Surface{}, discardLog())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/llms.txt", nil))
	body := rec.Body.String()
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") || !strings.HasPrefix(body, "# blygd\n\n> ") {
		t.Fatalf("llms.txt: %d %q %.80q", rec.Code, rec.Header().Get("Content-Type"), body)
	}
	// A client that holds a token learns where the blyg is mounted from
	// /api/me: nothing is served at the host root.
	for _, want := range []string{"(/api/openapi.json)", "(/studio/api/)", "(/studio/)", "https://github.com/blygger/blygger-spec", "`GET /api/me`"} {
		if !strings.Contains(body, want) {
			t.Errorf("llms.txt lacks %s", want)
		}
	}
	// Every host-relative link resolves on this server.
	for _, m := range regexp.MustCompile(`\]\((/[^)]*)\)`).FindAllStringSubmatch(body, -1) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", m[1], nil))
		if rec.Code != 200 {
			t.Errorf("llms.txt links %s: %d", m[1], rec.Code)
		}
	}
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
