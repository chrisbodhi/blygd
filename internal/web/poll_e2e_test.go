package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chrisbodhi/blygd/internal/mention"
	"github.com/chrisbodhi/blygd/internal/testblyg"
)

// POST /api/subscriptions/{sid}/poll polls now, within §12.3's etiquette.
func TestPollNowAPI(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice, bob := e.user("alice"), e.user("bob")
	ctx := context.Background()
	remote := &testblyg.Blyg{S: testblyg.New(t), Mount: "/r/", Title: "Remote"}
	first := &testblyg.Item{ID: "0a00000000000000000000000a", Kind: "fragment", Version: 1, ContentMD: "one", Updated: time.Now()}
	remote.Items = []*testblyg.Item{first}
	remote.Publish()
	sid := itoa(alice.subscribe(t, remote.Origin())) // subscribe polls once
	path := "/subscriptions/" + sid + "/poll"
	age := func(sql string) {
		t.Helper()
		if _, err := e.pool.Exec(ctx, `UPDATE remote_origins SET `+sql+` WHERE id = $1`, sid); err != nil {
			t.Fatal(err)
		}
	}

	// Just polled: the shared mirror's floor applies.
	r := alice.api("POST", path, nil)
	expect(t, r, 429)
	if r.header.Get("Retry-After") == "" || !strings.Contains(r.json(t)["message"].(string), "polled less than") {
		t.Fatalf("floor refusal: %v %s", r.header, r.body)
	}

	// Past the floor, it polls and picks up what changed.
	remote.Items = append(remote.Items, &testblyg.Item{ID: "0b00000000000000000000000b", Kind: "fragment", Version: 1, ContentMD: "two", Updated: time.Now()})
	remote.Publish()
	age("last_polled_at = now() - interval '2 minutes'")
	// Someone else's subscription is not yours to poll, even when it is due.
	expect(t, bob.api("POST", path, nil), 404)
	r = alice.api("POST", path, nil)
	expect(t, r, 200)
	if r.json(t)["items"].(float64) != 2 {
		t.Fatalf("poll did not import the new item: %s", r.body)
	}

	// §12.3: polling never tightens on failure. A backing-off origin waits.
	remote.S.SetResponse("/r/feed.xml", testblyg.Response{Status: 500})
	age("last_polled_at = now() - interval '2 minutes'")
	expect(t, alice.api("POST", path, nil), 200) // this poll fails, and backs off
	age("last_polled_at = now() - interval '2 minutes'")
	r = alice.api("POST", path, nil)
	expect(t, r, 429)
	if !strings.Contains(r.json(t)["message"].(string), "backing off") {
		t.Fatalf("backoff refusal: %s", r.body)
	}

	// Never beside a poll already running.
	age("poll_failures = 0, next_poll_at = now(), last_polled_at = now() - interval '2 minutes', poll_lease_until = now() + interval '1 minute'")
	r = alice.api("POST", path, nil)
	expect(t, r, 429)
	if !strings.Contains(r.json(t)["message"].(string), "already running") {
		t.Fatalf("lease refusal: %s", r.body)
	}

	// Reading is not polling.
	tok := alice.api("POST", "/tokens", map[string]any{"name": "ro", "scopes": []string{"reader:read"}}).json(t)["token"].(string)
	expect(t, e.do("POST", e.srv.URL+"/api"+path, tok, nil), 403)
}
