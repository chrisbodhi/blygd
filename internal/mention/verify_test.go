package mention

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chrisbodhi/blygd/internal/fetch"
)

const (
	targetID = "7c9wk2mhq0v3xj8tn5rzfd41bg"
	sourceID = "1vgtgz0gq5b2c9k7d3m8r4n6xy"
)

// world is a tiny web: a receiver origin ("/us/") and a source origin
// ("/them/") on one test server, plus a mirror ("/mirror/").
type world struct {
	srv  *httptest.Server
	docs map[string]string // path → body
	html map[string]string
}

func newWorld(t *testing.T) *world {
	w := &world{docs: map[string]string{}, html: map[string]string{}}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if b, ok := w.docs[r.URL.Path]; ok {
			rw.Header().Set("Content-Type", "application/json")
			fmt.Fprint(rw, b)
			return
		}
		if b, ok := w.html[r.URL.Path]; ok {
			rw.Header().Set("Content-Type", "text/html")
			fmt.Fprint(rw, b)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/redirect/") {
			http.Redirect(rw, r, strings.TrimPrefix(r.URL.Path, "/redirect"), http.StatusFound)
			return
		}
		http.NotFound(rw, r)
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *world) us() string   { return w.srv.URL + "/us/" }
func (w *world) them() string { return w.srv.URL + "/them/" }

func (w *world) doc(origin string, extra map[string]any) string {
	d := map[string]any{"blyg": "0.3", "id": sourceID, "kind": "thread", "origin": origin, "version": 2, "page": "t/" + sourceID + "/",
		"author": map[string]any{"name": "Them"}, "content_md": "words", "content_html": "<p>words</p>"}
	for k, v := range extra {
		d[k] = v
	}
	b, _ := json.Marshal(d)
	return string(b)
}

func (w *world) verify(t *testing.T, source string, pinned ...int) Outcome {
	t.Helper()
	c := &fetch.Client{AllowPrivate: true}
	return Verify(context.Background(), c, source, Target{Origin: w.us(), ItemID: targetID, IsPinned: func(v int) bool {
		for _, p := range pinned {
			if p == v {
				return true
			}
		}
		return false
	}})
}

func TestVerifyRelations(t *testing.T) {
	w := newWorld(t)
	docPath := "/them/items/" + sourceID + ".json"
	pagePath := "/them/t/" + sourceID + "/"
	w.html[pagePath] = `<html><head><link rel="alternate" type="application/json" href="../../items/` + sourceID + `.json"></head><body>hi</body></html>`

	cases := []struct {
		name     string
		extra    map[string]any
		pinned   []int
		status   string
		relation string
	}{
		{"stub", map[string]any{"stub_of": map[string]any{"origin": w.us(), "id": targetID, "version": 1, "cited": map[string]any{"retrieved": "x"}}}, nil, "verified", "stub"},
		{"stub origin normalized", map[string]any{"stub_of": map[string]any{"origin": strings.ToUpper(w.srv.URL[:4]) + w.srv.URL[4:] + "/us", "id": targetID, "version": 1}}, nil, "verified", "stub"},
		{"transclusion", map[string]any{"transclusions": []any{map[string]any{"id": "other0000000000000000000000"}, map[string]any{"origin": w.us(), "id": targetID, "version": 3}}}, nil, "verified", "transclusion"},
		{"transclusion without origin is own-origin", map[string]any{"transclusions": []any{map[string]any{"id": targetID, "version": 3}}}, nil, "failed", ""},
		{"fork of pinned", map[string]any{"forked_from": map[string]any{"origin": w.us(), "id": targetID, "version": 2}}, []int{2}, "verified", "fork"},
		{"fork of unpinned", map[string]any{"forked_from": map[string]any{"origin": w.us(), "id": targetID, "version": 2}}, []int{1}, "failed", ""},
		{"stub of another item", map[string]any{"stub_of": map[string]any{"origin": w.us(), "id": "0000000000000000000000000a", "version": 1}}, nil, "failed", ""},
		{"stub of same id at another origin", map[string]any{"stub_of": map[string]any{"origin": w.srv.URL + "/elsewhere/", "id": targetID, "version": 1}}, nil, "failed", ""},
		{"withdrawn", map[string]any{"kind": "withdrawn", "content_md": "", "content_html": ""}, nil, "gone", ""},
		{"plain link only", map[string]any{"content_html": `<p><a href="` + w.us() + `f/` + targetID + `/">x</a></p>`}, nil, "failed", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w.docs[docPath] = w.doc(w.them(), tc.extra)
			for _, src := range []string{w.srv.URL + docPath, w.srv.URL + pagePath} {
				out := w.verify(t, src, tc.pinned...)
				if out.Status != tc.status || out.Relation != tc.relation {
					t.Errorf("%s: got %s/%s (%s), want %s/%s", src, out.Status, out.Relation, out.Reason, tc.status, tc.relation)
				}
				if out.Transient {
					t.Errorf("%s: unexpectedly transient", src)
				}
			}
		})
	}
}

