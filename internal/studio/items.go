package studio

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/chrisbodhi/blygd/internal/blyg"
)

// Item is an own item: its studio-private draft state plus where it stands
// on the wire.
type Item struct {
	ID               string          `json:"id"`
	OriginID         int64           `json:"-"`
	Kind             string          `json:"kind"` // authored kind: fragment or thread
	DraftMD          string          `json:"draft_md"`
	DraftNote        *string         `json:"draft_note"`
	Version          int             `json:"version"`
	Withdrawn        bool            `json:"withdrawn"`
	Page             *string         `json:"page"`
	ForkedFrom       json.RawMessage `json:"forked_from,omitempty"`
	FirstPublishedAt *time.Time      `json:"first_published_at"`
	PublishedAt      *time.Time      `json:"published_at"`
	CreatedAt        time.Time       `json:"created_at"`
	DraftUpdatedAt   time.Time       `json:"draft_updated_at"`
}

// State is draft, published or withdrawn.
func (it *Item) State() string {
	switch {
	case it.Version == 0:
		return "draft"
	case it.Withdrawn:
		return "withdrawn"
	}
	return "published"
}

const itemCols = `id, origin_id, authored_kind, draft_md, draft_note, version, withdrawn, page, forked_from, first_published_at, published_at, created_at, draft_updated_at`

func scanItem(row pgx.Row) (*Item, error) {
	var it Item
	err := row.Scan(&it.ID, &it.OriginID, &it.Kind, &it.DraftMD, &it.DraftNote, &it.Version, &it.Withdrawn, &it.Page, &it.ForkedFrom, &it.FirstPublishedAt, &it.PublishedAt, &it.CreatedAt, &it.DraftUpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound("item not found")
	}
	return &it, err
}

// Version is one published version.
type Version struct {
	ItemID        string          `json:"-"`
	Version       int             `json:"version"`
	Kind          string          `json:"kind"`
	At            time.Time       `json:"at"`
	Note          *string         `json:"note"`
	NoteGenerated bool            `json:"note_generated"`
	ContentMD     string          `json:"content_md"`
	ContentHTML   string          `json:"content_html"`
	ContentHash   string          `json:"content_hash"`
	Author        json.RawMessage `json:"author,omitempty"`
	Transclusions json.RawMessage `json:"transclusions,omitempty"`
	StubOf        json.RawMessage `json:"stub_of,omitempty"`
	Generated     json.RawMessage `json:"generated,omitempty"`
	PinnedAt      *time.Time      `json:"pinned_at"`
}

const versionCols = `item_id, version, kind, at, note, note_generated, content_md, content_html, content_hash, author, transclusions, stub_of, generated, pinned_at`

func scanVersion(row pgx.Row) (*Version, error) {
	var v Version
	err := row.Scan(&v.ItemID, &v.Version, &v.Kind, &v.At, &v.Note, &v.NoteGenerated, &v.ContentMD, &v.ContentHTML, &v.ContentHash, &v.Author, &v.Transclusions, &v.StubOf, &v.Generated, &v.PinnedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound("version not found")
	}
	return &v, err
}

func loadVersions(ctx context.Context, q pgx.Tx, itemID string) ([]*Version, error) {
	rows, err := q.Query(ctx, `SELECT `+versionCols+` FROM item_versions WHERE item_id = $1 ORDER BY version`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Version
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetItem loads an item and its published versions.
func (s *Studio) GetItem(ctx context.Context, originID int64, id string) (*Item, []*Version, error) {
	var it *Item
	var vs []*Version
	err := pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		it, err = scanItem(tx.QueryRow(ctx, `SELECT `+itemCols+` FROM items WHERE origin_id = $1 AND id = $2`, originID, id))
		if err != nil {
			return err
		}
		vs, err = loadVersions(ctx, tx, id)
		return err
	})
	return it, vs, err
}

