// Package studio is the authoritative side of blygd: the own-origin plane.
// It owns the item lifecycle (draft → publish → withdraw, pins) and renders
// the public surface inside the same transaction as each change, so what is
// served is always exactly what was committed.
package studio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chrisbodhi/blygd/internal/apperr"
	"github.com/chrisbodhi/blygd/internal/blyg"
	"github.com/chrisbodhi/blygd/internal/fetch"
	"github.com/chrisbodhi/blygd/internal/tx"
)

// Studio is the publishing engine.
type Studio struct {
	Pool         *pgxpool.Pool
	Generator    string // name/version, §3.2
	GeneratorURL string
	// Level is the manifest's informative conformance claim (§3.2).
	Level int
	Fetch *fetch.Client
	Now   func() time.Time
	// FragmentCap is the studio-enforced fragment length in characters
	// (§5.3: SHOULD cap at 2,000; RECOMMENDED 1,000).
	FragmentCap int
	// MediaMax caps an upload in bytes; 0 means DefaultMediaMax.
	MediaMax int
	// Imports supplies imported snapshots for threads (§10.2 step 2). Nil
	// means only local items resolve.
	Imports Imports
}

func (s *Studio) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC().Truncate(time.Second)
	}
	return time.Now().UTC().Truncate(time.Second)
}

// Error is a studio error with a stable code the API maps to a status.
type Error struct {
	Code string // not_found, invalid, conflict, stale
	Msg  string
	// Problems lists every reason a draft cannot be published (§10.2:
	// unresolvable directives and links are publish errors).
	Problems []blyg.Problem
}

func (e *Error) Error() string { return e.Msg }

// ErrorKind implements apperr.Kinded.
func (e *Error) ErrorKind() apperr.Kind {
	switch e.Code {
	case "not_found":
		return apperr.NotFound
	case "invalid":
		return apperr.Invalid
	case "conflict":
		return apperr.Conflict
	case "stale":
		return apperr.Stale
	}
	return 0
}

func notFound(format string, a ...any) error {
	return &Error{Code: "not_found", Msg: fmt.Sprintf(format, a...)}
}
func invalid(format string, a ...any) error {
	return &Error{Code: "invalid", Msg: fmt.Sprintf(format, a...)}
}
func conflict(format string, a ...any) error {
	return &Error{Code: "conflict", Msg: fmt.Sprintf(format, a...)}
}
func stale(format string, a ...any) error {
	return &Error{Code: "stale", Msg: fmt.Sprintf(format, a...)}
}

// IsCode reports whether err is a studio Error with the given code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// Origin is one own origin.
type Origin struct {
	ID             int64
	OwnerID        int64
	BaseURL        string
	Title          string
	Description    string
	Author         json.RawMessage
	Byline         json.RawMessage
	AcceptMentions bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

const originCols = `id, owner_id, base_url, title, description, author, byline, accept_mentions, created_at, updated_at`

func scanOrigin(row pgx.Row) (*Origin, error) {
	var o Origin
	err := row.Scan(&o.ID, &o.OwnerID, &o.BaseURL, &o.Title, &o.Description, &o.Author, &o.Byline, &o.AcceptMentions, &o.CreatedAt, &o.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound("origin not found")
	}
	return &o, err
}

// GetOrigin loads an origin by id.
func (s *Studio) GetOrigin(ctx context.Context, id int64) (*Origin, error) {
	return scanOrigin(s.Pool.QueryRow(ctx, `SELECT `+originCols+` FROM origins WHERE id = $1`, id))
}

// OriginForOwner returns the user's origin. One origin per user for now;
// the schema allows more.
func (s *Studio) OriginForOwner(ctx context.Context, userID int64) (*Origin, error) {
	return scanOrigin(s.Pool.QueryRow(ctx, `SELECT `+originCols+` FROM origins WHERE owner_id = $1 ORDER BY id LIMIT 1`, userID))
}

// OriginForURL finds the own origin whose base URL is the longest prefix of
// u (an already-normalized absolute URL).
func (s *Studio) OriginForURL(ctx context.Context, u string) (*Origin, error) {
	return scanOrigin(s.Pool.QueryRow(ctx,
		`SELECT `+originCols+` FROM origins WHERE starts_with($1, base_url) ORDER BY length(base_url) DESC LIMIT 1`, u))
}

func lockOrigin(ctx context.Context, tx pgx.Tx, id int64) (*Origin, error) {
	// One publish at a time per origin: the index, feed and manifest are
	// rebuilt from the whole origin, so concurrent publishes must serialize.
	return scanOrigin(tx.QueryRow(ctx, `SELECT `+originCols+` FROM origins WHERE id = $1 FOR UPDATE`, id))
}

// CreateOrigin creates an origin for ownerID and renders its surface, an
// empty index and feed and its manifest, in tx: an origin is never seen
// without its blyg.json (§4). It implements account.OriginMaker.
func (s *Studio) CreateOrigin(ctx context.Context, tx pgx.Tx, ownerID int64, baseURL, title string) (int64, error) {
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO origins (owner_id, base_url, title) VALUES ($1, $2, $3) RETURNING id`,
		ownerID, baseURL, title).Scan(&id); err != nil {
		return 0, err
	}
	o, err := lockOrigin(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	return id, s.renderAll(ctx, tx, o)
}

// OriginSettings are the editable origin fields; nil leaves a field alone.
type OriginSettings struct {
	Title          *string
	Description    *string
	Author         json.RawMessage
	Byline         json.RawMessage
	AcceptMentions *bool
}

// UpdateOrigin changes settings and re-renders the whole surface.
func (s *Studio) UpdateOrigin(ctx context.Context, id int64, in OriginSettings) (*Origin, error) {
	for _, raw := range []json.RawMessage{in.Author, in.Byline} {
		if raw != nil && !isJSONObjectOrNull(raw) {
			return nil, invalid("author and byline must be JSON objects (§5.5)")
		}
	}
	var out *Origin
	err := tx.Run(ctx, s.Pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := lockOrigin(ctx, tx, id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE origins SET
			title = coalesce($2, title),
			description = coalesce($3, description),
			author = CASE WHEN $4::jsonb IS NULL THEN author WHEN $4::jsonb = 'null'::jsonb THEN NULL ELSE $4::jsonb END,
			byline = CASE WHEN $5::jsonb IS NULL THEN byline WHEN $5::jsonb = 'null'::jsonb THEN NULL ELSE $5::jsonb END,
			accept_mentions = coalesce($6, accept_mentions),
			updated_at = now()
			WHERE id = $1`, id, in.Title, in.Description, nullableJSON(in.Author), nullableJSON(in.Byline), in.AcceptMentions)
		if err != nil {
			return err
		}
		o, err := scanOrigin(tx.QueryRow(ctx, `SELECT `+originCols+` FROM origins WHERE id = $1`, id))
		if err != nil {
			return err
		}
		out = o
		return s.renderAll(ctx, tx, o)
	})
	return out, err
}

// Rebuild re-renders every mutable surface file for an origin. Pins are
// write-once and are left as they are.
func (s *Studio) Rebuild(ctx context.Context, originID int64) error {
	return tx.Run(ctx, s.Pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		o, err := lockOrigin(ctx, tx, originID)
		if err != nil {
			return err
		}
		return s.renderAll(ctx, tx, o)
	})
}

func nullableJSON(raw json.RawMessage) any {
	if raw == nil {
		return nil
	}
	return string(raw)
}

func isJSONObjectOrNull(raw json.RawMessage) bool {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	if v == nil {
		return true
	}
	_, ok := v.(map[string]any)
	return ok
}
