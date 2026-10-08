// Package hopper is the private overlay's curation construct: named,
// many-to-many, tag-like collections of items (README glossary). Hoppers are
// studio-side and never protocol. Members are references, not copies — a
// copy would outlive a withdrawal, which §13.4 forbids — and may be the
// user's own items (drafts included) or imported items.
package hopper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the hopper store.
type Store struct {
	Pool *pgxpool.Pool
}

// ErrNotFound covers hoppers, members and items the user cannot see.
var ErrNotFound = errors.New("not found")

// ErrExists is a duplicate member.
var ErrExists = errors.New("already a member")

// Hopper is one hopper.
type Hopper struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Slug        string    `json:"slug"`
	Public      bool      `json:"public"`
	SlugFrozen  bool      `json:"slug_frozen"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	MemberCount int       `json:"member_count"`
}

// Member is one reference in a hopper.
type Member struct {
	ID       int64      `json:"id"`
	AddedAt  time.Time  `json:"added_at"`
	Own      *OwnRef    `json:"own,omitempty"`
	Imported *ImportRef `json:"imported,omitempty"`
}

// OwnRef points at one of the user's own items.
type OwnRef struct {
	ItemID  string  `json:"item_id"`
	State   string  `json:"state"` // draft, published, withdrawn
	Version int     `json:"version"`
	Page    *string `json:"page"`
}

// ImportRef points at an imported item. Its state follows the mirror, so a
// withdrawn item shows as withdrawn here with no content (§13.4). Doc is
// the origin's verbatim document: clients sanitize it (§14).
type ImportRef struct {
	SubscriptionID int64           `json:"subscription_id"`
	Origin         string          `json:"origin"`
	ItemID         string          `json:"item_id"`
	Kind           string          `json:"kind"`
	State          string          `json:"state"`
	Version        int             `json:"version"`
	Page           *string         `json:"page"`
	Doc            json.RawMessage `json:"doc"`
}

const hopperCols = `h.id, h.name, h.slug, h.public, h.slug_frozen, h.created_at, h.updated_at,
	(SELECT count(*) FROM hopper_members m WHERE m.hopper_id = h.id)`

func scanHopper(row pgx.Row) (*Hopper, error) {
	var h Hopper
	err := row.Scan(&h.ID, &h.Name, &h.Slug, &h.Public, &h.SlugFrozen, &h.CreatedAt, &h.UpdatedAt, &h.MemberCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &h, err
}

// List returns the user's hoppers, most recently changed first.
func (s *Store) List(ctx context.Context, userID int64) ([]*Hopper, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+hopperCols+` FROM hoppers h WHERE h.user_id = $1 ORDER BY h.updated_at DESC, h.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Hopper
	for rows.Next() {
		h, err := scanHopper(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Create makes a hopper with a slug derived from its name.
func (s *Store) Create(ctx context.Context, userID int64, name string) (*Hopper, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 200 {
		return nil, fmt.Errorf("name must be 1–200 characters")
	}
	var h *Hopper
	err := pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		slug, err := freeSlug(ctx, tx, userID, Slugify(name), 0)
		if err != nil {
			return err
		}
		h, err = scanHopper(tx.QueryRow(ctx, `WITH h AS (INSERT INTO hoppers (user_id, name, slug) VALUES ($1, $2, $3) RETURNING *)
			SELECT h.id, h.name, h.slug, h.public, h.slug_frozen, h.created_at, h.updated_at, 0 FROM h`, userID, name, slug))
		return err
	})
	return h, err
}

// Get returns a hopper and its members, newest first.
func (s *Store) Get(ctx context.Context, userID, id int64) (*Hopper, []Member, error) {
	h, err := scanHopper(s.Pool.QueryRow(ctx, `SELECT `+hopperCols+` FROM hoppers h WHERE h.user_id = $1 AND h.id = $2`, userID, id))
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT m.id, m.added_at,
			i.id, i.version, i.withdrawn, i.page,
			r.id, r.identity_url, ii.item_id, ii.kind, ii.state, ii.version, ii.page_url, ii.doc
		FROM hopper_members m
		LEFT JOIN items i ON i.id = m.own_item_id
		LEFT JOIN imported_items ii ON ii.id = m.imported_item_id
		LEFT JOIN remote_origins r ON r.id = ii.remote_origin_id
		WHERE m.hopper_id = $1 ORDER BY m.added_at DESC, m.id DESC`, id)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	members := []Member{}
	for rows.Next() {
		var m Member
		var ownID, impOrigin, impItem, impKind, impState, impPage *string
		var ownVersion, impVersion *int
		var impRemote *int64
		var ownWithdrawn *bool
		var ownPage *string
		var impDoc []byte
		if err := rows.Scan(&m.ID, &m.AddedAt, &ownID, &ownVersion, &ownWithdrawn, &ownPage,
			&impRemote, &impOrigin, &impItem, &impKind, &impState, &impVersion, &impPage, &impDoc); err != nil {
			return nil, nil, err
		}
		if ownID != nil {
			state := "published"
			switch {
			case *ownVersion == 0:
				state = "draft"
			case *ownWithdrawn:
				state = "withdrawn"
			}
			m.Own = &OwnRef{ItemID: *ownID, State: state, Version: *ownVersion, Page: ownPage}
		} else if impItem != nil {
			m.Imported = &ImportRef{SubscriptionID: *impRemote, Origin: *impOrigin, ItemID: *impItem, Kind: *impKind,
				State: *impState, Version: *impVersion, Page: impPage}
			if impDoc != nil {
				m.Imported.Doc = impDoc
			}
		}
		members = append(members, m)
	}
	return h, members, rows.Err()
}

// Rename changes a hopper's name. Its slug follows the name until the hopper
// has ever been public; after that the slug is someone's link and stays.
func (s *Store) Rename(ctx context.Context, userID, id int64, name string) (*Hopper, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 200 {
		return nil, fmt.Errorf("name must be 1–200 characters")
	}
	var h *Hopper
	err := pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		cur, err := scanHopper(tx.QueryRow(ctx, `SELECT `+hopperCols+` FROM hoppers h WHERE h.user_id = $1 AND h.id = $2 FOR UPDATE`, userID, id))
		if err != nil {
			return err
		}
		slug := cur.Slug
		if !cur.SlugFrozen && Slugify(name) != cur.Slug {
			if slug, err = freeSlug(ctx, tx, userID, Slugify(name), id); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE hoppers SET name = $3, slug = $4, updated_at = now() WHERE user_id = $1 AND id = $2`, userID, id, name, slug); err != nil {
			return err
		}
		h, err = scanHopper(tx.QueryRow(ctx, `SELECT `+hopperCols+` FROM hoppers h WHERE h.id = $1`, id))
		return err
	})
	return h, err
}

