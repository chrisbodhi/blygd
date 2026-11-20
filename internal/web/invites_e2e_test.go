package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chrisbodhi/blygd/internal/mention"
)

func (e *env) invite(t *testing.T) string {
	t.Helper()
	codes, err := e.accts.CreateInvites(context.Background(), 1, time.Now().Add(time.Hour), "test")
	if err != nil {
		t.Fatal(err)
	}
	return codes[0]
}

func (e *env) redeem(code, handle string, hdr ...string) resp {
	return e.do("POST", e.srv.URL+"/api/invites/redeem", "", map[string]any{"code": code, "handle": handle, "title": "A blyg"}, hdr...)
}

// redeemedBy is the handle an invite made, or "" while it is unused.
func (e *env) redeemedBy(t *testing.T, code string) string {
	t.Helper()
	var handle *string
	if err := e.pool.QueryRow(context.Background(), `SELECT u.handle FROM invites i LEFT JOIN users u ON u.id = i.redeemed_by
		WHERE i.code_hash = sha256($1::text::bytea)`, code).Scan(&handle); err != nil {
		t.Fatal(err)
	}
	if handle == nil {
		return ""
	}
	return *handle
}

// An invite makes a user, their origin, served at once, and an owner
// token, shown once; and only one user.
func TestInviteMakesOneUser(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	code := e.invite(t)

	r := e.redeem("  "+code+" ", " Alice ")
	expect(t, r, 201)
	got := r.json(t)
	if got["handle"] != "alice" || got["origin"] != e.srv.URL+"/~alice/" {
		t.Fatalf("redeemed: %s", r.body)
	}
	alice := &user{e: e, token: got["token"].(string), origin: got["origin"].(string)}
	if me := alice.api("GET", "/me", nil).json(t); me["handle"] != "alice" || me["scopes"].([]any)[0] != "*" || me["expires_at"] != nil {
		t.Fatalf("me: %v", me)
	}
	if m := alice.get("blyg.json").json(t); m["site"] != alice.origin || m["title"] != "A blyg" {
		t.Fatalf("manifest: %v", m)
	}
	if e.redeemedBy(t, code) != "alice" {
		t.Fatal("the invite does not name its user")
	}

	r = e.redeem(code, "bob")
	expect(t, r, 422)
	expect(t, e.do("GET", e.srv.URL+"/~bob/blyg.json", "", nil), 404)
}

// The invite form is told where the blyg will live before the code is
// spent: under the public URL, whichever host asks (a former public host
// still serves the API), and exactly where redeeming then puts it.
func TestInviteOriginIsWhereRedeemingMounts(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	r := e.do("GET", e.srv.URL+"/api/invites/origin", "", nil)
	expect(t, r, 200)
	tmpl, _ := r.json(t)["origin_template"].(string)
	if tmpl != e.srv.URL+"/~{handle}/" {
		t.Fatalf("template: %s", r.body)
	}

	req, _ := http.NewRequest("GET", e.srv.URL+"/api/invites/origin", nil)
	req.Host = "old-host.example"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		OriginTemplate string `json:"origin_template"`
	}
	err = json.NewDecoder(res.Body).Decode(&got)
	res.Body.Close()
	if err != nil || got.OriginTemplate != tmpl {
		t.Fatalf("asked by another host: %q, %v", got.OriginTemplate, err)
	}

	made := e.redeem(e.invite(t), "alice")
	expect(t, made, 201)
	if o := made.json(t)["origin"]; o != strings.Replace(tmpl, "{handle}", "alice", 1) {
		t.Fatalf("template %s, but the blyg is at %v", tmpl, o)
	}
}

