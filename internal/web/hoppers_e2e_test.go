package web

import (
	"context"
	"strings"
	"testing"
	"time"

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
		`<a href="` + bob.origin + `f/` + pinned + `/">read it there</a>`, `<a href="` + bob.origin + `">Bob&#39;s blyg</a>`} {
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
