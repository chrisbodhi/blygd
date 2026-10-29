package web

import (
	"context"
	"errors"
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
