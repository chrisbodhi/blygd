package web

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

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

// A withdrawn approval and the page that stops showing it commit together
// (render inside the transaction). If the render fails, neither happens and
// the claim comes due again; with the old split, the page kept showing a
// response the database no longer approved, and nothing retried it.
func TestWithdrawnApprovalCommitsWithItsPage(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice, bob := e.user("alice"), e.user("bob")
	ctx := context.Background()
	page := func(id string) string { return string(alice.get("f/" + id + "/").body) }
	approved := func(mid string) bool {
		var ok bool
		if err := e.pool.QueryRow(ctx, `SELECT approved_at IS NOT NULL FROM inbound_mentions WHERE id = $1`, mid).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}

	target := alice.draft(t, map[string]any{"content_md": "A claim."})
	alice.publish(t, target, 200)
	bob.subscribe(t, alice.origin)
	stub := bob.draft(t, map[string]any{"stub_of": map[string]any{"id": target}})
	bob.edit(t, stub, "![["+target+"]]\n\nA reply.")
	bob.publish(t, stub, 200)
	if _, err := e.workers.SendBatch(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.workers.VerifyBatch(ctx); err != nil {
		t.Fatal(err)
	}
	ms := alice.api("GET", "/mentions", nil).json(t)["mentions"].([]any)
	mid := itoa(int(ms[0].(map[string]any)["id"].(float64)))
	expect(t, alice.api("PUT", "/mentions/"+mid+"/approval", nil), 204)
	if !strings.Contains(page(target), "Responses") {
		t.Fatal("approved response not shown")
	}

	// The stub is withdrawn, and the re-render after the update fails.
	expect(t, bob.api("POST", "/items/"+stub+"/withdraw", nil), 200)
	if _, err := e.workers.SendBatch(ctx); err != nil {
		t.Fatal(err)
	}
	real := e.workers.Display
	e.workers.Display = func(ctx context.Context, originID int64, change func(context.Context, pgx.Tx) (string, bool, error)) error {
		return real(ctx, originID, func(ctx context.Context, tx pgx.Tx) (string, bool, error) {
			id, changed, err := change(ctx, tx)
			if changed {
				return "", false, errors.New("render failed")
			}
			return id, changed, err
		})
	}
	if _, err := e.workers.VerifyBatch(ctx); err == nil {
		t.Fatal("a failed render was not reported")
	}
	if !approved(mid) || !strings.Contains(page(target), "Responses") {
		t.Fatalf("approved %v; the database and the page disagree after a failed render", approved(mid))
	}

	// Next time round the render works, and both change.
	e.workers.Display = real
	if _, err := e.pool.Exec(ctx, `UPDATE inbound_mentions SET next_attempt_at = now() WHERE id = $1`, mid); err != nil {
		t.Fatal(err)
	}
	if _, err := e.workers.VerifyBatch(ctx); err != nil {
		t.Fatal(err)
	}
	if approved(mid) || strings.Contains(page(target), "Responses") {
		t.Fatalf("approved %v after the retry; the response should be off the page", approved(mid))
	}
}

// Notices are held apart from responses (§15.5, decisions #79 and #80): a
// repost verifies with its relation unchanged, is classed a notice by the
// receiver, and can never be shown among responses.
func TestNoticesAreHeldApart(t *testing.T) {
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
	mentions := func(q string) []map[string]any {
		var out []map[string]any
		for _, m := range alice.api("GET", "/mentions"+q, nil).json(t)["mentions"].([]any) {
			out = append(out, m.(map[string]any))
		}
		return out
	}
	page := func(id string) string { return string(alice.get("f/" + id + "/").body) }

	target := alice.draft(t, map[string]any{"content_md": "A claim."})
	alice.publish(t, target, 200)
	bob.subscribe(t, alice.origin)
	th := bob.draft(t, map[string]any{"kind": "thread", "content_md": "![[" + target + "]]"})
	bob.publish(t, th, 200)
	run()

	ms := mentions("")
	if len(ms) != 1 || ms[0]["status"] != "verified" || ms[0]["relation"] != "transclusion" || ms[0]["class"] != "notice" {
		t.Fatalf("a repost: %v", ms)
	}
	mid := itoa(int(ms[0]["id"].(float64)))
	expect(t, alice.api("PUT", "/mentions/"+mid+"/approval", nil), 409)
	if len(mentions("?class=response")) != 0 || len(mentions("?class=notice")) != 1 {
		t.Error("the class filter does not hold notices apart")
	}
	expect(t, alice.api("GET", "/mentions?class=other", nil), 400)

	// Words of its own make it a response, which may be shown; losing them
	// again makes it a notice, and the approval goes with the class.
	resend := url.Values{"source": {bob.origin + "t/" + th + "/"}, "target": {alice.origin + "f/" + target + "/"}}
	bob.edit(t, th, "![["+target+"]]\n\nWords of my own.")
	bob.publish(t, th, 200)
	expect(t, e.do("POST", alice.origin+"webmention", "", resend), 202)
	run()
	if m := mentions("")[0]; m["class"] != "response" {
		t.Fatalf("a quote with words: %v", m)
	}
	expect(t, alice.api("PUT", "/mentions/"+mid+"/approval", nil), 204)
	if !strings.Contains(page(target), "quoted this") {
		t.Fatal("an approved response is not shown")
	}
	bob.edit(t, th, "![["+target+"]]")
	bob.publish(t, th, 200)
	expect(t, e.do("POST", alice.origin+"webmention", "", resend), 202)
	run()
	if m := mentions("")[0]; m["class"] != "notice" || m["approved_at"] != nil || strings.Contains(page(target), "quoted this") {
		t.Fatalf("a response that became a repost kept its approval: %v", m)
	}

	// The schema holds the classes whatever the Go does.
	for _, set := range []string{
		"approved_at = now()",                   // a notice shown
		"relation = 'link', class = 'response'", // a link is always a notice
		"class = NULL",                          // class goes with relation
		"class = 'other'",
	} {
		if _, err := e.pool.Exec(ctx, `UPDATE inbound_mentions SET `+set+` WHERE id = $1`, mid); err == nil {
			t.Errorf("the schema allowed %s", set)
		}
	}
	for _, set := range []string{"relation = 'source', class = 'response'", "relation = 'link', class = 'notice'"} {
		if _, err := e.pool.Exec(ctx, `UPDATE inbound_mentions SET `+set+` WHERE id = $1`, mid); err != nil {
			t.Errorf("the schema refused %s: %v", set, err)
		}
	}
	for rel, ok := range map[string]bool{"source": true, "link": true, "bogus": false} {
		_, err := e.pool.Exec(ctx, `UPDATE outbound_mentions SET relation = $1`, rel)
		if (err == nil) != ok {
			t.Errorf("outbound relation %s: %v", rel, err)
		}
	}
}
