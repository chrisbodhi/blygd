package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/chrisbodhi/blygd/internal/account"
	"github.com/chrisbodhi/blygd/internal/mention"
)

// A new user's origin is published by the transaction that creates it:
// its manifest is served at once, with no rebuild (architecture review D4).
func TestNewOriginIsServedAtOnce(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice := e.user("alice")
	expect(t, e.do("GET", alice.origin+"blyg.json", "", nil), 200)
	expect(t, e.do("GET", alice.origin+"items/index.json", "", nil), 200)
}

type failingOrigins struct{ real account.OriginMaker }

func (f failingOrigins) CreateOrigin(ctx context.Context, tx pgx.Tx, ownerID int64, baseURL, title string) (int64, error) {
	if _, err := f.real.CreateOrigin(ctx, tx, ownerID, baseURL, title); err != nil {
		return 0, err
	}
	return 0, errors.New("render failed")
}

// If the origin cannot be made, neither is the user: nothing is left
// half created, an account with no origin or an origin with no surface.
func TestFailedOriginLeavesNoUser(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	e.accts.Origins = failingOrigins{e.studio}
	if _, _, _, err := e.accts.CreateUser(context.Background(), "alice", e.srv.URL+"/", "Alice"); err == nil {
		t.Fatal("CreateUser succeeded with its origin failing")
	}
	var users, origins, files int
	if err := e.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM origins),
		(SELECT count(*) FROM surface_files)`).Scan(&users, &origins, &files); err != nil {
		t.Fatal(err)
	}
	if users+origins+files != 0 {
		t.Fatalf("left %d users, %d origins, %d surface files", users, origins, files)
	}
}

// Users mount under "~", as home directories did on a shared host, so no
// handle can shadow a host-rooted route. An origin's spelling is part of
// its identity (§12.2), so "%7E" is redirected to "~", and a reader that
// subscribes by the encoded spelling ends with the one the origin writes.
// An origin made before the mount keeps its URL, served beside the rest.
func TestUsersMountUnderTilde(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice, bob, carol := e.user("alice"), e.user("bob"), e.user("carol")
	ctx := context.Background()

	if alice.origin != e.srv.URL+"/~alice/" {
		t.Fatalf("origin %s", alice.origin)
	}
	if m := alice.get("blyg.json").json(t); m["site"] != alice.origin {
		t.Fatalf("manifest site %v", m["site"])
	}
	expect(t, e.do("GET", e.srv.URL+"/alice/blyg.json", "", nil), 404)
	expect(t, e.do("GET", e.srv.URL+"/~alice", "", nil), 200) // the trailing-slash redirect still applies

	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, spelling := range []string{"/%7Ealice/blyg.json", "/%7ealice/blyg.json", "/%7Ealice/items/index.json?x=1"} {
		r, err := noFollow.Get(e.srv.URL + spelling)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		want := strings.NewReplacer("%7E", "~", "%7e", "~").Replace(spelling)
		if r.StatusCode != http.StatusMovedPermanently || r.Header.Get("Location") != want {
			t.Errorf("%s: %d to %q, want 301 to %q", spelling, r.StatusCode, r.Header.Get("Location"), want)
		}
	}
	// Never a way off the host: ServeMux cleans "//host" paths before the
	// surface sees them, and every hop of the chain stays here.
	for _, path := range []string{"//evil.example/%7Ealice/blyg.json", "///evil.example/%7Ealice/", "/%2F%2Fevil.example/%7Ealice/"} {
		at := e.srv.URL + path
		for hop := 0; hop < 4; hop++ {
			req, _ := http.NewRequest("GET", at, nil)
			r, err := noFollow.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			r.Body.Close()
			loc := r.Header.Get("Location")
			if loc == "" {
				break
			}
			next, err := r.Request.URL.Parse(loc)
			if err != nil || next.Host != r.Request.URL.Host {
				t.Fatalf("%s: hop %d redirects off the host, to %q", path, hop, loc)
			}
			at = next.String()
		}
	}

	// A mention whose target spells "~" as "%7E" names the same item
	// (§15.3 is membership): accepted, and stored as "~".
	item := alice.draft(t, map[string]any{"content_md": "Mention me."})
	alice.publish(t, item, 200)
	encoded := e.srv.URL + "/%7Ealice/f/" + item + "/"
	claim := url.Values{"source": {"https://elsewhere.test/a-reply"}, "target": {encoded}}
	expect(t, e.do("POST", alice.origin+"webmention", "", claim), 202)
	var stored string
	if err := e.pool.QueryRow(ctx, `SELECT target_url FROM inbound_mentions WHERE target_item_id = $1`, item).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != alice.origin+"f/"+item+"/" {
		t.Fatalf("stored target %s", stored)
	}

	r := bob.api("POST", "/subscriptions", map[string]any{"url": e.srv.URL + "/%7Ealice/"})
	expect(t, r, 201)
	if got := r.json(t)["subscription"].(map[string]any)["origin"]; got != alice.origin {
		t.Fatalf("subscribed by %%7E, identity %v, want %s", got, alice.origin)
	}

	// An origin from before the mount: its URL is its identity, so it keeps
	// it, and the longest base_url still routes every request.
	legacy := e.srv.URL + "/carol/"
	var oid int64
	if err := e.pool.QueryRow(ctx, `UPDATE origins SET base_url = $1 WHERE owner_id = $2 RETURNING id`, legacy, carol.id).Scan(&oid); err != nil {
		t.Fatal(err)
	}
	if err := e.studio.Rebuild(ctx, oid); err != nil {
		t.Fatal(err)
	}
	r = e.do("GET", legacy+"blyg.json", "", nil)
	expect(t, r, 200)
	if m := r.json(t); m["site"] != legacy {
		t.Fatalf("legacy manifest site %v", m["site"])
	}
	expect(t, e.do("GET", alice.origin+"blyg.json", "", nil), 200)
	if me := carol.api("GET", "/me", nil).json(t); me["origin"] != legacy {
		t.Fatalf("legacy /me origin %v", me["origin"])
	}
}
