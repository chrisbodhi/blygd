package web

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/chrisbodhi/blygd/internal/blyg"
	"github.com/chrisbodhi/blygd/internal/mention"
	"github.com/chrisbodhi/blygd/internal/testblyg"
)

func (u *user) publish(t *testing.T, id string, status int) map[string]any {
	t.Helper()
	r := u.api("POST", "/items/"+id+"/publish", nil)
	expect(t, r, status)
	return r.json(t)
}

func (u *user) edit(t *testing.T, id, md string) {
	t.Helper()
	expect(t, u.api("PATCH", "/items/"+id, map[string]any{"content_md": md}), 200)
}

func (u *user) subscribe(t *testing.T, to string) int {
	t.Helper()
	r := u.api("POST", "/subscriptions", map[string]any{"url": to})
	expect(t, r, 201)
	sid := int(r.json(t)["subscription"].(map[string]any)["id"].(float64))
	u.e.poll(sid)
	return sid
}

// problems returns the reasons a 422 publish listed.
func problems(t *testing.T, m map[string]any) string {
	t.Helper()
	ps, ok := m["problems"].([]any)
	if !ok || len(ps) == 0 {
		t.Fatalf("422 without problems: %v", m)
	}
	var out []string
	for _, p := range ps {
		out = append(out, p.(map[string]any)["reason"].(string))
	}
	return strings.Join(out, " | ")
}

// compact strips a document's indentation for comparison.
func compact(t *testing.T, raw []byte) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func refsJSON(t *testing.T, d blyg.ItemDoc) string {
	t.Helper()
	b, _ := json.Marshal(d.Transclusions)
	return string(b)
}

func TestThreadsLocal(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice := e.user("alice")

	f := alice.draft(t, map[string]any{"content_md": "Stigmergy is *the* point.\n\nSecond para."})
	alice.publish(t, f, 200)

	th := alice.draft(t, map[string]any{"kind": "thread", "content_md": "Intro [[" + f + "]].\n\n![[" + f + "]]\n> the point.\n\n![[" + f + "]]\n"})
	alice.publish(t, th, 200)
	d := alice.itemDoc(t, th)
	if d.Kind != "thread" || d.Page != "t/"+th+"/" || d.ContentMD != "Intro [["+f+"]].\n\n![["+f+"]]\n> the point.\n\n![["+f+"]]\n" {
		t.Fatalf("thread doc: %+v", d)
	}
	if got := refsJSON(t, d); got != `[{"id":"`+f+`","version":1,"selector":{"exact":"the point.","prefix":"Stigmergy is ","suffix":"\nSecond para."}},{"id":"`+f+`","version":1}]` {
		t.Fatalf("transclusions: %s", got)
	}
	for _, want := range []string{
		`<a href="` + alice.origin + `f/` + f + `/">“Stigmergy is the point. Second para.”</a>`,
		`<blockquote class="blyg-transclusion blyg-partial" data-blyg-id="` + f + `" data-blyg-version="1">` + "\n<p>the point.</p>\n</blockquote>",
		`<blockquote class="blyg-transclusion" data-blyg-id="` + f + `" data-blyg-version="1">` + "\n<p>Stigmergy is <em>the</em> point.</p>",
	} {
		if !strings.Contains(d.ContentHTML, want) {
			t.Errorf("content_html missing %s:\n%s", want, d.ContentHTML)
		}
	}
	if strings.Contains(d.ContentHTML, "data-blyg-origin") {
		t.Error("own-origin bakes carry no data-blyg-origin (§10.2)")
	}
	// The thread page and the feed carry the baked HTML (§10.5).
	if !strings.Contains(string(alice.get("t/"+th+"/").body), "blyg-transclusion") ||
		!strings.Contains(string(alice.get("feed.xml").body), "blyg-transclusion") {
		t.Error("thread page or feed lacks the bake")
	}
	// Provenance is part of the published version: the schema refuses to
	// rewrite it (§10.3, §10.6 rule 4, §5.7 rule 5).
	for _, col := range []string{"transclusions = '[]'", "stub_of = '{}'", "generated = '[]'", "author = '{}'", "note_generated = true"} {
		if _, err := e.pool.Exec(context.Background(), `UPDATE item_versions SET `+col+` WHERE item_id = $1 AND version = 1`, th); err == nil {
			t.Errorf("published version allowed %s", col)
		}
	}
	if out := alice.api("GET", "/outbound", nil).json(t)["outbound"].([]any); len(out) != 0 {
		t.Errorf("own-origin quotes send no mentions: %v", out)
	}

	// Unchanged text, unchanged sources: nothing to publish. A newer source
	// version re-resolves on republish (§10.2).
	alice.publish(t, th, 409)
	alice.edit(t, f, "Stigmergy is *the* point.\n\nSecond para, sharpened.")
	alice.publish(t, f, 200)
	alice.publish(t, th, 200)
	if d = alice.itemDoc(t, th); d.Version != 2 || !strings.Contains(d.ContentHTML, `data-blyg-version="2"`) {
		t.Fatalf("republish did not re-resolve: %s", d.ContentHTML)
	}

	// The local closure check: t2 quotes th, so th may not quote t2.
	t2 := alice.draft(t, map[string]any{"kind": "thread", "content_md": "![[" + th + "]]"})
	alice.publish(t, t2, 200)
	alice.edit(t, th, "![["+t2+"]]")
	if p := problems(t, alice.publish(t, th, 422)); !strings.Contains(p, "already quotes this one") {
		t.Errorf("closure: %s", p)
	}
	self := alice.draft(t, map[string]any{"kind": "thread", "content_md": "x"})
	alice.edit(t, self, "![["+self+"]]")
	if p := problems(t, alice.publish(t, self, 422)); !strings.Contains(p, "draft") {
		t.Errorf("self before first publish: %s", p)
	}

	// Drafts, withdrawn items, reserved versions and stray directives in a
	// fragment are all publish errors, reported together.
	dr := alice.draft(t, map[string]any{"content_md": "unpublished"})
	alice.edit(t, th, "![["+dr+"]]\n\n![["+f+"@v1]]\n\nsee [[0000000000000000000000000z]]")
	p := problems(t, alice.publish(t, th, 422))
	for _, want := range []string{"draft", "reserved", "unknown item"} {
		if !strings.Contains(p, want) {
			t.Errorf("problems %q lack %q", p, want)
		}
	}
	expect(t, alice.api("POST", "/items/"+f+"/withdraw", nil), 200)
	alice.edit(t, th, "![["+f+"]]")
	if p := problems(t, alice.publish(t, th, 422)); !strings.Contains(p, "withdrawn") {
		t.Errorf("withdrawn source: %s", p)
	}
	frag := alice.draft(t, map[string]any{"content_md": "![[" + t2 + "]]"})
	if p := problems(t, alice.publish(t, frag, 422)); !strings.Contains(p, "only threads transclude") {
		t.Errorf("fragment directive: %s", p)
	}

	// The withdrawn thread's endcap carries transclusions [] (§9, §10.3).
	expect(t, alice.api("POST", "/items/"+t2+"/withdraw", nil), 200)
	if d = alice.itemDoc(t, t2); d.Transclusions == nil || len(*d.Transclusions) != 0 {
		t.Errorf("endcap transclusions: %+v", d.Transclusions)
	}
}

