package reader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chrisbodhi/blygd/internal/blyg"
)

// Reader is the import side and the reading overlay.
type Reader struct {
	Pool  *pgxpool.Pool
	Fetch Fetcher
	Log   *slog.Logger
	Now   func() time.Time

	// PollInterval is the steady-state interval per origin. Failure only
	// ever lengthens it (§12.3). Default 30 minutes.
	PollInterval time.Duration
	// ReconcileEvery is how often an index diff runs without suspicion
	// (§13.2: readers SHOULD reconcile periodically). Default 24 hours.
	ReconcileEvery time.Duration
	// MaxFetches bounds the item and pin documents one poll of one origin
	// may fetch. A first subscribe to a large blyg finishes over several
	// polls instead of in one burst. Default 200.
	MaxFetches int
	// Recheck is how many already-current documents each reconciliation
	// re-fetches, oldest first, to catch stealth edits (§13.3). Default 10.
	Recheck int
	Batch   int
	Idle    time.Duration
	// ManualPollFloor is the least time between polls of one origin that a
	// user may ask for (PollFor). Default one minute.
	ManualPollFloor time.Duration
}

func (r *Reader) recheckPerReconcile() int {
	if r.Recheck > 0 {
		return r.Recheck
	}
	return 10
}

func (r *Reader) pollInterval() time.Duration {
	if r.PollInterval > 0 {
		return r.PollInterval
	}
	return 30 * time.Minute
}

func (r *Reader) reconcileEvery() time.Duration {
	if r.ReconcileEvery > 0 {
		return r.ReconcileEvery
	}
	return 24 * time.Hour
}

func (r *Reader) maxFetches() int {
	if r.MaxFetches > 0 {
		return r.MaxFetches
	}
	return 200
}

// importedDoc is the subset of an item document the importer reads. The
// whole document is stored verbatim; unknown members are kept and ignored
// (§13.1 rule 4).
type importedDoc struct {
	ID          string  `json:"id"`
	Kind        string  `json:"kind"`
	Origin      string  `json:"origin"`
	Page        string  `json:"page"`
	Updated     string  `json:"updated"`
	Version     int     `json:"version"`
	ContentMD   *string `json:"content_md"`
	ContentHash string  `json:"content_hash"`
	Changelog   []struct {
		Version int  `json:"version"`
		Pinned  bool `json:"pinned"`
	} `json:"changelog"`
}

// knownKind reports whether a reader at 0.3 understands kind (§5.3).
// Unknown kinds are ignored, never rejected.
func knownKind(kind string) bool {
	return kind == "fragment" || kind == "thread" || kind == "withdrawn"
}

// ApplyResult says what one fetched document did to local state.
type ApplyResult struct {
	Outcome string // new, advanced, unchanged, stealth, regression, ignored
	Pinned  []int  // versions the changelog marks pinned (§8)
}

// errNotFromOrigin means a document was not fetched from the identity
// origin's own path for it. It does not speak for the origin (§12.2).
var errNotFromOrigin = errors.New("document was not served from its origin's own URL")

