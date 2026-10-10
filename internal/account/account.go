// Package account holds users and their per-tool, scoped, revocable tokens
// (§16.6). The write surface is never normative, so these scopes are this
// studio's own vocabulary.
package account

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chrisbodhi/blygd/internal/blyg"
)

// Scopes. Consequential public acts are separate scopes, so a drafting tool
// or an agent can be given write access without the power to publish, pin
// (irrevocable) or withdraw.
const (
	ScopeAll           = "*"
	ScopeItemsRead     = "items:read"
	ScopeItemsWrite    = "items:write"
	ScopeItemsPublish  = "items:publish"
	ScopeItemsWithdraw = "items:withdraw"
	ScopeItemsPin      = "items:pin"
	ScopeHoppersRead   = "hoppers:read"
	ScopeHoppersWrite  = "hoppers:write"
	ScopeMentionsRead  = "mentions:read"
	// Moderating mentions: hiding and blocking. Approving one for display
	// also needs origin:write, because it publishes (§15.5).
	ScopeMentionsWrite = "mentions:write"
	ScopeOriginWrite   = "origin:write"
	ScopeTokensWrite   = "tokens:write"
	// The reader. Subscribing is private (§11: there is no protocol
	// follow); adding a subscription to the public blogroll also needs
	// origin:write, because publishing a reading choice is a public act.
	ScopeReaderRead  = "reader:read"
	ScopeReaderWrite = "reader:write"
)

// AllScopes lists every grantable scope except "*".
var AllScopes = []string{
	ScopeItemsRead, ScopeItemsWrite, ScopeItemsPublish, ScopeItemsWithdraw, ScopeItemsPin,
	ScopeHoppersRead, ScopeHoppersWrite, ScopeMentionsRead, ScopeMentionsWrite, ScopeOriginWrite, ScopeTokensWrite,
	ScopeReaderRead, ScopeReaderWrite,
}

// ReservedHandles cannot be users: they are, or may become, host-rooted
// routes beside path-mounted origins.
var ReservedHandles = []string{"api", "admin", "healthz", "static", "studio", "www", "webmention", "media", "items", "feed", "h"}

// Principal is an authenticated token.
type Principal struct {
	UserID  int64
	Handle  string
	TokenID int64
	Scopes  []string
}

// Can reports whether the principal holds scope.
func (p *Principal) Can(scope string) bool {
	return slices.Contains(p.Scopes, ScopeAll) || slices.Contains(p.Scopes, scope)
}

// Accounts manages users and tokens.
type Accounts struct {
	Pool *pgxpool.Pool
}

var (
	ErrUnauthorized = errors.New("missing, unknown or revoked token")
	ErrBadHandle    = errors.New("handle must be 1–63 lowercase letters, digits or hyphens, and not reserved")
)

const tokenPrefix = "blygd_"

var tokenEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

func hashSecret(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// CreateUser creates a user and their path-mounted origin at
// publicURL + handle + "/", and mints an all-scopes owner token. The
// secret is returned once and never stored.
func (a *Accounts) CreateUser(ctx context.Context, handle, publicURL, title string) (userID, originID int64, secret string, err error) {
	handle = strings.ToLower(strings.TrimSpace(handle))
	if slices.Contains(ReservedHandles, handle) || !validHandle(handle) {
		return 0, 0, "", ErrBadHandle
	}
	base, err := blyg.NormalizeOrigin(publicURL)
	if err != nil {
		return 0, 0, "", fmt.Errorf("public URL: %w", err)
	}
	origin := base + handle + "/"
	err = pgx.BeginTxFunc(ctx, a.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO users (handle) VALUES ($1) RETURNING id`, handle).Scan(&userID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO origins (owner_id, base_url, title) VALUES ($1, $2, $3) RETURNING id`,
			userID, origin, title).Scan(&originID); err != nil {
			return err
		}
		secret, _, err = mint(ctx, tx, userID, "owner", []string{ScopeAll})
		return err
	})
	return userID, originID, secret, err
}

func validHandle(h string) bool {
	if len(h) == 0 || len(h) > 63 || h[0] == '-' {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// UserByHandle returns a user id.
func (a *Accounts) UserByHandle(ctx context.Context, handle string) (int64, error) {
	var id int64
	err := a.Pool.QueryRow(ctx, `SELECT id FROM users WHERE handle = $1`, handle).Scan(&id)
	return id, err
}

// Mint creates a token with the given scopes for userID.
func (a *Accounts) Mint(ctx context.Context, userID int64, name string, scopes []string) (secret string, id int64, err error) {
	err = pgx.BeginTxFunc(ctx, a.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		secret, id, err = mint(ctx, tx, userID, name, scopes)
		return err
	})
	return secret, id, err
}

func mint(ctx context.Context, tx pgx.Tx, userID int64, name string, scopes []string) (string, int64, error) {
	if strings.TrimSpace(name) == "" {
		return "", 0, errors.New("token name is required")
	}
	if len(scopes) == 0 {
		return "", 0, errors.New("a token needs at least one scope")
	}
	for _, s := range scopes {
		if s != ScopeAll && !slices.Contains(AllScopes, s) {
			return "", 0, fmt.Errorf("unknown scope %q", s)
		}
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", 0, err
	}
	secret := tokenPrefix + strings.ToLower(tokenEncoding.EncodeToString(raw[:]))
	var id int64
	err := tx.QueryRow(ctx, `INSERT INTO tokens (user_id, name, secret_hash, scopes) VALUES ($1, $2, $3, $4) RETURNING id`,
		userID, name, hashSecret(secret), scopes).Scan(&id)
	return secret, id, err
}

// Authenticate resolves an Authorization header value.
func (a *Accounts) Authenticate(ctx context.Context, header string) (*Principal, error) {
	secret, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || !strings.HasPrefix(secret, tokenPrefix) {
		return nil, ErrUnauthorized
	}
	var p Principal
	err := a.Pool.QueryRow(ctx, `UPDATE tokens t SET last_used_at = now()
		FROM users u WHERE u.id = t.user_id AND t.secret_hash = $1 AND t.revoked_at IS NULL
		RETURNING t.user_id, u.handle, t.id, t.scopes`, hashSecret(strings.TrimSpace(secret))).Scan(&p.UserID, &p.Handle, &p.TokenID, &p.Scopes)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// Token is a token's public description; the secret is never readable.
type Token struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

// ListTokens lists a user's tokens.
func (a *Accounts) ListTokens(ctx context.Context, userID int64) ([]Token, error) {
	rows, err := a.Pool.Query(ctx, `SELECT id, name, scopes, created_at, last_used_at, revoked_at
		FROM tokens WHERE user_id = $1 ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Token])
}

// Revoke revokes a token. Revocation is immediate: the next request with it
// is unauthorized.
func (a *Accounts) Revoke(ctx context.Context, userID, tokenID int64) (bool, error) {
	tag, err := a.Pool.Exec(ctx, `UPDATE tokens SET revoked_at = now() WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, tokenID, userID)
	return tag.RowsAffected() == 1, err
}
