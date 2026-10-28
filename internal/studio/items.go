package studio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	StubOf           json.RawMessage `json:"stub_of,omitempty"`    // §10.6, fixed at creation
	Generation       json.RawMessage `json:"generation,omitempty"` // per [TK] scope: model, at
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

const itemCols = `id, origin_id, authored_kind, draft_md, draft_note, version, withdrawn, page, forked_from, draft_stub_of, draft_generation, first_published_at, published_at, created_at, draft_updated_at`

// scanItem scans itemCols, then any extra columns into extra.
func scanItem(row pgx.Row, extra ...any) (*Item, error) {
	var it Item
	err := row.Scan(append([]any{&it.ID, &it.OriginID, &it.Kind, &it.DraftMD, &it.DraftNote, &it.Version, &it.Withdrawn, &it.Page, &it.ForkedFrom, &it.StubOf, &it.Generation, &it.FirstPublishedAt, &it.PublishedAt, &it.CreatedAt, &it.DraftUpdatedAt}, extra...)...)
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

// ListedItem is an item in a listing, with what its current version shows.
type ListedItem struct {
	*Item
	// ContentHTML is the current version's content_html: empty for a draft
	// never published, and for a withdrawal.
	ContentHTML string
	// UnpublishedChanges: a published item's draft would publish as
	// different content_md from its current version's.
	UnpublishedChanges bool
}

// ListItems lists an origin's items, newest activity first. state filters
// to draft, published or withdrawn; empty means all.
func (s *Studio) ListItems(ctx context.Context, originID int64, state string, limit int) ([]*ListedItem, error) {
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
	rows, err := s.Pool.Query(ctx, `SELECT `+itemCols+`, coalesce(v.html, ''), coalesce(v.md, '')
		FROM items LEFT JOIN LATERAL (SELECT content_html AS html, content_md AS md FROM item_versions
			WHERE item_id = items.id AND item_versions.version = items.version) v ON true
		WHERE origin_id = $1`+cond+
		` ORDER BY greatest(published_at, draft_updated_at) DESC, id LIMIT $2`, originID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ListedItem
	for rows.Next() {
		var li ListedItem
		var publishedMD string
		var err error
		li.Item, err = scanItem(rows, &li.ContentHTML, &publishedMD)
		if err != nil {
			return nil, err
		}
		if li.State() == "published" {
			md, ok := blyg.WireMD(li.DraftMD)
			li.UnpublishedChanges = !ok || md != publishedMD
		}
		out = append(out, &li)
	}
	return out, rows.Err()
}

// NewDraft is the input to CreateDraft.
type NewDraft struct {
	Kind       string // fragment or thread
	ContentMD  string
	ForkedFrom *blyg.Reference // §5.6: fixed now, immutable after
	StubOf     *StubTarget     // §10.6: fixed now; makes the draft a stub
}

// StubTarget names what a stub responds to: an item by id (and origin, to
// choose among imports), or a plain-web URL.
type StubTarget struct {
	Origin string          `json:"origin,omitempty"`
	ID     string          `json:"id,omitempty"`
	URL    string          `json:"url,omitempty"`
	Cited  json.RawMessage `json:"cited,omitempty"` // {url} targets only: the client's label
}

// CreateDraft creates a never-published item. Drafts are invisible to the
// protocol (§5.2) and 404 on the surface (§4).
//
// A fork sent without content_md starts from its source's pinned document,
// flattened (§5.6 rule 6), and takes the source's kind unless one is given.
func (s *Studio) CreateDraft(ctx context.Context, originID int64, in NewDraft) (*Item, error) {
	if in.Kind != "" && in.Kind != "fragment" && in.Kind != "thread" {
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
		if strings.TrimSpace(in.ContentMD) == "" {
			o, err := s.GetOrigin(ctx, originID)
			if err != nil {
				return nil, err
			}
			md, kind, err := s.forkContent(ctx, o, ref)
			if err != nil {
				return nil, err
			}
			in.ContentMD = md
			if in.Kind == "" {
				in.Kind = kind
			}
		}
		b, _ := json.Marshal(ref)
		forked = string(b)
	}
	if in.Kind == "" {
		in.Kind = "fragment"
		if in.StubOf != nil {
			in.Kind = "thread"
		}
	}
	var stub any
	if in.StubOf != nil {
		if in.Kind != "thread" {
			return nil, invalid("a stub is a thread (§10.6)")
		}
		o, err := s.GetOrigin(ctx, originID)
		if err != nil {
			return nil, err
		}
		raw, body, err := s.stubOf(ctx, o, in.StubOf)
		if err != nil {
			return nil, err
		}
		stub = string(raw)
		if strings.TrimSpace(in.ContentMD) == "" {
			in.ContentMD = body
		}
	}
	return scanItem(s.Pool.QueryRow(ctx, `INSERT INTO items (id, origin_id, authored_kind, draft_md, forked_from, draft_stub_of)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6::jsonb) RETURNING `+itemCols, blyg.NewID(), originID, in.Kind, in.ContentMD, forked, stub))
}

// DraftEdit changes a draft; nil leaves a field alone.
type DraftEdit struct {
	ContentMD *string
	Note      *string // "" clears
	// Generation is what the client declares about each [TK] scope, by
	// position: [{"model": …, "at": …}] (§5.7). JSON null clears it.
	Generation json.RawMessage
}

// UpdateDraft edits the draft content, the note for the next publish, and
// the generation metadata. Editing a published item's draft changes
// nothing on the wire until the next publish.
func (s *Studio) UpdateDraft(ctx context.Context, originID int64, id string, in DraftEdit) (*Item, error) {
	var gen any
	if in.Generation != nil {
		norm, err := normalizeGeneration(in.Generation)
		if err != nil {
			return nil, err
		}
		gen = norm
	}
	return scanItem(s.Pool.QueryRow(ctx, `UPDATE items SET
		draft_md = coalesce($3, draft_md),
		draft_note = CASE WHEN $4::text IS NULL THEN draft_note WHEN $4 = '' THEN NULL ELSE $4 END,
		draft_generation = CASE WHEN $5::jsonb IS NULL THEN draft_generation WHEN $5::jsonb = 'null'::jsonb THEN NULL ELSE $5::jsonb END,
		draft_updated_at = now()
		WHERE origin_id = $1 AND id = $2 RETURNING `+itemCols, originID, id, in.ContentMD, in.Note, gen))
}

// normalizeGeneration checks a generation array and puts each `at` in the
// protocol's timestamp form.
func normalizeGeneration(raw json.RawMessage) (string, error) {
	if string(raw) == "null" {
		return "null", nil
	}
	var in []struct {
		Model string `json:"model"`
		At    string `json:"at"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return "", invalid("generation must be an array of {model, at}, one per [TK] scope: %v", err)
	}
	out := make([]blyg.GenMeta, len(in))
	for i, g := range in {
		out[i].Model = strings.TrimSpace(g.Model)
		if g.At != "" {
			t, err := time.Parse(time.RFC3339, g.At)
			if err != nil {
				return "", invalid("generation[%d].at must be an RFC 3339 time", i)
			}
			out[i].At = blyg.Time(t)
		}
	}
	b, _ := json.Marshal(out)
	return string(b), nil
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

// PublishOptions are the per-publish inputs beside the draft.
type PublishOptions struct {
	Note *string // overrides the draft note when set
	// NoteGenerated says the client wrote the note, not the author (§5.2).
	NoteGenerated bool
}

// Publish publishes the draft as the next version. A thread's directives
// and every [[id]] link re-resolve now, against what the publisher holds:
// local items first, then imported snapshots (§10.2). Nothing here touches
// the network inside the transaction.
func (s *Studio) Publish(ctx context.Context, originID int64, id string, opts PublishOptions) (*PublishResult, error) {
	pre, _, err := s.GetItem(ctx, originID, id)
	if err != nil {
		return nil, err
	}
	o, err := s.GetOrigin(ctx, originID)
	if err != nil {
		return nil, err
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
	// The import lookup is a read of the mirror through reader, done before
	// the transaction like any other outside input (CLAUDE.md).
	var imports map[string][]Snapshot
	if ids := referencedIDs(pre.DraftMD); len(ids) > 0 && s.Imports != nil {
		if imports, err = s.Imports.Snapshots(ctx, o.OwnerID, ids); err != nil {
			return nil, err
		}
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
		if strings.TrimSpace(it.DraftMD) == "" {
			return invalid("nothing to publish: the draft is empty")
		}
		if it.DraftMD != pre.DraftMD {
			// The imports were looked up for the draft read above.
			return conflict("the draft changed while publishing; publish again")
		}
		page := blyg.DefaultPage(it.Kind, it.ID)
		if it.Page != nil {
			page = *it.Page
		}
		var gen []blyg.GenMeta
		if it.Generation != nil {
			if err := json.Unmarshal(it.Generation, &gen); err != nil {
				return fmt.Errorf("stored generation is malformed: %w", err)
			}
		}
		rs := &resolver{ctx: ctx, tx: tx, origin: o, self: id, imports: imports}
		c, err := blyg.Compose(blyg.ComposeInput{
			ContentMD: it.DraftMD, Thread: it.Kind == "thread", PageURL: o.BaseURL + page, Origin: o.BaseURL,
			Resolve: rs.resolve, Generation: gen,
		})
		if err != nil {
			return err
		}
		media := blyg.MediaRefs(c.ContentHTML, o.BaseURL)
		missing, err := recordMedia(ctx, tx, o, media)
		if err != nil {
			return err
		}
		if problems := append(c.Problems, missing...); len(problems) > 0 {
			return unpublishable(problems)
		}
		md := c.ContentMD
		if strings.TrimSpace(md) == "" {
			return invalid("nothing to publish: the draft is empty")
		}
		if n := utf8.RuneCountInString(md); it.Kind == "fragment" && s.FragmentCap > 0 && n > s.FragmentCap {
			return invalid("fragment is %d characters; the cap is %d (§5.3)", n, s.FragmentCap)
		}
		var prev *Version
		if it.Version > 0 {
			if prev, err = scanVersion(tx.QueryRow(ctx, `SELECT `+versionCols+` FROM item_versions WHERE item_id = $1 AND version = $2`, id, it.Version)); err != nil {
				return err
			}
			// A thread whose text is unchanged may still republish: its
			// quotes re-resolve to newer snapshots (§10.2).
			if !it.Withdrawn && prev.ContentMD == md && prev.ContentHTML == c.ContentHTML {
				// A publish event with nothing in it is churn every reader
				// pays for (a new feed entry, a new guid).
				return conflict("no change since v%d", it.Version)
			}
		}
		note := opts.Note
		if note == nil {
			note = it.DraftNote
		}
		if note != nil && strings.TrimSpace(*note) == "" {
			note = nil
		}
		at := s.now()
		if prev != nil && at.Before(prev.At) {
			at = prev.At // keep the changelog ordered if the clock stepped back
		}

		var transclusions, stubOf, generated any
		if it.Kind == "thread" {
			for i, t := range c.Quoted {
				if t.Origin == "" {
					continue
				}
				// §5.9: each remote entry carries its human half, frozen now.
				c.Transclusions[i].Cited = cited(rs.quoted[t.Origin+" "+t.ID], at)
				if !t.Pinned {
					res.Warnings = append(res.Warnings, fmt.Sprintf(
						"![[%s]] quotes v%d at %s, which is not pinned: if it is withdrawn, this thread cannot be republished until the quote is removed (§10.2)",
						t.ID, t.Version, t.Origin))
				}
			}
			b, err := json.Marshal(c.Transclusions)
			if err != nil {
				return err
			}
			transclusions = string(b)
			if it.StubOf != nil {
				// §10.6 rule 3: the value is monotonic, so it starts from the
				// last one published, not the one frozen at creation. An
				// endcap carries none (§9), so a return looks past it.
				last := it.StubOf
				var published []byte
				err := tx.QueryRow(ctx, `SELECT stub_of FROM item_versions WHERE item_id = $1 AND stub_of IS NOT NULL
					ORDER BY version DESC LIMIT 1`, id).Scan(&published)
				switch {
				case err == nil:
					last = published
				case !errors.Is(err, pgx.ErrNoRows):
					return err
				}
				agreed, err := stubAgreement(last, o.BaseURL, c.Transclusions)
				if err != nil {
					return err
				}
				stubOf = string(agreed)
			}
		}
		if len(c.Generated) > 0 {
			b, err := json.Marshal(c.Generated)
			if err != nil {
				return err
			}
			generated = string(b)
		}
		next := it.Version + 1
		v, err := scanVersion(tx.QueryRow(ctx, `INSERT INTO item_versions
			(item_id, version, kind, at, note, note_generated, content_md, content_html, content_hash, author, transclusions, stub_of, generated)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11::jsonb, $12::jsonb, $13::jsonb) RETURNING `+versionCols,
			id, next, it.Kind, at, note, note != nil && opts.NoteGenerated, md, c.ContentHTML, blyg.ContentHash(md),
			nullableJSON(o.Byline), transclusions, stubOf, generated))
		if err != nil {
			return err
		}
		if err := insertVersionMedia(ctx, tx, o, id, next, media); err != nil {
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
		// §15.2, §15.7: a withdrawn stub re-sends its mention once, so the
		// target finds a document that no longer verifies. The same is true
		// of every reference the item made (a withdrawn thread's quotes stop
		// verifying too), so all of them are re-sent.
		if _, err := tx.Exec(ctx, `UPDATE outbound_mentions SET status = 'pending', attempts = 0,
			next_attempt_at = now(), updated_at = now() WHERE source_item_id = $1`, id); err != nil {
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
