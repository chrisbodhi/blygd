package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chrisbodhi/blygd/internal/account"
	"github.com/chrisbodhi/blygd/internal/blyg"
	"github.com/chrisbodhi/blygd/internal/config"
	"github.com/chrisbodhi/blygd/internal/events"
	"github.com/chrisbodhi/blygd/internal/fetch"
	"github.com/chrisbodhi/blygd/internal/mention"
	"github.com/chrisbodhi/blygd/internal/reader"
	"github.com/chrisbodhi/blygd/internal/studio"
	"github.com/chrisbodhi/blygd/internal/testdb"
)

// env is one running blygd instance on a test server.
type env struct {
	t       *testing.T
	srv     *httptest.Server
	pool    *pgxpool.Pool
	studio  *studio.Studio
	accts   *account.Accounts
	workers *mention.Workers
	reader  *reader.Reader
	fetch   *fetch.Client
	api     *API
	surface *Surface
	hub     *events.Hub
}

func newEnv(t *testing.T, limits mention.RateLimits) *env {
	pool := testdb.New(t)
	srv := httptest.NewUnstartedServer(nil)
	srv.Start()
	t.Cleanup(srv.Close)

	// The production wiring, configured as a test needs: private addresses
	// reachable (the server is on 127.0.0.1), no request budgets, and the
	// given mention limits.
	app := New(&config.Config{PublicURL: srv.URL + "/", AllowPrivate: true, Level: 2, FragmentCap: 2000},
		pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	app.Receiver.Limits = limits
	app.Reader.ReconcileEvery = 1000 * time.Hour
	srv.Config.Handler = app.Handler
	hubCtx, stopHub := context.WithCancel(context.Background())
	t.Cleanup(stopHub)
	go app.Hub.Run(hubCtx)
	return &env{t: t, srv: srv, pool: pool, studio: app.Studio, accts: app.Accounts, workers: app.Workers,
		reader: app.Reader, fetch: app.Fetch, api: app.API, surface: app.Surface, hub: app.Hub}
}

type user struct {
	e      *env
	token  string
	origin string
	id     int64
}

func (e *env) user(handle string) *user {
	uid, oid, secret, err := e.accts.CreateUser(context.Background(), handle, e.srv.URL+"/", strings.ToUpper(handle[:1])+handle[1:]+"'s blyg")
	if err != nil {
		e.t.Fatalf("create user: %v", err)
	}
	if err := e.studio.Rebuild(context.Background(), oid); err != nil {
		e.t.Fatal(err)
	}
	o, _ := e.studio.GetOrigin(context.Background(), oid)
	return &user{e: e, token: secret, origin: o.BaseURL, id: uid}
}

type resp struct {
	status int
	header http.Header
	body   []byte
}

func (r resp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("not JSON (%d): %s", r.status, r.body)
	}
	return m
}

