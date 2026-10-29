package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chrisbodhi/blygd/internal/account"
	"github.com/chrisbodhi/blygd/internal/apperr"
	"github.com/chrisbodhi/blygd/internal/blyg"
	"github.com/chrisbodhi/blygd/internal/hopper"
	"github.com/chrisbodhi/blygd/internal/mention"
	"github.com/chrisbodhi/blygd/internal/reader"
	"github.com/chrisbodhi/blygd/internal/studio"
	"github.com/jackc/pgx/v5/pgconn"
)

// handleErr is the one place an error becomes a status. What the domain
// names keeps its status; everything else is the server's fault.
func TestHandleErr(t *testing.T) {
	a := &API{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, c := range []struct {
		err    error
		status int
		body   string
	}{
		{apperr.New(apperr.BadRequest, "name must be 1–200 characters"), 400, `"error": "invalid"`},
		{fmt.Errorf("deep: %w", hopper.ErrNotFound), 404, `"error": "not_found"`},
		{reader.ErrNotFound, 404, `"error": "not_found"`},
		{reader.ErrTooManySubscriptions, 409, "subscription limit reached"},
		{&studio.Error{Code: "invalid", Msg: "cannot publish", Problems: []blyg.Problem{{Reason: "unresolved"}}}, 422, `"problems"`},
		{&studio.Error{Code: "not_found", Msg: "no such item"}, 404, "no such item"},
		{&reader.ResolveError{Reason: "no manifest"}, 422, `"trail": []`},
		{&reader.TooSoon{Until: time.Now().Add(time.Minute), Reason: "polled just now"}, 429, `"too_soon"`},
		{&pgconn.PgError{Code: "23505"}, 409, "already exists"},
		{&pgconn.PgError{Code: "P0001", Message: "versions are immutable"}, 409, "versions are immutable"},
		{context.Canceled, 500, `"internal"`},
		{fmt.Errorf("acquire: %w", context.DeadlineExceeded), 500, `"internal"`},
		{errors.New("connection reset"), 500, `"internal"`},
		{&studio.Error{Code: "unheard_of", Msg: "?"}, 500, `"internal"`},
	} {
		w := httptest.NewRecorder()
		a.handleErr(w, httptest.NewRequest("GET", "/api/x", nil), c.err)
		if w.Code != c.status || !strings.Contains(w.Body.String(), c.body) {
			t.Errorf("%v: %d %s, want %d with %s", c.err, w.Code, w.Body, c.status, c.body)
		}
	}
}

// A domain call that fails for the server's reasons is a 500, not a 400:
// handlers used to call anything that was not a database error the
// caller's fault (architecture review P1).
func TestServerFailuresAreNotTheCallersFault(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice := e.user("alice")
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), principalKey{},
		&account.Principal{UserID: alice.id, Scopes: []string{account.ScopeAll}}))
	cancel()
	for _, c := range []struct {
		name, body string
		call       func(http.ResponseWriter, *http.Request) error
	}{
		{"create hopper", `{"name": "fine"}`, e.api.createHopper},
		{"mint token", `{"name": "fine", "scopes": ["reader:read"]}`, e.api.mintToken},
	} {
		name, call := c.name, c.call
		r := httptest.NewRequest("POST", "/api/x", strings.NewReader(c.body)).WithContext(ctx)
		w := httptest.NewRecorder()
		if err := call(w, r); err != nil {
			e.api.handleErr(w, r, err)
		}
		if w.Code != 500 {
			t.Errorf("%s with the request gone: %d %s, want 500", name, w.Code, w.Body)
		}
	}
	// What the caller got wrong is still theirs.
	expect(t, alice.api("POST", "/hoppers", map[string]any{"name": ""}), 400)
	expect(t, alice.api("POST", "/tokens", map[string]any{"name": "t", "scopes": []string{"no:such"}}), 400)
}
