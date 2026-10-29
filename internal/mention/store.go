package mention

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chrisbodhi/blygd/internal/apperr"
)

// Store lists an origin's mentions, received and sent, for its studio.
type Store struct {
	Pool *pgxpool.Pool
}

// Inbound is a received mention as the studio sees it.
type Inbound struct {
	ID            int64           `json:"id"`
	TargetItemID  string          `json:"target_item_id"`
	SourceURL     string          `json:"source_url"`
	TargetURL     string          `json:"target_url"`
	Status        string          `json:"status"`
	Relation      *string         `json:"relation"`
	Class         *string         `json:"class"` // response or notice (§15.5), with relation
	SourceOrigin  *string         `json:"source_origin"`
	SourceItemID  *string         `json:"source_item_id"`
	SourceVersion *int            `json:"source_version"`
	SourceKind    *string         `json:"source_kind"`
	SourceAuthor  json.RawMessage `json:"source_author"`
	SourcePage    *string         `json:"source_page"`
	Failure       *string         `json:"failure"`
	ReceivedAt    time.Time       `json:"received_at"`
	VerifiedAt    *time.Time      `json:"verified_at"`
	SourceHost    string          `json:"source_host"`
	ApprovedAt    *time.Time      `json:"approved_at"` // shown on the target's page (§15.5)
	Blocked       bool            `json:"blocked"`
}

// Filter narrows Inbound. Empty fields match everything; mentions from
// blocked sources stay out of view unless WithBlocked asks for them.
type Filter struct {
	Status      string // pending, verified, failed or gone
	Class       string // response or notice: held apart (§15.5)
	WithBlocked bool
}

// Inbound returns an origin's newest 200 received mentions that match f.
func (st *Store) Inbound(ctx context.Context, originID int64, f Filter) ([]Inbound, error) {
	if f.Status != "" && !slices.Contains([]string{"pending", "verified", "failed", "gone"}, f.Status) {
		return nil, apperr.New(apperr.BadRequest, "unknown status")
	}
	if f.Class != "" && !slices.Contains([]string{"response", "notice"}, f.Class) {
		return nil, apperr.New(apperr.BadRequest, "unknown class")
	}
	rows, err := st.Pool.Query(ctx, `SELECT * FROM (SELECT m.id, m.target_item_id, m.source_url, m.target_url, m.status, m.relation, m.class,
		m.source_origin, m.source_item_id, m.source_version, m.source_kind, m.source_author, m.source_page, m.failure, m.received_at, m.verified_at,
		m.source_host, m.approved_at,
		EXISTS (SELECT 1 FROM mention_blocks b WHERE b.origin_id = m.origin_id
			AND ((b.kind = 'host' AND b.value = m.source_host) OR (b.kind = 'domain' AND b.value = m.source_domain))) AS blocked
		FROM inbound_mentions m WHERE m.origin_id = $1 AND ($2 = '' OR m.status = $2) AND ($4 = '' OR m.class = $4)) x
		WHERE $3 OR NOT x.blocked
		ORDER BY x.received_at DESC LIMIT 200`, originID, f.Status, f.WithBlocked, f.Class)
	if err != nil {
		return nil, err
	}
	ms, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Inbound])
	if ms == nil {
		ms = []Inbound{}
	}
	return ms, err
}

// Sent is an outbound mention and how its delivery stands.
type Sent struct {
	ID            int64     `json:"id"`
	SourceItemID  string    `json:"source_item_id"`
	Relation      string    `json:"relation"`
	TargetURL     string    `json:"target_url"`
	TargetVersion *int      `json:"target_version"`
	Status        string    `json:"status"`
	Endpoint      *string   `json:"endpoint"`
	Attempts      int       `json:"attempts"`
	LastResult    *string   `json:"last_result"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Outbound returns an origin's 200 most recently updated sent mentions.
func (st *Store) Outbound(ctx context.Context, originID int64) ([]Sent, error) {
	rows, err := st.Pool.Query(ctx, `SELECT id, source_item_id, relation, target_url, target_version, status,
		endpoint, attempts, last_result, updated_at FROM outbound_mentions WHERE origin_id = $1
		ORDER BY updated_at DESC LIMIT 200`, originID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Sent])
	if out == nil {
		out = []Sent{}
	}
	return out, err
}
