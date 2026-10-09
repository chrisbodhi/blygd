package studio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/chrisbodhi/blygd/internal/blyg"
)

// DefaultMediaMax is the upload cap when Studio.MediaMax is unset.
const DefaultMediaMax = 10 << 20

// Media is one uploaded object (§5.4).
type Media struct {
	Path      string    `json:"path"` // origin-relative: what markdown writes
	URL       string    `json:"url"`
	Mime      string    `json:"mime"`
	Size      int       `json:"size"`
	SHA256    string    `json:"sha256"`
	Published bool      `json:"published"` // referenced by a published version: public and permanent
	CreatedAt time.Time `json:"created_at"`
}

func (s *Studio) mediaMax() int {
	if s.MediaMax > 0 {
		return s.MediaMax
	}
	return DefaultMediaMax
}

// MediaMaxBytes is the upload cap.
func (s *Studio) MediaMaxBytes() int { return s.mediaMax() }

const mediaCols = `m.path, m.mime, length(m.bytes), m.sha256, m.created_at,
	EXISTS (SELECT 1 FROM version_media vm WHERE vm.origin_id = m.origin_id AND vm.path = m.path)`

func scanMedia(o *Origin, row pgx.Row) (*Media, error) {
	var m Media
	var sum []byte
	err := row.Scan(&m.Path, &m.Mime, &m.Size, &sum, &m.CreatedAt, &m.Published)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound("media not found")
	}
	m.SHA256, m.URL = hex.EncodeToString(sum), o.BaseURL+m.Path
	return &m, err
}

// Upload stores an image. The path is derived from the bytes, so the same
// bytes are the same object, and a path can never name other bytes (§5.4).
// The declared type must be one blygd accepts and must match what the bytes
// are.
func (s *Studio) Upload(ctx context.Context, o *Origin, mime string, body []byte) (*Media, error) {
	mime = strings.ToLower(strings.TrimSpace(strings.SplitN(mime, ";", 2)[0]))
	ext, ok := blyg.MediaTypes[mime]
	if !ok {
		return nil, invalid("media must be PNG, JPEG, GIF or WebP, declared in Content-Type")
	}
	if len(body) == 0 {
		return nil, invalid("empty upload")
	}
	if len(body) > s.mediaMax() {
		return nil, invalid("media is %d bytes; the cap is %d", len(body), s.mediaMax())
	}
	if sniffed := http.DetectContentType(body); sniffed != mime {
		return nil, invalid("the bytes are %s, not %s", sniffed, mime)
	}
	sum := sha256.Sum256(body)
	path := "media/" + hex.EncodeToString(sum[:16]) + "." + ext
	if _, err := s.Pool.Exec(ctx, `INSERT INTO media (origin_id, path, mime, bytes, sha256)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (origin_id, path) DO NOTHING`, o.ID, path, mime, body, sum[:]); err != nil {
		return nil, err
	}
	return s.GetMedia(ctx, o, path)
}

// GetMedia describes one object.
func (s *Studio) GetMedia(ctx context.Context, o *Origin, path string) (*Media, error) {
	return scanMedia(o, s.Pool.QueryRow(ctx, `SELECT `+mediaCols+` FROM media m WHERE m.origin_id = $1 AND m.path = $2`, o.ID, path))
}

// ListMedia lists an origin's media, newest first.
func (s *Studio) ListMedia(ctx context.Context, o *Origin) ([]*Media, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+mediaCols+` FROM media m WHERE m.origin_id = $1 ORDER BY m.created_at DESC, m.path`, o.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Media{}
	for rows.Next() {
		m, err := scanMedia(o, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteMedia deletes an object no published version references. Once any
// version references it, it is permanent: that version may be pinned at
// any time (§8 rules 2 and 4), and the schema refuses the delete.
func (s *Studio) DeleteMedia(ctx context.Context, o *Origin, path string) error {
	m, err := s.GetMedia(ctx, o, path)
	if err != nil {
		return err
	}
	if m.Published {
		return conflict("%s is referenced by a published version and is kept forever (§8 rule 4)", path)
	}
	_, err = s.Pool.Exec(ctx, `DELETE FROM media WHERE origin_id = $1 AND path = $2`, o.ID, path)
	return err
}

// PublicMedia returns a media object's bytes if a published version
// references it, and nil otherwise: until then it is a draft's (§4).
func (s *Studio) PublicMedia(ctx context.Context, originID int64, path string) (mime string, body, sum []byte, err error) {
	err = s.Pool.QueryRow(ctx, `SELECT m.mime, m.bytes, m.sha256 FROM media m
		WHERE m.origin_id = $1 AND m.path = $2
		AND EXISTS (SELECT 1 FROM version_media vm WHERE vm.origin_id = m.origin_id AND vm.path = m.path)`,
		originID, path).Scan(&mime, &body, &sum)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, nil, nil
	}
	return mime, body, sum, err
}

// recordMedia checks that every own media object a new version references
// exists, then records the references. Returns problems for missing ones.
func recordMedia(ctx context.Context, tx pgx.Tx, o *Origin, refs []blyg.MediaRef) ([]blyg.Problem, error) {
	var problems []blyg.Problem
	for _, r := range refs {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM media WHERE origin_id = $1 AND path = $2)`, o.ID, r.Path).Scan(&ok); err != nil {
			return nil, err
		}
		if !ok {
			problems = append(problems, blyg.Problem{At: r.Path, Reason: "no such media here: upload it first (POST /api/media)"})
		}
	}
	return problems, nil
}

func insertVersionMedia(ctx context.Context, tx pgx.Tx, o *Origin, itemID string, version int, refs []blyg.MediaRef) error {
	for i, r := range refs {
		if _, err := tx.Exec(ctx, `INSERT INTO version_media (origin_id, item_id, version, ord, path, alt)
			VALUES ($1, $2, $3, $4, $5, $6)`, o.ID, itemID, version, i, r.Path, r.Alt); err != nil {
			return err
		}
	}
	return nil
}

// versionMedia is the media array of one version (§5.4).
func versionMedia(ctx context.Context, tx pgx.Tx, itemID string, version int) ([]blyg.Media, error) {
	rows, err := tx.Query(ctx, `SELECT vm.path, m.mime, vm.alt FROM version_media vm
		JOIN media m ON m.origin_id = vm.origin_id AND m.path = vm.path
		WHERE vm.item_id = $1 AND vm.version = $2 ORDER BY vm.ord`, itemID, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []blyg.Media{}
	for rows.Next() {
		var m blyg.Media
		if err := rows.Scan(&m.URL, &m.Mime, &m.Alt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
