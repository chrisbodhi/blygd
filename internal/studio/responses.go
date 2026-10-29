package studio

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/net/publicsuffix"

	"github.com/chrisbodhi/blygd/internal/blyg"
	"github.com/chrisbodhi/blygd/internal/tx"
)

// Responses (§15.5): verified mentions are studio signals. One shows on its
// target's page only after the owner approves it: a stranger who really
// stubs your item still cannot put anything on your page without consent.
// What shows is a verb, the source's author name, its host and a link; no
// words from the source, which is never stored.

// ChangeDisplay runs change under the origin's lock and, when it reports an
// item whose shown responses changed, re-renders that item in the same
// transaction. The mention worker records each verification through it, so
// a withdrawn approval and the page that stops showing it commit together:
// if the render fails, the approval stands and the claim comes due again.
// The lock order, origin then mention, is SetMentionApproval's.
func (s *Studio) ChangeDisplay(ctx context.Context, originID int64, change func(ctx context.Context, tx pgx.Tx) (itemID string, changed bool, err error)) error {
	return tx.Run(ctx, s.Pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		o, err := lockOrigin(ctx, tx, originID)
		if err != nil {
			return err
		}
		itemID, changed, err := change(ctx, tx)
		if err != nil || !changed {
			return err
		}
		return s.renderItem(ctx, tx, o, itemID)
	})
}

// SetMentionApproval shows or hides one mention on its target's page.
// Only a verified mention can be shown.
func (s *Studio) SetMentionApproval(ctx context.Context, originID, mentionID int64, approve bool) error {
	return tx.Run(ctx, s.Pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		o, err := lockOrigin(ctx, tx, originID)
		if err != nil {
			return err
		}
		var itemID, status string
		var class *string
		err = tx.QueryRow(ctx, `SELECT target_item_id, status, class FROM inbound_mentions WHERE origin_id = $1 AND id = $2 FOR UPDATE`,
			originID, mentionID).Scan(&itemID, &status, &class)
		if errors.Is(err, pgx.ErrNoRows) {
			return notFound("mention not found")
		}
		if err != nil {
			return err
		}
		if approve && status != "verified" {
			return conflict("only a verified mention can be shown; this one is %s", status)
		}
		if approve && (class == nil || *class != "response") {
			// §15.5: notices (links and reposts) never appear among responses.
			return conflict("a notice is not a response and is never shown among them (§15.5)")
		}
		if _, err := tx.Exec(ctx, `UPDATE inbound_mentions SET approved_at = CASE WHEN $3 THEN coalesce(approved_at, now()) END
			WHERE origin_id = $1 AND id = $2`, originID, mentionID, approve); err != nil {
			return err
		}
		return s.renderItem(ctx, tx, o, itemID)
	})
}

// Block is a blocked source.
type Block struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"` // host or domain
	Value     string    `json:"value"`
	CreatedAt time.Time `json:"created_at"`
}

// ListBlocks lists an origin's blocks.
func (s *Studio) ListBlocks(ctx context.Context, originID int64) ([]Block, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, kind, value, created_at FROM mention_blocks WHERE origin_id = $1 ORDER BY created_at DESC, id`, originID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Block])
	if out == nil {
		out = []Block{}
	}
	return out, err
}

// normalizeBlock accepts a host, a domain, or a URL, and returns what the
// receiver compares against: a lowercase host, or a registrable domain.
func normalizeBlock(kind, value string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(value))
	if u, err := url.Parse(v); err == nil && u.Host != "" {
		v = u.Hostname()
	}
	v = strings.TrimSuffix(v, ".")
	if v == "" || strings.ContainsAny(v, "/ @") {
		return "", invalid("give a host, a domain, or a URL")
	}
	switch kind {
	case "host":
		return v, nil
	case "domain":
		d, err := publicsuffix.EffectiveTLDPlusOne(v)
		if err != nil {
			return "", invalid("%s is not a registrable domain", v)
		}
		return d, nil
	}
	return "", invalid("kind must be host or domain")
}

// BlockSource stops all mentions from a host or registrable domain. Those
// already received lose any approval and leave the page at once.
func (s *Studio) BlockSource(ctx context.Context, originID int64, kind, value string) (*Block, error) {
	v, err := normalizeBlock(kind, value)
	if err != nil {
		return nil, err
	}
	var b Block
	err = tx.Run(ctx, s.Pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		o, err := lockOrigin(ctx, tx, originID)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO mention_blocks (origin_id, kind, value) VALUES ($1, $2, $3)
			ON CONFLICT (origin_id, kind, value) DO UPDATE SET kind = EXCLUDED.kind
			RETURNING id, kind, value, created_at`, originID, kind, v).Scan(&b.ID, &b.Kind, &b.Value, &b.CreatedAt); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `UPDATE inbound_mentions SET approved_at = NULL
			WHERE origin_id = $1 AND approved_at IS NOT NULL
			AND ((source_host = $3 AND $2 = 'host') OR (source_domain = $3 AND $2 = 'domain'))
			RETURNING target_item_id`, originID, kind, v)
		if err != nil {
			return err
		}
		items, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		done := map[string]bool{}
		for _, id := range items {
			if !done[id] {
				done[id] = true
				if err := s.renderItem(ctx, tx, o, id); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return &b, err
}

// Unblock removes a block. Approvals the block withdrew stay withdrawn.
func (s *Studio) Unblock(ctx context.Context, originID, blockID int64) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM mention_blocks WHERE origin_id = $1 AND id = $2`, originID, blockID)
	if err == nil && tag.RowsAffected() == 0 {
		return notFound("block not found")
	}
	return err
}

var responseVerbs = map[string]string{"stub": "responded", "transclusion": "quoted this", "fork": "forked this", "source": "drew on this"}

// authorCap bounds a shown author name: it is the source's assertion.
const authorCap = 80

// shownResponses are an item's approved, verified mentions, oldest first.
func shownResponses(ctx context.Context, tx pgx.Tx, itemID string) ([]blyg.PageResponse, error) {
	rows, err := tx.Query(ctx, `SELECT relation, source_author, coalesce(source_page, source_url), source_host
		FROM inbound_mentions WHERE target_item_id = $1 AND status = 'verified' AND class = 'response' AND approved_at IS NOT NULL
		ORDER BY approved_at, id`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []blyg.PageResponse
	for rows.Next() {
		var rel, link, host string
		var author []byte
		if err := rows.Scan(&rel, &author, &link, &host); err != nil {
			return nil, err
		}
		var a struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(author, &a)
		name := []rune(strings.TrimSpace(a.Name))
		if len(name) > authorCap {
			name = append(name[:authorCap-1], '…')
		}
		out = append(out, blyg.PageResponse{Verb: responseVerbs[rel], Author: string(name), Host: host, URL: link})
	}
	return out, rows.Err()
}
