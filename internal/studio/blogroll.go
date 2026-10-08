package studio

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/chrisbodhi/blygd/internal/blyg"
)

// SetBlogrollEntry adds, replaces or (with e nil) removes the blogroll
// entry for one subscription, and re-renders the surface in the same
// transaction (§11). The caller computes e from the subscription; the
// studio stores the copy, so rendering the blogroll never reads the import
// mirror and imported content has no path onto the surface (§13.5).
func (s *Studio) SetBlogrollEntry(ctx context.Context, originID, subscriptionID int64, e *blyg.BlogrollEntry) error {
	if e != nil {
		e.Text = strings.TrimSpace(e.Text)
		for _, u := range []string{e.XMLURL, e.HTMLURL} {
			if _, err := blyg.NormalizeURL(u); err != nil {
				return invalid("blogroll URLs must be absolute http(s): %q", u)
			}
		}
		if e.Text == "" {
			e.Text = e.HTMLURL
		}
	}
	return pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		o, err := lockOrigin(ctx, tx, originID)
		if err != nil {
			return err
		}
		if e == nil {
			tag, err := tx.Exec(ctx, `DELETE FROM blogroll_entries WHERE origin_id = $1 AND remote_origin_id = $2`, originID, subscriptionID)
			if err != nil || tag.RowsAffected() == 0 {
				return err // nothing to remove, nothing to render
			}
		} else {
			_, err := tx.Exec(ctx, `INSERT INTO blogroll_entries (origin_id, remote_origin_id, text, xml_url, html_url)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (origin_id, remote_origin_id) DO UPDATE SET text = EXCLUDED.text,
					xml_url = EXCLUDED.xml_url, html_url = EXCLUDED.html_url`,
				originID, subscriptionID, e.Text, e.XMLURL, e.HTMLURL)
			if err != nil {
				return err
			}
		}
		return s.renderAggregates(ctx, tx, o)
	})
}

// blogroll loads the origin's published blogroll, in display order.
func blogroll(ctx context.Context, tx pgx.Tx, originID int64) ([]blyg.BlogrollEntry, error) {
	rows, err := tx.Query(ctx, `SELECT text, xml_url, html_url FROM blogroll_entries
		WHERE origin_id = $1 ORDER BY lower(text), xml_url`, originID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (blyg.BlogrollEntry, error) {
		var e blyg.BlogrollEntry
		err := r.Scan(&e.Text, &e.XMLURL, &e.HTMLURL)
		return e, err
	})
}

// deleteFile removes a non-permanent surface file, so it 404s again.
func deleteFile(ctx context.Context, tx pgx.Tx, originID int64, path string) error {
	_, err := tx.Exec(ctx, `DELETE FROM surface_files WHERE origin_id = $1 AND path = $2 AND NOT permanent`, originID, path)
	return err
}
