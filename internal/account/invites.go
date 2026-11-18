package account

import (
	"context"
	"crypto/rand"
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
// transaction, and returns them. They are never readable again.
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
	for i := range codes {
		var raw [20]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return nil, err
		}
		codes[i] = invitePrefix + strings.ToLower(tokenEncoding.EncodeToString(raw[:]))
	}
	err := tx.Run(ctx, a.Pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		for _, c := range codes {
			if _, err := tx.Exec(ctx, `INSERT INTO invites (code_hash, note, expires_at) VALUES ($1, $2, $3)`,
				hashSecret(c), note, expiresAt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return codes, nil
}

// Redeem spends an invite on a new user, as CreateUser makes one: the
// invite is marked in the transaction that makes the user, so a handle
// that is taken, or an origin that fails, leaves the invite unused, and
// two redemptions of one code make one user.
func (a *Accounts) Redeem(ctx context.Context, code, handle, publicURL, title string) (userID, originID int64, secret string, err error) {
	if handle, err = cleanHandle(handle); err != nil {
		return 0, 0, "", err
	}
	base, err := blyg.NormalizeOrigin(publicURL)
	if err != nil {
		return 0, 0, "", fmt.Errorf("public URL: %w", err)
	}
	if title = strings.TrimSpace(title); utf8.RuneCountInString(title) > 200 {
		return 0, 0, "", apperr.New(apperr.BadRequest, "a title is at most 200 characters")
	}
	code = strings.ToLower(strings.TrimSpace(code))
	if !strings.HasPrefix(code, invitePrefix) {
		return 0, 0, "", ErrBadInvite
	}
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
		if userID, originID, secret, err = a.createUser(ctx, tx, handle, base+UserMount+handle+"/", title); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE invites SET redeemed_at = now(), redeemed_by = $2 WHERE id = $1`, inviteID, userID)
		return err
	})
	if err != nil {
		return 0, 0, "", err
	}
	return userID, originID, secret, nil
}