// ApplyDocument applies one fetched item document to the mirror (§13.1–
// §13.4). It runs one short transaction and touches the network not at
// all: the caller fetched first. Only a fetched document ever advances
// state (§13.2).
func (r *Reader) ApplyDocument(ctx context.Context, remoteID int64, origin, itemID string, body []byte, finalURL string) (ApplyResult, error) {
	if finalURL != blyg.ItemDocURL(origin, itemID) {
		return ApplyResult{}, fmt.Errorf("%w: wanted %s, got %s", errNotFromOrigin, blyg.ItemDocURL(origin, itemID), finalURL)
	}
	var d importedDoc
	if err := json.Unmarshal(body, &d); err != nil {
		return ApplyResult{}, fmt.Errorf("item document %s: %w", itemID, err)
	}
	if !knownKind(d.Kind) {
		return ApplyResult{Outcome: "ignored"}, nil // §5.3, §13.1 rule 4
	}
	if d.ID != itemID {
		return ApplyResult{}, fmt.Errorf("item document at %s names id %q", finalURL, d.ID)
	}
	if d.Version < 1 {
		return ApplyResult{}, fmt.Errorf("item document %s: version %d", itemID, d.Version)
	}
	res := ApplyResult{}
	for _, c := range d.Changelog {
		if c.Pinned && c.Version >= 1 && c.Version <= d.Version {
			res.Pinned = append(res.Pinned, c.Version)
		}
	}

	var notes []string
	if d.Origin != "" && !blyg.SameOrigin(d.Origin, origin) {
		notes = append(notes, fmt.Sprintf("document asserts origin %s; it was fetched from %s (§12.2)", d.Origin, origin))
	}
	// §13.3: verify content_hash. A mismatch is a defect report, and the
	// content is adopted anyway.
	if d.ContentMD != nil && d.ContentHash != blyg.ContentHash(*d.ContentMD) {
		notes = append(notes, fmt.Sprintf("v%d content_hash does not match content_md (§13.3)", d.Version))
	}
	var pageURL any
	if d.Page != "" {
		if u, err := blyg.NormalizeURL(blyg.Resolve(origin, d.Page)); err == nil {
			pageURL = u
		}
	}
	var claimed any
	if t, err := time.Parse(time.RFC3339, d.Updated); err == nil {
		claimed = t
	}
	state, doc := "current", any(string(body))
	if d.Kind == "withdrawn" {
		// §13.4: roll up to null and keep nothing the origin said. Pins
		// live in imported_pins and are untouched.
		state, doc = "withdrawn", nil
	}
	now := r.now()

	err := pgx.BeginTxFunc(ctx, r.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var watermark, version int
		var curHash *string
		var curState string
		err := tx.QueryRow(ctx, `SELECT watermark, version, state, content_hash FROM imported_items
			WHERE remote_origin_id = $1 AND item_id = $2 FOR UPDATE`, remoteID, itemID).Scan(&watermark, &version, &curState, &curHash)
		if errors.Is(err, pgx.ErrNoRows) {
			res.Outcome = "new"
			_, err = tx.Exec(ctx, `INSERT INTO imported_items
				(remote_origin_id, item_id, state, watermark, version, kind, doc, content_hash, page_url, claimed_updated,
				 first_seen_at, version_seen_at, fetched_at, discrepancy, discrepancy_at)
				VALUES ($1, $2, $3, $4, $4, $5, $6, $7, $8, $9, $10, $10, $10, $11, CASE WHEN $11::text IS NULL THEN NULL ELSE $10::timestamptz END)`,
				remoteID, itemID, state, d.Version, d.Kind, doc, d.ContentHash, pageURL, claimed, now, joinNotes(notes))
			return err
		}
		if err != nil {
			return err
		}
		switch {
		case d.Version < watermark:
			// §13.3: a lower version is the origin rewriting history. Never
			// adopt it silently; say so loudly.
			res.Outcome = "regression"
			notes = append([]string{fmt.Sprintf("origin served v%d below the watermark v%d; not adopted (§13.3)", d.Version, watermark)}, notes...)
			_, err = tx.Exec(ctx, `UPDATE imported_items SET fetched_at = $3, discrepancy = $4, discrepancy_at = $3
				WHERE remote_origin_id = $1 AND item_id = $2`, remoteID, itemID, now, joinNotes(notes))
			return err
		case d.Version == version && curHash != nil && *curHash == d.ContentHash && (curState == "withdrawn") == (d.Kind == "withdrawn"):
			res.Outcome = "unchanged"
			_, err = tx.Exec(ctx, `UPDATE imported_items SET fetched_at = $3, page_url = coalesce($4, page_url),
				doc = CASE WHEN state = 'current' THEN $5::jsonb ELSE NULL END,
				discrepancy = coalesce($6, discrepancy), discrepancy_at = CASE WHEN $6::text IS NULL THEN discrepancy_at ELSE $3 END
				WHERE remote_origin_id = $1 AND item_id = $2`, remoteID, itemID, now, pageURL, doc, joinNotes(notes))
			return err
		case d.Version == version:
			// §13.3: same version, different content — a stealth edit.
			// Content ground truth wins; the watermark is unchanged.
			res.Outcome = "stealth"
			notes = append([]string{fmt.Sprintf("stealth edit: v%d changed without a version bump; adopted (§13.3)", d.Version)}, notes...)
			_, err = tx.Exec(ctx, `UPDATE imported_items SET state = $3, kind = $4, doc = $5, content_hash = $6,
				page_url = coalesce($7, page_url), claimed_updated = $8, fetched_at = $9, discrepancy = $10, discrepancy_at = $9
				WHERE remote_origin_id = $1 AND item_id = $2`,
				remoteID, itemID, state, d.Kind, doc, d.ContentHash, pageURL, claimed, now, joinNotes(notes))
			return err
		}
		// A higher version: adopt it, and the watermark follows. A later
		// version after an endcap is the item returning (§13.1 rule 3).
		res.Outcome = "advanced"
		_, err = tx.Exec(ctx, `UPDATE imported_items SET state = $3, kind = $4, doc = $5, content_hash = $6,
			page_url = coalesce($7, page_url), claimed_updated = $8, version = $9, watermark = $9,
			version_seen_at = $10, fetched_at = $10,
			discrepancy = coalesce($11, discrepancy), discrepancy_at = CASE WHEN $11::text IS NULL THEN discrepancy_at ELSE $10 END
			WHERE remote_origin_id = $1 AND item_id = $2`,
			remoteID, itemID, state, d.Kind, doc, d.ContentHash, pageURL, claimed, d.Version, now, joinNotes(notes))
		return err
	})
	return res, err
}

