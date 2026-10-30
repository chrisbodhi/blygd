package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chrisbodhi/blygd/internal/events"
	"github.com/chrisbodhi/blygd/internal/mention"
	"github.com/chrisbodhi/blygd/internal/testblyg"
)

type sse struct {
	Type string
	Data map[string]any
}

// openStream opens GET /api/events and delivers its events on a channel.
func openStream(t *testing.T, base, token string) (<-chan sse, *http.Response) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, resp
	}
	out := make(chan sse, 64)
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		var ev sse
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.Type = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev.Data)
			case line == "" && ev.Type != "":
				out <- ev
				ev = sse{}
			}
		}
	}()
	return out, resp
}

// next returns the next event of a type, or fails after a deadline.
func next(t *testing.T, ch <-chan sse, typ string) sse {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type == typ {
				return ev
			}
		case <-deadline:
			t.Fatalf("no %s event", typ)
		}
	}
}

// none fails if an event of a type arrives within a short while.
func none(t *testing.T, ch <-chan sse, typ, why string) {
	t.Helper()
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case ev := <-ch:
			if ev.Type == typ {
				t.Fatalf("%s: got %v", why, ev)
			}
		case <-deadline:
			return
		}
	}
}

func TestEventStream(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice, bob := e.user("alice"), e.user("bob")
	<-e.hub.Ready()
	ctx := context.Background()
	aliceEv, _ := openStream(t, e.srv.URL, alice.token)
	bobEv, _ := openStream(t, e.srv.URL, bob.token)
	readerTok := alice.api("POST", "/tokens", map[string]any{"name": "r", "scopes": []string{"reader:read"}}).json(t)["token"].(string)
	readerEv, _ := openStream(t, e.srv.URL, readerTok)

	// A backfill reports progress batch by batch, to subscribers only.
	remote := &testblyg.Blyg{S: testblyg.New(t), Mount: "/r/", Title: "Remote"}
	for _, id := range []string{"0a00000000000000000000000a", "0b00000000000000000000000b", "0c00000000000000000000000c"} {
		remote.Items = append(remote.Items, &testblyg.Item{ID: id, Kind: "fragment", Version: 1, ContentMD: id, Updated: time.Now()})
	}
	remote.Publish()
	e.reader.MaxFetches = 2
	r := alice.api("POST", "/subscriptions", map[string]any{"url": remote.Origin()})
	expect(t, r, 201)
	sid := int(r.json(t)["subscription"].(map[string]any)["id"].(float64))
	e.poll(sid)
	ev := next(t, aliceEv, "import")
	if ev.Data["subscription_id"] != float64(sid) || ev.Data["items"] != float64(2) || ev.Data["more"] != true {
		t.Fatalf("first batch: %v", ev.Data)
	}
	e.poll(sid)
	if ev = next(t, aliceEv, "import"); ev.Data["items"] != float64(3) || ev.Data["more"] != false {
		t.Fatalf("last batch: %v", ev.Data)
	}
	next(t, readerEv, "import") // reader:read hears imports
	none(t, bobEv, "import", "bob does not subscribe")

	// Mentions reach the target origin's owner; outbound sends, the sender.
	f := alice.draft(t, map[string]any{"content_md": "Quote me."})
	alice.publish(t, f, 200)
	bob.subscribe(t, alice.origin)
	th := bob.draft(t, map[string]any{"kind": "thread", "content_md": "![[" + f + "]]"})
	bob.publish(t, th, 200)
	if _, err := e.workers.SendBatch(ctx); err != nil {
		t.Fatal(err)
	}
	if ev = next(t, bobEv, "outbound"); ev.Data["status"] != "sent" {
		t.Fatalf("outbound: %v", ev.Data)
	}
	if ev = next(t, aliceEv, "mention"); ev.Data["status"] != "pending" {
		t.Fatalf("mention received: %v", ev.Data)
	}
	if _, err := e.workers.VerifyBatch(ctx); err != nil {
		t.Fatal(err)
	}
	if ev = next(t, aliceEv, "mention"); ev.Data["status"] != "verified" || ev.Data["relation"] != "transclusion" {
		t.Fatalf("mention verified: %v", ev.Data)
	}
	none(t, readerEv, "mention", "a reader:read token hears no mentions")
	none(t, bobEv, "mention", "alice's mentions are not bob's")

	// A user's streams are capped.
	for i := 0; i < maxStreams-2; i++ { // two are open already
		if _, resp := openStream(t, e.srv.URL, alice.token); resp.StatusCode != 200 {
			t.Fatalf("stream %d refused early: %d", i, resp.StatusCode)
		}
	}
	if _, resp := openStream(t, e.srv.URL, alice.token); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("stream over the cap: %d", resp.StatusCode)
	}
}

