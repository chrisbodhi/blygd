package web

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/chrisbodhi/blygd/internal/config"
	"github.com/chrisbodhi/blygd/internal/mention"
)

// The host root page links the API description (also as service-desc,
// §16.6), the source, Blygger and llms.txt; lists the operator's links
// as rel="me"; and every host-relative link on it resolves.
func TestHomePage(t *testing.T) {
	page := string(HomePage("https://github.com/chrisbodhi/blygd", []config.Link{
		{Label: "Mastodon", URL: "https://social.example/@me"},
		{Label: `<b>"x"</b>`, URL: "https://ex.com/?a=1&b=2"},
	}))
	for _, want := range []string{
		`<link rel="service-desc" type="application/openapi+json" href="/api/openapi.json">`,
		`<a href="/api/openapi.json">`,
		`<a href="https://github.com/chrisbodhi/blygd">`,
		`<a href="https://blygger.com/">`,
		`<a href="/llms.txt">`,
		`<a rel="me" href="https://social.example/@me">Mastodon</a>`,
		`<a rel="me" href="https://ex.com/?a=1&amp;b=2">&lt;b&gt;&#34;x&#34;&lt;/b&gt;</a>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("home page lacks %s:\n%s", want, page)
		}
	}
	if strings.Contains(page, "<script") || strings.Contains(page, "<style") || strings.Contains(page, " style=") {
		t.Error("home page is plain HTML: no script, no styles")
	}

	h := Router(&API{}, &Surface{}, discardLog())
	for _, m := range regexp.MustCompile(`href="(/[^"]*)"`).FindAllStringSubmatch(page, -1) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", m[1], nil))
		if rec.Code != 200 {
			t.Errorf("home page links %s: %d", m[1], rec.Code)
		}
	}

	// Without operator links the section is gone, heading and all.
	if bare := string(HomePage("https://ex.com/src", nil)); strings.Contains(bare, "rel=\"me\"") || strings.Contains(bare, "Who runs") {
		t.Errorf("operator section without links:\n%s", bare)
	}
}

// The root is the home page only while no origin is mounted there: a
// root-mounted origin (a custom domain, later) serves its own index.
func TestHomePageShadowsNoOrigin(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	r := e.do("GET", e.srv.URL+"/", "", nil)
	expect(t, r, 200)
	if !strings.Contains(string(r.body), "<h1>blygd</h1>") || r.header.Get("Content-Security-Policy") != homeCSP {
		t.Fatalf("root: %q %s", r.header.Get("Content-Security-Policy"), r.body)
	}
	expect(t, e.do("HEAD", e.srv.URL+"/", "", nil), 200)
	expect(t, e.do("POST", e.srv.URL+"/", "", nil), 404)
	expect(t, e.do("GET", e.srv.URL+"/nobody/", "", nil), 404)

	alice := e.user("alice")
	if _, err := e.pool.Exec(t.Context(), `UPDATE origins SET base_url = $1 WHERE base_url = $2`, e.srv.URL+"/", alice.origin); err != nil {
		t.Fatal(err)
	}
	r = e.do("GET", e.srv.URL+"/", "", nil)
	expect(t, r, 200)
	if strings.Contains(string(r.body), "<h1>blygd</h1>") || r.header.Get("Content-Security-Policy") != pageCSP {
		t.Fatalf("a root-mounted origin's index was shadowed: %s", r.body)
	}
}
