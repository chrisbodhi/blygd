package web

import (
	"encoding/json"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/chrisbodhi/blygd/internal/config"
	"github.com/chrisbodhi/blygd/internal/mention"
)

// The host root page links the API description (also as service-desc,
// §16.6), the source, Blygger, llms.txt and the studio's invite form; lists the operator's links
// as rel="me"; and every host-relative link on it resolves.
func TestHomePage(t *testing.T) {
	page := string(HomePage("", "https://github.com/chrisbodhi/blygd", []config.Link{
		{Label: "Mastodon", URL: "https://social.example/@me"},
		{Label: `<b>"x"</b>`, URL: "https://ex.com/?a=1&b=2"},
	}))
	for _, want := range []string{
		`<link rel="service-desc" type="application/openapi+json" href="/api/openapi.json">`,
		`<a href="/api/openapi.json">`,
		`<a href="https://github.com/chrisbodhi/blygd">`,
		`<a href="https://blygger.com/">`,
		`<a href="/llms.txt">`,
		`<a href="/studio/#invite">`,
		`<a rel="me" href="https://social.example/@me">Mastodon</a>`,
		`<a rel="me" href="https://ex.com/?a=1&amp;b=2">&lt;b&gt;&#34;x&#34;&lt;/b&gt;</a>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("home page lacks %s:\n%s", want, page)
		}
	}
	if strings.Contains(page, "<script") || strings.Contains(page, " style=") {
		t.Error("home page runs nothing and has no style attributes: its one stylesheet is the sea's")
	}

	h := Router(&API{}, &Surface{}, discardLog())
	for _, m := range regexp.MustCompile(`href="(/[^"]*)"`).FindAllStringSubmatch(page, -1) {
		path, _, _ := strings.Cut(m[1], "#") // a browser never sends the fragment
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Errorf("home page links %s: %d", m[1], rec.Code)
		}
	}

	// Without operator links the section is gone, heading and all.
	if bare := string(HomePage("", "https://ex.com/src", nil)); strings.Contains(bare, "rel=\"me\"") || strings.Contains(bare, "Who runs") {
		t.Errorf("operator section without links:\n%s", bare)
	}
}

// The home page is headed with the instance's name, as text, and still
// says it runs blygd; unnamed, it is blygd.
func TestHomePageName(t *testing.T) {
	if page := string(HomePage("", "https://ex.com/src", nil)); !strings.Contains(page, "<title>blygd</title>") ||
		!strings.Contains(page, "<h1>blygd</h1>\n<p>This host runs blygd, a Blygger studio") {
		t.Errorf("unnamed:\n%s", page)
	}
	if page := string(HomePage("Eurypos", "https://ex.com/src", nil)); !strings.Contains(page, "<title>Eurypos</title>") ||
		!strings.Contains(page, "<h1>Eurypos</h1>\n<p>Eurypos runs blygd, a Blygger studio") {
		t.Errorf("named:\n%s", page)
	}
	if page := string(HomePage(`<i>E</i> & co`, "https://ex.com/src", nil)); strings.Contains(page, "<i>") ||
		!strings.Contains(page, "<h1>&lt;i&gt;E&lt;/i&gt; &amp; co</h1>") {
		t.Errorf("a name is text, never markup:\n%s", page)
	}
}

// GET /api/instance names the instance, and llms.txt is headed with it.
func TestInstanceName(t *testing.T) {
	h := Router(&API{Name: "Eurypos", PublicURL: "https://eurypos.com/"}, &Surface{}, discardLog())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/instance", nil))
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); rec.Code != 200 || err != nil ||
		got["name"] != "Eurypos" || got["origin_template"] != "https://eurypos.com/~{handle}/" {
		t.Errorf("instance: %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/llms.txt", nil))
	if !strings.HasPrefix(rec.Body.String(), "# Eurypos\n\n> Eurypos runs blygd, a Blygger studio: ") {
		t.Errorf("llms.txt: %.120q", rec.Body)
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

// The home page's CSP allows its stylesheet by hash, so the hash must be
// of the page as rendered (html/template could rewrite it), and the page
// may carry no style attributes, which the policy would block.
func TestHomeStyleMatchesCSP(t *testing.T) {
	page := string(HomePage("Eurypos", "https://example.com/src", nil))
	if !strings.Contains(homeCSP, "style-src '"+styleHash(page)+"';") {
		t.Errorf("the CSP's style hash is not the rendered stylesheet's: %s", homeCSP)
	}
	if strings.Contains(page, "style=") {
		t.Error("the home page has a style attribute, which its CSP blocks")
	}
}