// Delete deletes a hopper. Members are references, so nothing they point at
// is touched.
func (s *Store) Delete(ctx context.Context, userID, id int64) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM hoppers WHERE user_id = $1 AND id = $2`, userID, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// AddOwn adds one of the user's own items.
func (s *Store) AddOwn(ctx context.Context, userID, hopperID int64, itemID string) (int64, error) {
	var mid int64
	err := s.Pool.QueryRow(ctx, `INSERT INTO hopper_members (hopper_id, own_item_id)
		SELECT h.id, i.id FROM hoppers h, items i JOIN origins o ON o.id = i.origin_id
		WHERE h.user_id = $1 AND h.id = $2 AND i.id = $3 AND o.owner_id = $1
		RETURNING id`, userID, hopperID, itemID).Scan(&mid)
	return mid, s.addErr(ctx, err, hopperID)
}

// AddImported adds an imported item from an origin the user subscribes to.
// Membership never reaches past the user's own subscriptions.
func (s *Store) AddImported(ctx context.Context, userID, hopperID int64, origin, itemID string) (int64, error) {
	var mid int64
	err := s.Pool.QueryRow(ctx, `INSERT INTO hopper_members (hopper_id, imported_item_id)
		SELECT h.id, ii.id FROM hoppers h, imported_items ii
		JOIN remote_origins r ON r.id = ii.remote_origin_id
		JOIN subscriptions sub ON sub.remote_origin_id = r.id AND sub.user_id = $1
		WHERE h.user_id = $1 AND h.id = $2 AND r.identity_url = $3 AND ii.item_id = $4
		RETURNING id`, userID, hopperID, origin, itemID).Scan(&mid)
	return mid, s.addErr(ctx, err, hopperID)
}

// AddImportedBySubscription adds an imported item by the subscription it
// came from. This is the only way to name an L0 item, whose identity is a
// feed URL rather than an origin.
func (s *Store) AddImportedBySubscription(ctx context.Context, userID, hopperID, remoteID int64, itemID string) (int64, error) {
	var mid int64
	err := s.Pool.QueryRow(ctx, `INSERT INTO hopper_members (hopper_id, imported_item_id)
		SELECT h.id, ii.id FROM hoppers h, imported_items ii
		JOIN subscriptions sub ON sub.remote_origin_id = ii.remote_origin_id AND sub.user_id = $1
		WHERE h.user_id = $1 AND h.id = $2 AND ii.remote_origin_id = $3 AND ii.item_id = $4
		RETURNING id`, userID, hopperID, remoteID, itemID).Scan(&mid)
	return mid, s.addErr(ctx, err, hopperID)
}

func (s *Store) addErr(ctx context.Context, err error, hopperID int64) error {
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		_, _ = s.Pool.Exec(ctx, `UPDATE hoppers SET updated_at = now() WHERE id = $1`, hopperID)
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return ErrExists
	}
	return err
}

// RemoveMember removes one member.
func (s *Store) RemoveMember(ctx context.Context, userID, hopperID, memberID int64) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM hopper_members m USING hoppers h
		WHERE m.id = $3 AND m.hopper_id = h.id AND h.id = $2 AND h.user_id = $1`, userID, hopperID, memberID)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// Slugify makes a URL slug from a name.
func Slugify(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.TrimRight(b.String(), "-")
	if len(s) > 60 {
		s = strings.TrimRight(s[:60], "-")
	}
	if s == "" {
		return "hopper"
	}
	return s
}

func freeSlug(ctx context.Context, tx pgx.Tx, userID int64, base string, except int64) (string, error) {
	for n := 1; n < 1000; n++ {
		slug := base
		if n > 1 {
			slug = fmt.Sprintf("%s-%d", base, n)
		}
		var taken bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM hoppers WHERE user_id = $1 AND slug = $2 AND id <> $3)`,
			userID, slug, except).Scan(&taken); err != nil {
			return "", err
		}
		if !taken {
			return slug, nil
		}
	}
	return "", errors.New("could not find a free slug")
}
