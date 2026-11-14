package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chrisbodhi/blygd/internal/limit"
	"github.com/chrisbodhi/blygd/internal/mention"
)

// preflight sends what a browser sends before a cross-origin request with a
// token: OPTIONS, an Origin, and the method and headers it means to use. It
// goes around e.do, which holds every /api/ response to openapi.json; a
// preflight is the browser's, not an operation.
func preflight(t *testing.T, e *env, path string, hdr ...string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("OPTIONS", e.srv.URL+"/api"+path, nil)
	req.Header.Set("Origin", "https://client.example")
	req.Header.Set("Access-Control-Request-Method", "PATCH")
	req.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	return r
}

// readable fails unless a browser on another origin may read the response,
// and the headers a client needs to behave, and never with credentials.
func readable(t *testing.T, what string, h http.Header) {
	t.Helper()
	if h.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("%s: Access-Control-Allow-Origin %q", what, h.Get("Access-Control-Allow-Origin"))
	}
	for _, want := range []string{"Retry-After", "WWW-Authenticate"} {
		if !strings.Contains(h.Get("Access-Control-Expose-Headers"), want) {
			t.Errorf("%s: %s not exposed: %q", what, want, h.Get("Access-Control-Expose-Headers"))
		}
	}
	if h.Get("Access-Control-Allow-Credentials") != "" {
		t.Errorf("%s: carries Access-Control-Allow-Credentials", what)
	}
}

// A client on another origin can use the API as the studio does from this
// one: the token is the only credential, so every origin gets the same
// answer, and none gets credentials.
func TestCrossOriginClients(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice := e.user("alice")

	// The preflight needs no token and reaches no handler.
	for _, path := range []string{"/items", "/items/0000000000000000000000000/publish", "/events", "/no/such/route"} {
		r := preflight(t, e, path)
		if r.StatusCode != 204 {
			t.Fatalf("preflight %s: %d", path, r.StatusCode)
		}
		readable(t, "preflight "+path, r.Header)
		for k, want := range map[string]string{
			"Access-Control-Allow-Methods": "PATCH",
			"Access-Control-Allow-Headers": "Authorization",
			"Access-Control-Max-Age":       "86400",
		} {
			if !strings.Contains(r.Header.Get(k), want) {
				t.Errorf("preflight %s: %s %q lacks %q", path, k, r.Header.Get(k), want)
			}
		}
		if !strings.Contains(r.Header.Get("Access-Control-Allow-Headers"), "Content-Type") {
			t.Errorf("preflight %s: Content-Type not allowed", path)
		}
	}

	// Every answer is readable: success, the description, and each kind of
	// refusal, so a client can tell them apart.
	cross := func(method, path, token string, body any) resp {
		return e.do(method, e.srv.URL+"/api"+path, token, body, "Origin", "https://client.example")
	}
	for what, r := range map[string]resp{
		"200":         cross("GET", "/me", alice.token, nil),
		"description": cross("GET", "/openapi.json", "", nil),
		"201":         cross("POST", "/items", alice.token, map[string]any{"content_md": "From elsewhere."}),
		"401":         cross("GET", "/me", "", nil),
		"403":         cross("GET", "/tokens", alice.mintScoped(t, "items:read"), nil),
		"404":         cross("GET", "/items/0000000000000000000000000", alice.token, nil),
	} {
		readable(t, what, r.header)
	}
	if r := cross("GET", "/me", "", nil); r.status != 401 || r.header.Get("WWW-Authenticate") == "" {
		t.Fatalf("401 without WWW-Authenticate: %d %v", r.status, r.header)
	}

	// The event stream too: browsers read it with fetch(), not EventSource.
	req, _ := http.NewRequest("GET", e.srv.URL+"/api/events", nil)
	req.Header.Set("Authorization", "Bearer "+alice.token)
	req.Header.Set("Origin", "https://client.example")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	readable(t, "events", r.Header)

	// The origin surface keeps its own, narrower CORS (GET and HEAD, §4),
	// and the studio stays same-origin only.
	s := e.do("GET", e.srv.URL+"/studio/", "", nil, "Origin", "https://client.example")
	if s.header.Get("Access-Control-Allow-Origin") != "" || !strings.Contains(s.header.Get("Content-Security-Policy"), "connect-src 'self'") {
		t.Errorf("studio: ACAO %q, CSP %q", s.header.Get("Access-Control-Allow-Origin"), s.header.Get("Content-Security-Policy"))
	}
}

func (u *user) mintScoped(t *testing.T, scopes ...string) string {
	t.Helper()
	v, _ := u.mint(t, map[string]any{"name": "narrow", "scopes": scopes})
	return v.token
}

// A 429 is readable cross-origin, with its Retry-After, so a browser client
// can back off; and preflights are charged to the address's public budget,
// since they carry no token to charge.
func TestCrossOriginBudgets(t *testing.T) {
	saved := SurfaceBudget
	SurfaceBudget = limit.Rate{PerMinute: 60, Burst: 3}
	t.Cleanup(func() { SurfaceBudget = saved })
	e := newEnv(t, mention.DefaultRateLimits)
	alice := e.user("alice")
	now := time.Unix(1_000_000, 0)
	e.withLimits(&now)

	for i := 0; i < 3; i++ {
		if r := preflight(t, e, "/items", "X-Forwarded-For", "203.0.113.40"); r.StatusCode != 204 {
			t.Fatalf("preflight %d inside the budget: %d", i, r.StatusCode)
		}
	}
	r := preflight(t, e, "/items", "X-Forwarded-For", "203.0.113.40")
	if r.StatusCode != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("preflight over the public budget: %d, Retry-After %q", r.StatusCode, r.Header.Get("Retry-After"))
	}
	readable(t, "preflight 429", r.Header)
	// One public budget: the address's surface requests are refused too.
	expect(t, e.do("GET", alice.origin+"blyg.json", "", nil, "X-Forwarded-For", "203.0.113.40"), 429)
	// Another address is untouched, and a token's requests are charged to
	// the token, not the address.
	if r := preflight(t, e, "/items", "X-Forwarded-For", "203.0.113.41"); r.StatusCode != 204 {
		t.Fatalf("another address: %d", r.StatusCode)
	}
	expect(t, alice.apiFrom("203.0.113.40", "GET", "/me"), 200)

	// export: 2 per token. The third is a 429 a browser can read.
	for i := 0; i < 2; i++ {
		expect(t, alice.apiFrom("203.0.113.42", "GET", "/origin/export"), 200)
	}
	over := e.do("GET", e.srv.URL+"/api/origin/export", alice.token, nil, "X-Forwarded-For", "203.0.113.42", "Origin", "https://client.example")
	expect(t, over, 429)
	readable(t, "429", over.header)
	if over.header.Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
}
