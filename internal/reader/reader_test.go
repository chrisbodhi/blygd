package reader

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chrisbodhi/blygd/internal/blyg"
	"github.com/chrisbodhi/blygd/internal/fetch"
	"github.com/chrisbodhi/blygd/internal/hopper"
	"github.com/chrisbodhi/blygd/internal/testblyg"
	"github.com/chrisbodhi/blygd/internal/testdb"
)

const (
	idA = "0a00000000000000000000000a"
	idB = "0b00000000000000000000000b"
	idC = "0c00000000000000000000000c"
)

type rig struct {
	t   *testing.T
	r   *Reader
	b   *testblyg.Blyg
	uid int64
	sid int64
}

func newRig(t *testing.T) *rig {
	pool := testdb.New(t)
	ctx := context.Background()
	var uid int64
	if err := pool.QueryRow(ctx, `INSERT INTO users (handle) VALUES ('reader') RETURNING id`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	r := &Reader{Pool: pool, Fetch: &fetch.Client{AllowPrivate: true, UserAgent: "blygd-test"},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ReconcileEvery: 1000 * time.Hour}
	b := &testblyg.Blyg{S: testblyg.New(t), Mount: "/blyg/", Title: "Remote"}
	return &rig{t: t, r: r, b: b, uid: uid}
}

func (g *rig) subscribe() {
	g.t.Helper()
	g.b.Publish()
	sid, res, err := g.r.Subscribe(context.Background(), g.uid, g.b.Origin())
	if err != nil {
		g.t.Fatal(err)
	}
	if res.Identity != g.b.Origin() || res.Kind != "blyg" {
		g.t.Fatalf("resolution: %+v", res)
	}
	g.sid = sid
}

// reconcileNext makes the next poll reconcile, and makes every stored
// document old enough to re-check.
func (g *rig) reconcileNext() {
	g.t.Helper()
	ctx := context.Background()
	if _, err := g.r.Pool.Exec(ctx, `UPDATE remote_origins SET reconciled_at = NULL WHERE id = $1`, g.sid); err != nil {
		g.t.Fatal(err)
	}
	if _, err := g.r.Pool.Exec(ctx, `UPDATE imported_items SET fetched_at = now() - interval '1 hour' WHERE remote_origin_id = $1`, g.sid); err != nil {
		g.t.Fatal(err)
	}
}

func (g *rig) poll() {
	g.t.Helper()
	if err := g.r.PollNow(context.Background(), g.sid); err != nil {
		g.t.Fatal(err)
	}
}

type row struct {
	state, kind        string
	version, watermark int
	hasDoc             bool
	discrepancy        string
	page               string
}

func (g *rig) row(id string) *row {
	g.t.Helper()
	var r row
	var disc, page *string
	err := g.r.Pool.QueryRow(context.Background(), `SELECT state, kind, version, watermark, doc IS NOT NULL, discrepancy, page_url
		FROM imported_items WHERE remote_origin_id = $1 AND item_id = $2`, g.sid, id).
		Scan(&r.state, &r.kind, &r.version, &r.watermark, &r.hasDoc, &disc, &page)
	if err != nil {
		return nil
	}
	if disc != nil {
		r.discrepancy = *disc
	}
	if page != nil {
		r.page = *page
	}
	return &r
}

func (g *rig) docField(id, field string) string {
	var s *string
	_ = g.r.Pool.QueryRow(context.Background(), `SELECT doc->>$3 FROM imported_items WHERE remote_origin_id = $1 AND item_id = $2`,
		g.sid, id, field).Scan(&s)
	if s == nil {
		return ""
	}
	return *s
}

func TestImportReconcilesAndWatermarks(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	a := &testblyg.Item{ID: idA, Kind: "fragment", Version: 2, ContentMD: "alpha two", Updated: now, Page: "posts/a/"}
	b := &testblyg.Item{ID: idB, Kind: "fragment", Version: 1, ContentMD: "beta", Updated: now}
	g.b.Items = []*testblyg.Item{a, b, {ID: idC, Kind: "poem", Version: 1, ContentMD: "unknown kind", Updated: now}}
	g.subscribe()

	// First poll: a full index diff (§13.2), stored verbatim, unknown kinds
	// ignored (§5.3), page resolved against the origin (§5.8).
	g.poll()
	ra := g.row(idA)
	if ra == nil || ra.version != 2 || ra.watermark != 2 || ra.state != "current" || ra.page != g.b.Origin()+"posts/a/" ||
		g.docField(idA, "content_md") != "alpha two" || !strings.Contains(g.docField(idA, "x-unknown"), "kept") {
		t.Fatalf("a after first poll: %+v", ra)
	}
	if g.row(idB) == nil || g.row(idC) != nil {
		t.Fatalf("b imported, c (unknown kind) ignored: %v %v", g.row(idB), g.row(idC))
	}

	// A poll with nothing new costs conditional requests and no documents.
	docHits := g.b.S.Hits("/blyg/items/" + idA + ".json")
	g.poll()
	if g.b.S.Hits("/blyg/items/"+idA+".json") != docHits || g.b.S.LastCondition("/blyg/feed.xml") == "" {
		t.Errorf("unchanged poll refetched a document, or sent no validator")
	}

	// The feed claims v9, but only the fetched document advances state
	// (§13.2): the document still says v2.
	g.b.S.Set("/blyg/feed.xml", strings.Replace(mustGet(t, g.b, "/blyg/feed.xml"), "<blyg:version>2</blyg:version>", "<blyg:version>9</blyg:version>", 1))
	g.poll()
	if ra = g.row(idA); ra.version != 2 {
		t.Fatalf("feed advanced state: %+v", ra)
	}

	// An edit: v3 is adopted and the watermark follows.
	a.Version, a.ContentMD = 3, "alpha three"
	g.b.Publish()
	g.poll()
	if ra = g.row(idA); ra.version != 3 || ra.watermark != 3 || g.docField(idA, "content_md") != "alpha three" {
		t.Fatalf("v3: %+v", ra)
	}

	// §13.3: the origin rolls back to v2. The index diff fetches the
	// document because its version differs from ours. Not adopted; loud.
	a.Version, a.ContentMD = 2, "alpha two, rewritten"
	g.b.Publish()
	g.reconcileNext()
	g.poll()
	if ra = g.row(idA); ra.version != 3 || ra.watermark != 3 || g.docField(idA, "content_md") != "alpha three" ||
		!strings.Contains(ra.discrepancy, "below the watermark") {
		t.Fatalf("regression adopted or silent: %+v", ra)
	}

	// §13.3: a stealth edit at the same version is adopted and recorded.
	// Neither feed nor index shows it; the rolling re-check fetches it.
	a.Version, a.ContentMD = 3, "alpha three, quietly changed"
	g.b.Publish()
	g.reconcileNext()
	g.poll()
	if ra = g.row(idA); ra.version != 3 || g.docField(idA, "content_md") != "alpha three, quietly changed" ||
		!strings.Contains(ra.discrepancy, "stealth edit") {
		t.Fatalf("stealth edit: %+v", ra)
	}

	// §13.3: a bad content_hash is a discrepancy, and the content is
	// adopted anyway.
	b.Version, b.ContentMD, b.Hash = 2, "beta two", "sha256:"+strings.Repeat("0", 64)
	g.b.Publish()
	g.poll()
	if rb := g.row(idB); rb.version != 2 || g.docField(idB, "content_md") != "beta two" || !strings.Contains(rb.discrepancy, "content_hash") {
		t.Fatalf("bad hash: %+v", rb)
	}

	// The schema refuses a watermark regression whatever the Go does.
	if _, err := g.r.Pool.Exec(ctx, `UPDATE imported_items SET watermark = 1, version = 1 WHERE item_id = $1`, idA); err == nil {
		t.Error("watermark regression accepted by the schema")
	}
}

func mustGet(t *testing.T, b *testblyg.Blyg, path string) string {
	t.Helper()
	resp, err := (&fetch.Client{AllowPrivate: true}).Get(context.Background(), b.S.URL+path, fetch.ReaderLimits, "")
	if err != nil || resp.Status != 200 {
		t.Fatalf("get %s: %v", path, err)
	}
	return string(resp.Body)
}

func TestWithdrawalRetainsOnlyPins(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	a := &testblyg.Item{ID: idA, Kind: "fragment", Version: 2, ContentMD: "second words", Updated: now, Pinned: []int{1}}
	g.b.Items = []*testblyg.Item{a}
	g.b.S.Set("/blyg/items/"+idA+"/v1.json", g.b.Pin(a, 1, "first words"))
	g.subscribe()
	g.poll()

	var pins int
	g.r.Pool.QueryRow(ctx, `SELECT count(*) FROM imported_pins WHERE item_id = $1`, idA).Scan(&pins)
	if pins != 1 {
		t.Fatalf("pin not retained: %d", pins)
	}

	// The endcap rolls the item to null: no stored content, not in the
	// feed; the pin survives (§13.4).
	a.Kind, a.Version, a.ContentMD = "withdrawn", 3, ""
	g.b.Publish()
	g.poll()
	ra := g.row(idA)
	if ra.state != "withdrawn" || ra.hasDoc || ra.version != 3 || ra.page == "" {
		t.Fatalf("endcap: %+v", ra)
	}
	page, err := g.r.Feed(ctx, g.uid, FeedQuery{})
	items := page.Items
	if err != nil || len(items) != 0 {
		t.Fatalf("withdrawn item still in the feed: %v %v", items, err)
	}
	it, err := g.r.GetItem(ctx, g.uid, g.sid, idA)
	if err != nil || it.Doc != nil || len(it.Pins) != 1 || !strings.Contains(string(it.Pins[0].Doc), "first words") ||
		it.Pins[0].URL != g.b.Origin()+"items/"+idA+"/v1.json" {
		t.Fatalf("withdrawn item view: %+v %v", it, err)
	}
	var leaked int
	g.r.Pool.QueryRow(ctx, `SELECT count(*) FROM imported_items WHERE doc::text LIKE '%second words%'`).Scan(&leaked)
	if leaked != 0 {
		t.Error("withdrawn content kept")
	}

	// The watermark survives withdrawal (§13.4, finding F5): content at or
	// below the endcap's version is a replay, not the item returning.
	// No poll fetches a document at the version it holds, so the endcap's
	// own version is applied directly, as any fetch of it would be.
	a.Kind, a.Version, a.ContentMD = "fragment", 3, "second words, replayed"
	if _, err := g.r.ApplyDocument(ctx, g.sid, blyg.Surface{Origin: g.b.Origin()}, idA, []byte(g.b.Doc(a)), g.b.Origin()+"items/"+idA+".json"); err != nil {
		t.Fatal(err)
	}
	if ra = g.row(idA); ra.state != "withdrawn" || ra.hasDoc || ra.version != 3 || !strings.Contains(ra.discrepancy, "endcap") {
		t.Fatalf("replay at the endcap's version adopted or silent: %+v", ra)
	}
	a.Version = 2
	g.b.Publish()
	g.reconcileNext()
	g.poll()
	if ra = g.row(idA); ra.state != "withdrawn" || ra.hasDoc || ra.version != 3 || !strings.Contains(ra.discrepancy, "below the watermark") {
		t.Fatalf("replay below the endcap adopted or silent: %+v", ra)
	}

	// A later version is the item returning (§9, §13.1 rule 3).
	a.Kind, a.Version, a.ContentMD = "fragment", 4, "back again"
	g.b.Publish()
	g.poll()
	if ra = g.row(idA); ra.state != "current" || !ra.hasDoc || ra.version != 4 {
		t.Fatalf("return: %+v", ra)
	}
	if _, err := g.r.Pool.Exec(ctx, `UPDATE imported_pins SET doc = '{}' WHERE item_id = $1`, idA); err == nil {
		t.Error("retained pin rewritten")
	}
}

func TestGapsFallBackToTheIndex(t *testing.T) {
	g := newRig(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	g.b.FeedWindow = 1
	g.b.Items = []*testblyg.Item{{ID: idA, Kind: "fragment", Version: 1, ContentMD: "a", Updated: now}}
	g.subscribe()
	g.poll()

	// Two publishes while we were away; the window shows only the newest,
	// so the newest entry we saw has left it: index diff.
	g.b.Items = append(g.b.Items,
		&testblyg.Item{ID: idB, Kind: "fragment", Version: 1, ContentMD: "b", Updated: now},
		&testblyg.Item{ID: idC, Kind: "fragment", Version: 1, ContentMD: "c", Updated: now})
	g.b.Publish()
	g.poll()
	if g.row(idB) == nil || g.row(idC) == nil {
		t.Fatalf("missed an item behind the feed window: b=%v c=%v", g.row(idB), g.row(idC))
	}

	// An unparseable feed degrades to an index diff and corrupts nothing.
	g.b.S.Set("/blyg/feed.xml", "<rss><channel><item>")
	g.b.Items[0].Version, g.b.Items[0].ContentMD = 2, "a2"
	feed := mustGet(t, g.b, "/blyg/feed.xml")
	g.b.Publish()
	g.b.S.Set("/blyg/feed.xml", feed)
	g.b.S.Set("/blyg/feed.xml", "not xml at all")
	g.poll()
	if ra := g.row(idA); ra.version != 2 {
		t.Fatalf("unparseable feed did not fall back to the index: %+v", ra)
	}

	// No index at all: lossy, and the feed window still works.
	g.b.NoIndex = true
	g.b.FeedWindow = 0
	g.b.Publish()
	g.reconcileNext()
	g.poll()
	s, err := g.r.GetSubscription(context.Background(), g.uid, 0, g.sid)
	if err != nil || !s.Lossy || s.LastError == nil || !strings.Contains(*s.LastError, "index") || s.PollFailures != 0 {
		t.Fatalf("lossy subscription: %+v %v", s, err)
	}
}

func TestPollBudgetAndBackoff(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		id := blyg.NewID()
		g.b.Items = append(g.b.Items, &testblyg.Item{ID: id, Kind: "fragment", Version: 1, ContentMD: id, Updated: now})
	}
	g.r.MaxFetches = 2
	g.subscribe()
	count := func() int {
		var n int
		g.r.Pool.QueryRow(ctx, `SELECT count(*) FROM imported_items`).Scan(&n)
		return n
	}
	g.poll()
	if count() != 2 {
		t.Fatalf("budget: %d items after one poll", count())
	}
	var next time.Time
	g.r.Pool.QueryRow(ctx, `SELECT next_poll_at FROM remote_origins WHERE id = $1`, g.sid).Scan(&next)
	if time.Until(next) > 2*time.Minute {
		t.Errorf("an unfinished backfill should come back soon, not in %v", time.Until(next))
	}
	g.poll()
	g.poll()
	if count() != 5 {
		t.Fatalf("backfill did not finish: %d", count())
	}

	// New items found by the feed trigger, cut short by the budget, are
	// still fetched on later polls although the feed has not changed since:
	// the validators that would 304 them away are not saved until done.
	for i := 0; i < 3; i++ {
		id := blyg.NewID()
		g.b.Items = append(g.b.Items, &testblyg.Item{ID: id, Kind: "fragment", Version: 1, ContentMD: id, Updated: now})
	}
	g.b.Publish()
	g.poll()
	g.poll()
	if count() != 8 {
		t.Fatalf("triggered documents lost behind a 304: %d items", count())
	}

	// §12.3: failure backs off exponentially, honors Retry-After, and never
	// polls sooner than the steady interval.
	g.b.S.SetResponse("/blyg/feed.xml", testblyg.Response{Status: 503, Header: map[string]string{"Retry-After": "86400"}})
	g.poll()
	var failures int
	g.r.Pool.QueryRow(ctx, `SELECT poll_failures, next_poll_at FROM remote_origins WHERE id = $1`, g.sid).Scan(&failures, &next)
	if failures != 1 || time.Until(next) < 23*time.Hour {
		t.Fatalf("Retry-After ignored: failures=%d next in %v", failures, time.Until(next))
	}
	g.b.S.SetResponse("/blyg/feed.xml", testblyg.Response{Status: 500})
	g.poll()
	g.r.Pool.QueryRow(ctx, `SELECT poll_failures, next_poll_at FROM remote_origins WHERE id = $1`, g.sid).Scan(&failures, &next)
	if failures != 2 || time.Until(next) < 4*g.r.pollInterval()-time.Minute {
		t.Fatalf("no exponential backoff: failures=%d next in %v", failures, time.Until(next))
	}
	for f := 0; f < 20; f++ {
		if d := g.r.nextDelay(pollResult{failed: true}, f); d < g.r.pollInterval() || d > maxBackoff {
			t.Errorf("delay after %d failures: %v", f, d)
		}
	}
	// Failure altered no stored item (§13.2).
	if count() != 8 {
		t.Errorf("poll failure changed item state")
	}
}

func TestDocumentMustComeFromItsOrigin(t *testing.T) {
	g := newRig(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	a := &testblyg.Item{ID: idA, Kind: "fragment", Version: 1, ContentMD: "a", Updated: now}
	g.b.Items = []*testblyg.Item{a}
	g.subscribe()
	// The item document redirects to a neighbour's path on the same host.
	g.b.S.Set("/other/items/"+idA+".json", g.b.Doc(a))
	g.b.S.SetResponse("/blyg/items/"+idA+".json", testblyg.Response{Status: 302, Redirect: "/other/items/" + idA + ".json"})
	g.poll()
	if g.row(idA) != nil {
		t.Fatal("adopted a document served from another origin's path (§12.2)")
	}
}

func TestLegacyFeedWrapper(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	rss := func(items string) string {
		return `<rss version="2.0"><channel><title>Old Blog</title><link>` + g.b.S.URL + `/</link>` + items + `</channel></rss>`
	}
	g.b.S.Set("/rss", rss(`<item><guid>p1</guid><link>/p/1</link><title>One</title><description>&lt;p&gt;first&lt;/p&gt;&lt;script&gt;x()&lt;/script&gt;</description></item>`))
	sid, res, err := g.r.Subscribe(ctx, g.uid, g.b.S.URL+"/rss")
	if err != nil || res.Kind != "l0" || res.Identity != g.b.S.URL+"/rss" {
		t.Fatalf("subscribe L0: %+v %v", res, err)
	}
	g.sid = sid
	g.poll()
	id := blyg.L0ID("p1", "")
	r := g.row(id)
	if r == nil || r.kind != "l0" || r.version != 1 || r.page != g.b.S.URL+"/p/1" || blyg.ValidID(id) {
		t.Fatalf("L0 row: %+v", r)
	}
	// Stored verbatim (§14): the script is still there for the client to
	// sanitize; the plain-text cut is separate.
	if !strings.Contains(g.docField(id, "summary_html"), "<script>") || g.docField(id, "summary_text") == "" {
		t.Errorf("L0 doc: %s / %s", g.docField(id, "summary_html"), g.docField(id, "summary_text"))
	}

	// Same GUID, new content: a local version bump. An entry that leaves the
	// window is kept.
	g.b.S.Set("/rss", rss(`<item><guid>p1</guid><link>/p/1</link><title>One (edited)</title></item>`))
	g.poll()
	g.b.S.Set("/rss", rss(`<item><guid>p2</guid><link>/p/2</link><title>Two</title></item>`))
	g.poll()
	if r = g.row(id); r == nil || r.version != 2 || r.state != "current" || g.row(blyg.L0ID("p2", "")) == nil {
		t.Fatalf("L0 edit and retention: %+v", r)
	}
	page, err := g.r.Feed(ctx, g.uid, FeedQuery{})
	items := page.Items
	if err != nil || len(items) != 2 || !items[0].L0 {
		t.Fatalf("L0 in the feed: %+v %v", items, err)
	}
	// An L0 identity is a feed URL, so hoppers name its items by
	// subscription.
	hs := &hopper.Store{Pool: g.r.Pool}
	h, err := hs.Create(ctx, g.uid, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hs.AddImportedBySubscription(ctx, g.uid, h.ID, g.sid, id); err != nil {
		t.Fatalf("hopper add L0 item: %v", err)
	}
	if _, err := hs.AddImportedBySubscription(ctx, g.uid+1, h.ID, g.sid, id); err == nil {
		t.Error("another user added to this hopper")
	}
}

func TestFeedOrderAndReads(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	future := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	g.b.Items = []*testblyg.Item{
		{ID: idA, Kind: "fragment", Version: 1, ContentMD: "old", Updated: past},
		{ID: idB, Kind: "fragment", Version: 1, ContentMD: "from the future", Updated: future},
	}
	g.subscribe()
	g.poll()
	page, err := g.r.Feed(ctx, g.uid, FeedQuery{})
	items := page.Items
	if err != nil || len(items) != 2 {
		t.Fatal(items, err)
	}
	// §13.7: the future-dated item sorts no newer than when we saw it.
	if items[0].ItemID != idB || items[0].SortAt.After(time.Now().Add(time.Minute)) || !items[1].SortAt.Equal(past) {
		t.Fatalf("order: %v %v / %v %v", items[0].ItemID, items[0].SortAt, items[1].ItemID, items[1].SortAt)
	}
	// Paging.
	p1, _ := g.r.Feed(ctx, g.uid, FeedQuery{Limit: 1})
	p2, _ := g.r.Feed(ctx, g.uid, FeedQuery{Limit: 1, Before: p1.Next})
	page1, page2 := p1.Items, p2.Items
	if len(page1) != 1 || len(page2) != 1 || page1[0].ItemID == page2[0].ItemID {
		t.Fatalf("paging: %v %v", page1, page2)
	}

	// Read at v1, unread again at v2.
	if err := g.r.MarkRead(ctx, g.uid, g.sid, idA, true); err != nil {
		t.Fatal(err)
	}
	up, _ := g.r.Feed(ctx, g.uid, FeedQuery{UnreadOnly: true})
	unread := up.Items
	if len(unread) != 1 || unread[0].ItemID != idB {
		t.Fatalf("unread: %v", unread)
	}
	g.b.Items[0].Version, g.b.Items[0].ContentMD = 2, "old, edited"
	g.b.Publish()
	g.poll()
	up, _ = g.r.Feed(ctx, g.uid, FeedQuery{UnreadOnly: true})
	unread = up.Items
	if len(unread) != 2 {
		t.Fatalf("an edit should be unread again: %v", unread)
	}
	if err := g.r.MarkRead(ctx, g.uid, g.sid, "nope", true); err != ErrNotFound {
		t.Errorf("marking an unknown item: %v", err)
	}
}

// relocate moves the rig's blyg onto a templated surface (§6.1), with query
// strings, as a CMS behind a JSON API would serve it.
func (g *rig) relocate() {
	g.b.Feed = "api?route=/feed"
	g.b.Index = "api?route=/items"
	g.b.ItemTmpl = "api?route=/items/{id}"
	g.b.PinTmpl = g.b.S.URL + "/elsewhere/pins/{id}/{n}" // absolute, on purpose
}

func TestImportFromARelocatedSurface(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	const page = "https://www.example.com/2026/10/a-post/"
	a := &testblyg.Item{ID: idA, Kind: "fragment", Version: 1, ContentMD: "words", Page: page, Pinned: []int{1}, Updated: now}
	pageless := &testblyg.Item{ID: idB, Kind: "fragment", Version: 1, ContentMD: "no page", NoPage: true, Updated: now}
	g.b.Items = []*testblyg.Item{a, pageless}
	g.relocate()
	g.b.SetPin(a, 1, "words")
	g.subscribe()
	g.poll()

	r := g.row(idA)
	if r == nil || r.version != 1 {
		t.Fatalf("item not imported through the item template: %+v", r)
	}
	// §5.8: an absolute page is taken as given, never prefixed.
	if r.page != page {
		t.Errorf("page %q, want %q", r.page, page)
	}
	it, err := g.r.GetItem(ctx, g.uid, g.sid, idA)
	if err != nil || len(it.Pins) != 1 || it.Pins[0].URL != g.b.S.URL+"/elsewhere/pins/"+idA+"/1" {
		t.Fatalf("pin through the pin template: %+v %v", it, err)
	}
	for _, p := range []string{"/blyg/feed.xml", "/blyg/items/index.json", "/blyg/items/" + idA + ".json", "/blyg/items/" + idA + "/v1.json"} {
		if n := g.b.S.Hits(p); n != 0 {
			t.Errorf("fetched the default path %s %d times; the manifest relocates it", p, n)
		}
	}
	e, err := g.r.BlogrollEntry(ctx, g.uid, g.sid)
	if err != nil || e.XMLURL != g.b.Origin()+"api?route=/feed" {
		t.Errorf("blogroll xmlUrl: %+v %v", e, err)
	}
	// With no page, a snapshot's address is its item document, from the
	// template (§5.8, §15.2 #85), not the f/{id}/ guess.
	snaps, err := g.r.Snapshots(ctx, g.uid, []string{idB})
	if err != nil || len(snaps[idB]) != 1 || snaps[idB][0].Page != g.b.Origin()+"api?route=/items/"+idB {
		t.Errorf("snapshot page without a page: %+v %v", snaps[idB], err)
	}

	// The default path does not speak for a relocated origin (§12.2, §6.1).
	if _, err := g.r.ApplyDocument(ctx, g.sid, g.b.Surface(), idA, []byte(g.b.Doc(a)), g.b.Origin()+"items/"+idA+".json"); err == nil {
		t.Error("adopted a document from the default path under an item template")
	}
	if err := g.r.ApplyPin(ctx, g.sid, g.b.Surface(), idA, 1, []byte(g.b.Pin(a, 1, "words")), g.b.Origin()+"items/"+idA+"/v1.json"); err == nil {
		t.Error("retained a pin from the default path under a pin template")
	}
}

func TestManifestRefreshRelocatesTheSurface(t *testing.T) {
	g := newRig(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	a := &testblyg.Item{ID: idA, Kind: "fragment", Version: 1, ContentMD: "one", Updated: now}
	g.b.Items = []*testblyg.Item{a}
	g.subscribe()
	g.poll()
	if r := g.row(idA); r == nil || r.version != 1 {
		t.Fatalf("v1: %+v", r)
	}

	// The blyg moves onto templates and edits. Its old default files stay
	// behind at v1, so only a reader that read the new manifest sees v2.
	g.relocate()
	a.Version, a.ContentMD = 2, "two"
	g.b.Publish()
	g.reconcileNext()
	g.poll()
	if r := g.row(idA); r == nil || r.version != 2 {
		t.Fatalf("after relocation: %+v; the reconciliation should read the refreshed manifest's surface", r)
	}
}

func TestManifestNamedByRelIsReReadThere(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	g.b.Items = []*testblyg.Item{{ID: idA, Kind: "fragment", Version: 1, ContentMD: "a", Updated: now}}
	g.b.ManifestPath = "/blyg/manifest"
	g.b.Publish()
	g.b.S.Set("/post/", `<html><head><link rel="blyg" href="/blyg/manifest"></head></html>`)
	sid, res, err := g.r.Subscribe(ctx, g.uid, g.b.S.URL+"/post/")
	if err != nil || res.Identity != g.b.Origin() {
		t.Fatalf("§12.1 step 4: %+v %v", res, err)
	}
	g.sid = sid
	g.poll()

	g.b.Title = "Renamed"
	g.b.Publish()
	g.reconcileNext()
	g.poll()
	var title string
	if err := g.r.Pool.QueryRow(ctx, `SELECT title FROM remote_origins WHERE id = $1`, g.sid).Scan(&title); err != nil || title != "Renamed" {
		t.Errorf("title %q %v: the manifest was not re-read where it was found", title, err)
	}
	if n := g.b.S.Hits("/blyg/blyg.json"); n != 0 {
		t.Errorf("probed /blyg/blyg.json %d times; the manifest lives at /blyg/manifest", n)
	}
	if g.row(idA) == nil {
		t.Error("nothing imported")
	}
}

// slowBlyg publishes n fragments on a server of its own that answers every
// request after delay.
func slowBlyg(t *testing.T, n int, delay time.Duration) *testblyg.Blyg {
	b := &testblyg.Blyg{S: testblyg.New(t), Mount: "/blyg/", Title: "Slow"}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for i := range n {
		id := fmt.Sprintf("0d%022d0d", i)
		b.Items = append(b.Items, &testblyg.Item{ID: id, Kind: "fragment", Version: 1, ContentMD: "slow", Updated: now})
	}
	b.Publish()
	b.S.SetDelay(delay)
	return b
}

func TestPollStopsAtItsDeadline(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	g.r.PollDeadline = 300 * time.Millisecond
	g.b = slowBlyg(t, 40, 0)
	g.subscribe()
	g.b.S.SetDelay(50 * time.Millisecond) // 40 documents: 2 s unbounded

	start := time.Now()
	g.poll()
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Fatalf("poll took %s; its deadline is %s", took, g.r.PollDeadline)
	}
	var n, failures int
	var lastErr string
	var feedETag *string
	_ = g.r.Pool.QueryRow(ctx, `SELECT count(*) FROM imported_items WHERE remote_origin_id = $1`, g.sid).Scan(&n)
	if err := g.r.Pool.QueryRow(ctx, `SELECT poll_failures, coalesce(last_error, ''), feed_etag FROM remote_origins WHERE id = $1`, g.sid).
		Scan(&failures, &lastErr, &feedETag); err != nil {
		t.Fatal(err)
	}
	if n == 0 || n == 40 {
		t.Errorf("%d of 40 imported; the deadline should keep what it fetched and stop", n)
	}
	if failures != 1 || !strings.Contains(lastErr, "deadline") {
		t.Errorf("failures %d, last error %q: a poll cut short backs off (§12.3)", failures, lastErr)
	}
	if feedETag != nil {
		t.Errorf("feed validator %q saved past unfetched entries", *feedETag)
	}

	// At full speed the next poll finishes the job.
	g.b.S.SetDelay(0)
	g.poll()
	_ = g.r.Pool.QueryRow(ctx, `SELECT count(*) FROM imported_items WHERE remote_origin_id = $1`, g.sid).Scan(&n)
	if n != 40 {
		t.Errorf("%d of 40 after a full-speed poll", n)
	}
}

func TestBatchPollsOriginsSideBySide(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	g.r.PollDeadline = 400 * time.Millisecond
	var slow []*testblyg.Blyg
	for range 3 {
		b := slowBlyg(t, 20, 0)
		if _, _, err := g.r.Subscribe(ctx, g.uid, b.Origin()); err != nil {
			t.Fatal(err)
		}
		b.S.SetDelay(50 * time.Millisecond)
		slow = append(slow, b)
	}
	start := time.Now()
	n, err := g.r.PollBatch(ctx)
	if err != nil || n != 3 {
		t.Fatalf("PollBatch: %d %v", n, err)
	}
	// Each origin runs into its deadline; one after another would take
	// three deadlines.
	if took := time.Since(start); took > 1000*time.Millisecond {
		t.Errorf("batch took %s: one slow origin held up the others", took)
	}
	for _, b := range slow {
		if b.S.Hits("/blyg/feed.xml") == 0 {
			t.Errorf("%s never polled", b.Origin())
		}
	}
}

// "Mark all read" marks what a feed read at as_of could have shown. A
// version seen after it stays unread, even one dated earlier, which sorts
// among the items already read (§13.7).
func TestMarkAllRead(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	clock := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	g.r.Now = func() time.Time { return clock }
	past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	g.b.Items = []*testblyg.Item{
		{ID: idA, Kind: "fragment", Version: 1, ContentMD: "a", Updated: past},
		{ID: idB, Kind: "fragment", Version: 1, ContentMD: "b", Updated: past},
	}
	g.subscribe()
	g.poll()
	clock = clock.Add(time.Minute)
	shown, err := g.r.Feed(ctx, g.uid, FeedQuery{})
	if err != nil || len(shown.Items) != 2 || !shown.AsOf.Equal(clock) {
		t.Fatalf("feed: %+v %v", shown, err)
	}
	unread := func() []string {
		t.Helper()
		p, err := g.r.Feed(ctx, g.uid, FeedQuery{UnreadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, it := range p.Items {
			ids = append(ids, it.ItemID)
		}
		return ids
	}

	// After the feed was read: a backdated edit of a, and a backdated c.
	clock = clock.Add(time.Minute)
	g.b.Items[0].Version, g.b.Items[0].ContentMD = 2, "a, edited"
	g.b.Items = append(g.b.Items, &testblyg.Item{ID: idC, Kind: "fragment", Version: 1, ContentMD: "c", Updated: past})
	g.b.Publish()
	g.poll()
	if n, err := g.r.MarkAllRead(ctx, g.uid, 0, shown.AsOf); err != nil || n != 1 {
		t.Fatalf("marked %d, %v; want b alone", n, err)
	}
	if got := unread(); len(got) != 2 || slices.Contains(got, idB) {
		t.Fatalf("unread after marking: %v, want a's v2 and c", got)
	}
	if _, err := g.r.MarkAllRead(ctx, g.uid, g.sid+1, shown.AsOf); err != ErrNotFound {
		t.Errorf("another subscription: %v", err)
	}

	// A feed read now covers them, and marking twice marks nothing more.
	now, _ := g.r.Feed(ctx, g.uid, FeedQuery{})
	if n, err := g.r.MarkAllRead(ctx, g.uid, g.sid, now.AsOf); err != nil || n != 2 || len(unread()) != 0 {
		t.Fatalf("marked %d, %v; unread %v", n, err, unread())
	}
	if n, err := g.r.MarkAllRead(ctx, g.uid, g.sid, now.AsOf); err != nil || n != 0 {
		t.Fatalf("marking again marked %d, %v", n, err)
	}
}
