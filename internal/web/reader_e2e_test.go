package web

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chrisbodhi/blygd/internal/fetch"
	"github.com/chrisbodhi/blygd/internal/mention"
	"github.com/chrisbodhi/blygd/internal/reader"
	"github.com/chrisbodhi/blygd/internal/testblyg"
)

// refuse is a remote fetcher that fails every request, to prove a
// same-instance subscription never leaves the process.
type refuse struct{}

func (refuse) GetIf(context.Context, string, fetch.Limits, string, fetch.Conditional) (*fetch.Response, error) {
	return nil, errors.New("network used for a same-instance origin")
}

func (e *env) poll(sid int) {
	e.t.Helper()
	if err := e.reader.PollNow(context.Background(), int64(sid)); err != nil {
		e.t.Fatal(err)
	}
}

func feedItems(t *testing.T, u *user, query string) []map[string]any {
	t.Helper()
	r := u.api("GET", "/reader/feed"+query, nil)
	expect(t, r, 200)
	m := r.json(t)
	if !strings.Contains(m["notice"].(string), "Sanitize") {
		t.Errorf("feed lacks the §14 notice: %v", m["notice"])
	}
	var out []map[string]any
	for _, it := range m["items"].([]any) {
		out = append(out, it.(map[string]any))
	}
	return out
}

