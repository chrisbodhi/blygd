package web

import (
	"strings"
	"testing"

	"github.com/chrisbodhi/blygd/internal/mention"
)

// A listing carries what each item's current version shows, so a client
// can render it as readers see it, and flags drafts edited since.
func TestItemListShowsCurrentVersion(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice := e.user("alice")

	f := alice.draft(t, map[string]any{"content_md": "Quoted *words*."})
	alice.publish(t, f, 200)
	th := alice.draft(t, map[string]any{"kind": "thread", "content_md": "![[" + f + "]]\n\nComment."})
	alice.publish(t, th, 200)
	d := alice.draft(t, map[string]any{"content_md": "Not yet."})
	// The wire drops [TK] grammar (§5.7), so a draft carrying it is not a change.
	tk := alice.draft(t, map[string]any{"content_md": "Mine [TK]impyrt=pasted[/TK]."})
	alice.publish(t, tk, 200)

	list := func() map[string]map[string]any {
		t.Helper()
		r := alice.api("GET", "/items", nil)
		expect(t, r, 200)
		out := map[string]map[string]any{}
		for _, it := range r.json(t)["items"].([]any) {
			m := it.(map[string]any)
			out[m["id"].(string)] = m
		}
		return out
	}

	got := list()
	html, _ := got[th]["content_html"].(string)
	if !strings.Contains(html, `class="blyg-transclusion"`) || !strings.Contains(html, "Quoted <em>words</em>.") || !strings.Contains(html, "Comment.") {
		t.Errorf("thread content_html: %q", html)
	}
	if _, ok := got[d]["content_html"]; ok {
		t.Errorf("never-published draft has content_html: %v", got[d])
	}
	for _, id := range []string{f, th, d, tk} {
		if _, ok := got[id]["unpublished_changes"]; ok {
			t.Errorf("%s: unpublished_changes before any edit", id)
		}
	}

	// A save that changes nothing is not an unpublished change.
	alice.edit(t, th, "![["+f+"]]\n\nComment.")
	if _, ok := list()[th]["unpublished_changes"]; ok {
		t.Error("no-op save flagged as an unpublished change")
	}

	alice.edit(t, th, "![["+f+"]]\n\nSecond thoughts.")
	got = list()
	if got[th]["unpublished_changes"] != true {
		t.Errorf("edit after publish not flagged: %v", got[th])
	}
	if html, _ := got[th]["content_html"].(string); !strings.Contains(html, "Comment.") || strings.Contains(html, "Second thoughts.") {
		t.Errorf("content_html should stay the published version: %q", html)
	}

	alice.publish(t, th, 200)
	if _, ok := list()[th]["unpublished_changes"]; ok {
		t.Error("still flagged after publishing the edit")
	}

	expect(t, alice.api("POST", "/items/"+f+"/withdraw", nil), 200)
	if _, ok := list()[f]["content_html"]; ok {
		t.Errorf("withdrawn item has content_html: %v", list()[f])
	}
}