func joinNotes(notes []string) any {
	if len(notes) == 0 {
		return nil
	}
	return strings.Join(notes, "; ")
}

// pinDocHead is what a retained pin must say about itself.
type pinDocHead struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Version int    `json:"version"`
}

// ApplyPin stores a fetched pin document (§13.4: the only content a reader
// may retain past withdrawal). Pins are write-once here as at the origin.
func (r *Reader) ApplyPin(ctx context.Context, remoteID int64, origin, itemID string, version int, body []byte, finalURL string) error {
	if finalURL != blyg.PinURL(origin, itemID, version) {
		return fmt.Errorf("%w: wanted %s, got %s", errNotFromOrigin, blyg.PinURL(origin, itemID, version), finalURL)
	}
	var h pinDocHead
	if err := json.Unmarshal(body, &h); err != nil {
		return fmt.Errorf("pin %s v%d: %w", itemID, version, err)
	}
	if h.ID != itemID || h.Version != version || h.Kind == "withdrawn" {
		return fmt.Errorf("pin at %s is not v%d of %s", finalURL, version, itemID)
	}
	_, err := r.Pool.Exec(ctx, `INSERT INTO imported_pins (remote_origin_id, item_id, version, doc)
		VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, remoteID, itemID, version, string(body))
	return err
}

// l0Doc is the stored shape of a wrapped legacy RSS entry (§13.6). It is
// local, never a protocol document, and never leaves this instance except
// through the private reading API.
type l0Doc struct {
	Kind        string `json:"kind"` // always "l0"
	GUID        string `json:"guid,omitempty"`
	Link        string `json:"link,omitempty"`
	Title       string `json:"title,omitempty"`
	SummaryHTML string `json:"summary_html"`        // verbatim (§14): sanitize at render
	SummaryText string `json:"summary_text"`        // plain text, cut to the fragment ethos
	Published   string `json:"published,omitempty"` // as the feed claims
	Feed        string `json:"feed"`                // where it came from
	Note        string `json:"note"`                // the wrapper says what it is
}

// l0SummaryCap bounds a stored summary; l0TextCap is the fragment ethos
// (§5.3's RECOMMENDED studio cap).
const (
	l0SummaryCap = 64 << 10
	l0TextCap    = 1000
)

// ApplyL0 wraps the entries of a legacy feed (§13.6): synthetic ids, a
// local version bump when an entry's content changes, no withdrawal, and
// entries that scroll out of the window are simply kept.
func (r *Reader) ApplyL0(ctx context.Context, remoteID int64, feedURL string, feed *blyg.ParsedFeed) (int, error) {
	now := r.now()
	changed := 0
	err := pgx.BeginTxFunc(ctx, r.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		seen := map[string]bool{}
		for _, e := range feed.Entries {
			id := blyg.L0ID(e.GUID, e.Link)
			if id == "" || seen[id] {
				continue // an entry with no identity at all is skipped (§13.2)
			}
			seen[id] = true
			summary := e.Description
			if len(summary) > l0SummaryCap {
				summary = truncUTF8(summary, l0SummaryCap)
			}
			d := l0Doc{Kind: "l0", GUID: e.GUID, Link: e.Link, Title: e.Title, SummaryHTML: summary,
				SummaryText: blyg.Excerpt(summary, l0TextCap), Feed: feedURL,
				Note: "wrapped legacy RSS entry: a summary and a link, not a blyg item (§13.6)"}
			var claimed any
			if !e.Published.IsZero() {
				d.Published = blyg.Time(e.Published)
				claimed = e.Published
			}
			body, err := json.Marshal(d)
			if err != nil {
				return err
			}
			hash := blyg.ContentHash(e.Title + "\n" + e.Link + "\n" + summary)
			var page any
			if e.Link != "" {
				page = e.Link
			}
			tag, err := tx.Exec(ctx, `INSERT INTO imported_items
				(remote_origin_id, item_id, state, watermark, version, kind, doc, content_hash, page_url, claimed_updated,
				 first_seen_at, version_seen_at, fetched_at)
				VALUES ($1, $2, 'current', 1, 1, 'l0', $3, $4, $5, $6, $7, $7, $7)
				ON CONFLICT (remote_origin_id, item_id) DO UPDATE SET
					doc = EXCLUDED.doc, content_hash = EXCLUDED.content_hash, page_url = EXCLUDED.page_url,
					claimed_updated = EXCLUDED.claimed_updated,
					version = imported_items.version + 1, watermark = imported_items.watermark + 1,
					version_seen_at = EXCLUDED.fetched_at, fetched_at = EXCLUDED.fetched_at
				WHERE imported_items.content_hash IS DISTINCT FROM EXCLUDED.content_hash`,
				remoteID, id, string(body), hash, page, claimed, now)
			if err != nil {
				return err
			}
			changed += int(tag.RowsAffected())
		}
		return nil
	})
	return changed, err
}

func truncUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}
