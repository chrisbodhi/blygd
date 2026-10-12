package web

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/chrisbodhi/blygd/internal/blyg"
	"github.com/chrisbodhi/blygd/internal/mention"
)

// untar reads a gzipped tar into name -> bytes.
func untar(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	out := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		out[h.Name] = body
	}
}

// A static export is the live surface as plain files, minus the Webmention
// endpoint (§15.1, §15.7), plus published media, and writes nothing.
func TestStaticExport(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice := e.user("alice")

	shown := alice.upload(pngBytes(t, 10), "image/png").json(t)["path"].(string)
	unused := alice.upload(pngBytes(t, 200), "image/png").json(t)["path"].(string)
	f := alice.draft(t, map[string]any{"content_md": "First, with ![a dot](" + shown + ")."})
	alice.publish(t, f, 200)
	alice.edit(t, f, "Second, with ![a dot]("+shown+").")
	alice.publish(t, f, 200)
	expect(t, alice.api("POST", "/items/"+f+"/versions/1/pin", nil), 200)
	draft := alice.draft(t, map[string]any{"content_md": "Never published."})
	page := alice.itemDoc(t, f).Page

	// The live surface advertises the endpoint: the export must not.
	link := `<link rel="webmention" href="` + alice.origin + `webmention">`
	if !strings.Contains(string(alice.get("").body), link) || !strings.Contains(string(alice.get("blyg.json").body), `"webmention"`) {
		t.Fatal("live surface does not advertise its endpoint; the checks below would prove nothing")
	}

	r := alice.api("GET", "/origin/export", nil)
	expect(t, r, 200)
	if r.header.Get("Content-Type") != "application/gzip" || !strings.Contains(r.header.Get("Content-Disposition"), ".tar.gz") {
		t.Fatalf("headers: %v", r.header)
	}
	files := untar(t, r.body)

	// Each file against what the live surface serves at that path.
	live := map[string]string{
		"index.html": "", "blyg.json": "blyg.json", "feed.xml": "feed.xml", "items/index.json": "items/index.json",
		"items/" + f + ".json": "items/" + f + ".json", "items/" + f + "/v1.json": "items/" + f + "/v1.json",
		page + "index.html": page, blyg.PinnedPage(page, 1) + "index.html": blyg.PinnedPage(page, 1),
		shown: shown,
	}
	for name, path := range live {
		got, ok := files[name]
		if !ok {
			t.Errorf("export lacks %s", name)
			continue
		}
		want := alice.get(path)
		expect(t, want, 200)
		switch {
		case name == "blyg.json":
			var m map[string]any
			if err := json.Unmarshal(got, &m); err != nil || m["webmention"] != nil || m["site"] != alice.origin {
				t.Errorf("manifest: %s", got)
			}
		case strings.HasSuffix(name, ".html"):
			// Exactly the live page without the endpoint link.
			if w := strings.Replace(string(want.body), "\n"+link, "", 1); string(got) != w {
				t.Errorf("%s differs from the live page beyond the webmention link:\n%s\nwant\n%s", name, got, w)
			}
		case !bytes.Equal(got, want.body):
			t.Errorf("%s differs from the live surface", name)
		}
	}
	for name, body := range files {
		if bytes.Contains(body, []byte("webmention")) {
			t.Errorf("%s mentions webmention", name)
		}
		if strings.Contains(name, draft) || bytes.Contains(body, []byte(draft)) {
			t.Errorf("%s carries the draft", name)
		}
	}
	if _, ok := files[unused]; ok {
		t.Error("export carries media no published version references")
	}
	if len(files) != len(live) {
		names := make([]string, 0, len(files))
		for n := range files {
			names = append(names, n)
		}
		t.Errorf("export has %d files, want %d: %v", len(files), len(live), names)
	}

	// Nothing was written: the live surface still advertises the endpoint.
	if !strings.Contains(string(alice.get("").body), link) || !strings.Contains(string(alice.get("blyg.json").body), `"webmention"`) {
		t.Error("export changed the live surface")
	}
}

// Every HTML page an origin serves runs no script and gets an opaque
// origin, so nothing on it can reach the studio's token on the same host;
// data files carry no page policy.
func TestPagesRunNoScript(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice := e.user("alice")
	f := alice.draft(t, map[string]any{"content_md": "One."})
	alice.publish(t, f, 200)
	alice.edit(t, f, "Two.")
	alice.publish(t, f, 200)
	expect(t, alice.api("POST", "/items/"+f+"/versions/1/pin", nil), 200)
	page := alice.itemDoc(t, f).Page
	for _, p := range []string{"", page, blyg.PinnedPage(page, 1)} {
		r := alice.get(p)
		expect(t, r, 200)
		csp := r.header.Get("Content-Security-Policy")
		if !strings.HasPrefix(csp, "default-src 'none';") || strings.Contains(csp, "script-src") || !strings.Contains(csp, "sandbox") || strings.Contains(csp, "allow-same-origin") {
			t.Errorf("%q: CSP %q", p, csp)
		}
		if r.header.Get("Referrer-Policy") == "" {
			t.Errorf("%q: no Referrer-Policy", p)
		}
	}
	if csp := alice.get("blyg.json").header.Get("Content-Security-Policy"); csp != "" {
		t.Errorf("blyg.json carries a page policy: %q", csp)
	}
}
