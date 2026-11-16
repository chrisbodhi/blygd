package hopper

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/chrisbodhi/blygd/internal/blyg"
)

// Public display (§13.5): a public hopper is a shown list with attribution
// and links to the origin, and no protocol surface. It is served live by
// the web layer at {origin}h/{slug}/ and never written to surface_files, so
// nothing that renders the protocol surface reads the mirror (CLAUDE.md).
//
// Each entry is an excerpt in plain text plus links: the list curates, it
// does not republish, and no imported HTML reaches the page.

// SetPublic makes a hopper public or private. Going public freezes its slug
// for good: from then on the URL is someone's link.
func (s *Store) SetPublic(ctx context.Context, userID, id int64, public bool) (*Hopper, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE hoppers SET public = $3, slug_frozen = slug_frozen OR $3, updated_at = now()
		WHERE user_id = $1 AND id = $2`, userID, id, public)
	if err == nil && tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return scanHopper(s.Pool.QueryRow(ctx, `SELECT `+hopperCols+` FROM hoppers h WHERE h.id = $1`, id))
}

// PublicEntry is one shown member.
type PublicEntry struct {
	Excerpt string // plain text
	URL     string // the item's page at its origin, else its item document (§5.8, §6.1); an L0 entry's own link
	Origin  string // where the attribution links
	Source  string // the origin's title, as we hold it
	Author  string // the item's own author.name, passed through (§5.5)
	Note    string // e.g. "withdrawn; pinned v2", "plain RSS"
	AddedAt time.Time

	// What a hopper feed needs besides (§13.5 rule (d)).
	Own        bool   // one of the owner's own items
	Identity   string // the member's identity origin; an L0 entry's feed URL
	ItemID     string // the member's id; an L0 entry's synthetic one
	SourceFeed string // the member origin's feed, for <source>; "" when Own
}

// PublicView is a public hopper as its page shows it.
type PublicView struct {
	Name    string
	Slug    string
	Entries []PublicEntry
}

// excerptLen is how much of an item a public hopper shows.
const excerptLen = 280

// Public returns the public hopper with this slug, owned by ownerID, or
// ErrNotFound. Own items show only while published. Imported items show
// only from origins the owner still subscribes to, because only a polled
// copy learns of a withdrawal; a withdrawn one shows only its retained pin
// (§13.4), else nothing.
func (s *Store) Public(ctx context.Context, ownerID int64, slug string) (*PublicView, error) {
	var v PublicView
	var hid int64
	err := s.Pool.QueryRow(ctx, `SELECT id, name, slug FROM hoppers WHERE user_id = $1 AND slug = $2 AND public`,
		ownerID, slug).Scan(&hid, &v.Name, &v.Slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT m.added_at,
			i.id, o.base_url, o.title, v.author, i.page, v.content_html,
			r.identity_url, coalesce(r.site_url, r.identity_url), r.title, r.manifest, ii.item_id, ii.kind, ii.state, ii.page_url, ii.doc, p.version, p.doc
		FROM hopper_members m
		LEFT JOIN items i ON i.id = m.own_item_id AND i.version > 0 AND NOT i.withdrawn
		LEFT JOIN origins o ON o.id = i.origin_id
		LEFT JOIN item_versions v ON v.item_id = i.id AND v.version = i.version
		LEFT JOIN imported_items ii ON ii.id = m.imported_item_id
			AND EXISTS (SELECT 1 FROM subscriptions sub WHERE sub.user_id = $2 AND sub.remote_origin_id = ii.remote_origin_id)
		LEFT JOIN remote_origins r ON r.id = ii.remote_origin_id
		LEFT JOIN LATERAL (SELECT version, doc FROM imported_pins
			WHERE remote_origin_id = ii.remote_origin_id AND item_id = ii.item_id
			ORDER BY version DESC LIMIT 1) p ON ii.state = 'withdrawn'
		WHERE m.hopper_id = $1 ORDER BY m.added_at DESC, m.id DESC`, hid, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			e                                          PublicEntry
			ownID, ownBase, ownTitle, ownPage, ownHTML *string
			ownByline, manifest                        []byte
			rOrigin, rSite, rTitle, iID                *string
			iKind, iState, iPage                       *string
			doc, pinDoc                                []byte
			pinVersion                                 *int
		)
		if err := rows.Scan(&e.AddedAt, &ownID, &ownBase, &ownTitle, &ownByline, &ownPage, &ownHTML,
			&rOrigin, &rSite, &rTitle, &manifest, &iID, &iKind, &iState, &iPage, &doc, &pinVersion, &pinDoc); err != nil {
			return nil, err
		}
		if rOrigin != nil {
			e.Identity, e.ItemID = *rOrigin, deref(iID)
			if *iKind == "l0" {
				e.SourceFeed = *rOrigin // an L0 subscription's identity is its feed (§13.6)
			} else {
				e.SourceFeed = blyg.SurfaceOf(*rOrigin, manifest).FeedURL()
			}
		}
		// An imported member with no page is linked by its item document,
		// where its origin's manifest puts it; a permalink is never guessed
		// (§5.8, §13.5 rule (d), decision #102).
		page := func() string {
			if iPage != nil {
				return *iPage
			}
			return blyg.SurfaceOf(*rOrigin, manifest).ItemURL(*iID)
		}
		switch {
		case ownBase != nil:
			e.Origin, e.URL, e.Source = *ownBase, *ownBase+*ownPage, deref(ownTitle)
			e.Excerpt, e.Author = blyg.Excerpt(*ownHTML, excerptLen), authorName(ownByline)
			e.Own, e.Identity, e.ItemID = true, *ownBase, *ownID
		case rOrigin != nil && *iKind == "l0":
			// §13.6: a wrapper around a legacy feed's summary, marked as one.
			var d struct {
				Link        string `json:"link"`
				Title       string `json:"title"`
				SummaryText string `json:"summary_text"`
			}
			_ = json.Unmarshal(doc, &d)
			e.Origin, e.Source, e.URL, e.Note = *rSite, deref(rTitle), d.Link, "plain RSS"
			if e.Excerpt = d.Title; e.Excerpt == "" {
				e.Excerpt = clip(d.SummaryText, excerptLen)
			}
		case rOrigin != nil && *iState == "current":
			e.Origin, e.Source, e.URL = *rOrigin, deref(rTitle), page()
			e.Excerpt, e.Author = docExcerpt(doc)
		case rOrigin != nil && pinVersion != nil:
			e.Origin, e.Source, e.URL = *rOrigin, deref(rTitle), page()
			e.Excerpt, e.Author = docExcerpt(pinDoc)
			e.Note = "withdrawn by its author; pinned v" + strconv.Itoa(*pinVersion)
		default:
			continue // a draft, a withdrawal with nothing retained, or no longer followed
		}
		v.Entries = append(v.Entries, e)
	}
	return &v, rows.Err()
}

func docExcerpt(raw []byte) (string, string) {
	var d struct {
		ContentHTML string          `json:"content_html"`
		Author      json.RawMessage `json:"author"`
	}
	_ = json.Unmarshal(raw, &d)
	return blyg.Excerpt(d.ContentHTML, excerptLen), authorName(d.Author)
}

func authorName(raw []byte) string {
	var a struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &a) != nil {
		return ""
	}
	return a.Name
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func clip(s string, n int) string { return blyg.Clip(s, n) }
