package web

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chrisbodhi/blygd/internal/mention"
)

// The queue line says what is waiting, and is a warning only when
// something is late: a late row means a worker is behind or stopped.
func TestQueueLine(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice, bob := e.user("alice"), e.user("bob")
	ctx := context.Background()
	app := &App{Workers: e.workers, Reader: e.reader}
	line := func() (slog.Level, map[string]string) {
		t.Helper()
		level, attrs := app.queueLine(ctx)
		m := map[string]string{}
		for i := 0; i+1 < len(attrs); i += 2 {
			m[attrs[i].(string)] = fmt.Sprint(attrs[i+1])
		}
		if m["err"] != "" {
			t.Fatal(m["err"])
		}
		return level, m
	}
	want := func(level slog.Level, wantLevel slog.Level, m map[string]string, pairs ...string) {
		t.Helper()
		if level != wantLevel {
			t.Errorf("level %v, want %v: %v", level, wantLevel, m)
		}
		for i := 0; i+1 < len(pairs); i += 2 {
			if m[pairs[i]] != pairs[i+1] {
				t.Errorf("%s = %s, want %s: %v", pairs[i], m[pairs[i]], pairs[i+1], m)
			}
		}
	}

	l, m := line()
	want(l, slog.LevelInfo, m, "send", "0", "verify", "0", "polled", "0", "import", "0")
	for _, k := range []string{"verify", "verify_late", "send", "send_late", "send_abandoned_24h", "polled", "poll_late", "poll_failing", "import", "import_late"} {
		if _, ok := m[k]; !ok {
			t.Errorf("no %s in the line: %v", k, m)
		}
	}

	// A link to bob's item queues a mention to send.
	b := bob.draft(t, map[string]any{"content_md": "Bob's."})
	bob.publish(t, b, 200)
	alice.subscribe(t, bob.origin)
	a := alice.draft(t, map[string]any{"kind": "thread", "content_md": "See [[" + b + "]]."})
	alice.publish(t, a, 200)
	l, m = line()
	want(l, slog.LevelInfo, m, "send", "1", "send_late", "0", "polled", "1", "poll_late", "0")

	// Late: past due by more than any lease.
	if _, err := e.pool.Exec(ctx, `UPDATE outbound_mentions SET next_attempt_at = now() - interval '1 hour' WHERE status = 'pending'`); err != nil {
		t.Fatal(err)
	}
	l, m = line()
	want(l, slog.LevelWarn, m, "send", "1", "send_late", "1")

	// Sent, and the queue drains to quiet; then a poll falls behind.
	if _, err := e.workers.SendBatch(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.workers.VerifyBatch(ctx); err != nil {
		t.Fatal(err)
	}
	l, m = line()
	want(l, slog.LevelInfo, m, "send", "0", "send_late", "0", "verify", "0")
	if _, err := e.pool.Exec(ctx, `UPDATE remote_origins SET next_poll_at = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	l, m = line()
	want(l, slog.LevelWarn, m, "polled", "1", "poll_late", "1")
	if _, err := e.pool.Exec(ctx, `UPDATE remote_origins SET next_poll_at = now() + interval '1 hour'`); err != nil {
		t.Fatal(err)
	}

	// A long import is due all at once and worked off one entry per user
	// per batch: its tail waiting is not the worker failing. Only the head
	// of a user's line can be late.
	var opml strings.Builder
	opml.WriteString(`<?xml version="1.0"?><opml version="2.0"><head><title>x</title></head><body>`)
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&opml, `<outline type="rss" text="F%d" xmlUrl="https://feeds.invalid/%d.xml"/>`, i, i)
	}
	opml.WriteString(`</body></opml>`)
	expect(t, alice.api("POST", "/subscriptions/imports", map[string]any{"opml": opml.String()}), 202)
	if _, err := e.pool.Exec(ctx, `UPDATE subscription_imports SET next_attempt_at = now() - interval '20 minutes'`); err != nil {
		t.Fatal(err)
	}
	l, m = line()
	want(l, slog.LevelWarn, m, "import", "50", "import_late", "1")
	if _, err := e.pool.Exec(ctx, `UPDATE subscription_imports SET next_attempt_at = now()`); err != nil {
		t.Fatal(err)
	}
	l, m = line()
	want(l, slog.LevelInfo, m, "import", "50", "import_late", "0")

	// Abandoned sends count over a day, so the number reads on its own.
	if _, err := e.pool.Exec(ctx, `UPDATE outbound_mentions SET status = 'abandoned', updated_at = now()`); err != nil {
		t.Fatal(err)
	}
	_, m = line()
	want(slog.LevelInfo, slog.LevelInfo, m, "send_abandoned_24h", "1")
	if _, err := e.pool.Exec(ctx, `UPDATE outbound_mentions SET updated_at = now() - interval '2 days'`); err != nil {
		t.Fatal(err)
	}
	_, m = line()
	want(slog.LevelInfo, slog.LevelInfo, m, "send_abandoned_24h", "0")
}

// Revoking every token a user holds suspends them: nothing they hold
// authenticates, what they published is still served, and no one else is
// touched. A new token lifts it.
func TestRevokeAllSuspends(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice, bob := e.user("alice"), e.user("bob")
	ctx := context.Background()
	id := alice.draft(t, map[string]any{"content_md": "Still here."})
	alice.publish(t, id, 200)
	tool, _ := alice.mint(t, map[string]any{"name": "a tool", "scopes": []string{"items:read"}})

	n, err := e.accts.RevokeAll(ctx, alice.id)
	if err != nil || n != 2 {
		t.Fatalf("revoked %d, %v; want the owner token and the tool's", n, err)
	}
	expect(t, alice.api("GET", "/me", nil), 401)
	expect(t, tool.api("GET", "/me", nil), 401)
	expect(t, alice.get("items/"+id+".json"), 200)
	expect(t, bob.api("GET", "/me", nil), 200)
	if n, _ := e.accts.RevokeAll(ctx, alice.id); n != 0 {
		t.Errorf("a second revoke-all revoked %d", n)
	}

	secret, _, err := e.accts.Mint(ctx, alice.id, "owner", []string{"*"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	back := *alice
	back.token = secret
	expect(t, back.api("GET", "/me", nil), 200)
}

// Suspending a user stops what they had in motion, not only their logins:
// their unsent mentions are abandoned, so nothing more reaches the people
// they were suspended over, and an event stream they opened before ends at
// the next heartbeat.
func TestSuspendStopsWorkInMotion(t *testing.T) {
	saved := heartbeat
	heartbeat = 100 * time.Millisecond
	t.Cleanup(func() { heartbeat = saved })
	e := newEnv(t, mention.DefaultRateLimits)
	alice, bob := e.user("alice"), e.user("bob")
	ctx := context.Background()
	app := &App{Accounts: e.accts, Studio: e.studio, Mentions: &mention.Store{Pool: e.pool}}

	target := bob.draft(t, map[string]any{"content_md": "Bob's."})
	bob.publish(t, target, 200)
	alice.subscribe(t, bob.origin)
	stub := alice.draft(t, map[string]any{"stub_of": map[string]any{"id": target}})
	alice.edit(t, stub, "![["+target+"]]\n\nSomething bob would rather not get.")
	alice.publish(t, stub, 200)

	// Cancelled at cleanup, before the server closes: a stream left open by
	// a failure would otherwise keep the test server from shutting down.
	streamCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(streamCtx, "GET", e.srv.URL+"/api/events", nil)
	req.Header.Set("Authorization", "Bearer "+alice.token)
	stream, err := http.DefaultClient.Do(req)
	if err != nil || stream.StatusCode != 200 {
		t.Fatalf("stream: %v %v", err, stream)
	}
	ended := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(stream.Body)
		for sc.Scan() {
		}
		stream.Body.Close()
		close(ended)
	}()

	tokens, sends, err := app.Suspend(ctx, "alice")
	if err != nil || tokens != 1 || sends != 1 {
		t.Fatalf("suspend: %d tokens, %d sends, %v", tokens, sends, err)
	}
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("a stream opened before the suspension is still open")
	}
	if _, err := e.workers.SendBatch(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.workers.VerifyBatch(ctx); err != nil {
		t.Fatal(err)
	}
	if ms := bob.api("GET", "/mentions", nil).json(t)["mentions"].([]any); len(ms) != 0 {
		t.Fatalf("a suspended user's queued mention reached its target: %v", ms)
	}
	expect(t, alice.api("GET", "/me", nil), 401)
	expect(t, alice.get("t/"+stub+"/"), 200) // what they published stays published
	if _, _, err := app.Suspend(ctx, "nobody"); err == nil {
		t.Error("suspended a user who does not exist")
	}
}
