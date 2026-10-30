package reader

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chrisbodhi/blygd/internal/testblyg"
)

// drain runs import batches until none is due, and returns how many
// entries each batch took.
func (g *rig) drain() []int {
	g.t.Helper()
	var sizes []int
	for {
		n, err := g.r.ImportBatch(context.Background())
		if err != nil {
			g.t.Fatal(err)
		}
		if n == 0 {
			return sizes
		}
		sizes = append(sizes, n)
	}
}

func (g *rig) importOf(url string) *ImportEntry {
	g.t.Helper()
	es, err := g.r.Imports(context.Background(), g.uid, "")
	if err != nil {
		g.t.Fatal(err)
	}
	for _, e := range es {
		if e.URL == url {
			return e
		}
	}
	g.t.Fatalf("no import of %s", url)
	return nil
}

func (g *rig) displayTitle(sid int64) *string {
	g.t.Helper()
	var t *string
	if err := g.r.Pool.QueryRow(context.Background(), `SELECT display_title FROM subscriptions WHERE user_id = $1 AND remote_origin_id = $2`,
		g.uid, sid).Scan(&t); err != nil {
		g.t.Fatal(err)
	}
	return t
}

// An OPML list becomes subscriptions in the background, one entry per user
// per batch, each resolved like a subscribe (§12.1): a blyg's feed upgrades
// to the blyg (§11), a plain feed stays L0 (§13.6), and a dead address is
// tried three times and then fails.
func TestSubscriptionImport(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	g.b.Items = []*testblyg.Item{{ID: idA, Kind: "fragment", Version: 1, ContentMD: "a", Updated: time.Now()}}
	g.b.Publish()
	other := &testblyg.Blyg{S: g.b.S, Mount: "/other/", Title: "Other"}
	other.Publish()
	// A plain feed, on a host of its own: resolution would find the blyg
	// beside it otherwise.
	legacy := testblyg.New(t)
	legacy.Set("/rss", `<rss version="2.0"><channel><title>Old Blog</title><link>`+legacy.URL+`/</link></channel></rss>`)
	dead := "http://127.0.0.1:1/feed.xml"

	// Already followed, and named: an import does not rename it.
	otherSid, _, err := g.r.Subscribe(ctx, g.uid, other.Origin())
	if err != nil {
		t.Fatal(err)
	}
	mine := "Mine"
	if err := g.r.SetDisplayTitle(ctx, g.uid, otherSid, &mine); err != nil {
		t.Fatal(err)
	}

	opml := `<opml version="2.0"><body><outline text="Folder">
		<outline type="rss" text="Custom name" xmlUrl="` + g.b.Origin() + `feed.xml"/>
		<outline text="Theirs" htmlUrl="` + other.Origin() + `"/>
		<outline type="rss" text="Old Blog" xmlUrl="` + legacy.URL + `/rss"/>
		<outline type="rss" text="Gone" xmlUrl="` + dead + `"/>
		<outline type="rss" text="Mail" xmlUrl="mailto:someone@example.com"/>
		<outline type="rss" text="Again" xmlUrl="` + g.b.Origin() + `feed.xml"/>
	</outline></body></opml>`
	res, err := g.r.ImportOPML(ctx, g.uid, []byte(opml))
	if err != nil || res.Entries != 4 || res.Queued != 4 {
		t.Fatalf("import: %+v %v", res, err)
	}
	if res, _ = g.r.ImportOPML(ctx, g.uid, []byte(opml)); res.Queued != 0 {
		t.Fatalf("importing twice queued %d", res.Queued)
	}

	// Another user's single entry is not stuck behind four.
	var other2 int64
	if err := g.r.Pool.QueryRow(ctx, `INSERT INTO users (handle) VALUES ('second') RETURNING id`).Scan(&other2); err != nil {
		t.Fatal(err)
	}
	if _, err := g.r.ImportOPML(ctx, other2, []byte(`<opml><body><outline xmlUrl="`+legacy.URL+`/rss"/></body></opml>`)); err != nil {
		t.Fatal(err)
	}
	if sizes := g.drain(); len(sizes) != 4 || sizes[0] != 2 || sizes[1] != 1 {
		t.Fatalf("batches %v: want both users in the first, then one entry a batch", sizes)
	}

	feed := g.importOf(g.b.Origin() + "feed.xml")
	if feed.Status != "subscribed" || feed.SubscriptionID == nil {
		t.Fatalf("blyg feed entry: %+v", feed)
	}
	s, err := g.r.GetSubscription(ctx, g.uid, 0, *feed.SubscriptionID)
	if err != nil || s.Kind != "blyg" || s.Origin != g.b.Origin() {
		t.Fatalf("the feed did not upgrade to its blyg (§11): %+v %v", s, err)
	}
	if d := g.displayTitle(*feed.SubscriptionID); d == nil || *d != "Custom name" {
		t.Errorf("display title: %v", d)
	}
	if e := g.importOf(other.Origin()); e.Status != "subscribed" || *e.SubscriptionID != otherSid {
		t.Errorf("already followed: %+v", e)
	}
	if d := g.displayTitle(otherSid); d == nil || *d != "Mine" {
		t.Errorf("an import renamed a named subscription: %v", d)
	}
	if e := g.importOf(legacy.URL + "/rss"); e.Status != "subscribed" {
		t.Errorf("L0 entry: %+v", e)
	} else if s, _ := g.r.GetSubscription(ctx, g.uid, 0, *e.SubscriptionID); s.Kind != "l0" || s.DisplayTitle != nil {
		t.Errorf("L0 subscription: %+v", s)
	}

	// The dead address waits, then fails on its third try.
	gone := g.importOf(dead)
	if gone.Status != "pending" || gone.Error == nil || gone.Attempts != 1 || !gone.NextAttemptAt.After(time.Now()) {
		t.Fatalf("after one failure: %+v", gone)
	}
	for range 2 {
		if _, err := g.r.Pool.Exec(ctx, `UPDATE subscription_imports SET next_attempt_at = now() WHERE id = $1`, gone.ID); err != nil {
			t.Fatal(err)
		}
		g.drain()
	}
	if gone = g.importOf(dead); gone.Status != "failed" || gone.Attempts != 3 {
		t.Fatalf("after three failures: %+v", gone)
	}

	// Importing again tries the failure again, from the start.
	if res, _ = g.r.ImportOPML(ctx, g.uid, []byte(opml)); res.Queued != 1 {
		t.Fatalf("re-import queued %d, want the failed entry", res.Queued)
	}
	if gone = g.importOf(dead); gone.Status != "pending" || gone.Attempts != 0 || gone.Error != nil {
		t.Fatalf("re-queued: %+v", gone)
	}

	// At the subscription cap, an entry fails at once: waiting will not help.
	g.r.MaxSubscriptions = 3
	if _, err := g.r.ImportOPML(ctx, g.uid, []byte(`<opml><body><outline xmlUrl="`+legacy.URL+`/more"/></body></opml>`)); err != nil {
		t.Fatal(err)
	}
	g.drain()
	if e := g.importOf(legacy.URL + "/more"); e.Status != "failed" || e.Attempts != 1 || !strings.Contains(*e.Error, "limit") {
		t.Fatalf("over the cap: %+v", e)
	}

	// The re-queued dead entry met the cap too.
	if e := g.importOf(dead); e.Status != "failed" || !strings.Contains(*e.Error, "limit") {
		t.Fatalf("re-queued entry at the cap: %+v", e)
	}
	if n, err := g.r.ClearImports(ctx, g.uid, "failed"); err != nil || n != 2 {
		t.Fatalf("clear failed: %d %v", n, err)
	}
	if _, err := g.r.ImportOPML(ctx, g.uid, []byte(`<rss/>`)); err == nil {
		t.Error("a feed imported as OPML")
	}
}
