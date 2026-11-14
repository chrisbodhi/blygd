package web

import (
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
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

// studioCall is one API request the studio's source makes. Path
// parameters are "{}". fn is the helper that makes it: api, with the
// studio's own token, or call, with a token pasted at sign-in.
type studioCall struct{ file, fn, method, path string }

// studioCalls reads every API request out of the studio's source:
// api("GET", "/items/" + id), call("POST", "/tokens", …) and
// api("POST", `/items/${id}/publish`). An interpolation, or a value
// appended after a slash, is a path parameter; the rest is a query, and
// dropped. It also returns every fetch() target as is.
func studioCalls(t *testing.T, files ...string) (calls []studioCall, fetches []studioCall) {
	t.Helper()
	call := regexp.MustCompile("\\b(api|call)\\(\"([A-Z]+)\",\\s*(?:\"([^\"]*)\"(\\s*\\+\\s*[A-Za-z_$])?|`([^`]*)`)")
	interp := regexp.MustCompile(`\$\{[^}]*\}`)
	fetch := regexp.MustCompile("\\bfetch\\(\\s*([\"'`])([^\"'`]*)")
	for _, file := range files {
		f, err := studioFiles.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		src, _ := io.ReadAll(f)
		for _, m := range call.FindAllStringSubmatch(string(src), -1) {
			path := m[3]
			if m[5] != "" {
				path = interp.ReplaceAllString(m[5], "{}")
			} else if m[4] != "" && strings.HasSuffix(path, "/") {
				path += "{}"
			}
			path, _, _ = strings.Cut(path, "?")
			calls = append(calls, studioCall{file, m[1], m[2], "/api" + path})
		}
		for _, m := range fetch.FindAllStringSubmatch(string(src), -1) {
			fetches = append(fetches, studioCall{file, "fetch", "GET", m[2]})
		}
	}
	// A regexp that stops matching would pass every check on the result.
	if len(calls) < 10 {
		t.Fatalf("found %d API calls in %v; has the call syntax changed?", len(calls), files)
	}
	return calls, fetches
}

// operationFor names the documented operation a studio call reaches, as
// its openapi.json path template, or "" when none does. A literal segment
// beats a parameter, as in the router: /api/tokens/self is not
// /api/tokens/{tid}.
func operationFor(paths map[string]map[string]json.RawMessage, method, path string) string {
	segs := strings.Split(path, "/")
	best, bestParams := "", 0
next:
	for tmpl, ops := range paths {
		if _, ok := ops[strings.ToLower(method)]; !ok {
			continue
		}
		want := strings.Split(tmpl, "/")
		if len(want) != len(segs) {
			continue
		}
		for i, w := range want {
			param := strings.HasPrefix(w, "{")
			if segs[i] == "{}" && !param || segs[i] != "{}" && segs[i] != w && !param {
				continue next
			}
		}
		if n := strings.Count(tmpl, "{"); best == "" || n < bestParams {
			best, bestParams = tmpl, n
		}
	}
	return best
}

// The studio is one client of the API, with no back door: every request
// it makes is an operation openapi.json describes, so another client can
// do anything it does (DESIGN.md, The studio).
func TestStudioCallsOnlyTheDescribedAPI(t *testing.T) {
	paths, _ := loadOpenAPI(t)
	calls, fetches := studioCalls(t, "studio/app.js", "studio/api/docs.js")
	if len(calls) < 20 {
		t.Fatalf("found %d API calls in the studio; has the call syntax changed?", len(calls))
	}
	for _, c := range calls {
		if operationFor(paths, c.method, c.path) == "" {
			t.Errorf("%s calls %s %s, which openapi.json does not describe", c.file, c.method, c.path)
		}
	}
	for _, f := range fetches {
		if !strings.HasPrefix(f.path, "/api") {
			t.Errorf("%s fetches %q: the studio reaches the server only through /api/", f.file, f.path)
		} else if f.path != "/api" && operationFor(paths, "GET", f.path) == "" {
			t.Errorf("%s fetches %s, which openapi.json does not describe", f.file, f.path)
		}
	}
}

// studioScopes reads STUDIO_SCOPES, the scopes of the token the studio
// mints for itself, out of app.js.
func studioScopes(t *testing.T) []string {
	t.Helper()
	src, err := fs.ReadFile(studioFiles, "studio/app.js")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)const STUDIO_SCOPES = \[(.*?)\];`).FindSubmatch(src)
	if m == nil {
		t.Fatal("app.js has no STUDIO_SCOPES")
	}
	var scopes []string
	for _, q := range regexp.MustCompile(`"([a-z:*]+)"`).FindAllSubmatch(m[1], -1) {
		scopes = append(scopes, string(q[1]))
	}
	if len(scopes) < 5 {
		t.Fatalf("read %d studio scopes: has the declaration changed?", len(scopes))
	}
	return scopes
}

// alsoNeeds lists what an operation checks beyond the one scope its
// security requirement names, because it can make something public.
var alsoNeeds = map[string]string{
	"PUT /api/mentions/{mid}/approval": "origin:write", // showing a response publishes (§15.5)
}

// The studio's own token holds every scope its calls need, so no button it
// shows answers 403. A studio that calls a new operation without widening
// STUDIO_SCOPES fails here.
func TestStudioHoldsWhatItCalls(t *testing.T) {
	paths, _ := loadOpenAPI(t)
	held := studioScopes(t)
	calls, _ := studioCalls(t, "studio/app.js")
	for _, c := range calls {
		if c.fn == "call" {
			continue // made with the pasted token, to mint the studio's own
		}
		tmpl := operationFor(paths, c.method, c.path)
		if tmpl == "" {
			continue // TestStudioCallsOnlyTheDescribedAPI reports it
		}
		var op struct {
			Security []map[string][]string `json:"security"`
		}
		if err := json.Unmarshal(paths[tmpl][strings.ToLower(c.method)], &op); err != nil {
			t.Fatal(err)
		}
		need := []string{}
		for _, req := range op.Security {
			need = append(need, req["bearer"]...)
		}
		if extra, ok := alsoNeeds[c.method+" "+tmpl]; ok {
			need = append(need, extra)
		}
		for _, s := range need {
			if !slices.Contains(held, s) {
				t.Errorf("app.js calls %s %s, which needs %s; STUDIO_SCOPES lacks it", c.method, tmpl, s)
			}
		}
	}
}