// ListItems lists an origin's items, newest activity first. state filters
// to draft, published or withdrawn; empty means all.
func (s *Studio) ListItems(ctx context.Context, originID int64, state string, limit int) ([]*Item, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	cond := ""
	switch state {
	case "":
	case "draft":
		cond = " AND version = 0"
	case "published":
		cond = " AND version > 0 AND NOT withdrawn"
	case "withdrawn":
		cond = " AND withdrawn"
	default:
		return nil, invalid("unknown state %q", state)
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+itemCols+` FROM items WHERE origin_id = $1`+cond+
		` ORDER BY greatest(published_at, draft_updated_at) DESC, id LIMIT $2`, originID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// NewDraft is the input to CreateDraft.
type NewDraft struct {
	Kind       string // fragment or thread
	ContentMD  string
	ForkedFrom *blyg.Reference // §5.6: fixed now, immutable after
}

// CreateDraft creates a never-published item. Drafts are invisible to the
// protocol (§5.2) and 404 on the surface (§4).
func (s *Studio) CreateDraft(ctx context.Context, originID int64, in NewDraft) (*Item, error) {
	if in.Kind == "" {
		in.Kind = "fragment"
	}
	if in.Kind != "fragment" && in.Kind != "thread" {
		return nil, invalid("kind must be fragment or thread")
	}
	var forked any
	if in.ForkedFrom != nil {
		ref := *in.ForkedFrom
		norm, err := blyg.NormalizeOrigin(ref.Origin)
		if err != nil || ref.Origin == "" {
			return nil, invalid("forked_from.origin is required and must be an absolute http(s) origin (§5.6 rule 1)")
		}
		ref.Origin = norm
		if !blyg.ValidID(ref.ID) || ref.Version < 1 {
			return nil, invalid("forked_from needs a valid id and a version ≥ 1")
		}
		if ref.Selector != nil {
			return nil, invalid("forked_from takes no selector")
		}
		b, _ := json.Marshal(ref)
		forked = string(b)
	}
	return scanItem(s.Pool.QueryRow(ctx, `INSERT INTO items (id, origin_id, authored_kind, draft_md, forked_from)
		VALUES ($1, $2, $3, $4, $5::jsonb) RETURNING `+itemCols, blyg.NewID(), originID, in.Kind, in.ContentMD, forked))
}

// UpdateDraft edits the draft content and/or the note for the next publish.
// Editing a published item's draft changes nothing on the wire until the
// next publish.
func (s *Studio) UpdateDraft(ctx context.Context, originID int64, id string, contentMD, note *string) (*Item, error) {
	return scanItem(s.Pool.QueryRow(ctx, `UPDATE items SET
		draft_md = coalesce($3, draft_md),
		draft_note = CASE WHEN $4::text IS NULL THEN draft_note WHEN $4 = '' THEN NULL ELSE $4 END,
		draft_updated_at = now()
		WHERE origin_id = $1 AND id = $2 RETURNING `+itemCols, originID, id, contentMD, note))
}

// DeleteDraft hard-deletes a never-published item (§9: discarding a draft is
// a hard delete; nothing was ever public). Published items can only be
// withdrawn.
func (s *Studio) DeleteDraft(ctx context.Context, originID int64, id string) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM items WHERE origin_id = $1 AND id = $2 AND version = 0`, originID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var v int
		if err := s.Pool.QueryRow(ctx, `SELECT version FROM items WHERE origin_id = $1 AND id = $2`, originID, id).Scan(&v); err != nil {
			return notFound("item not found")
		}
		return conflict("item has been published; withdraw it instead")
	}
	return nil
}

// PublishResult reports a publish and anything the author should hear.
type PublishResult struct {
	Item     *Item    `json:"item"`
	Version  *Version `json:"version"`
	Warnings []string `json:"warnings,omitempty"`
}

// internalLink matches [[id]] and ![[id]] (§10.1) anywhere. Resolving them is
// Phase 3; until then publishing content that contains one is refused,
// because §10.1 makes an unresolvable link a publish error.
var internalLink = regexp.MustCompile(`\[\[[0-9a-hjkmnp-tv-z]{26}(@v\d+)?\]\]`)

// Publish publishes the draft as the next version.
func (s *Studio) Publish(ctx context.Context, originID int64, id string, note *string) (*PublishResult, error) {
	// Network checks happen before the transaction, never inside it.
	pre, _, err := s.GetItem(ctx, originID, id)
	if err != nil {
		return nil, err
	}
	if pre.Kind == "thread" {
		return nil, invalid("threads are not supported yet (docs/PLAN.md, Phase 3)")
	}
	var warnings []string
	var forkKind string
	if pre.Version == 0 && pre.ForkedFrom != nil {
		w, kind, err := s.checkForkPin(ctx, originID, pre.ForkedFrom)
		if err != nil {
			return nil, err
		}
		warnings = append(warnings, w...)
		forkKind = kind
	}

	res := &PublishResult{Warnings: warnings}
	err = pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		o, err := lockOrigin(ctx, tx, originID)
		if err != nil {
			return err
		}
		it, err := scanItem(tx.QueryRow(ctx, `SELECT `+itemCols+` FROM items WHERE origin_id = $1 AND id = $2 FOR UPDATE`, originID, id))
		if err != nil {
			return err
		}
		md := it.DraftMD
		if strings.TrimSpace(md) == "" {
			return invalid("nothing to publish: the draft is empty")
		}
		if n := utf8.RuneCountInString(md); s.FragmentCap > 0 && n > s.FragmentCap {
			return invalid("fragment is %d characters; the cap is %d (§5.3)", n, s.FragmentCap)
		}
		if internalLink.MatchString(md) {
			return invalid("[[id]] links and ![[id]] directives resolve at publish, which arrives in Phase 3")
		}
		var prev *Version
		if it.Version > 0 {
			if prev, err = scanVersion(tx.QueryRow(ctx, `SELECT `+versionCols+` FROM item_versions WHERE item_id = $1 AND version = $2`, id, it.Version)); err != nil {
				return err
			}
			if !it.Withdrawn && prev.ContentMD == md {
				// A publish event with nothing in it is churn every reader
				// pays for (a new feed entry, a new guid).
				return conflict("no change since v%d", it.Version)
			}
		}
		if note == nil {
			note = it.DraftNote
		}
		if note != nil && strings.TrimSpace(*note) == "" {
			note = nil
		}

		page := blyg.DefaultPage(it.Kind, it.ID)
		if it.Page != nil {
			page = *it.Page
		}
		html, err := blyg.RenderMarkdown(md, o.BaseURL+page)
		if err != nil {
			return err
		}
		at := s.now()
		if prev != nil && at.Before(prev.At) {
			at = prev.At // keep the changelog ordered if the clock stepped back
		}
		next := it.Version + 1
		v, err := scanVersion(tx.QueryRow(ctx, `INSERT INTO item_versions
			(item_id, version, kind, at, note, content_md, content_html, content_hash, author)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb) RETURNING `+versionCols,
			id, next, it.Kind, at, note, md, html, blyg.ContentHash(md), nullableJSON(o.Byline)))
		if err != nil {
			return err
		}
		it, err = scanItem(tx.QueryRow(ctx, `UPDATE items SET version = $2, withdrawn = false, page = $3,
			first_published_at = coalesce(first_published_at, $4), published_at = $4, draft_note = NULL
			WHERE id = $1 RETURNING `+itemCols, id, next, page, at))
		if err != nil {
			return err
		}
		if err := enqueueOutbound(ctx, tx, o, it, v, forkKind); err != nil {
			return err
		}
		if err := s.renderItem(ctx, tx, o, id); err != nil {
			return err
		}
		if err := s.renderAggregates(ctx, tx, o); err != nil {
			return err
		}
		res.Item, res.Version = it, v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// Withdraw publishes a withdrawal endcap (§9). It is reversible: publishing
// again returns the item under the same id.
func (s *Studio) Withdraw(ctx context.Context, originID int64, id string, note *string) (*PublishResult, error) {
	res := &PublishResult{}
	err := pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		o, err := lockOrigin(ctx, tx, originID)
		if err != nil {
			return err
		}
		it, err := scanItem(tx.QueryRow(ctx, `SELECT `+itemCols+` FROM items WHERE origin_id = $1 AND id = $2 FOR UPDATE`, originID, id))
		if err != nil {
			return err
		}
		if it.Version == 0 {
			return conflict("a draft cannot be withdrawn; delete it instead")
		}
		if it.Withdrawn {
			return conflict("already withdrawn")
		}
		prev, err := scanVersion(tx.QueryRow(ctx, `SELECT `+versionCols+` FROM item_versions WHERE item_id = $1 AND version = $2`, id, it.Version))
		if err != nil {
			return err
		}
		if note != nil && strings.TrimSpace(*note) == "" {
			note = nil
		}
		at := s.now()
		if at.Before(prev.At) {
			at = prev.At
		}
		var transclusions any
		if it.Kind == "thread" {
			transclusions = "[]" // §9: a thread's endcap empties transclusions to []
		}
		v, err := scanVersion(tx.QueryRow(ctx, `INSERT INTO item_versions
			(item_id, version, kind, at, note, content_md, content_html, content_hash, author, transclusions)
			VALUES ($1, $2, 'withdrawn', $3, $4, '', '', $5, $6::jsonb, $7::jsonb) RETURNING `+versionCols,
			id, it.Version+1, at, note, blyg.ContentHash(""), nullableJSON(prev.Author), transclusions))
		if err != nil {
			return err
		}
		it, err = scanItem(tx.QueryRow(ctx, `UPDATE items SET version = $2, withdrawn = true, published_at = $3
			WHERE id = $1 RETURNING `+itemCols, id, it.Version+1, at))
		if err != nil {
			return err
		}
		// §15.2: a withdrawn stub re-sends its mention once, so the target
		// finds a document that no longer verifies.
		if _, err := tx.Exec(ctx, `UPDATE outbound_mentions SET status = 'pending', attempts = 0,
			next_attempt_at = now(), updated_at = now() WHERE source_item_id = $1 AND relation = 'stub'`, id); err != nil {
			return err
		}
		if err := s.renderItem(ctx, tx, o, id); err != nil {
			return err
		}
		if err := s.renderAggregates(ctx, tx, o); err != nil {
			return err
		}
		res.Item, res.Version = it, v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// Pin makes version n of an item fetchable forever (§8). Irrevocable; the
// schema refuses to unpin. Pinning emits no feed event (§7).
func (s *Studio) Pin(ctx context.Context, originID int64, id string, n int) (*Version, error) {
	var out *Version
	err := pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		o, err := lockOrigin(ctx, tx, originID)
		if err != nil {
			return err
		}
		if _, err := scanItem(tx.QueryRow(ctx, `SELECT `+itemCols+` FROM items WHERE origin_id = $1 AND id = $2 FOR UPDATE`, originID, id)); err != nil {
			return err
		}
		v, err := scanVersion(tx.QueryRow(ctx, `SELECT `+versionCols+` FROM item_versions WHERE item_id = $1 AND version = $2`, id, n))
		if err != nil {
			return err
		}
		if v.Kind == "withdrawn" {
			return invalid("a withdrawal endcap cannot be pinned (§8 rule 2)")
		}
		if v.PinnedAt == nil {
			if v, err = scanVersion(tx.QueryRow(ctx, `UPDATE item_versions SET pinned_at = $3
				WHERE item_id = $1 AND version = $2 RETURNING `+versionCols, id, n, s.now())); err != nil {
				return err
			}
		}
		out = v
		return s.renderItem(ctx, tx, o, id)
	})
	return out, err
}