func TestThreadsImported(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice, bob, carol := e.user("alice"), e.user("bob"), e.user("carol")
	ctx := context.Background()

	pinned := alice.draft(t, map[string]any{"content_md": "Pinned words, see [rel](notes/)."})
	alice.publish(t, pinned, 200)
	expect(t, alice.api("POST", "/items/"+pinned+"/versions/1/pin", nil), 200)
	loose := alice.draft(t, map[string]any{"content_md": "Unpinned words."})
	alice.publish(t, loose, 200)
	sid := bob.subscribe(t, alice.origin)

	th := bob.draft(t, map[string]any{"kind": "thread", "content_md": "![[" + pinned + "]]\n\n![[" + loose + "]]\n> Unpinned"})
	res := bob.publish(t, th, 200)
	warn, _ := json.Marshal(res["warnings"])
	if !strings.Contains(string(warn), loose) || strings.Contains(string(warn), pinned) {
		t.Errorf("§10.2: warn about the unpinned remote quote only: %s", warn)
	}
	d := bob.itemDoc(t, th)
	if !strings.Contains(d.ContentHTML, `data-blyg-id="`+pinned+`" data-blyg-version="1" data-blyg-origin="`+alice.origin+`"`) ||
		!strings.Contains(d.ContentHTML, `href="`+alice.origin+`f/`+pinned+`/notes/"`) {
		t.Fatalf("remote bake: %s", d.ContentHTML)
	}
	refs := *d.Transclusions
	if len(refs) != 2 || refs[0].Origin != alice.origin || refs[1].Selector == nil {
		t.Fatalf("transclusions: %s", refsJSON(t, d))
	}
	if c := refs[0].Cited; c == nil || c.Source != "Alice's blyg" || c.URL != alice.origin+"f/"+pinned+"/" || c.Retrieved == "" || c.Excerpt != "Pinned words, see rel." {
		t.Errorf("cited: %+v", c)
	}

	// Each remote quote notifies its origin (§10.3), and alice verifies it.
	if n, err := e.workers.SendBatch(ctx); err != nil || n != 2 {
		t.Fatalf("send: %d %v", n, err)
	}
	if n, err := e.workers.VerifyBatch(ctx); err != nil || n != 2 {
		t.Fatalf("verify: %d %v", n, err)
	}
	ms := alice.api("GET", "/mentions", nil).json(t)["mentions"].([]any)
	if len(ms) != 2 || ms[0].(map[string]any)["status"] != "verified" || ms[0].(map[string]any)["relation"] != "transclusion" {
		t.Fatalf("mentions: %v", ms)
	}

	// Quoting needs a subscription: carol has none.
	ct := carol.draft(t, map[string]any{"kind": "thread", "content_md": "![[" + pinned + "]]"})
	if p := problems(t, carol.publish(t, ct, 422)); !strings.Contains(p, "unknown item") {
		t.Errorf("unsubscribed: %s", p)
	}

	// The same id from a second origin is ambiguous, never a guess (§10.2).
	other := &testblyg.Blyg{S: testblyg.New(t), Mount: "/b/", Title: "Other"}
	other.Items = []*testblyg.Item{{ID: pinned, Kind: "fragment", Version: 1, ContentMD: "collision", Updated: time.Now()}}
	other.Publish()
	osid := bob.subscribe(t, other.Origin())
	bob.edit(t, th, "![["+pinned+"]]")
	if p := problems(t, bob.publish(t, th, 422)); !strings.Contains(p, "ambiguous") {
		t.Errorf("ambiguity: %s", p)
	}
	expect(t, bob.api("DELETE", "/subscriptions/"+itoa(osid), nil), 204)

	// Withdrawal at the origin: a pin-retained snapshot stays quotable,
	// anything else is a publish error on republish (§10.2, §13.4).
	expect(t, alice.api("POST", "/items/"+pinned+"/withdraw", nil), 200)
	expect(t, alice.api("POST", "/items/"+loose+"/withdraw", nil), 200)
	e.poll(sid)
	bob.edit(t, th, "Now:\n\n![["+pinned+"]]")
	bob.publish(t, th, 200)
	if d = bob.itemDoc(t, th); !strings.Contains(d.ContentHTML, "Pinned words") || (*d.Transclusions)[0].Version != 1 {
		t.Fatalf("retained pin: %s", d.ContentHTML)
	}
	bob.edit(t, th, "![["+loose+"]]")
	if p := problems(t, bob.publish(t, th, 422)); !strings.Contains(p, "withdrawn by its origin") {
		t.Errorf("withdrawn import: %s", p)
	}
}

