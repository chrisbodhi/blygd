package web

import (
	"testing"
	"time"

	"github.com/chrisbodhi/blygd/internal/mention"
)

func expiry(t *testing.T, m map[string]any) *time.Time {
	t.Helper()
	v, ok := m["expires_at"]
	if !ok {
		t.Fatalf("no expires_at: %v", m)
	}
	if v == nil {
		return nil
	}
	at, err := time.Parse(time.RFC3339, v.(string))
	if err != nil {
		t.Fatal(err)
	}
	return &at
}

func (u *user) mint(t *testing.T, body map[string]any) (*user, map[string]any) {
	t.Helper()
	r := u.api("POST", "/tokens", body)
	expect(t, r, 201)
	m := r.json(t)
	v := *u
	v.token = m["token"].(string)
	return &v, m
}

// Tokens may expire; a minted token never outlives its minter; an expired
// or self-revoked token no longer authenticates.
func TestTokenExpiry(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice := e.user("alice")
	if expiry(t, alice.api("GET", "/me", nil).json(t)) != nil {
		t.Fatal("the owner token expires")
	}

	// An hour, and allowed to mint.
	tool, m := alice.mint(t, map[string]any{"name": "tool", "scopes": []string{"items:read", "tokens:write"}, "expires_in": 3600})
	toolExp := expiry(t, m)
	if toolExp == nil || toolExp.Sub(time.Now()) < 59*time.Minute || toolExp.Sub(time.Now()) > time.Hour {
		t.Fatalf("expires_in 3600 gave %v", toolExp)
	}
	if got := expiry(t, tool.api("GET", "/me", nil).json(t)); got == nil || !got.Equal(*toolExp) {
		t.Errorf("/me expires_at %v, want %v", got, toolExp)
	}

	// Its children: none outlives it, asked or not.
	for _, body := range []map[string]any{
		{"name": "child", "scopes": []string{"items:read"}},
		{"name": "child", "scopes": []string{"items:read"}, "expires_in": 7200},
	} {
		_, m := tool.mint(t, body)
		if got := expiry(t, m); got == nil || !got.Equal(*toolExp) {
			t.Errorf("%v: expires %v, want the minter's %v", body, got, toolExp)
		}
	}
	_, m = tool.mint(t, map[string]any{"name": "short", "scopes": []string{"items:read"}, "expires_in": 60})
	if got := expiry(t, m); got == nil || !got.Before(*toolExp) {
		t.Errorf("a shorter expiry was not kept: %v", got)
	}
	expect(t, alice.api("POST", "/tokens", map[string]any{"name": "x", "scopes": []string{"items:read"}, "expires_in": 0}), 400)
	_, m = alice.mint(t, map[string]any{"name": "forever", "scopes": []string{"items:read"}})
	if expiry(t, m) != nil {
		t.Error("a non-expiring minter's child without expires_in should not expire")
	}

	// The list shows it.
	found := false
	for _, tk := range alice.api("GET", "/tokens", nil).json(t)["tokens"].([]any) {
		if tk.(map[string]any)["name"] == "tool" {
			found = expiry(t, tk.(map[string]any)) != nil
		}
	}
	if !found {
		t.Error("the token list does not show tool's expiry")
	}

	// Past its time it is refused.
	if _, err := e.pool.Exec(t.Context(), `UPDATE tokens SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 second' WHERE name = 'tool'`); err != nil {
		t.Fatal(err)
	}
	expect(t, tool.api("GET", "/me", nil), 401)
}

func TestRevokeSelf(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice := e.user("alice")
	// A narrow token, without tokens:write, can still sign itself out.
	studio, _ := alice.mint(t, map[string]any{"name": "studio", "scopes": []string{"items:read"}, "expires_in": 3600})
	expect(t, studio.api("GET", "/me", nil), 200)
	expect(t, studio.api("DELETE", "/tokens/1", nil), 403) // not another's
	expect(t, studio.api("DELETE", "/tokens/self", nil), 204)
	expect(t, studio.api("GET", "/me", nil), 401)
	expect(t, alice.api("GET", "/me", nil), 200) // the minter is untouched
}
