package web

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chrisbodhi/blygd/internal/limit"
	"github.com/chrisbodhi/blygd/internal/mention"
)

// Every route's cost class is the one openapi.json states, and every
// entry in routeCosts is a real route.
func TestRouteCostsMatchOpenAPI(t *testing.T) {
	paths, _ := loadOpenAPI(t)
	routes := map[string]bool{}
	for _, r := range (&API{}).routes() {
		routes[r.pattern] = true
		method, path, _ := strings.Cut(r.pattern, " ")
		var op struct {
			Cost      string                     `json:"x-blygd-cost"`
			Responses map[string]json.RawMessage `json:"responses"`
		}
		if err := json.Unmarshal(paths[path][strings.ToLower(method)], &op); err != nil {
			t.Fatalf("%s: %v", r.pattern, err)
		}
		if op.Cost != routeCost(r.pattern) {
			t.Errorf("%s: cost %q in code, %q in openapi.json", r.pattern, routeCost(r.pattern), op.Cost)
		}
		if op.Responses["429"] == nil {
			t.Errorf("%s: 429 is not documented", r.pattern)
		}
	}
	for pattern := range routeCosts {
		if !routes[pattern] {
			t.Errorf("routeCosts names %s, which is not a route", pattern)
		}
	}
}

// syncBuffer is a log sink the server goroutines can share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// withLimits turns budgets on behind a trusted proxy, so each request can
// name its address in X-Forwarded-For, on a clock the test moves. It
// returns the request log.
func (e *env) withLimits(now *time.Time) *syncBuffer {
	l := NewLimits(true)
	l.SetNow(func() time.Time { return *now })
	e.api.Limits = l
	logs := &syncBuffer{}
	e.srv.Config.Handler = Router(e.api, e.surface, slog.New(slog.NewTextHandler(logs, nil)))
	return logs
}

func (u *user) apiFrom(addr, method, path string) resp {
	return u.e.do(method, u.e.srv.URL+"/api"+path, u.token, nil, "X-Forwarded-For", "198.51.100.1, "+addr)
}

func TestTokenAndUserBudgets(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice := e.user("alice")
	now := time.Unix(1_000_000, 0)
	logs := e.withLimits(&now)

	// export: 2 per token, 4 per user.
	for i := 0; i < 2; i++ {
		expect(t, alice.apiFrom("203.0.113.5", "GET", "/origin/export"), 200)
	}
	r := alice.apiFrom("203.0.113.5", "GET", "/origin/export")
	expect(t, r, 429)
	if r.header.Get("Retry-After") != "30" || r.json(t)["error"] != "rate_limited" {
		t.Fatalf("429: Retry-After %q, %s", r.header.Get("Retry-After"), r.body)
	}
	// Other classes are budgeted apart.
	expect(t, alice.apiFrom("203.0.113.5", "GET", "/items"), 200)

	// A second token of the same user has its own export budget, until the
	// user's runs out.
	mint := func() *user {
		secret, _, err := e.accts.Mint(t.Context(), alice.id, "tool", []string{"items:read"})
		if err != nil {
			t.Fatal(err)
		}
		u := *alice
		u.token = secret
		return &u
	}
	tool := mint()
	for i := 0; i < 2; i++ {
		expect(t, tool.apiFrom("203.0.113.6", "GET", "/origin/export"), 200)
	}
	expect(t, mint().apiFrom("203.0.113.7", "GET", "/origin/export"), 429) // the user's 4 are spent

	now = now.Add(30 * time.Second)
	expect(t, alice.apiFrom("203.0.113.5", "GET", "/origin/export"), 200)

	// Each request is logged with its address, and its token when it had one.
	if !strings.Contains(logs.String(), "status=429") || !strings.Contains(logs.String(), "addr=203.0.113.5") || !strings.Contains(logs.String(), "token=") {
		t.Errorf("request log lacks status, address or token:\n%s", logs.String())
	}
}

func TestFailedAuthenticationBudget(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice := e.user("alice")
	now := time.Unix(1_000_000, 0)
	e.withLimits(&now)

	bad := *alice
	bad.token = "nope"
	for i := 0; i < int(AuthFailBudget.Burst); i++ {
		expect(t, bad.apiFrom("203.0.113.9", "GET", "/me"), 401)
	}
	// Spent: even a good token from that address waits.
	r := alice.apiFrom("203.0.113.9", "GET", "/me")
	expect(t, r, 429)
	if r.header.Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	// Elsewhere, and later, all is well. IPv6 is charged per /64.
	expect(t, alice.apiFrom("203.0.113.10", "GET", "/me"), 200)
	for i := 0; i < int(AuthFailBudget.Burst); i++ {
		bad.apiFrom("2001:db8:1:2::"+string(rune('a'+i%6)), "GET", "/me")
	}
	expect(t, alice.apiFrom("2001:db8:1:2::ffff", "GET", "/me"), 429)
	expect(t, alice.apiFrom("2001:db8:1:3::1", "GET", "/me"), 200)
	now = now.Add(time.Minute)
	expect(t, alice.apiFrom("203.0.113.9", "GET", "/me"), 200)
}

func TestPublicBudgets(t *testing.T) {
	saved := SurfaceBudget
	SurfaceBudget = limit.Rate{PerMinute: 60, Burst: 3}
	t.Cleanup(func() { SurfaceBudget = saved })
	e := newEnv(t, mention.DefaultRateLimits)
	alice := e.user("alice")
	now := time.Unix(1_000_000, 0)
	e.withLimits(&now)
	from := func(addr, path string) resp {
		return e.do("GET", alice.origin+path, "", nil, "X-Forwarded-For", addr)
	}
	for _, p := range []string{"blyg.json", "feed.xml", ""} {
		expect(t, from("203.0.113.20", p), 200)
	}
	r := from("203.0.113.20", "blyg.json")
	expect(t, r, 429)
	if r.header.Get("Retry-After") != "1" {
		t.Errorf("Retry-After %q", r.header.Get("Retry-After"))
	}
	expect(t, e.do("GET", e.srv.URL+"/studio/", "", nil, "X-Forwarded-For", "203.0.113.20"), 429) // one budget for everything public
	expect(t, from("203.0.113.21", "blyg.json"), 200)
	expect(t, e.do("GET", e.srv.URL+"/healthz", "", nil, "X-Forwarded-For", "203.0.113.20"), 200) // probes are free

	// Webmention posts have their own, smaller budget per address.
	form := url.Values{"source": {"https://elsewhere.test/x"}, "target": {alice.origin}}
	post := func(addr string) int {
		return e.do("POST", alice.origin+"webmention", "", form, "X-Forwarded-For", addr).status
	}
	for i := 0; i < int(MentionBudget.Burst); i++ {
		if s := post("203.0.113.30"); s == http.StatusTooManyRequests {
			t.Fatalf("mention %d refused inside the budget", i)
		}
	}
	if s := post("203.0.113.30"); s != http.StatusTooManyRequests {
		t.Fatalf("mention over the budget: %d", s)
	}
}

func TestSubscriptionCap(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice := e.user("alice")
	bob := e.user("bob")
	carol := e.user("carol")
	e.reader.MaxSubscriptions = 1
	alice.subscribe(t, bob.origin)
	r := alice.api("POST", "/subscriptions", map[string]any{"url": carol.origin})
	expect(t, r, 409)
	if !strings.Contains(string(r.body), "subscription limit") {
		t.Errorf("409 body: %s", r.body)
	}
}