func (e *env) do(method, u, token string, body any, hdr ...string) resp {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		switch b := body.(type) {
		case url.Values:
			rd = strings.NewReader(b.Encode())
		case []byte:
			rd = bytes.NewReader(b) // raw upload; the caller sets Content-Type
		default:
			j, _ := json.Marshal(b)
			rd = bytes.NewReader(j)
		}
	}
	req, _ := http.NewRequest(method, u, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if _, ok := body.(url.Values); ok {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	if api := e.srv.URL + "/api/"; strings.HasPrefix(u, api) {
		// Every API response the suite sees must match openapi.json.
		checkResponse(e.t, method, req.URL.Path, r.StatusCode, r.Header.Get("Content-Type"), b)
	}
	return resp{r.StatusCode, r.Header, b}
}

func (u *user) api(method, path string, body any) resp {
	return u.e.do(method, u.e.srv.URL+"/api"+path, u.token, body)
}

func (u *user) get(path string, hdr ...string) resp {
	return u.e.do("GET", u.origin+path, "", nil, hdr...)
}

func expect(t *testing.T, r resp, status int) {
	t.Helper()
	if r.status != status {
		t.Fatalf("status %d, want %d: %s", r.status, status, r.body)
	}
}

func (u *user) draft(t *testing.T, body map[string]any) string {
	t.Helper()
	r := u.api("POST", "/items", body)
	expect(t, r, 201)
	return r.json(t)["id"].(string)
}

func (u *user) itemDoc(t *testing.T, id string) blyg.ItemDoc {
	t.Helper()
	r := u.get("items/" + id + ".json")
	expect(t, r, 200)
	var d blyg.ItemDoc
	if err := json.Unmarshal(r.body, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestPublishLifecycle(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice := e.user("alice")

	// An empty blyg is still a blyg.
	expect(t, alice.get("blyg.json"), 200)
	expect(t, alice.get("feed.xml"), 200)
	idx := alice.get("items/index.json")
	expect(t, idx, 200)
	if !strings.Contains(string(idx.body), `"items": []`) {
		t.Errorf("empty index: %s", idx.body)
	}

	id := alice.draft(t, map[string]any{"content_md": "Hello *world* <b>x</b> [link](../)"})
	expect(t, alice.get("items/"+id+".json"), 404) // drafts 404 (§4)

	r := alice.api("POST", "/items/"+id+"/publish", map[string]any{"note": "first"})
	expect(t, r, 200)
	d := alice.itemDoc(t, id)
	if d.Blyg != "0.4" || d.Kind != "fragment" || d.Origin != alice.origin || d.Page != "f/"+id+"/" || d.Version != 1 ||
		d.ContentHash != blyg.ContentHash(d.ContentMD) || d.Created != d.Updated || len(d.Changelog) != 1 ||
		*d.Changelog[0].Note != "first" || d.Media == nil {
		t.Fatalf("v1 doc: %+v", d)
	}
	if !strings.Contains(d.ContentHTML, "&lt;b&gt;") || !strings.Contains(d.ContentHTML, `href="`+alice.origin+`f/"`) {
		t.Errorf("content_html not escaped/absolute: %s", d.ContentHTML)
	}

	// No-op republish is refused; an edit is v2.
	expect(t, alice.api("POST", "/items/"+id+"/publish", nil), 409)
	expect(t, alice.api("PATCH", "/items/"+id, map[string]any{"content_md": "Hello again", "note": "edit"}), 200)
	expect(t, alice.api("POST", "/items/"+id+"/publish", nil), 200)
	d = alice.itemDoc(t, id)
	if d.Version != 2 || d.ContentMD != "Hello again" || len(d.Changelog) != 2 || *d.Changelog[1].Note != "edit" || d.Updated != d.Changelog[1].At {
		t.Fatalf("v2 doc: %+v", d)
	}

	// Feed: one entry per publish event, both carrying the latest content.
	feed := string(alice.get("feed.xml").body)
	for _, want := range []string{"blyg:" + id + ":v1", "blyg:" + id + ":v2", "<title>edit — Hello again</title>", "<title>first — Hello again</title>"} {
		if !strings.Contains(feed, want) {
			t.Errorf("feed missing %q:\n%s", want, feed)
		}
	}
	if strings.Contains(feed, "world") {
		t.Errorf("feed leaks withheld v1 content:\n%s", feed)
	}

	// Pins: v1 becomes fetchable forever; v2 stays withheld.
	expect(t, alice.get("items/"+id+"/v1.json"), 404)
	expect(t, alice.api("POST", "/items/"+id+"/versions/1/pin", nil), 200)
	pin := alice.get("items/" + id + "/v1.json")
	expect(t, pin, 200)
	if !strings.Contains(string(pin.body), "Hello *world*") || !strings.Contains(pin.header.Get("Cache-Control"), "immutable") {
		t.Errorf("pin: %s %v", pin.body, pin.header)
	}
	expect(t, alice.get("items/"+id+"/v2.json"), 404)
	if d = alice.itemDoc(t, id); !d.Changelog[0].Pinned || d.Changelog[1].Pinned {
		t.Errorf("changelog pinned flags: %+v", d.Changelog)
	}

	// Conditional GET, CORS, and the HTML page's links.
	m := alice.get("blyg.json")
	if m.header.Get("Access-Control-Allow-Origin") != "*" || !strings.Contains(string(m.body), `"webmention": "webmention"`) {
		t.Errorf("manifest: %v %s", m.header, m.body)
	}
	expect(t, alice.get("blyg.json", "If-None-Match", m.header.Get("ETag")), 304)
	page := alice.get("f/" + id + "/")
	expect(t, page, 200)
	if !strings.Contains(page.header.Get("Link"), `rel="webmention"`) ||
		!strings.Contains(string(page.body), `<link rel="alternate" type="application/json" href="`+alice.origin+"items/"+id+`.json">`) {
		t.Errorf("permalink page: %v\n%s", page.header, page.body)
	}
	if r := e.do("GET", alice.origin+"f/"+id, "", nil); r.status != 200 { // follows the slash redirect
		t.Errorf("slashless permalink: %d", r.status)
	}

	// Withdrawal: the doc stays 200, empty; the pin survives; the feed shows
	// one entry for the item, titled withdrawn.
	pinBefore := pin.body
	expect(t, alice.api("POST", "/items/"+id+"/withdraw", map[string]any{"note": "taking it back"}), 200)
	d = alice.itemDoc(t, id)
	if d.Kind != "withdrawn" || d.ContentMD != "" || d.ContentHTML != "" || d.ContentHash != blyg.ContentHash("") || d.Page == "" || d.Version != 3 {
		t.Fatalf("endcap: %+v", d)
	}
	if p := alice.get("items/" + id + "/v1.json"); p.status != 200 || !bytes.Equal(p.body, pinBefore) {
		t.Errorf("pin changed after withdrawal")
	}
	feed = string(alice.get("feed.xml").body)
	if strings.Count(feed, "<blyg:id>"+id+"</blyg:id>") != 1 || !strings.Contains(feed, "<title>withdrawn</title>") {
		t.Errorf("withdrawn feed:\n%s", feed)
	}
	expect(t, alice.api("POST", "/items/"+id+"/versions/3/pin", nil), 422) // endcaps are never pinned
	expect(t, alice.api("DELETE", "/items/"+id, nil), 409)                 // published items are never deleted

	// Return under the same id.
	expect(t, alice.api("POST", "/items/"+id+"/publish", nil), 200)
	if d = alice.itemDoc(t, id); d.Kind != "fragment" || d.Version != 4 || d.ContentMD != "Hello again" {
		t.Errorf("returned item: %+v", d)
	}

	// The schema enforces what the code promises.
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `UPDATE item_versions SET content_md = 'rewrite' WHERE item_id = $1 AND version = 1`, id); err == nil {
		t.Error("history rewrite accepted")
	}
	if _, err := e.pool.Exec(ctx, `UPDATE item_versions SET pinned_at = NULL WHERE item_id = $1 AND version = 1`, id); err == nil {
		t.Error("unpin accepted")
	}
	if _, err := e.pool.Exec(ctx, `DELETE FROM surface_files WHERE path = $1`, "items/"+id+".json"); err == nil {
		t.Error("deleting a permanent surface file accepted")
	}

	// Index lists the item at its latest version.
	if !strings.Contains(string(alice.get("items/index.json").body), `"version": 4`) {
		t.Errorf("index not updated")
	}

	// Drafts can be discarded.
	d2 := alice.draft(t, map[string]any{"content_md": "never mind"})
	expect(t, alice.api("DELETE", "/items/"+d2, nil), 204)
	expect(t, alice.api("GET", "/items/"+d2, nil), 404)
}

// Both tenants are on one instance, and no private address may be dialed,
// as behind a private reverse proxy: every fetch here, the pin check, the
// endpoint discovery, the send and the verification, is answered in-process.
func TestForkMentionRoundTrip(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	e.sameInstanceOnly()
	ctx := context.Background()
	alice, bob := e.user("alice"), e.user("bob")

	src := alice.draft(t, map[string]any{"content_md": "An idea worth forking."})
	expect(t, alice.api("POST", "/items/"+src+"/publish", nil), 200)

	// Forking an unpinned version is refused on evidence (§5.6 rule 2).
	early := bob.draft(t, map[string]any{"content_md": "too early", "forked_from": map[string]any{"origin": alice.origin, "id": src, "version": 1}})
	expect(t, bob.api("POST", "/items/"+early+"/publish", nil), 422)

	expect(t, alice.api("POST", "/items/"+src+"/versions/1/pin", nil), 200)
	copied := bob.draft(t, map[string]any{"forked_from": map[string]any{"origin": alice.origin, "id": src, "version": 1}})
	if md := bob.api("GET", "/items/"+copied, nil).json(t)["draft_md"]; md != "An idea worth forking." {
		t.Fatalf("a copying fork reads the pin: %v", md)
	}
	fork := bob.draft(t, map[string]any{"content_md": "An idea worth forking, forked.",
		"forked_from": map[string]any{"origin": alice.origin, "id": src, "version": 1}})
	r := bob.api("POST", "/items/"+fork+"/publish", nil)
	expect(t, r, 200)
	if d := bob.itemDoc(t, fork); !strings.Contains(string(d.ForkedFrom), src) {
		t.Fatalf("fork doc lacks lineage: %s", d.ForkedFrom)
	}

	// The outbox holds one fork mention; sending it discovers alice's
	// endpoint through her manifest and gets a 202.
	if n, err := e.workers.SendBatch(ctx); err != nil || n != 1 {
		t.Fatalf("send batch: %d %v", n, err)
	}
	out := bob.api("GET", "/outbound", nil).json(t)["outbound"].([]any)[0].(map[string]any)
	if out["status"] != "sent" || out["endpoint"] != alice.origin+"webmention" || out["relation"] != "fork" {
		t.Fatalf("outbound: %v", out)
	}

	// Alice verifies it structurally.
	if n, err := e.workers.VerifyBatch(ctx); err != nil || n != 1 {
		t.Fatalf("verify batch: %d %v", n, err)
	}
	ms := alice.api("GET", "/mentions", nil).json(t)["mentions"].([]any)
	m := ms[0].(map[string]any)
	if m["status"] != "verified" || m["relation"] != "fork" || m["source_origin"] != bob.origin || m["source_item_id"] != fork {
		t.Fatalf("mention: %v", m)
	}

	// A republish with the same reference sends nothing new.
	expect(t, bob.api("PATCH", "/items/"+fork, map[string]any{"content_md": "edited fork"}), 200)
	expect(t, bob.api("POST", "/items/"+fork+"/publish", nil), 200)
	if n, _ := e.workers.SendBatch(ctx); n != 0 {
		t.Errorf("unchanged reference re-sent")
	}

	// Bob withdraws; re-sending the mention by hand now finds an endcap, so
	// the verified mention becomes gone and the record is kept.
	expect(t, bob.api("POST", "/items/"+fork+"/withdraw", nil), 200)
	if d := bob.itemDoc(t, fork); d.ForkedFrom == nil {
		t.Errorf("endcap dropped forked_from (§9 keeps it)")
	}
	// Bob holds no page for the source, so the mention named its item
	// document (§15.2, #85), and the re-send names the same target.
	if out["target_url"] != alice.origin+"items/"+src+".json" {
		t.Errorf("target %v: with no page held it is the item document, never f/{id}/", out["target_url"])
	}
	wm := url.Values{"source": {bob.origin + "f/" + fork + "/"}, "target": {alice.origin + "items/" + src + ".json"}}
	expect(t, e.do("POST", alice.origin+"webmention", "", wm), 202)
	if _, err := e.workers.VerifyBatch(ctx); err != nil {
		t.Fatal(err)
	}
	m = alice.api("GET", "/mentions", nil).json(t)["mentions"].([]any)[0].(map[string]any)
	if m["status"] != "gone" || m["relation"] != "fork" {
		t.Fatalf("after withdrawal: %v", m)
	}
}

func TestWebmentionEndpoint(t *testing.T) {
	e := newEnv(t, mention.RateLimits{PerHost: 3, PerDomain: 100, Global: 100})
	alice := e.user("alice")
	id := alice.draft(t, map[string]any{"content_md": "target"})
	expect(t, alice.api("POST", "/items/"+id+"/publish", nil), 200)
	draft := alice.draft(t, map[string]any{"content_md": "unpublished"})
	ep := alice.origin + "webmention"
	page := alice.origin + "f/" + id + "/"

	post := func(source, target string) int {
		return e.do("POST", ep, "", url.Values{"source": {source}, "target": {target}}).status
	}
	cases := []struct {
		source, target string
		want           int
	}{
		{"not a url", page, 400},
		{"ftp://example.com/x", page, 400},
		{page, page, 400},
		{"https://example.com/x", "https://example.com/elsewhere", 400},
		{"https://example.com/x", alice.origin + "f/" + draft + "/", 400}, // drafts are not published items
		{"https://example.com/x", alice.origin + "f/00000000000000000000000000/", 400},
	}
	for _, c := range cases {
		if got := post(c.source, c.target); got != c.want {
			t.Errorf("source=%q target=%q: %d, want %d", c.source, c.target, got, c.want)
		}
	}
	expect(t, e.do("GET", ep, "", nil), 405)

	// Accepted by page or by document URL; a re-send does not duplicate.
	if got := post("https://a.example.com/x", page); got != 202 {
		t.Fatalf("accept by page: %d", got)
	}
	if got := post("https://a.example.com/x", alice.origin+"items/"+id+".json"); got != 202 {
		t.Fatalf("accept by doc: %d", got)
	}
	if got := post("https://a.example.com/x", page); got != 202 {
		t.Fatalf("re-send: %d", got)
	}
	var n int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM inbound_mentions`).Scan(&n)
	if n != 2 {
		t.Errorf("%d mention rows, want 2 (one per source/target pair)", n)
	}
	// Per-host cap of 3 is reached; another host still gets in.
	if got := post("https://a.example.com/y", page); got != 429 {
		t.Errorf("fourth claim from one host: %d, want 429", got)
	}
	if got := post("https://b.example.org/y", page); got != 202 {
		t.Errorf("other host: %d", got)
	}

	// A target is known by membership (§15.3 step 1, decision #84): a
	// pinned version's document and page are this blyg's item too; an
	// unpinned version has no such address.
	expect(t, alice.api("POST", "/items/"+id+"/versions/1/pin", nil), 200)
	alice.edit(t, id, "target, revised")
	expect(t, alice.api("POST", "/items/"+id+"/publish", nil), 200)
	for i, c := range []struct {
		target string
		want   int
	}{
		{alice.origin + "items/" + id + "/v1.json", 202},
		{page + "v1/", 202},
		{alice.origin + "items/" + id + "/v2.json", 400},
		{page + "v2/", 400},
	} {
		if got := post(fmt.Sprintf("https://p%d.example.net/x", i), c.target); got != c.want {
			t.Errorf("target %s: %d, want %d", c.target, got, c.want)
		}
	}

	// An origin that does not accept mentions has no endpoint.
	f := false
	expect(t, alice.api("PATCH", "/origin", map[string]any{"accept_mentions": &f}), 200)
	expect(t, e.do("POST", ep, "", url.Values{"source": {"https://c.example/x"}, "target": {page}}), 404)
	if strings.Contains(string(alice.get("blyg.json").body), "webmention") {
		t.Errorf("manifest still advertises an endpoint")
	}
}

func TestTokensAndHoppers(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice, bob := e.user("alice"), e.user("bob")
	id := alice.draft(t, map[string]any{"content_md": "raw material"})
	bobItem := bob.draft(t, map[string]any{"content_md": "bob's"})

	// Scoped tokens: a drafting tool cannot publish, pin or mint.
	r := alice.api("POST", "/tokens", map[string]any{"name": "drafter", "scopes": []string{"items:read", "items:write"}})
	expect(t, r, 201)
	drafter := &user{e: e, token: r.json(t)["token"].(string), origin: alice.origin}
	expect(t, drafter.api("PATCH", "/items/"+id, map[string]any{"content_md": "edited by tool"}), 200)
	expect(t, drafter.api("POST", "/items/"+id+"/publish", nil), 403)
	expect(t, drafter.api("POST", "/tokens", map[string]any{"name": "x", "scopes": []string{"items:read"}}), 403)
	tid := int(r.json(t)["id"].(float64))
	expect(t, alice.api("DELETE", "/tokens/"+itoa(tid), nil), 204)
	expect(t, drafter.api("GET", "/items", nil), 401)
	expect(t, e.do("GET", e.srv.URL+"/api/items", "", nil), 401)

	// Hoppers hold references to the user's own items, drafts included,
	// and never to someone else's.
	r = alice.api("POST", "/hoppers", map[string]any{"name": "Raw Material!"})
	expect(t, r, 201)
	h := r.json(t)
	hid := itoa(int(h["id"].(float64)))
	if h["slug"] != "raw-material" {
		t.Errorf("slug %v", h["slug"])
	}
	expect(t, alice.api("POST", "/hoppers", map[string]any{"name": "raw material"}), 201) // gets raw-material-2
	expect(t, alice.api("POST", "/hoppers/"+hid+"/members", map[string]any{"own_item_id": id}), 201)
	expect(t, alice.api("POST", "/hoppers/"+hid+"/members", map[string]any{"own_item_id": id}), 409)
	expect(t, alice.api("POST", "/hoppers/"+hid+"/members", map[string]any{"own_item_id": bobItem}), 404)
	expect(t, alice.api("POST", "/hoppers/"+hid+"/members", map[string]any{"imported": map[string]any{"origin": "https://elsewhere.example/", "id": id}}), 404)
	expect(t, bob.api("GET", "/hoppers/"+hid, nil), 404) // private to its owner

	g := alice.api("GET", "/hoppers/"+hid, nil).json(t)
	members := g["members"].([]any)
	if len(members) != 1 || members[0].(map[string]any)["own"].(map[string]any)["state"] != "draft" {
		t.Fatalf("members: %v", members)
	}
	r = alice.api("PATCH", "/hoppers/"+hid, map[string]any{"name": "Kiln"})
	if r.json(t)["slug"] != "kiln" {
		t.Errorf("rename slug: %s", r.body)
	}
	mid := itoa(int(members[0].(map[string]any)["id"].(float64)))
	expect(t, alice.api("DELETE", "/hoppers/"+hid+"/members/"+mid, nil), 204)
	expect(t, alice.api("DELETE", "/hoppers/"+hid, nil), 204)
	expect(t, alice.api("GET", "/items/"+id, nil), 200) // deleting a hopper touches no item
}

func itoa(n int) string { return strconv.Itoa(n) }
