package studio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/chrisbodhi/blygd/internal/blyg"
)

// Content types for surface files.
const (
	TypeJSON = "application/json; charset=utf-8"
	TypeRSS  = "application/rss+xml; charset=utf-8"
	TypeHTML = "text/html; charset=utf-8"
)

// putFile writes one surface file. Pins are write-once: the first render is
// the promise, and a later render (say, after a code change in field order)
// must not alter promised bytes, so they insert-or-ignore.
func putFile(ctx context.Context, tx pgx.Tx, originID int64, path, ctype string, body []byte, permanent, immutable bool) error {
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	var err error
	if immutable {
		_, err = tx.Exec(ctx, `INSERT INTO surface_files (origin_id, path, content_type, body, etag, permanent, immutable)
			VALUES ($1, $2, $3, $4, $5, true, true) ON CONFLICT (origin_id, path) DO NOTHING`,
			originID, path, ctype, body, etag)
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO surface_files (origin_id, path, content_type, body, etag, permanent)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (origin_id, path) DO UPDATE SET content_type = EXCLUDED.content_type,
				body = EXCLUDED.body, etag = EXCLUDED.etag, permanent = surface_files.permanent OR EXCLUDED.permanent,
				updated_at = now()
			WHERE surface_files.etag <> EXCLUDED.etag OR surface_files.content_type <> EXCLUDED.content_type`,
			originID, path, ctype, body, etag, permanent)
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// renderAll re-renders every item and the aggregates.
func (s *Studio) renderAll(ctx context.Context, tx pgx.Tx, o *Origin) error {
	rows, err := tx.Query(ctx, `SELECT id FROM items WHERE origin_id = $1 AND version > 0`, o.ID)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.renderItem(ctx, tx, o, id); err != nil {
			return err
		}
	}
	return s.renderAggregates(ctx, tx, o)
}

// renderItem writes items/{id}.json, every pin of the item, and its
// permalink page.
func (s *Studio) renderItem(ctx context.Context, tx pgx.Tx, o *Origin, id string) error {
	it, err := scanItem(tx.QueryRow(ctx, `SELECT `+itemCols+` FROM items WHERE origin_id = $1 AND id = $2`, o.ID, id))
	if err != nil {
		return err
	}
	if it.Version == 0 {
		return nil // drafts have no surface (§4)
	}
	vs, err := loadVersions(ctx, tx, id)
	if err != nil {
		return err
	}
	if len(vs) == 0 || vs[len(vs)-1].Version != it.Version {
		return fmt.Errorf("item %s: versions out of step with item row", id)
	}
	latest := vs[len(vs)-1]

	doc := itemDoc(o, it, vs)
	body, err := blyg.Marshal(doc)
	if err != nil {
		return err
	}
	if err := putFile(ctx, tx, o.ID, "items/"+id+".json", TypeJSON, body, true, false); err != nil {
		return err
	}

	var pins []int
	for _, v := range vs {
		if v.PinnedAt == nil {
			continue
		}
		pins = append(pins, v.Version)
		pb, err := blyg.Marshal(pinDoc(o, it, v))
		if err != nil {
			return err
		}
		if err := putFile(ctx, tx, o.ID, fmt.Sprintf("items/%s/v%d.json", id, v.Version), TypeJSON, pb, true, true); err != nil {
			return err
		}
	}

	page, err := blyg.RenderPermalink(pageSite(o), blyg.PageItem{
		ID: id, Kind: latest.Kind, Version: it.Version, Updated: latest.At, Page: *it.Page,
		ContentHTML: template.HTML(latest.ContentHTML), Pins: pins, Withdrawn: it.Withdrawn,
	})
	if err != nil {
		return err
	}
	// §5.8: an endcap's page is 200 forever, like its document.
	return putFile(ctx, tx, o.ID, *it.Page, TypeHTML, page, true, false)
}

func pageSite(o *Origin) blyg.PageSite {
	title := o.Title
	if title == "" {
		title = o.BaseURL
	}
	return blyg.PageSite{Origin: o.BaseURL, Title: title, Webmention: o.AcceptMentions}
}

func itemDoc(o *Origin, it *Item, vs []*Version) *blyg.ItemDoc {
	latest := vs[len(vs)-1]
	changelog := make([]blyg.ChangelogEntry, 0, len(vs))
	for _, v := range vs {
		changelog = append(changelog, blyg.ChangelogEntry{
			Version:   v.Version,
			At:        blyg.Time(v.At),
			Note:      v.Note,
			Pinned:    v.PinnedAt != nil,
			Generated: v.NoteGenerated && v.Note != nil,
		})
	}
	doc := &blyg.ItemDoc{
		Blyg:        blyg.ProtocolVersion,
		ID:          it.ID,
		Kind:        latest.Kind,
		Origin:      o.BaseURL,
		Page:        *it.Page,
		Author:      latest.Author,
		Created:     blyg.Time(*it.FirstPublishedAt),
		Updated:     blyg.Time(latest.At),
		Version:     latest.Version,
		ContentMD:   latest.ContentMD,
		ContentHTML: latest.ContentHTML,
		ContentHash: latest.ContentHash,
		Media:       []blyg.Media{}, // media upload is Phase 4
		ForkedFrom:  it.ForkedFrom,  // survives withdrawal (§9)
		Changelog:   changelog,
	}
	if it.Kind == "thread" {
		refs := []blyg.Reference{}
		if latest.Transclusions != nil {
			_ = json.Unmarshal(latest.Transclusions, &refs)
		}
		doc.Transclusions = &refs
	}
	if latest.Kind != "withdrawn" {
		doc.StubOf = latest.StubOf
		doc.Generated = latest.Generated
	}
	return doc
}

func pinDoc(o *Origin, it *Item, v *Version) *blyg.PinDoc {
	doc := &blyg.PinDoc{
		Blyg:        blyg.ProtocolVersion,
		ID:          it.ID,
		Kind:        v.Kind,
		Version:     v.Version,
		At:          blyg.Time(v.At),
		Note:        v.Note,
		Pinned:      true,
		Origin:      o.BaseURL,
		Author:      v.Author,
		ContentMD:   v.ContentMD,
		ContentHTML: v.ContentHTML,
		ContentHash: v.ContentHash,
		StubOf:      v.StubOf,
		ForkedFrom:  it.ForkedFrom,
		Generated:   v.Generated,
	}
	if v.Kind == "thread" {
		refs := []blyg.Reference{}
		if v.Transclusions != nil {
			_ = json.Unmarshal(v.Transclusions, &refs)
		}
		doc.Transclusions = &refs
	}
	return doc
}

// renderAggregates writes items/index.json, feed.xml, blyg.json and the
// feed page — everything derived from the whole origin.
func (s *Studio) renderAggregates(ctx context.Context, tx pgx.Tx, o *Origin) error {
	// ── archive index (§6.2): every published item, withdrawn included.
	rows, err := tx.Query(ctx, `SELECT i.id, v.kind, i.first_published_at, i.published_at, i.version
		FROM items i JOIN item_versions v ON v.item_id = i.id AND v.version = i.version
		WHERE i.origin_id = $1 AND i.version > 0
		ORDER BY i.published_at DESC, i.id`, o.ID)
	if err != nil {
		return err
	}
	idx := blyg.Index{Items: []blyg.IndexEntry{}}
	latest := o.CreatedAt
	for rows.Next() {
		var e blyg.IndexEntry
		var created, updated time.Time
		if err := rows.Scan(&e.ID, &e.Kind, &created, &updated, &e.Version); err != nil {
			rows.Close()
			return err
		}
		e.Created, e.Updated = blyg.Time(created), blyg.Time(updated)
		if updated.After(latest) {
			latest = updated
		}
		idx.Items = append(idx.Items, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	idx.Updated = blyg.Time(latest)
	body, err := blyg.Marshal(idx)
	if err != nil {
		return err
	}
	if err := putFile(ctx, tx, o.ID, "items/index.json", TypeJSON, body, false, false); err != nil {
		return err
	}

	// ── feed (§7): one entry per publish event, newest first; a withdrawn
	// item contributes only its withdrawal event; every entry carries the
	// item's latest content.
	frows, err := tx.Query(ctx, `SELECT v.item_id, v.version, v.kind, v.at, v.note, i.first_published_at, i.page,
			latest.content_html, latest.author->>'name'
		FROM item_versions v
		JOIN items i ON i.id = v.item_id
		JOIN item_versions latest ON latest.item_id = i.id AND latest.version = i.version
		WHERE i.origin_id = $1 AND (NOT i.withdrawn OR v.version = i.version)
		ORDER BY v.at DESC, v.item_id, v.version DESC
		LIMIT $2`, o.ID, blyg.FeedWindow)
	if err != nil {
		return err
	}
	var entries []blyg.FeedEntry
	for frows.Next() {
		var e blyg.FeedEntry
		var authorName *string
		if err := frows.Scan(&e.ID, &e.Version, &e.Kind, &e.At, &e.Note, &e.Created, &e.Page, &e.LatestHTML, &authorName); err != nil {
			frows.Close()
			return err
		}
		if authorName != nil {
			e.AuthorName = *authorName
		}
		entries = append(entries, e)
	}
	frows.Close()
	if err := frows.Err(); err != nil {
		return err
	}
	title := o.Title
	if title == "" {
		title = o.BaseURL
	}
	feed := blyg.RenderFeed(blyg.FeedChannel{
		Origin: o.BaseURL, Title: title, Description: o.Description, Level: s.Level, BuiltAt: latest,
	}, entries)
	if err := putFile(ctx, tx, o.ID, "feed.xml", TypeRSS, feed, false, false); err != nil {
		return err
	}

	// ── manifest (§6.1).
	updated := latest
	if o.UpdatedAt.After(updated) {
		updated = o.UpdatedAt
	}
	m := blyg.Manifest{
		Blyg: blyg.ProtocolVersion, Level: s.Level, Generator: s.Generator, GeneratorURL: s.GeneratorURL,
		Site: o.BaseURL, Title: title, Author: o.Author,
		Feed: "feed.xml", Items: "items/index.json", Updated: blyg.Time(updated),
	}
	if o.AcceptMentions {
		m.Webmention = "webmention"
	}
	mb, err := blyg.Marshal(m)
	if err != nil {
		return err
	}
	if err := putFile(ctx, tx, o.ID, "blyg.json", TypeJSON, mb, false, false); err != nil {
		return err
	}

	// ── feed page: live items, newest first.
	prows, err := tx.Query(ctx, `SELECT i.id, v.kind, i.version, v.at, i.page, v.content_html
		FROM items i JOIN item_versions v ON v.item_id = i.id AND v.version = i.version
		WHERE i.origin_id = $1 AND i.version > 0 AND NOT i.withdrawn
		ORDER BY i.published_at DESC, i.id LIMIT $2`, o.ID, blyg.FeedWindow)
	if err != nil {
		return err
	}
	var items []blyg.PageItem
	for prows.Next() {
		var p blyg.PageItem
		var html string
		if err := prows.Scan(&p.ID, &p.Kind, &p.Version, &p.Updated, &p.Page, &html); err != nil {
			prows.Close()
			return err
		}
		p.ContentHTML = template.HTML(html)
		items = append(items, p)
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return err
	}
	fp, err := blyg.RenderFeedPage(pageSite(o), items)
	if err != nil {
		return err
	}
	return putFile(ctx, tx, o.ID, "", TypeHTML, fp, false, false)
}

// SurfaceFile is one stored surface file.
type SurfaceFile struct {
	ContentType string
	Body        []byte
	ETag        string
	Immutable   bool
}

// GetFile returns a surface file, or nil if there is none at path.
func (s *Studio) GetFile(ctx context.Context, originID int64, path string) (*SurfaceFile, error) {
	var f SurfaceFile
	err := s.Pool.QueryRow(ctx, `SELECT content_type, body, etag, immutable FROM surface_files WHERE origin_id = $1 AND path = $2`,
		originID, path).Scan(&f.ContentType, &f.Body, &f.ETag, &f.Immutable)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// TrimOrigin returns u's path relative to the origin base, and whether u is
// inside the origin at all.
func TrimOrigin(base, u string) (string, bool) {
	if !strings.HasPrefix(u, base) {
		return "", false
	}
	return u[len(base):], true
}