// Two redemptions of one code at once make one user.
func TestInviteRedeemedOnceUnderRace(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	code := e.invite(t)
	statuses := make([]int, 8)
	var wg sync.WaitGroup
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i] = e.redeem(code, "user-"+string(rune('a'+i))).status
		}()
	}
	wg.Wait()
	made := 0
	for _, s := range statuses {
		switch s {
		case 201:
			made++
		case 422:
		default:
			t.Errorf("status %d", s)
		}
	}
	var users int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM users`).Scan(&users)
	if made != 1 || users != 1 {
		t.Fatalf("%d redemptions succeeded and %d users exist, want 1 and 1 (%v)", made, users, statuses)
	}
}

// A refused handle, or an origin that cannot be made, leaves the invite
// for another try.
func TestRefusedRedemptionLeavesTheInvite(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	e.user("alice")
	code := e.invite(t)

	r := e.redeem(code, "alice")
	expect(t, r, 409)
	if r.json(t)["error"] != "conflict" {
		t.Fatalf("taken: %s", r.body)
	}
	expect(t, e.redeem(code, "studio"), 400)
	expect(t, e.redeem(code, "-bad"), 400)
	expect(t, e.do("POST", e.srv.URL+"/api/invites/redeem", "", map[string]any{"code": code, "handle": "long", "title": string(make([]byte, 201))}), 400)

	real := e.accts.Origins
	e.accts.Origins = failingOrigins{e.studio}
	if _, _, _, err := e.accts.Redeem(context.Background(), code, "bob", e.srv.URL+"/", ""); err == nil {
		t.Fatal("redeemed with the origin failing")
	}
	e.accts.Origins = real
	if e.redeemedBy(t, code) != "" {
		t.Fatal("a refused redemption spent the invite")
	}
	expect(t, e.redeem(code, "bob"), 201)
}

func TestExpiredInviteIsRefused(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	code := e.invite(t)
	if _, err := e.pool.Exec(context.Background(), `UPDATE invites SET expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	r := e.redeem(code, "alice")
	expect(t, r, 422)
	// Expired, used and unknown read alike, so a guess learns nothing.
	if unknown := e.redeem("blygd-invite-nope", "alice"); string(unknown.body) != string(r.body) {
		t.Fatalf("expired %s, unknown %s", r.body, unknown.body)
	}
	if _, err := e.accts.CreateInvites(context.Background(), 1, time.Now().Add(-time.Hour), ""); err == nil {
		t.Fatal("minted an invite that expired already")
	}
}

// A wrong code is a failed authentication, charged to the address as a
// wrong token is; and redeeming is charged to the address's fetch budget,
// since there is no token to charge.
func TestInviteBudgets(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice := e.user("alice")
	now := time.Unix(1_000_000, 0)
	e.withLimits(&now)
	from := func(addr string) []string { return []string{"X-Forwarded-For", "198.51.100.1, " + addr} }

	fetch := int(Budgets[costFetch].Burst)
	for i := 0; i < fetch; i++ {
		expect(t, e.redeem("blygd-invite-guess", "alice", from("203.0.113.30")...), 422)
	}
	r := e.redeem(e.invite(t), "bob", from("203.0.113.30")...)
	expect(t, r, 429)
	if r.header.Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	// The wrong codes count toward the failed-authentication budget: with
	// the rest of it spent on wrong tokens, a good token from there waits.
	bad := *alice
	bad.token = "nope"
	for i := fetch; i < int(AuthFailBudget.Burst); i++ {
		expect(t, bad.apiFrom("203.0.113.30", "GET", "/me"), 401)
	}
	expect(t, alice.apiFrom("203.0.113.30", "GET", "/me"), 429)

	// A handle refused before any code is read, or no code at all, is no
	// guess: with all but one failure spent on wrong tokens, neither
	// spends the last.
	for i := 1; i < int(AuthFailBudget.Burst); i++ {
		expect(t, bad.apiFrom("203.0.113.31", "GET", "/me"), 401)
	}
	expect(t, e.redeem("blygd-invite-guess", "studio", from("203.0.113.31")...), 400)
	expect(t, e.redeem("  ", "bob", from("203.0.113.31")...), 400)
	expect(t, alice.apiFrom("203.0.113.31", "GET", "/me"), 200)

	expect(t, e.redeem(e.invite(t), "bob", from("203.0.113.32")...), 201)
}
