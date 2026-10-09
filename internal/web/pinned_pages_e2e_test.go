package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chrisbodhi/blygd/internal/mention"
	"github.com/chrisbodhi/blygd/internal/testblyg"
)

// Pinned version pages (§8.4): gated exactly like the JSON pin, 200 forever,
// marked frozen, canonical to the live page, linked to their JSON twin,
// never in the feed or index, and sanitized like every page we serve.
func TestPinnedVersionPages(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice := e.user("alice")

	id := alice.draft(t, map[string]any{"content_md": "First words."})
	alice.publish(t, id, 200)
	alice.edit(t, id, "Second words.")
	alice.publish(t, id, 200)
	v1 := "f/" + id + "/v1/"
	expect(t, alice.get(v1), 404) // unpinned history is never served (§8.4 rule 1)

	expect(t, alice.api("POST", "/items/"+id+"/versions/1/pin", nil), 200)
	r := alice.get(v1)
	expect(t, r, 200)
	page := string(r.body)
	for _, want := range []string{
		"<p>First words.</p>",
		`<link rel="canonical" href="` + alice.origin + "f/" + id + `/">`,
		`<link rel="alternate" type="application/json" href="` + alice.origin + "items/" + id + `/v1.json">`,
		"Pinned snapshot: version 1",
		"current version</a> (v2)",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("pinned page lacks %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, "Second words") || strings.Contains(page, "webmention") || r.header.Get("Link") != "" {
		t.Errorf("pinned page leaks the live version or advertises Webmention (§15.3): %v\n%s", r.header, page)
	}
	expect(t, alice.get("f/"+id+"/v2/"), 404)

	// The live page cites the pin by its page; feed and index never list it.
	if live := string(alice.get("f/" + id + "/").body); !strings.Contains(live, `<a href="`+alice.origin+v1+`">v1</a>`) {
		t.Errorf("live page pin citation:\n%s", live)
	}
	for _, path := range []string{"feed.xml", "items/index.json", ""} {
		if strings.Contains(string(alice.get(path).body), "/v1/") {
			t.Errorf("%q lists a pinned page (§8.4 rule 4)", path)
		}
	}

	// Withdrawal: still 200, and the chrome says so; the page is permanent
	// but re-renderable, unlike its write-once JSON twin.
	expect(t, alice.api("POST", "/items/"+id+"/withdraw", nil), 200)
	r = alice.get(v1)
	expect(t, r, 200)
	if !strings.Contains(string(r.body), "has since been <a") || !strings.Contains(string(r.body), "First words.") {
		t.Errorf("pinned page after withdrawal:\n%s", r.body)
	}
	var permanent, immutable bool
	if err := e.pool.QueryRow(context.Background(), `SELECT permanent, immutable FROM surface_files WHERE path = $1`, v1).Scan(&permanent, &immutable); err != nil || !permanent || immutable {
		t.Errorf("pinned page flags: permanent=%v immutable=%v err=%v", permanent, immutable, err)
	}
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM surface_files WHERE path = $1`, v1); err == nil {
		t.Error("a pinned page was deletable")
	}

	// An own fork's lineage links the source's pinned page (§5.6 rule 7).
	fork := alice.draft(t, map[string]any{"forked_from": map[string]any{"origin": alice.origin, "id": id, "version": 1}})
	alice.publish(t, fork, 200)
	if fp := string(alice.get("f/" + fork + "/").body); !strings.Contains(fp, `Forked from <a href="`+alice.origin+v1+`">`) {
		t.Errorf("own fork lineage link:\n%s", fp)
	}
}

// A pinned thread's page shows its baked quotes sanitized; the JSON twin
// keeps them verbatim (DESIGN.md: the deliberate departure from §8.4 rule 2).
func TestPinnedThreadPageIsSanitized(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	bob := e.user("bob")
	remote := &testblyg.Blyg{S: testblyg.New(t), Mount: "/r/", Title: "Remote"}
	qid := "0r00000000000000000000000r"
	remote.Items = []*testblyg.Item{{ID: qid, Kind: "fragment", Version: 1, Updated: time.Now(), ContentMD: `quoted <script>alert(1)</script>`}}
	remote.Publish()
	bob.subscribe(t, remote.Origin())

	th := bob.draft(t, map[string]any{"kind": "thread", "content_md": "![[" + qid + "]]"})
	bob.publish(t, th, 200)
	expect(t, bob.api("POST", "/items/"+th+"/versions/1/pin", nil), 200)
	if !strings.Contains(string(bob.get("items/"+th+"/v1.json").body), `<script>alert(1)</script>`) {
		t.Error("the JSON pin must keep the baked bytes verbatim")
	}
	page := string(bob.get("t/" + th + "/v1/").body)
	if !strings.Contains(page, "blyg-transclusion") || !strings.Contains(page, "quoted") || strings.Contains(page, "<script>alert") {
		t.Errorf("pinned thread page:\n%s", page)
	}
}
