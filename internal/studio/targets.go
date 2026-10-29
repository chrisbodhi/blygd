package studio

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Querier is what a read of plane 1 needs: a pool or a transaction. The
// lookups here are the studio's answers about its own items for packages
// that must not know its tables (the mention receiver and verifier).
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// TargetItem resolves an origin-relative path to one of this origin's
// items, by the addresses it itself serves for it (§15.3 step 1: membership,
// not location, decision #84): its document (items/{id}.json), its page, or
// a pinned version's document (items/{id}/v{n}.json) or page ({page}v{n}/).
// blygd never relocates its surface, so every such address is under the
// base URL, and the caller's prefix check refuses nothing it serves. A
// withdrawn target is fine (§15.3: people may respond to a withdrawal).
// Returns "" if none.
func TargetItem(ctx context.Context, q Querier, originID int64, rel string) (string, error) {
	if m := pinDocPath.FindStringSubmatch(rel); m != nil {
		return lookup(ctx, q, `SELECT i.id FROM items i JOIN item_versions v ON v.item_id = i.id
			WHERE i.origin_id = $1 AND i.id = $2 AND v.version = $3 AND v.pinned_at IS NOT NULL`, originID, m[1], atoi(m[2]))
	}
	if m := itemDocPath.FindStringSubmatch(rel); m != nil {
		return lookup(ctx, q, `SELECT id FROM items WHERE origin_id = $1 AND id = $2 AND version > 0`, originID, m[1])
	}
	page := rel
	if page != "" && !strings.HasSuffix(page, "/") {
		page += "/"
	}
	if m := pinPagePath.FindStringSubmatch(page); m != nil {
		id, err := lookup(ctx, q, `SELECT i.id FROM items i JOIN item_versions v ON v.item_id = i.id
			WHERE i.origin_id = $1 AND i.page = $2 AND v.version = $3 AND v.pinned_at IS NOT NULL`, originID, m[1], atoi(m[2]))
		if id != "" || err != nil {
			return id, err
		}
	}
	return lookup(ctx, q, `SELECT id FROM items WHERE origin_id = $1 AND page = $2 AND version > 0`, originID, page)
}

// atoi reads a version the path patterns have already bounded.
func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// The default layout's documents (blyg.ItemPath, blyg.PinPath), which is
// the only layout blygd publishes, and a pinned version's page.
var (
	itemDocPath = regexp.MustCompile(`^items/([0-9a-hjkmnp-tv-z]{26})\.json$`)
	pinDocPath  = regexp.MustCompile(`^items/([0-9a-hjkmnp-tv-z]{26})/v([1-9][0-9]{0,8})\.json$`)
	pinPagePath = regexp.MustCompile(`^(.+/)v([1-9][0-9]{0,8})/$`)
)

func lookup(ctx context.Context, q Querier, sql string, args ...any) (string, error) {
	var id string
	err := q.QueryRow(ctx, sql, args...).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("target lookup: %w", err)
	}
	return id, nil
}

// Pinned says whether version v of itemID is pinned. A failed lookup
// answers no, so a verification never credits a pin it could not see.
func Pinned(ctx context.Context, q Querier, itemID string, v int) bool {
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM item_versions
		WHERE item_id = $1 AND version = $2 AND pinned_at IS NOT NULL)`, itemID, v).Scan(&ok)
	return err == nil && ok
}
