package web

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/chrisbodhi/blygd/internal/mention"
)

// Responses on a page (§15.5) are the owner's curation, and the defaults
// assume someone may be trying to use them against the owner.
func TestResponsesAreCurated(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice, bob := e.user("alice"), e.user("bob")
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
	page := func(id string) string { return string(alice.get("f/" + id + "/").body) }
	mentions := func(q string) []map[string]any {
		var out []map[string]any
		for _, m := range alice.api("GET", "/mentions"+q, nil).json(t)["mentions"].([]any) {
			out = append(out, m.(map[string]any))
		}
		return out
	}

	target := alice.draft(t, map[string]any{"content_md": "A claim."})
	alice.publish(t, target, 200)
	expect(t, bob.api("PATCH", "/origin", map[string]any{"byline": map[string]any{"name": "Bob"}}), 200)
	bob.subscribe(t, alice.origin)
	stub := bob.draft(t, map[string]any{"stub_of": map[string]any{"id": target}})
	bob.edit(t, stub, "![["+target+"]]\n\nA reasonable reply.")
	bob.publish(t, stub, 200)
	run()

	// Verified is not shown: nothing appears until alice approves.
	ms := mentions("")
	if len(ms) != 1 || ms[0]["status"] != "verified" || ms[0]["approved_at"] != nil {
		t.Fatalf("mentions: %v", ms)
	}
	mid := itoa(int(ms[0]["id"].(float64)))
	if strings.Contains(page(target), "Responses") {
		t.Fatal("a verified mention showed without approval")
	}
	mod := alice.api("POST", "/tokens", map[string]any{"name": "moderator", "scopes": []string{"mentions:write"}}).json(t)["token"].(string)
	expect(t, e.do("PUT", e.srv.URL+"/api/mentions/"+mid+"/approval", mod, nil), 403) // showing publishes
	expect(t, alice.api("PUT", "/mentions/"+mid+"/approval", nil), 204)
	p := page(target)
	if !strings.Contains(p, "Bob, at <a href=\""+bob.origin+"t/"+stub+"/\" rel=\"nofollow ugc\">127.0.0.1</a>, responded") ||
		strings.Contains(p, "reasonable reply") {
		t.Fatalf("shown response (verb, author, host, link; no words):\n%s", p)
	}

	// The swap: once approved, the stubber changes what would show and
	// re-sends. Approval does not survive the change.
	expect(t, bob.api("PATCH", "/origin", map[string]any{"byline": map[string]any{"name": "Something abusive"}}), 200)
	bob.edit(t, stub, "![["+target+"]]\n\nA reasonable reply, edited.")
	bob.publish(t, stub, 200)
	resend := url.Values{"source": {bob.origin + "t/" + stub + "/"}, "target": {alice.origin + "f/" + target + "/"}}
	expect(t, e.do("POST", alice.origin+"webmention", "", resend), 202)
	run()
	if p = page(target); strings.Contains(p, "abusive") || strings.Contains(p, "Responses") {
		t.Fatalf("a re-sent mention kept its approval after changing:\n%s", p)
	}
	if m := mentions("")[0]; m["status"] != "verified" || m["approved_at"] != nil {
		t.Fatalf("changed mention: %v", m)
	}

	// Hiding works with moderation alone.
	expect(t, alice.api("PUT", "/mentions/"+mid+"/approval", nil), 204)
	expect(t, e.do("DELETE", e.srv.URL+"/api/mentions/"+mid+"/approval", mod, nil), 204)
	if strings.Contains(page(target), "Responses") {
		t.Fatal("hide did not take the response off the page")
	}

	// Blocking: existing approvals go, the list hides the source, and new
	// claims are answered like any other and dropped.
	expect(t, alice.api("PUT", "/mentions/"+mid+"/approval", nil), 204)
	r := e.do("POST", e.srv.URL+"/api/mention-blocks", mod, map[string]any{"kind": "host", "value": bob.origin})
	expect(t, r, 201)
	bid := itoa(int(r.json(t)["id"].(float64)))
	if strings.Contains(page(target), "Responses") || len(mentions("")) != 0 {
		t.Fatal("a block left the response shown or listed")
	}
	if all := mentions("?include_blocked=true"); len(all) != 1 || all[0]["blocked"] != true {
		t.Fatalf("include_blocked: %v", all)
	}
	var pending int
	expect(t, e.do("POST", alice.origin+"webmention", "", resend), 202)
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM inbound_mentions WHERE status = 'pending'`).Scan(&pending)
	if pending != 0 {
		t.Fatal("a blocked source's claim was queued")
	}
	expect(t, alice.api("PUT", "/mentions/"+mid+"/approval", nil), 204) // still verified; the owner may choose to
	expect(t, alice.api("DELETE", "/mentions/"+mid+"/approval", nil), 204)
	expect(t, e.do("DELETE", e.srv.URL+"/api/mention-blocks/"+bid, mod, nil), 204)
	if mentions("")[0]["approved_at"] != nil {
		t.Fatal("unblocking restored an approval")
	}

	// A withdrawn stub stops verifying, and its response leaves the page.
	expect(t, alice.api("PUT", "/mentions/"+mid+"/approval", nil), 204)
	expect(t, bob.api("POST", "/items/"+stub+"/withdraw", nil), 200)
	run()
	if m := mentions("")[0]; m["status"] != "gone" || m["approved_at"] != nil || strings.Contains(page(target), "Responses") {
		t.Fatalf("withdrawn stub: %v", m)
	}
	expect(t, alice.api("PUT", "/mentions/"+mid+"/approval", nil), 409) // only verified mentions show
	if _, err := e.pool.Exec(ctx, `UPDATE inbound_mentions SET approved_at = now() WHERE id = $1`, mid); err == nil {
		t.Error("the schema let a gone mention be approved")
	}
}