func TestVerifyIdentity(t *testing.T) {
	w := newWorld(t)
	stub := map[string]any{"stub_of": map[string]any{"origin": w.us(), "id": targetID, "version": 1}}

	// A mirror serving a document that claims /them/ as its origin.
	w.docs["/mirror/items/"+sourceID+".json"] = w.doc(w.them(), stub)
	if out := w.verify(t, w.srv.URL+"/mirror/items/"+sourceID+".json"); out.Status != "failed" {
		t.Errorf("mirror verified: %+v", out)
	}

	// A pinned version file never verifies (§15.4 rule 2).
	w.docs["/them/items/"+sourceID+"/v2.json"] = w.doc(w.them(), stub)
	if out := w.verify(t, w.srv.URL+"/them/items/"+sourceID+"/v2.json"); out.Status != "failed" {
		t.Errorf("pin verified: %+v", out)
	}

	// A redirect to the real document is fine: the final URL is what counts.
	w.docs["/them/items/"+sourceID+".json"] = w.doc(w.them(), stub)
	if out := w.verify(t, w.srv.URL+"/redirect/them/items/"+sourceID+".json"); out.Status != "verified" {
		t.Errorf("redirected source: %+v", out)
	}

	// A path-mounted neighbour on the same host cannot speak for /them/.
	w.docs["/them2/items/"+sourceID+".json"] = w.doc(w.them(), stub)
	if out := w.verify(t, w.srv.URL+"/them2/items/"+sourceID+".json"); out.Status != "failed" {
		t.Errorf("neighbour verified: %+v", out)
	}

	// Not a blyg document at all.
	w.docs["/them/random.json"] = `{"hello": "world"}`
	if out := w.verify(t, w.srv.URL+"/them/random.json"); out.Status != "failed" {
		t.Errorf("non-blyg JSON verified: %+v", out)
	}
	w.html["/them/page/"] = `<html><body><p>no alternate here</p><link rel="alternate" type="application/json" href="/them/items/` + sourceID + `.json"></body></html>`
	if out := w.verify(t, w.srv.URL+"/them/page/"); out.Status != "failed" {
		t.Errorf("alternate link in body accepted: %+v", out)
	}

	// Missing source is a conclusion; a server error is not.
	if out := w.verify(t, w.srv.URL+"/nothing"); out.Status != "failed" || out.Transient {
		t.Errorf("404 source: %+v", out)
	}
}

func TestVerifyRecordsNoContent(t *testing.T) {
	w := newWorld(t)
	w.docs["/them/items/"+sourceID+".json"] = w.doc(w.them(), map[string]any{
		"stub_of": map[string]any{"origin": w.us(), "id": targetID, "version": 1}})
	out := w.verify(t, w.srv.URL+"/them/items/"+sourceID+".json")
	if out.Status != "verified" || out.SourceID != sourceID || out.SourceVersion != 2 || out.SourceKind != "thread" ||
		out.SourcePage != w.them()+"t/"+sourceID+"/" || string(out.SourceAuthor) != `{"name":"Them"}` {
		t.Fatalf("outcome: %+v", out)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "words") {
		t.Errorf("outcome carries content: %s", b)
	}
}

func TestDiscovery(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/blyg/blyg.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"blyg":"0.3","webmention":"webmention"}`)
	})
	mux.HandleFunc("/header", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Link", `<https://example.com/other>; rel="preload", </wm/hdr>; rel="webmention other"`)
	})
	mux.HandleFunc("/html", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><head><link rel="stylesheet" href="/x.css"></head><body><a rel="webmention" href="/wm/a?x=1">wm</a></body></html>`)
	})
	mux.HandleFunc("/empty", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><head><link rel="webmention" href=""></head></html>`)
	})
	mux.HandleFunc("/none", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `<html></html>`) })
	mux.HandleFunc("/down", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := &fetch.Client{AllowPrivate: true}
	ctx := context.Background()

	cases := []struct {
		m      Outbound
		want   string
		status string
	}{
		{Outbound{TargetURL: srv.URL + "/blyg/f/x/", TargetOrigin: srv.URL + "/blyg/"}, srv.URL + "/blyg/webmention", ""},
		{Outbound{TargetURL: srv.URL + "/header"}, srv.URL + "/wm/hdr", ""},
		{Outbound{TargetURL: srv.URL + "/html"}, srv.URL + "/wm/a?x=1", ""},
		{Outbound{TargetURL: srv.URL + "/empty"}, srv.URL + "/empty", ""},
		{Outbound{TargetURL: srv.URL + "/none"}, "", "no_endpoint"},
		{Outbound{TargetURL: srv.URL + "/missing"}, "", "no_endpoint"},
		{Outbound{TargetURL: srv.URL + "/down"}, "", "retry"},
	}
	for _, tc := range cases {
		ep, res := Discover(ctx, c, tc.m)
		if ep != tc.want || res.Status != tc.status {
			t.Errorf("%s: got %q/%s (%s), want %q/%s", tc.m.TargetURL, ep, res.Status, res.Detail, tc.want, tc.status)
		}
	}
}

func TestSendClassification(t *testing.T) {
	codes := map[string]int{"/ok": 202, "/bad": 400, "/busy": 429, "/err": 500, "/moved": 301}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if r.FormValue("source") == "" || r.FormValue("target") == "" {
				w.WriteHeader(400)
				return
			}
			if r.URL.Path == "/moved" {
				w.Header().Set("Location", "/ok")
			}
			w.WriteHeader(codes[r.URL.Path])
			return
		}
		// Every page advertises the endpoint named by its query.
		w.Header().Set("Link", "<"+r.URL.Query().Get("ep")+`>; rel=webmention`)
	}))
	defer srv.Close()
	c := &fetch.Client{AllowPrivate: true}
	want := map[string]string{"/ok": "sent", "/bad": "failed", "/busy": "retry", "/err": "retry", "/moved": "failed"}
	for ep, status := range want {
		res := Send(context.Background(), c, Outbound{SourceURL: "https://s.example/x/", TargetURL: srv.URL + "/page?ep=" + ep})
		if res.Status != status {
			t.Errorf("%s: got %s (%s), want %s", ep, res.Status, res.Detail, status)
		}
	}
}