func TestStubs(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice, bob := e.user("alice"), e.user("bob")
	ctx := context.Background()

	f := alice.draft(t, map[string]any{"content_md": "A claim worth answering."})
	alice.publish(t, f, 200)
	bob.subscribe(t, alice.origin)

	// A stub starts with the quote of its target (§10.6 rule 2).
	r := bob.api("POST", "/items", map[string]any{"stub_of": map[string]any{"id": f}})
	expect(t, r, 201)
	created := r.json(t)
	st := created["id"].(string)
	if created["kind"] != "thread" || created["draft_md"] != "![["+f+"]]\n\n" {
		t.Fatalf("stub draft: %s", r.body)
	}
	bob.edit(t, st, "![["+f+"]]\n\nI disagree.")

	// The target moves on before bob publishes: version agreement makes
	// stub_of name the version actually baked (§10.6 rule 3).
	alice.edit(t, f, "A claim worth answering, revised.")
	alice.publish(t, f, 200)
	e.poll(1)
	bob.publish(t, st, 200)
	d := bob.itemDoc(t, st)
	var stub blyg.StubOf
	if err := json.Unmarshal(d.StubOf, &stub); err != nil {
		t.Fatal(err)
	}
	if stub.Origin != alice.origin || stub.ID != f || stub.Version != 2 || (*d.Transclusions)[0].Version != 2 {
		t.Fatalf("stub_of: %s / %s", d.StubOf, refsJSON(t, d))
	}
	// cited is frozen at creation: it still reads v1's words.
	if !strings.HasPrefix(compact(t, d.StubOf), `{"origin":`) || stub.Cited.Excerpt != "A claim worth answering." || stub.Cited.Retrieved == "" {
		t.Errorf("cited: %v", stub.Cited)
	}

	// A response by link keeps the version set when the stub was created.
	bob.edit(t, st, "I disagree, without quoting.")
	bob.publish(t, st, 200)
	if d = bob.itemDoc(t, st); !strings.Contains(compact(t, d.StubOf), `"version":1,`) {
		t.Errorf("response by link: %s", d.StubOf)
	}

	// Alice verifies the stub; a withdrawn stub's endcap drops stub_of.
	if _, err := e.workers.SendBatch(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.workers.VerifyBatch(ctx); err != nil {
		t.Fatal(err)
	}
	var rel []string
	for _, m := range alice.api("GET", "/mentions", nil).json(t)["mentions"].([]any) {
		rel = append(rel, m.(map[string]any)["relation"].(string)+"/"+m.(map[string]any)["status"].(string))
	}
	if strings.Join(rel, ",") != "stub/verified" {
		t.Errorf("mentions: %v", rel)
	}
	expect(t, bob.api("POST", "/items/"+st+"/withdraw", nil), 200)
	if d = bob.itemDoc(t, st); d.StubOf != nil {
		t.Errorf("endcap keeps stub_of: %s", d.StubOf)
	}

	// A plain-web stub carries the client's label, dated.
	r = bob.api("POST", "/items", map[string]any{"stub_of": map[string]any{"url": "https://simon.test/post/",
		"cited": map[string]any{"source": "Simon's weblog", "excerpt": strings.Repeat("x", 300)}}})
	expect(t, r, 201)
	web := r.json(t)["id"].(string)
	bob.edit(t, web, "A response.")
	bob.publish(t, web, 200)
	d = bob.itemDoc(t, web)
	var ws blyg.StubOf
	_ = json.Unmarshal(d.StubOf, &ws)
	if ws.URL != "https://simon.test/post/" || ws.Cited.Retrieved == "" || len([]rune(ws.Cited.Excerpt)) != 200 {
		t.Errorf("url stub: %s", d.StubOf)
	}

	// Stubs are threads, and a stub needs something to respond to.
	expect(t, bob.api("POST", "/items", map[string]any{"kind": "fragment", "stub_of": map[string]any{"id": f}}), 422)
	expect(t, bob.api("POST", "/items", map[string]any{"stub_of": map[string]any{"id": "0000000000000000000000000z"}}), 422)
}

func TestGeneratedProvenance(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice := e.user("alice")

	src := alice.draft(t, map[string]any{"content_md": "Source notes."})
	alice.publish(t, src, 200)
	id := alice.draft(t, map[string]any{"content_md": "Mine. [TK]summarize ![[" + src + "]][=]Generated summary.[/TK] [TK]impyrt other-model=Pasted.[/TK]"})
	expect(t, alice.api("PATCH", "/items/"+id, map[string]any{
		"generation": []map[string]any{{"model": "m-1", "at": "2026-10-08T12:00:00+02:00"}}}), 200)
	expect(t, alice.api("PATCH", "/items/"+id, map[string]any{"generation": "nope"}), 422)
	r := alice.api("POST", "/items/"+id+"/publish", map[string]any{"note": "Tightened wording.", "note_generated": true})
	expect(t, r, 200)
	d := alice.itemDoc(t, id)
	if d.ContentMD != "Mine. Generated summary. Pasted." {
		t.Fatalf("no TK grammar on the wire (§5.7 rule 2): %q", d.ContentMD)
	}
	if !strings.Contains(d.ContentHTML, `<span class="blyg-tk-gen">Generated summary.</span> <span class="blyg-tk-gen">Pasted.</span>`) {
		t.Errorf("tk-gen wrapping: %s", d.ContentHTML)
	}
	if compact(t, d.Generated) != `[{"sources":[{"id":"`+src+`","version":1}],"model":"m-1","at":"2026-10-08T10:00:00Z"},{"sources":[],"model":"other-model"}]` {
		t.Errorf("generated: %s", d.Generated)
	}
	if !d.Changelog[0].Generated || d.ContentHash != blyg.ContentHash(d.ContentMD) {
		t.Errorf("changelog generated flag / hash: %+v", d.Changelog[0])
	}
	if strings.Contains(string(alice.get("items/"+id+".json").body), "summarize") {
		t.Error("an instruction leaked onto the wire (§5.7 rule 2)")
	}
	// Pins carry the pinned version's generated array (§5.7 rule 5).
	expect(t, alice.api("POST", "/items/"+id+"/versions/1/pin", nil), 200)
	if !strings.Contains(string(alice.get("items/"+id+"/v1.json").body), `"model": "m-1"`) {
		t.Error("pin lacks generated[]")
	}
	// A scope with no output is not publishable.
	alice.edit(t, id, "[TK]write something[/TK]")
	if p := problems(t, alice.publish(t, id, 422)); !strings.Contains(p, "no output") {
		t.Errorf("ungenerated scope: %s", p)
	}
}

func TestForkFlattening(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice, bob := e.user("alice"), e.user("bob")

	f := alice.draft(t, map[string]any{"content_md": "Quoted words."})
	alice.publish(t, f, 200)
	th := alice.draft(t, map[string]any{"kind": "thread", "content_md": "Mine, see [[" + f + "]].\n\n![[" + f + "]]\nAfter."})
	alice.publish(t, th, 200)
	expect(t, alice.api("POST", "/items/"+th+"/versions/1/pin", nil), 200)

	// Bob forks the pin without sending content: the draft is the pinned
	// document, flattened (§5.6 rule 6), and a thread like its source.
	ref := map[string]any{"origin": alice.origin, "id": th, "version": 1}
	r := bob.api("POST", "/items", map[string]any{"forked_from": ref})
	expect(t, r, 201)
	fork := r.json(t)
	host := strings.TrimPrefix(strings.TrimSuffix(alice.origin, "/alice/"), "http://")
	want := "Mine, see [“Quoted words.”](" + alice.origin + "f/" + f + "/).\n\n" +
		"> Quoted words.\n>\n> — quoted from [" + host + " · v1](" + alice.origin + "items/" + f + ".json)\n\nAfter."
	if fork["kind"] != "thread" || fork["draft_md"] != want {
		t.Fatalf("fork draft:\n%v\nwant:\n%s", fork["draft_md"], want)
	}
	id := fork["id"].(string)
	bob.publish(t, id, 200)
	d := bob.itemDoc(t, id)
	if len(*d.Transclusions) != 0 || strings.Contains(d.ContentHTML, "blyg-transclusion") || d.ForkedFrom == nil {
		t.Fatalf("a flattened quote is plain-web, never a bake (§5.6 rule 6): %s", d.ContentHTML)
	}

	if page := string(bob.get("t/" + id + "/").body); !strings.Contains(page, `Forked from <a href="`+alice.origin+"items/"+th+`/v1.json">`) {
		t.Errorf("§5.6 rule 7: a fork's page shows its lineage:\n%s", page)
	}

	// Alice's own fork links her own page; an unpinned version has nothing
	// to copy.
	r = alice.api("POST", "/items", map[string]any{"forked_from": ref})
	expect(t, r, 201)
	if md := r.json(t)["draft_md"].(string); !strings.Contains(md, "· v1]("+alice.origin+"f/"+f+"/)") {
		t.Errorf("own fork attribution: %s", md)
	}
	expect(t, bob.api("POST", "/items", map[string]any{"forked_from": map[string]any{"origin": alice.origin, "id": f, "version": 1}}), 422)
	// Content sent with the fork is the author's to keep.
	r = bob.api("POST", "/items", map[string]any{"forked_from": ref, "content_md": "my own start"})
	expect(t, r, 201)
	if r.json(t)["draft_md"] != "my own start" || r.json(t)["kind"] != "fragment" {
		t.Errorf("explicit fork content: %s", r.body)
	}
}

// A bake keeps the source's bytes on the wire (§10.2), so our pages, which
// share a host with every tenant, sanitize them (§14).
func TestBakedHTMLIsSanitizedOnPages(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	bob := e.user("bob")
	remote := &testblyg.Blyg{S: testblyg.New(t), Mount: "/r/", Title: "Remote"}
	id := "0r00000000000000000000000r"
	remote.Items = []*testblyg.Item{{ID: id, Kind: "fragment", Version: 1, Updated: time.Now(),
		ContentMD: `quoted <script>alert(1)</script><img src=x onerror="alert(2)">`}}
	remote.Publish()
	bob.subscribe(t, remote.Origin())

	th := bob.draft(t, map[string]any{"kind": "thread", "content_md": "![[" + id + "]]"})
	bob.publish(t, th, 200)
	if d := bob.itemDoc(t, th); !strings.Contains(d.ContentHTML, "<script>alert(1)</script>") {
		t.Fatalf("the wire keeps the snapshot verbatim (§10.2): %s", d.ContentHTML)
	}
	for _, path := range []string{"t/" + th + "/", ""} {
		page := string(bob.get(path).body)
		if !strings.Contains(page, "quoted") || strings.Contains(page, "<script>alert") || strings.Contains(page, "onerror") {
			t.Errorf("page %q is not sanitized:\n%s", path, page)
		}
	}
}