func TestSameInstanceSubscription(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice, bob := e.user("alice"), e.user("bob")
	e.reader.Fetch = &reader.LocalFirst{Studio: e.studio, Remote: refuse{}}

	id := alice.draft(t, map[string]any{"content_md": "Hello from *alice*"})
	expect(t, alice.api("POST", "/items/"+id+"/publish", nil), 200)
	expect(t, alice.api("POST", "/items/"+id+"/versions/1/pin", nil), 200)

	// Subscribing from any page of hers resolves to her identity origin.
	r := bob.api("POST", "/subscriptions", map[string]any{"url": alice.origin + "f/" + id + "/"})
	expect(t, r, 201)
	sub := r.json(t)
	s := sub["subscription"].(map[string]any)
	sid := int(s["id"].(float64))
	if s["origin"] != alice.origin || s["kind"] != "blyg" || sub["resolution"].(map[string]any)["identity"] != alice.origin {
		t.Fatalf("subscription: %s", r.body)
	}
	expect(t, bob.api("POST", "/subscriptions", map[string]any{"url": "http://127.0.0.1:1/nothing"}), 422)

	e.poll(sid)
	items := feedItems(t, bob, "")
	if len(items) != 1 || items[0]["item_id"] != id || items[0]["read"] != false {
		t.Fatalf("feed: %v", items)
	}
	doc := items[0]["doc"].(map[string]any)
	if doc["content_md"] != "Hello from *alice*" || items[0]["page"] != alice.origin+"f/"+id+"/" {
		t.Fatalf("imported doc: %v", items[0])
	}

	// Read marks follow versions.
	expect(t, bob.api("PUT", "/reader/items/"+itoa(sid)+"/"+id+"/read", nil), 204)
	if items = feedItems(t, bob, "?unread=true"); len(items) != 0 {
		t.Fatalf("read item still unread: %v", items)
	}
	expect(t, alice.api("PATCH", "/items/"+id, map[string]any{"content_md": "Hello again"}), 200)
	expect(t, alice.api("POST", "/items/"+id+"/publish", nil), 200)
	e.poll(sid)
	if items = feedItems(t, bob, "?unread=true"); len(items) != 1 || items[0]["version"].(float64) != 2 {
		t.Fatalf("edited item not unread at v2: %v", items)
	}

	// Hoppers hold imported items as references.
	r = bob.api("POST", "/hoppers", map[string]any{"name": "From others"})
	hid := itoa(int(r.json(t)["id"].(float64)))
	expect(t, bob.api("POST", "/hoppers/"+hid+"/members", map[string]any{"imported": map[string]any{"origin": alice.origin, "id": id}}), 201)
	m := bob.api("GET", "/hoppers/"+hid, nil).json(t)["members"].([]any)[0].(map[string]any)["imported"].(map[string]any)
	if m["state"] != "current" || m["doc"].(map[string]any)["content_md"] != "Hello again" {
		t.Fatalf("imported member: %v", m)
	}

	// Nothing imported is re-emitted (§13.5): bob publishes, and none of
	// alice's ids or words appear anywhere on his surface.
	own := bob.draft(t, map[string]any{"content_md": "bob's own"})
	expect(t, bob.api("POST", "/items/"+own+"/publish", nil), 200)
	var leaks int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM surface_files sf JOIN origins o ON o.id = sf.origin_id
		WHERE o.base_url = $1 AND (position($2 in convert_from(sf.body, 'UTF8')) > 0 OR position('Hello again' in convert_from(sf.body, 'UTF8')) > 0)`,
		bob.origin, id).Scan(&leaks)
	if leaks != 0 {
		t.Fatalf("imported item re-emitted on %d of bob's surface files", leaks)
	}

	// Withdrawal: gone from the feed, no content in the hopper, the pin
	// retained and attributed (§13.4).
	expect(t, alice.api("POST", "/items/"+id+"/withdraw", nil), 200)
	e.poll(sid)
	if items = feedItems(t, bob, ""); len(items) != 0 {
		t.Fatalf("withdrawn item still in the feed: %v", items)
	}
	m = bob.api("GET", "/hoppers/"+hid, nil).json(t)["members"].([]any)[0].(map[string]any)["imported"].(map[string]any)
	if m["state"] != "withdrawn" || m["doc"] != nil {
		t.Fatalf("withdrawn member: %v", m)
	}
	it := bob.api("GET", "/reader/items/"+itoa(sid)+"/"+id, nil).json(t)["item"].(map[string]any)
	pins, _ := it["pins"].([]any)
	if it["state"] != "withdrawn" || it["doc"] != nil || len(pins) != 1 ||
		pins[0].(map[string]any)["url"] != alice.origin+"items/"+id+"/v1.json" {
		t.Fatalf("withdrawn item: %v", it)
	}

	// Another user cannot see bob's subscription or its items.
	carol := e.user("carol")
	expect(t, carol.api("GET", "/subscriptions/"+itoa(sid), nil), 404)
	expect(t, carol.api("GET", "/reader/items/"+itoa(sid)+"/"+id, nil), 404)

	expect(t, bob.api("DELETE", "/subscriptions/"+itoa(sid), nil), 204)
	if items = feedItems(t, bob, ""); len(items) != 0 {
		t.Fatalf("unsubscribed items in the feed: %v", items)
	}
}

func TestBlogroll(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice, bob := e.user("alice"), e.user("bob")
	r := bob.api("POST", "/subscriptions", map[string]any{"url": alice.origin})
	expect(t, r, 201)
	sid := itoa(int(r.json(t)["subscription"].(map[string]any)["id"].(float64)))

	// None yet: 404 and no manifest key (§11).
	expect(t, bob.get("blogroll.opml"), 404)
	if strings.Contains(string(bob.get("blyg.json").body), "blogroll") {
		t.Fatal("empty blogroll advertised")
	}

	// A reading tool may subscribe but not publish a reading choice.
	r = bob.api("POST", "/tokens", map[string]any{"name": "reader app", "scopes": []string{"reader:read", "reader:write"}})
	readerApp := &user{e: e, token: r.json(t)["token"].(string), origin: bob.origin}
	expect(t, readerApp.api("PATCH", "/subscriptions/"+sid, map[string]any{"in_blogroll": true}), 403)
	expect(t, readerApp.api("PATCH", "/subscriptions/"+sid, map[string]any{"display_title": "Alice"}), 200)

	r = bob.api("PATCH", "/subscriptions/"+sid, map[string]any{"in_blogroll": true})
	expect(t, r, 200)
	if r.json(t)["in_blogroll"] != true {
		t.Fatalf("in_blogroll: %s", r.body)
	}
	opml := bob.get("blogroll.opml")
	expect(t, opml, 200)
	for _, want := range []string{`text="Alice"`, `xmlUrl="` + alice.origin + `feed.xml"`, `htmlUrl="` + alice.origin + `"`, `type="rss"`} {
		if !strings.Contains(string(opml.body), want) {
			t.Errorf("blogroll missing %s:\n%s", want, opml.body)
		}
	}
	if !strings.Contains(string(bob.get("blyg.json").body), `"blogroll": "blogroll.opml"`) ||
		!strings.Contains(string(bob.get("").body), `rel="blogroll"`) {
		t.Error("blogroll not advertised in manifest and feed page")
	}

	// Unsubscribing withdraws the public claim too.
	expect(t, bob.api("DELETE", "/subscriptions/"+sid, nil), 204)
	expect(t, bob.get("blogroll.opml"), 404)
	if strings.Contains(string(bob.get("blyg.json").body), "blogroll") {
		t.Fatal("manifest still advertises a blogroll")
	}
}

func TestOutboundUsesImportedPage(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	ctx := context.Background()
	bob := e.user("bob")
	remote := &testblyg.Blyg{S: testblyg.New(t), Mount: "/blyg/", Title: "Remote"}
	src := &testblyg.Item{ID: "0a00000000000000000000000a", Kind: "fragment", Version: 1, ContentMD: "fork me",
		Page: "posts/fork-me/", Pinned: []int{1}, Updated: time.Now()}
	remote.Items = []*testblyg.Item{src}
	remote.Publish()
	remote.S.Set("/blyg/items/"+src.ID+"/v1.json", remote.Pin(src, 1, "fork me"))
	remote.S.Set("/blyg/posts/fork-me/", `<html><head><link rel="webmention" href="/wm"></head></html>`)
	remote.S.SetResponse("/wm", testblyg.Response{Status: 202})

	r := bob.api("POST", "/subscriptions", map[string]any{"url": remote.Origin()})
	expect(t, r, 201)
	e.poll(int(r.json(t)["subscription"].(map[string]any)["id"].(float64)))

	fork := bob.draft(t, map[string]any{"content_md": "forked", "forked_from": map[string]any{"origin": remote.Origin(), "id": src.ID, "version": 1}})
	expect(t, bob.api("POST", "/items/"+fork+"/publish", nil), 200)
	if n, err := e.workers.SendBatch(ctx); err != nil || n != 1 {
		t.Fatalf("send: %d %v", n, err)
	}
	out := bob.api("GET", "/outbound", nil).json(t)["outbound"].([]any)[0].(map[string]any)
	// §15.2: the target is the page the sender holds, not the f/{id}/
	// convention, and that page's endpoint got the mention.
	if out["target_url"] != remote.Origin()+"posts/fork-me/" || out["status"] != "sent" || remote.S.Hits("/wm") != 1 {
		t.Fatalf("outbound: %v (wm hits %d)", out, remote.S.Hits("/wm"))
	}
}

func TestMoveNeedsConfirmedIdentity(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice, bob := e.user("alice"), e.user("bob")
	alice2 := e.user("alice2")
	r := bob.api("POST", "/subscriptions", map[string]any{"url": alice.origin})
	sid := itoa(int(r.json(t)["subscription"].(map[string]any)["id"].(float64)))

	// Re-resolution reports and changes nothing (§12.2).
	r = bob.api("POST", "/subscriptions/"+sid+"/resolve", map[string]any{"url": alice2.origin})
	expect(t, r, 200)
	if m := r.json(t); m["changed"] != true || m["current"] != alice.origin {
		t.Fatalf("resolve: %s", r.body)
	}
	if subs := bob.api("GET", "/subscriptions", nil).json(t)["subscriptions"].([]any); len(subs) != 1 ||
		subs[0].(map[string]any)["origin"] != alice.origin {
		t.Fatalf("resolve changed the subscription: %v", subs)
	}
	// A move must name the identity the URL resolves to: a permalink of
	// hers resolves fine, but to her origin, which is not what was named.
	pid := alice2.draft(t, map[string]any{"content_md": "moved here"})
	expect(t, alice2.api("POST", "/items/"+pid+"/publish", nil), 200)
	expect(t, bob.api("POST", "/subscriptions/"+sid+"/move", map[string]any{"to": alice2.origin + "f/" + pid + "/"}), 422)
	expect(t, bob.api("POST", "/subscriptions/"+sid+"/move", map[string]any{"to": alice.origin}), 409)
	r = bob.api("POST", "/subscriptions/"+sid+"/move", map[string]any{"to": alice2.origin})
	expect(t, r, 200)
	subs := bob.api("GET", "/subscriptions", nil).json(t)["subscriptions"].([]any)
	if len(subs) != 1 || subs[0].(map[string]any)["origin"] != alice2.origin {
		t.Fatalf("after move: %v", subs)
	}
}
