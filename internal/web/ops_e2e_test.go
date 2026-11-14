package web

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

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
	for _, k := range []string{"verify", "verify_late", "send", "send_late", "send_abandoned", "polled", "poll_late", "poll_failing", "import", "import_late"} {
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