// A stream outlives the server's WriteTimeout, which would otherwise end
// every stream after a minute in production.
func TestEventStreamOutlivesWriteTimeout(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice := e.user("alice")
	<-e.hub.Ready()
	srv := httptest.NewUnstartedServer(Router(e.api, e.surface, discardLog()))
	srv.Config.WriteTimeout = time.Second
	srv.Start()
	t.Cleanup(srv.Close)
	ch, resp := openStream(t, srv.URL, alice.token)
	if resp.StatusCode != 200 {
		t.Fatalf("stream: %d", resp.StatusCode)
	}
	time.Sleep(1500 * time.Millisecond)
	var originID int64
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM origins WHERE base_url = $1`, alice.origin).Scan(&originID); err != nil {
		t.Fatal(err)
	}
	if err := events.Notify(context.Background(), e.pool, "mention", originID, 0, map[string]any{"id": 1, "status": "pending"}); err != nil {
		t.Fatal(err)
	}
	next(t, ch, "mention")
}

// A user's streams hear every change to their items, whichever client made
// it, with the draft's revision so a client can tell its own edit from
// another's. Nobody else hears them, and a change that rolls back is never
// heard.
func TestItemEvents(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice, bob := e.user("alice"), e.user("bob")
	<-e.hub.Ready()
	aliceEv, _ := openStream(t, e.srv.URL, alice.token)
	bobEv, _ := openStream(t, e.srv.URL, bob.token)
	readerTok := alice.api("POST", "/tokens", map[string]any{"name": "r", "scopes": []string{"reader:read"}}).json(t)["token"].(string)
	readerEv, _ := openStream(t, e.srv.URL, readerTok)
	want := func(change, id string, version int, rev any) {
		t.Helper()
		ev := next(t, aliceEv, "item")
		if ev.Data["id"] != id || ev.Data["change"] != change || ev.Data["draft_rev"] != rev ||
			(version > 0) != (ev.Data["version"] != nil) || version > 0 && ev.Data["version"] != float64(version) {
			t.Fatalf("want %s of %s (v%d, rev %v), got %v", change, id, version, rev, ev.Data)
		}
	}

	r := alice.api("POST", "/items", map[string]any{"content_md": "one"})
	expect(t, r, 201)
	id := r.json(t)["id"].(string)
	want("created", id, 0, r.json(t)["draft_rev"])
	r = alice.api("PATCH", "/items/"+id, map[string]any{"content_md": "two"})
	expect(t, r, 200)
	want("edited", id, 0, r.json(t)["draft_rev"])
	r = alice.api("POST", "/items/"+id+"/publish", nil)
	expect(t, r, 200)
	want("published", id, 1, r.json(t)["item"].(map[string]any)["draft_rev"])
	expect(t, alice.api("POST", "/items/"+id+"/versions/1/pin", nil), 200)
	want("pinned", id, 1, nil)
	expect(t, alice.api("POST", "/items/"+id+"/versions/1/pin", nil), 200)
	none(t, aliceEv, "item", "pinning a pinned version changes nothing")
	expect(t, alice.api("POST", "/items/"+id+"/withdraw", nil), 200)
	want("withdrawn", id, 2, nil)

	// Over the fragment cap: the publish rolls back, and so does its event.
	r = alice.api("POST", "/items", map[string]any{"content_md": strings.Repeat("x", 2001)})
	expect(t, r, 201)
	long := r.json(t)["id"].(string)
	want("created", long, 0, r.json(t)["draft_rev"])
	expect(t, alice.api("POST", "/items/"+long+"/publish", nil), 422)
	none(t, aliceEv, "item", "a refused publish")
	expect(t, alice.api("DELETE", "/items/"+long, nil), 204)
	want("deleted", long, 0, nil)

	none(t, bobEv, "item", "alice's items are not bob's")
	none(t, readerEv, "item", "a reader:read token hears no items")
}
