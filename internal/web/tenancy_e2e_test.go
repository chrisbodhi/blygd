package web

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chrisbodhi/blygd/internal/mention"
)

// Tenants on one instance never reach each other's things. Every operation
// that names something by an id in its path is called by one user with
// another user's id, and must answer 404, as if the id did not exist, and
// change nothing. The operations come from openapi.json, so a route added
// with a path id fails here until it has a case.
//
// Both users hold owner tokens ("*"), so no scope check can answer before
// the ownership check does: every refusal here is about whose thing it is.
func TestTenantsAreIsolated(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice, bob, carol, dave := e.user("alice"), e.user("bob"), e.user("carol"), e.user("dave")
	ctx := context.Background()
	run := func() {
		t.Helper()
		if _, err := e.workers.SendBatch(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := e.workers.VerifyBatch(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// Streams open before anything happens, so bob's would hear any hint
	// about alice's things from the first one.
	aliceHears, bobHears := hear(t, e.srv.URL, alice.token), hear(t, e.srv.URL, bob.token)

	// Words that are alice's alone. None may appear in anything bob is
	// answered.
	const (
		secretDraft  = "Alice's unpublished draft, zqxj."
		secretHopper = "Alice's private hopper zqxj"
		secretToken  = "alice-private-token-zqxj"
		secretBlock  = "blocked-by-alice-zqxj.example"
		secretTitle  = "Alice's private name for Carol zqxj"
	)

	// ── alice's fixture: one of everything ──

	cItem := carol.draft(t, map[string]any{"content_md": "Carol says something."})
	carol.publish(t, cItem, 200)

	aDraft := alice.draft(t, map[string]any{"content_md": secretDraft})
	aPub := alice.draft(t, map[string]any{"content_md": "Alice, in public."})
	alice.publish(t, aPub, 200)
	expect(t, alice.api("POST", "/items/"+aPub+"/versions/1/pin", nil), 200)

	aSub := itoa(alice.subscribe(t, carol.origin))
	expect(t, alice.api("PATCH", "/subscriptions/"+aSub, map[string]any{"display_title": secretTitle}), 200)
	expect(t, alice.api("PUT", "/reader/items/"+aSub+"/"+cItem+"/read", nil), 204)

	// A link to carol's item sends her a mention: alice has an outbound row.
	aLink := alice.draft(t, map[string]any{"kind": "thread", "content_md": "See [[" + cItem + "]]."})
	alice.publish(t, aLink, 200)

	r := alice.api("POST", "/hoppers", map[string]any{"name": secretHopper})
	expect(t, r, 201)
	aHop := itoa(int(r.json(t)["id"].(float64)))
	r = alice.api("POST", "/hoppers/"+aHop+"/members", map[string]any{"own_item_id": aPub})
	expect(t, r, 201)
	aMemOwn := itoa(int(r.json(t)["id"].(float64)))
	r = alice.api("POST", "/hoppers/"+aHop+"/members", map[string]any{"imported": map[string]any{"subscription_id": mustInt(t, aSub), "id": cItem}})
	expect(t, r, 201)

	img := pngBytes(t, 10)
	r = alice.upload(img, "image/png")
	expect(t, r, 201)
	aMedia := strings.TrimPrefix(r.json(t)["path"].(string), "media/")

	// Carol stubs alice's item: alice receives a verified mention.
	carol.subscribe(t, alice.origin)
	cStub := carol.draft(t, map[string]any{"stub_of": map[string]any{"id": aPub}})
	carol.publish(t, cStub, 200)
	run()
	r = alice.api("GET", "/mentions", nil)
	expect(t, r, 200)
	ms := r.json(t)["mentions"].([]any)
	if len(ms) != 1 || ms[0].(map[string]any)["status"] != "verified" {
		t.Fatalf("alice's mentions: %v", ms)
	}
	aMention := itoa(int(ms[0].(map[string]any)["id"].(float64)))

	r = alice.api("POST", "/mention-blocks", map[string]any{"kind": "host", "value": secretBlock})
	expect(t, r, 201)
	aBlock := itoa(int(r.json(t)["id"].(float64)))

	r = alice.api("POST", "/tokens", map[string]any{"name": secretToken, "scopes": []string{"items:read"}})
	expect(t, r, 201)
	aToken := itoa(int(r.json(t)["id"].(float64)))

	opml := `<?xml version="1.0"?><opml version="2.0"><head><title>x</title></head><body>` +
		`<outline type="rss" text="Dave" xmlUrl="` + dave.origin + `feed.xml" htmlUrl="` + dave.origin + `"/></body></opml>`
	expect(t, alice.api("POST", "/subscriptions/imports", map[string]any{"opml": opml}), 202)
	if _, err := e.reader.ImportBatch(ctx); err != nil {
		t.Fatal(err)
	}

	// What alice sees of her own things, before bob tries anything.
	before := view(t, alice, aDraft, aPub, aHop, aSub, cItem)

	// Bob's stream heard none of it, while alice's heard all of it, so the
	// stream was live and the silence means something. This is checked
	// before bob does anything, so every hint he could hear is someone
	// else's.
	waitFor(t, aliceHears, "item", "mention", "outbound", "import", "subscribe")
	time.Sleep(200 * time.Millisecond) // anything sent with alice's last hint has arrived
	if evs := bobHears(); len(evs) > 0 {
		t.Fatalf("bob's stream heard others' activity: %v", evs)
	}

	// ── every operation with a path id, as bob, with alice's ids ──

	type call struct {
		path string
		body any
	}
	cases := map[string]call{
		"GET /api/items/{id}":                       {"/items/" + aDraft, nil},
		"PATCH /api/items/{id}":                     {"/items/" + aDraft, map[string]any{"content_md": "bob was here"}},
		"DELETE /api/items/{id}":                    {"/items/" + aDraft, nil},
		"POST /api/items/{id}/preview":              {"/items/" + aDraft + "/preview", nil},
		"POST /api/items/{id}/publish":              {"/items/" + aDraft + "/publish", nil},
		"POST /api/items/{id}/withdraw":             {"/items/" + aPub + "/withdraw", nil},
		"POST /api/items/{id}/versions/{n}/pin":     {"/items/" + aLink + "/versions/1/pin", nil},
		"GET /api/hoppers/{hid}":                    {"/hoppers/" + aHop, nil},
		"PATCH /api/hoppers/{hid}":                  {"/hoppers/" + aHop, map[string]any{"name": "bob's now"}},
		"DELETE /api/hoppers/{hid}":                 {"/hoppers/" + aHop, nil},
		"POST /api/hoppers/{hid}/members":           {"/hoppers/" + aHop + "/members", nil}, // body: bob's own item, below
		"DELETE /api/hoppers/{hid}/members/{mid}":   {"/hoppers/" + aHop + "/members/" + aMemOwn, nil},
		"DELETE /api/media/{file}":                  {"/media/" + aMedia, nil},
		"PUT /api/mentions/{mid}/approval":          {"/mentions/" + aMention + "/approval", nil},
		"DELETE /api/mentions/{mid}/approval":       {"/mentions/" + aMention + "/approval", nil},
		"DELETE /api/mention-blocks/{bid}":          {"/mention-blocks/" + aBlock, nil},
		"GET /api/subscriptions/{sid}":              {"/subscriptions/" + aSub, nil},
		"PATCH /api/subscriptions/{sid}":            {"/subscriptions/" + aSub, map[string]any{"display_title": "bob's now"}},
		"DELETE /api/subscriptions/{sid}":           {"/subscriptions/" + aSub, nil},
		"POST /api/subscriptions/{sid}/resolve":     {"/subscriptions/" + aSub + "/resolve", nil},
		"POST /api/subscriptions/{sid}/move":        {"/subscriptions/" + aSub + "/move", map[string]any{"to": carol.origin}},
		"POST /api/subscriptions/{sid}/poll":        {"/subscriptions/" + aSub + "/poll", nil},
		"GET /api/reader/items/{sid}/{iid}":         {"/reader/items/" + aSub + "/" + cItem, nil},
		"PUT /api/reader/items/{sid}/{iid}/read":    {"/reader/items/" + aSub + "/" + cItem + "/read", nil},
		"DELETE /api/reader/items/{sid}/{iid}/read": {"/reader/items/" + aSub + "/" + cItem + "/read", nil},
		"DELETE /api/tokens/{tid}":                  {"/tokens/" + aToken, nil},
	}
	// Each case's path really names the operation it is filed under.
	for op, c := range cases {
		method, tmpl, _ := strings.Cut(op, " ")
		if !regexp.MustCompile("^" + strings.ReplaceAll(regexp.QuoteMeta(param.ReplaceAllString(tmpl, "\x00")), "\x00", "[^/]+") + "$").MatchString("/api" + c.path) {
			t.Errorf("case %s calls %s %s, another operation", op, method, c.path)
		}
	}
	// And every documented operation with a path id has a case.
	paths, _ := loadOpenAPI(t)
	var missing []string
	for p, ops := range paths {
		if !strings.Contains(p, "{") {
			continue
		}
		for m := range ops {
			if !isMethod(m) {
				continue
			}
			op := strings.ToUpper(m) + " " + p
			if _, ok := cases[op]; !ok {
				missing = append(missing, op)
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("operations with a path id and no cross-tenant case here:\n  %s", strings.Join(missing, "\n  "))
	}

	bobItem := bob.draft(t, map[string]any{"content_md": "Bob's own."})
	var answered []string // every body bob is sent, to look for alice's words
	ops := make([]string, 0, len(cases))
	for op := range cases {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	for _, op := range ops {
		c := cases[op]
		method, _, _ := strings.Cut(op, " ")
		body := c.body
		if op == "POST /api/hoppers/{hid}/members" {
			body = map[string]any{"own_item_id": bobItem}
		}
		r := bob.api(method, c.path, body)
		answered = append(answered, string(r.body))
		if r.status != 404 {
			t.Errorf("%s as another user: %d, want 404: %s", op, r.status, r.body)
		}
	}

	// A member id from alice's hopper, named under bob's own hopper.
	r = bob.api("POST", "/hoppers", map[string]any{"name": "Bob's hopper"})
	expect(t, r, 201)
	bHop := itoa(int(r.json(t)["id"].(float64)))
	expect(t, bob.api("DELETE", "/hoppers/"+bHop+"/members/"+aMemOwn, nil), 404)

	// Operations without a path id act only on the caller's own things.
	expect(t, bob.api("DELETE", "/subscriptions/imports", nil), 200)
	expect(t, bob.api("POST", "/reader/read", map[string]any{"as_of": time.Now().UTC().Format(time.RFC3339Nano), "subscription_id": mustInt(t, aSub)}), 404)

	// Media is content-addressed and kept per origin: the same bytes are
	// the same path for bob, and deleting his copy leaves alice's.
	r = bob.upload(img, "image/png")
	expect(t, r, 201)
	if got := strings.TrimPrefix(r.json(t)["path"].(string), "media/"); got != aMedia {
		t.Fatalf("same bytes, different path: %s and %s", got, aMedia)
	}
	expect(t, bob.api("DELETE", "/media/"+aMedia, nil), 204)
	expect(t, bob.api("DELETE", "/media/"+aMedia, nil), 404)
	r = alice.api("GET", "/media", nil)
	expect(t, r, 200)
	if !strings.Contains(string(r.body), aMedia) {
		t.Fatal("bob deleting his copy of the bytes deleted alice's")
	}

	// Nothing of alice's changed, and her surface still serves what it did.
	if after := view(t, alice, aDraft, aPub, aHop, aSub, cItem); after != before {
		t.Fatalf("alice's things changed under bob's calls:\nbefore %s\nafter  %s", before, after)
	}

	// ── bob's listings hold nothing of alice's ──

	for _, path := range []string{"/items", "/hoppers", "/media", "/mentions?include_blocked=true", "/mention-blocks",
		"/outbound", "/subscriptions", "/subscriptions/imports", "/reader/feed", "/tokens", "/origin", "/me"} {
		r := bob.api("GET", path, nil)
		expect(t, r, 200)
		answered = append(answered, string(r.body))
		for _, id := range []string{aDraft, aPub, aLink, cItem, aMedia, carol.origin, dave.origin} {
			if strings.Contains(string(r.body), id) {
				t.Errorf("bob's GET %s names alice's %s: %s", path, id, r.body)
			}
		}
	}

	// ── ids in request bodies ──

	// Bob's own hopper cannot take alice's items, published or not, so his
	// public hopper can never show her draft.
	for _, id := range []string{aDraft, aPub} {
		r := bob.api("POST", "/hoppers/"+bHop+"/members", map[string]any{"own_item_id": id})
		answered = append(answered, string(r.body))
		if r.status != 404 {
			t.Errorf("bob's hopper took alice's item %s: %d %s", id, r.status, r.body)
		}
	}
	// Nor an import from a subscription that is alice's.
	r = bob.api("POST", "/hoppers/"+bHop+"/members", map[string]any{"imported": map[string]any{"subscription_id": mustInt(t, aSub), "id": cItem}})
	answered = append(answered, string(r.body))
	if r.status != 404 {
		t.Errorf("bob's hopper took an import through alice's subscription: %d %s", r.status, r.body)
	}

	// Another tenant's draft never resolves, subscribed or not (§10.2: a
	// local id is this origin's; another origin's item comes only through
	// the publisher's own subscriptions, which hold only what was
	// published).
	unresolved := func(why string) {
		t.Helper()
		r := bob.api("POST", "/items", map[string]any{"stub_of": map[string]any{"id": aDraft}})
		answered = append(answered, string(r.body))
		if r.status < 400 || r.status >= 500 {
			t.Errorf("%s: a stub of alice's draft: %d %s", why, r.status, r.body)
		}
		for _, md := range []string{"![[" + aDraft + "]]", "Look: [[" + aDraft + "]]"} {
			id := bob.draft(t, map[string]any{"kind": "thread", "content_md": md})
			r := bob.api("POST", "/items/"+id+"/preview", nil)
			expect(t, r, 200)
			answered = append(answered, string(r.body))
			r = bob.api("POST", "/items/"+id+"/publish", nil)
			answered = append(answered, string(r.body))
			if r.status != 422 {
				t.Errorf("%s: publishing %q: %d %s", why, md, r.status, r.body)
			}
			expect(t, bob.api("DELETE", "/items/"+id, nil), 204)
		}
		r = bob.api("POST", "/items", map[string]any{"forked_from": map[string]any{"origin": alice.origin, "id": aDraft, "version": 1}})
		answered = append(answered, string(r.body))
		if r.status < 400 || r.status >= 500 {
			t.Errorf("%s: a fork of alice's draft: %d %s", why, r.status, r.body)
		}
	}
	unresolved("unsubscribed")
	// Unsubscribed, alice's published item does not resolve either.
	r = bob.api("POST", "/items", map[string]any{"stub_of": map[string]any{"id": aPub}})
	if r.status < 400 || r.status >= 500 {
		t.Errorf("a stub of an origin bob does not follow: %d %s", r.status, r.body)
	}
	bob.subscribe(t, alice.origin)
	unresolved("subscribed")
	// Subscribed, the published one resolves as any other origin's would.
	bob.draft(t, map[string]any{"stub_of": map[string]any{"id": aPub}})

	// ── nothing bob was answered carries alice's words ──

	for _, body := range answered {
		for _, secret := range []string{secretDraft, secretHopper, secretToken, secretBlock, secretTitle} {
			if strings.Contains(body, secret) {
				t.Errorf("bob was answered with alice's %q: %s", secret, body)
			}
		}
	}
}

var param = regexp.MustCompile(`\{[^}]+\}`)

func isMethod(m string) bool {
	switch m {
	case "get", "post", "put", "patch", "delete":
		return true
	}
	return false
}

func mustInt(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// volatile removes what reading changes: when a token was last used, and
// when a feed was read.
var volatile = regexp.MustCompile(`"(last_used_at|as_of)":\s*("[^"]*"|null)`)

// view is everything alice can read about her own things, through the API
// and her public surface, as one string to compare.
func view(t *testing.T, u *user, draft, pub, hop, sub, imported string) string {
	t.Helper()
	var b strings.Builder
	for _, path := range []string{"/origin", "/items", "/items/" + draft, "/items/" + pub, "/hoppers", "/hoppers/" + hop,
		"/media", "/mentions?include_blocked=true", "/mention-blocks", "/outbound", "/subscriptions", "/subscriptions/" + sub,
		"/reader/items/" + sub + "/" + imported, "/subscriptions/imports", "/tokens"} {
		r := u.api("GET", path, nil)
		expect(t, r, 200)
		b.WriteString(path + " " + volatile.ReplaceAllString(string(r.body), "") + "\n")
	}
	for _, path := range []string{"blyg.json", "items/index.json", "feed.xml", "items/" + pub + ".json", "items/" + pub + "/v1.json"} {
		r := u.get(path)
		expect(t, r, 200)
		b.WriteString(path + " " + r.header.Get("ETag") + "\n")
	}
	return b.String()
}

// hear opens a user's event stream and keeps every event it delivers. The
// returned function says what has arrived so far.
func hear(t *testing.T, base, token string) func() []sse {
	t.Helper()
	ch, resp := openStream(t, base, token)
	if ch == nil {
		t.Fatalf("event stream: %d", resp.StatusCode)
	}
	var mu sync.Mutex
	var got []sse
	go func() {
		for ev := range ch {
			mu.Lock()
			got = append(got, ev)
			mu.Unlock()
		}
	}()
	return func() []sse {
		mu.Lock()
		defer mu.Unlock()
		return append([]sse(nil), got...)
	}
}

// waitFor waits until a stream has delivered at least one event of each
// type.
func waitFor(t *testing.T, heard func() []sse, types ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		seen := map[string]bool{}
		for _, ev := range heard() {
			seen[ev.Type] = true
		}
		var lacking []string
		for _, typ := range types {
			if !seen[typ] {
				lacking = append(lacking, typ)
			}
		}
		if len(lacking) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stream never heard %v", lacking)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
