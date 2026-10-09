package reader

import (
	"context"
	"encoding/json"

	"github.com/chrisbodhi/blygd/internal/blyg"
	"github.com/chrisbodhi/blygd/internal/studio"
)

// snapshotDoc is the part of an item document or pin a snapshot needs.
type snapshotDoc struct {
	Kind        string          `json:"kind"`
	ContentHTML string          `json:"content_html"`
	Author      json.RawMessage `json:"author"`
	Changelog   []struct {
		Version int  `json:"version"`
		Pinned  bool `json:"pinned"`
	} `json:"changelog"`
}

// Snapshots implements studio.Imports: §10.2 step 2 over the mirror. It
// sees only origins the user subscribes to, so one tenant can never quote
// what another tenant's subscriptions imported. Copies the rule refuses
// come back with the reason, so the author is told why.
func (r *Reader) Snapshots(ctx context.Context, userID int64, ids []string) (map[string][]studio.Snapshot, error) {
	rows, err := r.Pool.Query(ctx, `SELECT ro.identity_url, ro.kind, coalesce(ro.title, ''), ii.item_id, ii.kind, ii.state,
			ii.version, ii.page_url, ii.doc, p.version, p.doc
		FROM subscriptions s
		JOIN remote_origins ro ON ro.id = s.remote_origin_id
		JOIN imported_items ii ON ii.remote_origin_id = s.remote_origin_id
		LEFT JOIN LATERAL (SELECT version, doc FROM imported_pins
			WHERE remote_origin_id = ii.remote_origin_id AND item_id = ii.item_id
			ORDER BY version DESC LIMIT 1) p ON true
		WHERE s.user_id = $1 AND ii.item_id = ANY($2)
		ORDER BY ro.identity_url`, userID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]studio.Snapshot{}
	for rows.Next() {
		var (
			s                   studio.Snapshot
			roKind, kind, state string
			page                *string
			doc, pinDoc         []byte
			pinVersion          *int
		)
		if err := rows.Scan(&s.Origin, &roKind, &s.SourceTitle, &s.ID, &kind, &state, &s.Version, &page, &doc, &pinVersion, &pinDoc); err != nil {
			return nil, err
		}
		switch {
		case roKind == "l0" || kind == "l0":
			s.Unusable = "a plain RSS (L0) entry is a wrapper, not a blyg item (§13.6)"
		case state == "current":
			s.Unusable = fillSnapshot(&s, doc, s.Version)
		case pinVersion != nil:
			// §13.4 retention is what keeps a withdrawn item quotable: the
			// snapshot is the latest retained pin.
			s.Version = *pinVersion
			s.Unusable = fillSnapshot(&s, pinDoc, s.Version)
			s.Pinned = true
		default:
			s.Unusable = "withdrawn by its origin, with no pinned version retained (§13.4)"
		}
		if page != nil {
			s.Page = *page
		} else {
			s.Page = s.Origin + blyg.DefaultPage(s.Kind, s.ID)
		}
		out[s.ID] = append(out[s.ID], s)
	}
	return out, rows.Err()
}

// fill reads the snapshot's content from a stored document, returning why
// it cannot be used when the document says nothing quotable.
func fillSnapshot(s *studio.Snapshot, raw []byte, version int) string {
	var d snapshotDoc
	if raw == nil || json.Unmarshal(raw, &d) != nil || (d.Kind != "fragment" && d.Kind != "thread") {
		return "the imported copy holds no quotable content"
	}
	s.Kind, s.ContentHTML, s.Author = d.Kind, d.ContentHTML, d.Author
	for _, e := range d.Changelog {
		if e.Version == version && e.Pinned {
			s.Pinned = true
		}
	}
	return ""
}
