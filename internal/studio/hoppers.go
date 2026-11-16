package studio

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/chrisbodhi/blygd/internal/blyg"
	"github.com/chrisbodhi/blygd/internal/tx"
)

// Public hoppers in the manifest (§6.1, §13.5; decision #86). A hopper is
// overlay (plane 3), and the studio never reads the overlay, so making one
// public copies its slug and title here, as opting a subscription into the
// blogroll copies its outline (§11). The renderer reads only the copy. The
// caller keeps the order safe: list after the hopper is public, unlist
// before it goes private, so a private hopper is never listed (§6.1 MUST
// NOT).

// SetPublicHopper lists the hopper at slug with title in the manifest, or
// unlists it when listed is false, and re-renders the manifest.
func (s *Studio) SetPublicHopper(ctx context.Context, originID int64, slug, title string, listed bool) error {
	title = strings.TrimSpace(title)
	if listed && (title == "" || len(title) > 200) {
		return invalid("a hopper's title must be 1–200 characters")
	}
	return tx.Run(ctx, s.Pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		o, err := lockOrigin(ctx, tx, originID)
		if err != nil {
			return err
		}
		if !listed {
			tag, err := tx.Exec(ctx, `DELETE FROM public_hoppers WHERE origin_id = $1 AND slug = $2`, originID, slug)
			if err != nil || tag.RowsAffected() == 0 {
				return err // nothing to unlist, nothing to render
			}
		} else {
			tag, err := tx.Exec(ctx, `INSERT INTO public_hoppers (origin_id, slug, title) VALUES ($1, $2, $3)
				ON CONFLICT (origin_id, slug) DO UPDATE SET title = EXCLUDED.title
				WHERE public_hoppers.title IS DISTINCT FROM EXCLUDED.title`, originID, slug, title)
			if err != nil || tag.RowsAffected() == 0 {
				return err // already listed as it is
			}
		}
		return s.renderAggregates(ctx, tx, o)
	})
}

// HopperListed reports whether the manifest lists the hopper at slug, which
// is what entitles it to a hopper feed (§13.5 rule (a)).
func (s *Studio) HopperListed(ctx context.Context, originID int64, slug string) (bool, error) {
	var one int
	err := s.Pool.QueryRow(ctx, `SELECT 1 FROM public_hoppers WHERE origin_id = $1 AND slug = $2`, originID, slug).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// publicHoppers is the manifest's hoppers list, by title.
func publicHoppers(ctx context.Context, tx pgx.Tx, originID int64) ([]blyg.ManifestHopper, error) {
	rows, err := tx.Query(ctx, `SELECT slug, title FROM public_hoppers WHERE origin_id = $1 ORDER BY lower(title), slug`, originID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (blyg.ManifestHopper, error) {
		var slug, title string
		err := r.Scan(&slug, &title)
		return blyg.ManifestHopper{Title: title, Page: blyg.HopperPage(slug), Feed: blyg.HopperFeedPath(slug)}, err
	})
}
