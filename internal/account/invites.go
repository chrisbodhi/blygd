package account

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/chrisbodhi/blygd/internal/apperr"
	"github.com/chrisbodhi/blygd/internal/blyg"
	"github.com/chrisbodhi/blygd/internal/tx"
)

// Invites let someone make their own user, origin and owner token without
// the operator at a shell: the operator mints codes (`blygd invite`) and
// hands them out, and each makes one user (POST /api/invites/redeem). Like
// tokens, only a code's SHA-256 is stored, and it is shown once.

const invitePrefix = "blygd-invite-"

// MaxInvites is the most codes one `blygd invite` mints.
const MaxInvites = 100

// ErrBadInvite is every refused code alike, unknown, used or expired, so
// a guess learns nothing about the codes that exist.
var ErrBadInvite = apperr.New(apperr.Invalid, "that invite is unknown, used or expired")

// CreateInvites mints n codes that expire at expiresAt, all in one
// statement, and returns them. They are never readable again.
func (a *Accounts) CreateInvites(ctx context.Context, n int, expiresAt time.Time, note string) ([]string, error) {
	if n < 1 || n > MaxInvites {
		return nil, apperr.Errorf(apperr.BadRequest, "mint 1 to %d invites at a time", MaxInvites)
	}
	if !expiresAt.After(time.Now()) {
		return nil, apperr.New(apperr.BadRequest, "an invite cannot expire in the past")
	}
	if utf8.RuneCountInString(note) > 200 {
		return nil, apperr.New(apperr.BadRequest, "a note is at most 200 characters")
	}
	codes := make([]string, n)
	hashes := make([][]byte, n)
	for i := range codes {
		c, err := newSecret(invitePrefix, 20)
		if err != nil {
			return nil, err
		}
		codes[i], hashes[i] = c, hashSecret(c)
	}
	if _, err := a.Pool.Exec(ctx, `INSERT INTO invites (code_hash, note, expires_at) SELECT h, $2, $3 FROM unnest($1::bytea[]) AS h`,
		hashes, note, expiresAt); err != nil {
		return nil, err
	}
	return codes, nil
}

// Redeem spends an invite on a new user, as CreateUser makes one: the
// invite is marked in the transaction that makes the user, so a handle
// that is taken, or an origin that fails, leaves the invite unused, and
// two redemptions of one code make one user. It returns the cleaned
// handle and the origin's URL with the owner token, so nothing is left to
// look up once the invite is spent.
func (a *Accounts) Redeem(ctx context.Context, code, handle, publicURL, title string) (_, origin, secret string, err error) {
	if handle, err = cleanHandle(handle); err != nil {
		return "", "", "", err
	}
	base, err := blyg.NormalizeOrigin(publicURL)
	if err != nil {
		return "", "", "", fmt.Errorf("public URL: %w", err)
	}
	if title = strings.TrimSpace(title); utf8.RuneCountInString(title) > 200 {
		return "", "", "", apperr.New(apperr.BadRequest, "a title is at most 200 characters")
	}
	// No code at all is a form sent too soon, not a guess: refused before
	// any code is read, it costs no failed authentication.
	if code = strings.ToLower(strings.TrimSpace(code)); code == "" {
		return "", "", "", apperr.New(apperr.BadRequest, "an invite code is required")
	}
	if !strings.HasPrefix(code, invitePrefix) {
		return "", "", "", ErrBadInvite
	}
	origin = UserOrigin(base, handle)
	err = tx.Run(ctx, a.Pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		// FOR UPDATE: a second redemption waits for the first, then finds
		// the code spent.
		var inviteID int64
		err := tx.QueryRow(ctx, `SELECT id FROM invites WHERE code_hash = $1 AND redeemed_at IS NULL AND expires_at > now() FOR UPDATE`,
			hashSecret(code)).Scan(&inviteID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrBadInvite
		}
		if err != nil {
			return err
		}
		var userID int64
		if userID, _, secret, err = a.createUser(ctx, tx, handle, origin, title); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE invites SET redeemed_at = now(), redeemed_by = $2 WHERE id = $1`, inviteID, userID)
		return err
	})
	if err != nil {
		return "", "", "", err
	}
	return handle, origin, secret, nil
}
