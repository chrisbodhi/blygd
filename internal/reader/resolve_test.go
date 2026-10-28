package reader

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/chrisbodhi/blygd/internal/fetch"
)

// mapFetcher serves a fixed web: URL → (final URL, status, body).
type mapFetcher struct {
	pages map[string]page
	asked []string
}

type page struct {
	final  string // empty: the URL itself
	status int
	body   string
}

func (m *mapFetcher) GetIf(_ context.Context, u string, _ fetch.Limits, _ string, _ fetch.Conditional) (*fetch.Response, error) {
	m.asked = append(m.asked, u)
	p, ok := m.pages[u]
	if !ok {
		return &fetch.Response{FinalURL: u, Status: 404, Header: http.Header{}}, nil
	}
	if p.status == -1 {
		return nil, errors.New("connection refused")
	}
	final := p.final
	if final == "" {
		final = u
	}
	st := p.status
	if st == 0 {
		st = 200
	}
	return &fetch.Response{FinalURL: final, Status: st, Header: http.Header{}, Body: []byte(p.body)}, nil
}

const manifest = `{"blyg": "0.3", "title": "Ex", "site": "https://ex.com/blyg/"}`

func TestResolveSteps(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		pages    map[string]page
		kind     string
		identity string
		maxAsked int
	}{
		{"direct probe", "https://ex.com/blyg/f/abc/?q=1#x",
			map[string]page{"https://ex.com/blyg/f/abc/blyg.json": {body: manifest}}, "blyg", "https://ex.com/blyg/f/abc/", 1},
		{"identity is the post-redirect fetch origin, not site", "https://old.example/",
			map[string]page{"https://old.example/blyg.json": {final: "https://new.example/b/blyg.json", body: manifest}},
			"blyg", "https://new.example/b/", 1},
		{"feed upgrade via blyg:manifest", "https://ex.com/feed.xml",
			map[string]page{
				"https://ex.com/feed.xml":       {body: `<rss version="2.0" xmlns:blyg="https://blygger.org/ns/0.1"><channel><blyg:manifest>https://ex.com/blyg/blyg.json</blyg:manifest></channel></rss>`},
				"https://ex.com/blyg/blyg.json": {body: manifest},
			}, "blyg", "https://ex.com/blyg/", 3},
		{"rel=blyg, one hop", "https://ex.com/about",
			map[string]page{
				"https://ex.com/about":           {body: `<html><head><link rel="blyg" href="/notes/"></head></html>`},
				"https://ex.com/notes/blyg.json": {body: manifest},
			}, "blyg", "https://ex.com/notes/", 4},
		{"rel=blyg naming the manifest itself (§12.1 step 4)", "https://ex.com/2026/10/a-post/",
			map[string]page{
				"https://ex.com/2026/10/a-post/":          {body: `<html><head><link rel="blyg" href="/wp-json/blyg/v1/manifest"></head></html>`},
				"https://ex.com/wp-json/blyg/v1/manifest": {body: manifest},
			}, "blyg", "https://ex.com/wp-json/blyg/v1/", 3},
		{"a manifest at a query-string URL is out of scope (#64)", "https://ex.com/post",
			map[string]page{
				"https://ex.com/post":                          {body: `<html><head><link rel="blyg" href="/?rest_route=/blyg/v1/manifest"></head></html>`},
				"https://ex.com/?rest_route=/blyg/v1/manifest": {body: manifest},
				"https://ex.com/blyg/blyg.json":                {body: manifest},
			}, "blyg", "https://ex.com/blyg/", 5},
		{"a feed naming a query-string manifest stays L0", "https://ex.com/rss",
			map[string]page{
				"https://ex.com/rss":   {body: `<rss version="2.0" xmlns:blyg="https://blygger.org/ns/0.1"><channel><blyg:manifest>https://ex.com/m?x=1</blyg:manifest></channel></rss>`},
				"https://ex.com/m?x=1": {body: manifest},
			}, "l0", "https://ex.com/rss", 6},
		{"conventional /blyg/ mount", "https://ex.com/some/page",
			map[string]page{"https://ex.com/some/page": {body: `<html></html>`}, "https://ex.com/blyg/blyg.json": {body: manifest}},
			"blyg", "https://ex.com/blyg/", 4},
		{"conventional root mount", "https://ex.com/some/page",
			map[string]page{"https://ex.com/blyg.json": {body: manifest}}, "blyg", "https://ex.com/", 5},
		{"held L0 feed when no manifest anywhere", "https://legacy.example/rss",
			map[string]page{"https://legacy.example/rss": {body: `<rss version="2.0"><channel><title>L</title><link>https://legacy.example/</link></channel></rss>`}},
			"l0", "https://legacy.example/rss", 5},
		{"autodiscovered feed is L0, within six fetches", "https://legacy.example/home",
			map[string]page{
				"https://legacy.example/home": {body: `<html><head><link rel="alternate" type="application/rss+xml" href="/rss"></head></html>`},
				"https://legacy.example/rss":  {body: `<rss version="2.0"><channel><title>L</title></channel></rss>`},
			}, "l0", "https://legacy.example/rss", 6},
		{"a manifest-looking body without the blyg key is not a manifest", "https://ex.com/x/",
			map[string]page{"https://ex.com/x/blyg.json": {body: `{"site": "https://ex.com/x/"}`}, "https://ex.com/blyg.json": {body: manifest}},
			"blyg", "https://ex.com/", 5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &mapFetcher{pages: c.pages}
			res, err := Resolve(context.Background(), f, c.input)
			if err != nil {
				t.Fatalf("%v (asked %v)", err, f.asked)
			}
			if res.Kind != c.kind || res.Identity != c.identity {
				t.Fatalf("got %s %s, want %s %s (asked %v)", res.Kind, res.Identity, c.kind, c.identity, f.asked)
			}
			if c.kind == "blyg" {
				last := f.asked[len(f.asked)-1]
				if p := c.pages[last]; p.final != "" {
					last = p.final
				}
				if res.ManifestURL != last {
					t.Errorf("manifest URL %q, want where the last fetch ended (asked %v)", res.ManifestURL, f.asked)
				}
			}
			if len(f.asked) > c.maxAsked || len(f.asked) > MaxResolveFetches || len(res.Trail) != len(f.asked) {
				t.Errorf("asked %d URLs (want ≤ %d): %v", len(f.asked), c.maxAsked, f.asked)
			}
		})
	}
}

