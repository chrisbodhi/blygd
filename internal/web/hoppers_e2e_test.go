package web

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chrisbodhi/blygd/internal/blyg"
	"github.com/chrisbodhi/blygd/internal/mention"
	"github.com/chrisbodhi/blygd/internal/testblyg"
)

// Public hoppers (§13.5): a shown list with attribution and links, served
// live, never protocol surface, and following §13.4 on withdrawal.
func TestPublicHoppers(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice, bob := e.user("alice"), e.user("bob")
	ctx := context.Background()

	own := alice.draft(t, map[string]any{"content_md": "Alice's own note."})
	alice.publish(t, own, 200)
	draft := alice.draft(t, map[string]any{"content_md": "Secret draft."})
	pinned := bob.draft(t, map[string]any{"content_md": "Bob, pinned."})
	bob.publish(t, pinned, 200)
	expect(t, bob.api("POST", "/items/"+pinned+"/versions/1/pin", nil), 200)
	loose := bob.draft(t, map[string]any{"content_md": "Bob, unpinned."})
	bob.publish(t, loose, 200)
	bsid := alice.subscribe(t, bob.origin)

	remote := &testblyg.Blyg{S: testblyg.New(t), Mount: "/r/", Title: "Remote"}
	evil := "0e00000000000000000000000e"
	remote.Items = []*testblyg.Item{{ID: evil, Kind: "fragment", Version: 1, Updated: time.Now(), ContentMD: `<img src=x onerror="alert(1)">harmless words`}}
	remote.Publish()
	rsid := alice.subscribe(t, remote.Origin())

	r := alice.api("POST", "/hoppers", map[string]any{"name": "Good reads"})
	expect(t, r, 201)
	hid := itoa(int(r.json(t)["id"].(float64)))
	for _, m := range []map[string]any{
		{"own_item_id": own}, {"own_item_id": draft},
		{"imported": map[string]any{"origin": bob.origin, "id": pinned}},
		{"imported": map[string]any{"origin": bob.origin, "id": loose}},
		{"imported": map[string]any{"subscription_id": rsid, "id": evil}},
	} {
		expect(t, alice.api("POST", "/hoppers/"+hid+"/members", m), 201)
	}
	page := "h/good-reads/"
	expect(t, alice.get(page), 404) // private

	// Going public is a publishing act: hoppers:write alone is not enough.
	tok := alice.api("POST", "/tokens", map[string]any{"name": "curator", "scopes": []string{"hoppers:write"}}).json(t)["token"].(string)
	expect(t, e.do("PATCH", e.srv.URL+"/api/hoppers/"+hid, tok, map[string]any{"public": true}), 403)
	r = alice.api("PATCH", "/hoppers/"+hid, map[string]any{"public": true})
	expect(t, r, 200)
	if r.json(t)["slug_frozen"] != true {
		t.Fatalf("going public freezes the slug: %s", r.body)
	}

	r = alice.get(page)
	expect(t, r, 200)
	body := string(r.body)
	for _, want := range []string{"Alice&#39;s own note.", "Bob, pinned.", "Bob, unpinned.", "harmless words",
		`<a href="` + bob.origin + `f/` + pinned + `/">read it there</a>`, `<a href="` + bob.origin + `"><bdi>Bob&#39;s blyg</bdi></a>`} { // §14: isolated
		if !strings.Contains(body, want) {
			t.Errorf("public hopper lacks %q:\n%s", want, body)
		}
	}
	for _, bad := range []string{"Secret draft", "onerror", "<img"} {
		if strings.Contains(body, bad) {
			t.Errorf("public hopper shows %q", bad)
		}
	}
	if r.header.Get("Link") != "" || r.header.Get("Content-Security-Policy") != hopperCSP {
		t.Errorf("headers: %v", r.header)
	}
	expect(t, alice.get(page, "If-None-Match", r.header.Get("ETag")), 304)
	expect(t, alice.get("h/good-reads"), 200) // redirected to the slash form

	// Live, never stored: no surface file, nothing in the feed or index.
	var n int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM surface_files sf JOIN origins o ON o.id = sf.origin_id
		WHERE o.base_url = $1 AND (sf.path LIKE 'h/%' OR convert_from(sf.body, 'UTF8') LIKE '%Bob, pinned%')`, alice.origin).Scan(&n)
	if n != 0 {
		t.Errorf("a public hopper reached surface_files (%d rows)", n)
	}

	// §13.4: withdrawn at the source, a pinned item shows its retained pin;
	// anything else disappears on the next request.
	expect(t, bob.api("POST", "/items/"+pinned+"/withdraw", nil), 200)
	expect(t, bob.api("POST", "/items/"+loose+"/withdraw", nil), 200)
	e.poll(bsid)
	body = string(alice.get(page).body)
	if !strings.Contains(body, "Bob, pinned.") || !strings.Contains(body, "pinned v1") || strings.Contains(body, "Bob, unpinned") {
		t.Errorf("after withdrawal:\n%s", body)
	}
	// An origin no longer followed is no longer polled, so its items go.
	expect(t, alice.api("DELETE", "/subscriptions/"+itoa(bsid), nil), 204)
	if body = string(alice.get(page).body); strings.Contains(body, "Bob, pinned") {
		t.Errorf("unfollowed origin still shown:\n%s", body)
	}

	// The slug outlives renames and going private.
	expect(t, alice.api("PATCH", "/hoppers/"+hid, map[string]any{"name": "Better reads"}), 200)
	expect(t, alice.get(page), 200)
	expect(t, alice.api("PATCH", "/hoppers/"+hid, map[string]any{"public": false}), 200)
	expect(t, alice.get(page), 404)
	if _, err := e.pool.Exec(ctx, `UPDATE hoppers SET slug = 'moved' WHERE id = $1`, hid); err == nil {
		t.Error("the schema let a frozen slug change")
	}
	// Another user's hoppers are not under alice's origin.
	expect(t, bob.get(page), 404)
}

// Hopper feeds (§6.1, §13.5, §13.6; decision #86): a public hopper is named
// in the manifest's hoppers list, with a live RSS feed of every current
// member, no blyg: namespace, and a guid per membership; a hopper that is
// not public is never listed and has no feed.
func TestHopperFeeds(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice, bob := e.user("alice"), e.user("bob")
	ctx := context.Background()

	own := alice.draft(t, map[string]any{"content_md": "Alice's own note."})
	alice.publish(t, own, 200)
	theirs := bob.draft(t, map[string]any{"content_md": "Bob's words."})
	bob.publish(t, theirs, 200)
	alice.subscribe(t, bob.origin)
	// A remote member with no page: its link is its item document, through
	// its origin's item template, never a guessed permalink.
	remote := &testblyg.Blyg{S: testblyg.New(t), Mount: "/r/", Title: "Remote", ItemTmpl: "docs/{id}.json"}
	pageless := "0e00000000000000000000000e"
	remote.Items = []*testblyg.Item{{ID: pageless, Kind: "fragment", Version: 1, Updated: time.Now(), ContentMD: "No page here.", NoPage: true}}
	remote.Publish()
	rsid := alice.subscribe(t, remote.Origin())

	r := alice.api("POST", "/hoppers", map[string]any{"name": "Good reads"})
	expect(t, r, 201)
	hid := itoa(int(r.json(t)["id"].(float64)))
	for _, m := range []map[string]any{
		{"own_item_id": own},
		{"imported": map[string]any{"origin": bob.origin, "id": theirs}},
		{"imported": map[string]any{"subscription_id": rsid, "id": pageless}},
	} {
		expect(t, alice.api("POST", "/hoppers/"+hid+"/members", m), 201)
	}
	manifest := func() map[string]any {
		t.Helper()
		r := alice.get("blyg.json")
		expect(t, r, 200)
		return r.json(t)
	}
	feed := "h/good-reads/feed.xml"
	if _, ok := manifest()["hoppers"]; ok {
		t.Fatal("a private hopper must not be listed (§6.1)")
	}
	expect(t, alice.get(feed), 404)

	expect(t, alice.api("PATCH", "/hoppers/"+hid, map[string]any{"public": true}), 200)
	hs, _ := manifest()["hoppers"].([]any)
	if len(hs) != 1 || fmt.Sprint(hs[0]) != "map[feed:h/good-reads/feed.xml page:h/good-reads/ title:Good reads]" {
		t.Fatalf("manifest hoppers: %v", hs)
	}
	r = alice.get(feed)
	expect(t, r, 200)
	body := string(r.body)
	if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/rss+xml") || r.header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("headers: %v", r.header)
	}
	if strings.Contains(body, "blyg:") || strings.Contains(body, blyg.Namespace) {
		t.Errorf("rule (b): no blyg: namespace anywhere, own items included:\n%s", body)
	}
	if n := strings.Count(body, "<item>"); n != 3 {
		t.Fatalf("rule (c): every current member, got %d:\n%s", n, body)
	}
	for _, want := range []string{
		`<link>` + alice.origin + `f/` + own + `/</link>`,
		`<link>` + bob.origin + `f/` + theirs + `/</link>`,
		`<link>` + remote.Origin() + `docs/` + pageless + `.json</link>`,
		`<source url="` + bob.origin + `feed.xml">Bob&#39;s blyg</source>`,
		`<source url="` + remote.Origin() + `feed.xml">Remote</source>`,
		`<guid isPermaLink="false">` + blyg.HopperGUID(alice.origin+"h/good-reads/", alice.origin, own) + `</guid>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("feed lacks %s:\n%s", want, body)
		}
	}
	if strings.Count(body, "<source ") != 2 {
		t.Errorf("rule (d): <source> for every member not the publisher's own:\n%s", body)
	}
	if strings.Index(body, pageless) > strings.Index(body, own) {
		t.Errorf("newest-joined first:\n%s", body)
	}
	pg := string(alice.get("h/good-reads/").body)
	if !strings.Contains(pg, `<link rel="alternate" type="application/rss+xml" title="Good reads" href="`+alice.origin+feed+`">`) {
		t.Errorf("the hopper page should link its feed:\n%s", pg)
	}
	expect(t, alice.get(feed, "If-None-Match", r.header.Get("ETag")), 304)

	// Rule (c): a member that leaves leaves the feed at once.
	h := alice.api("GET", "/hoppers/"+hid, nil).json(t)
	mid := itoa(int(h["members"].([]any)[0].(map[string]any)["id"].(float64)))
	expect(t, alice.api("DELETE", "/hoppers/"+hid+"/members/"+mid, nil), 204)
	if n := strings.Count(string(alice.get(feed).body), "<item>"); n != 2 {
		t.Errorf("after removing a member: %d items", n)
	}

	// A rename follows into the manifest; the slug stays.
	expect(t, alice.api("PATCH", "/hoppers/"+hid, map[string]any{"name": "Better reads"}), 200)
	if hs, _ := manifest()["hoppers"].([]any); len(hs) != 1 || hs[0].(map[string]any)["title"] != "Better reads" {
		t.Errorf("renamed: %v", hs)
	}

	// Rule (a): taken private, it leaves the list and its feed 404s.
	expect(t, alice.api("PATCH", "/hoppers/"+hid, map[string]any{"public": false}), 200)
	if _, ok := manifest()["hoppers"]; ok {
		t.Error("a hopper taken private is still listed")
	}
	expect(t, alice.get(feed), 404)
	// The schema-level copy is what the manifest reads, and it is gone.
	var n int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM public_hoppers`).Scan(&n)
	if n != 0 {
		t.Errorf("%d public_hoppers rows after going private", n)
	}

	// Public again, then deleted: unlisted first.
	expect(t, alice.api("PATCH", "/hoppers/"+hid, map[string]any{"public": true}), 200)
	expect(t, alice.get(feed), 200)
	expect(t, alice.api("DELETE", "/hoppers/"+hid, nil), 204)
	if _, ok := manifest()["hoppers"]; ok {
		t.Error("a deleted hopper is still listed")
	}
	expect(t, alice.get(feed), 404)
	// A static export has no hopper pages, so its manifest names none.
	expect(t, alice.api("PATCH", "/hoppers/"+itoa(int(alice.api("POST", "/hoppers", map[string]any{"name": "Kept"}).json(t)["id"].(float64))), map[string]any{"public": true}), 200)
	if _, ok := manifest()["hoppers"]; !ok {
		t.Fatal("the second public hopper is not listed")
	}
	r = alice.api("GET", "/origin/export", nil)
	expect(t, r, 200)
	if m := untar(t, r.body)["blyg.json"]; m == nil || strings.Contains(string(m), "hoppers") {
		t.Errorf("an export's manifest names live hopper pages it does not hold: %s", m)
	}
}