func TestResolveSiteDiscrepancyAndFailure(t *testing.T) {
	// §12.2: a mirror asserting another origin's site keeps its own identity.
	f := &mapFetcher{pages: map[string]page{"https://mirror.example/blyg.json": {body: manifest}}}
	res, err := Resolve(context.Background(), f, "http://mirror.example/")
	if err == nil {
		t.Fatalf("http input should not have resolved over https pages: %+v", res)
	}
	res, err = Resolve(context.Background(), f, "https://mirror.example/")
	if err != nil {
		t.Fatal(err)
	}
	if res.Identity != "https://mirror.example/" || !strings.Contains(res.SiteDiscrepancy, "https://ex.com/blyg/") {
		t.Errorf("site discrepancy: %+v", res)
	}

	// Failure reports the trail, and never exceeds the bound.
	f = &mapFetcher{pages: map[string]page{"https://dead.example/x": {status: -1}}}
	_, err = Resolve(context.Background(), f, "https://dead.example/x")
	var re *ResolveError
	if !errors.As(err, &re) || len(re.Trail) == 0 || len(re.Trail) > MaxResolveFetches || !strings.Contains(strings.Join(re.Trail, " "), "refused") {
		t.Fatalf("failure: %v %+v", err, re)
	}

	// The rel target's own HTML is never scanned: no second hop.
	f = &mapFetcher{pages: map[string]page{
		"https://a.example/":          {body: `<link rel="blyg" href="https://b.example/">`},
		"https://b.example/blyg.json": {body: `<html><head><link rel="blyg" href="https://c.example/"></head></html>`},
		"https://c.example/blyg.json": {body: manifest},
	}}
	if _, err := Resolve(context.Background(), f, "https://a.example/"); err == nil {
		t.Error("followed a second rel hop")
	}
	for _, u := range f.asked {
		if strings.HasPrefix(u, "https://c.example/") {
			t.Errorf("fetched %s", u)
		}
	}

	// A dead rel link costs two fetches at 0.4 (the target, then
	// {target}blyg.json), so with both conventional mounts probed the bound
	// is spent before step 6 can autodiscover a feed. The order and the
	// bound are both the spec's.
	f = &mapFetcher{pages: map[string]page{
		"https://legacy.example/home": {body: `<html><head><link rel="blyg" href="/nope/"><link rel="alternate" type="application/rss+xml" href="/rss"></head></html>`},
		"https://legacy.example/rss":  {body: `<rss version="2.0"><channel><title>L</title></channel></rss>`},
	}}
	if _, err := Resolve(context.Background(), f, "https://legacy.example/home"); !errors.As(err, &re) || len(f.asked) != MaxResolveFetches {
		t.Errorf("dead rel link: %v after %v", err, f.asked)
	}

	if _, err := Resolve(context.Background(), f, "ftp://x/"); err == nil {
		t.Error("resolved ftp")
	}
}

func TestIdentityFromManifestURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://Ex.com:443/blyg/blyg.json?x=1": "https://ex.com/blyg/",
		"https://ex.com/blyg.json":              "https://ex.com/",
		"https://ex.com/a/other.json":           "https://ex.com/a/",
	} {
		if got, err := IdentityFromManifestURL(in); err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", in, got, err, want)
		}
	}
}
