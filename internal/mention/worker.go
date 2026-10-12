package mention

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chrisbodhi/blygd/internal/batch"
	"github.com/chrisbodhi/blygd/internal/events"
	"github.com/chrisbodhi/blygd/internal/fetch"
)

// Workers drain the inbound verification queue and the outbound outbox.
//
// Both use the same lease pattern: a claim pushes next_attempt_at forward
// and bumps attempts in one UPDATE … FOR UPDATE SKIP LOCKED, then the
// network work runs outside any transaction. A crashed worker's rows simply
// come due again when the lease expires. No Redis, no job table.
type Workers struct {
	Pool  *pgxpool.Pool
	Fetch *fetch.Client
	Log   *slog.Logger
	Batch int
	Idle  time.Duration
	// OnDisplayChange re-renders an item whose shown responses changed
	// because a verification withdrew an approval (§15.5). The studio
	// renders; this package never writes the surface.
	OnDisplayChange func(ctx context.Context, originID int64, itemID string) error
}

// verifyRetry is the backoff for a verification the network could not
// conclude. After it is used up the claim fails.
var verifyRetry = []time.Duration{time.Minute, 10 * time.Minute, time.Hour}

// Run runs both loops until ctx ends.
func (wk *Workers) Run(ctx context.Context) {
	go batch.Loop(ctx, wk.idle(), wk.VerifyBatch, wk.Log.With("worker", "verify"))
	go batch.Loop(ctx, wk.idle(), wk.SendBatch, wk.Log.With("worker", "send"))
	go batch.Loop(ctx, 10*time.Minute, wk.pruneClaims, wk.Log.With("worker", "prune"))
}

func (wk *Workers) idle() time.Duration {
	if wk.Idle > 0 {
		return wk.Idle
	}
	return 5 * time.Second
}

func (wk *Workers) batch() int {
	if wk.Batch > 0 {
		return wk.Batch
	}
	return 10
}

type inboundClaim struct {
	id           int64
	sourceURL    string
	targetItemID string
	origin       string
	originID     int64
	attempts     int
	everVerified bool
}

// VerifyBatch verifies up to one batch of pending inbound mentions.
func (wk *Workers) VerifyBatch(ctx context.Context) (int, error) {
	rows, err := wk.Pool.Query(ctx, `UPDATE inbound_mentions m
		SET next_attempt_at = now() + interval '2 minutes', attempts = attempts + 1
		FROM origins o
		WHERE o.id = m.origin_id AND m.id IN (
			SELECT id FROM inbound_mentions
			WHERE status = 'pending' AND next_attempt_at <= now()
			ORDER BY next_attempt_at LIMIT $1 FOR UPDATE SKIP LOCKED)
		RETURNING m.id, m.source_url, m.target_item_id, o.base_url, o.id, m.attempts, m.ever_verified`, wk.batch())
	if err != nil {
		return 0, err
	}
	claims, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (inboundClaim, error) {
		var c inboundClaim
		err := r.Scan(&c.id, &c.sourceURL, &c.targetItemID, &c.origin, &c.originID, &c.attempts, &c.everVerified)
		return c, err
	})
	if err != nil {
		return 0, err
	}
	for _, c := range claims {
		out := Verify(ctx, wk.Fetch, c.sourceURL, Target{
			Origin:   c.origin,
			ItemID:   c.targetItemID,
			IsPinned: wk.pinned(ctx, c.targetItemID),
		})
		if err := wk.recordInbound(ctx, c, out); err != nil {
			return len(claims), err
		}
	}
	return len(claims), nil
}

func (wk *Workers) pinned(ctx context.Context, itemID string) func(int) bool {
	return func(v int) bool {
		var ok bool
		err := wk.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM item_versions
			WHERE item_id = $1 AND version = $2 AND pinned_at IS NOT NULL)`, itemID, v).Scan(&ok)
		return err == nil && ok
	}
}

func (wk *Workers) recordInbound(ctx context.Context, c inboundClaim, out Outcome) error {
	if out.Transient && c.attempts <= len(verifyRetry) {
		_, err := wk.Pool.Exec(ctx, `UPDATE inbound_mentions SET next_attempt_at = now() + make_interval(secs => $2), failure = $3
			WHERE id = $1`, c.id, verifyRetry[c.attempts-1].Seconds(), out.Reason)
		return err
	}
	status := out.Status
	// §15.4 rule 6: a mention that verified before and fails now is gone,
	// and the record is kept so a later return is recognized.
	if status == "failed" && c.everVerified {
		status = "gone"
	}
	var relation any
	if out.Relation != "" {
		relation = out.Relation
	}
	var dropped bool
	var originID int64
	var itemID string
	err := wk.Pool.QueryRow(ctx, `UPDATE inbound_mentions m SET
		-- §15.5: approval holds only while what is shown stays the same.
		approved_at = CASE WHEN $2 = 'verified'
			AND m.relation IS NOT DISTINCT FROM $3::text
			AND m.source_origin IS NOT DISTINCT FROM nullif($4, '')
			AND m.source_page IS NOT DISTINCT FROM nullif($9, '')
			AND m.source_author IS NOT DISTINCT FROM $8::jsonb
			THEN m.approved_at END,
		status = $2,
		relation = coalesce($3, CASE WHEN $2 = 'gone' THEN relation END),
		ever_verified = ever_verified OR $2 = 'verified',
		source_origin = coalesce(nullif($4, ''), source_origin),
		source_item_id = coalesce(nullif($5, ''), source_item_id),
		source_version = coalesce(nullif($6, 0), source_version),
		source_kind = coalesce(nullif($7, ''), source_kind),
		source_author = CASE WHEN $2 = 'verified' THEN $8::jsonb ELSE source_author END,
		source_page = coalesce(nullif($9, ''), source_page),
		failure = CASE WHEN $2 = 'verified' THEN NULL ELSE $10 END,
		verified_at = CASE WHEN $2 = 'verified' THEN now() ELSE verified_at END,
		next_attempt_at = NULL
		FROM (SELECT approved_at FROM inbound_mentions WHERE id = $1) old
		WHERE m.id = $1
		RETURNING old.approved_at IS NOT NULL AND m.approved_at IS NULL, m.origin_id, m.target_item_id`,
		c.id, status, relation, out.SourceOrigin, out.SourceID, out.SourceVersion, out.SourceKind,
		nullJSON(out.SourceAuthor), out.SourcePage, out.Reason).Scan(&dropped, &originID, &itemID)
	if err != nil {
		return err
	}
	wk.notify(ctx, "mention", originID, map[string]any{"id": c.id, "status": status, "relation": out.Relation})
	if !dropped || wk.OnDisplayChange == nil {
		return nil
	}
	// An approved response left the page: re-render it now.
	return wk.OnDisplayChange(ctx, originID, itemID)
}

func nullJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

type outboundClaim struct {
	id       int64
	m        Outbound
	targetID string
	attempts int
	originID int64
}

// importedPage returns the target's page as the import mirror holds it
// (§15.2: "the target's page if the sender holds it, else the reference
// convention"). The publish transaction wrote the convention, because it
// does not read the mirror; the worker, which writes no surface, upgrades
// the target here.
func (wk *Workers) importedPage(ctx context.Context, origin, itemID string) string {
	if origin == "" || itemID == "" {
		return ""
	}
	var page string
	err := wk.Pool.QueryRow(ctx, `SELECT ii.page_url FROM imported_items ii JOIN remote_origins ro ON ro.id = ii.remote_origin_id
		WHERE ro.kind = 'blyg' AND ro.identity_url = $1 AND ii.item_id = $2 AND ii.page_url IS NOT NULL`, origin, itemID).Scan(&page)
	if err != nil || !strings.HasPrefix(page, origin) {
		return "" // a page outside the target's origin is not its page to name
	}
	return page
}

// SendBatch sends up to one batch of due outbound mentions.
func (wk *Workers) SendBatch(ctx context.Context) (int, error) {
	rows, err := wk.Pool.Query(ctx, `UPDATE outbound_mentions
		SET next_attempt_at = now() + interval '10 minutes', attempts = attempts + 1, updated_at = now()
		WHERE id IN (
			SELECT id FROM outbound_mentions
			WHERE status = 'pending' AND next_attempt_at <= now()
			ORDER BY next_attempt_at LIMIT $1 FOR UPDATE SKIP LOCKED)
		RETURNING id, source_url, target_url, coalesce(target_origin, ''), coalesce(target_id, ''), attempts, origin_id`, wk.batch())
	if err != nil {
		return 0, err
	}
	claims, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (outboundClaim, error) {
		var c outboundClaim
		err := r.Scan(&c.id, &c.m.SourceURL, &c.m.TargetURL, &c.m.TargetOrigin, &c.targetID, &c.attempts, &c.originID)
		return c, err
	})
	if err != nil {
		return 0, err
	}
	for _, c := range claims {
		if page := wk.importedPage(ctx, c.m.TargetOrigin, c.targetID); page != "" && page != c.m.TargetURL {
			c.m.TargetURL = page
			if _, err := wk.Pool.Exec(ctx, `UPDATE outbound_mentions SET target_url = $2 WHERE id = $1`, c.id, page); err != nil {
				return len(claims), err
			}
		}
		res := Send(ctx, wk.Fetch, c.m)
		status, next := res.Status, any(nil)
		if status == "retry" {
			if c.attempts > len(RetrySchedule) {
				status = "abandoned"
			} else {
				status = "pending"
				next = RetryDelay(c.attempts, res.RetryAfter).Seconds()
			}
		}
		if _, err := wk.Pool.Exec(ctx, `UPDATE outbound_mentions SET status = $2,
			next_attempt_at = CASE WHEN $3::float8 IS NULL THEN NULL ELSE now() + make_interval(secs => $3::float8) END,
			endpoint = coalesce(nullif($4, ''), endpoint), last_result = $5, updated_at = now()
			WHERE id = $1`, c.id, status, next, res.Endpoint, res.Detail); err != nil {
			return len(claims), err
		}
		wk.notify(ctx, "outbound", c.originID, map[string]any{"id": c.id, "status": status, "last_result": res.Detail})
	}
	return len(claims), nil
}

// notify sends a hint to the origin owner's open studios. Delivery is not
// the worker's business, so a failure is only logged.
func (wk *Workers) notify(ctx context.Context, typ string, originID int64, data any) {
	if err := events.Notify(ctx, wk.Pool, typ, originID, 0, data); err != nil && wk.Log != nil {
		wk.Log.Warn("events: notify", "type", typ, "err", err)
	}
}

func (wk *Workers) pruneClaims(ctx context.Context) (int, error) {
	_, err := wk.Pool.Exec(ctx, `DELETE FROM mention_claims WHERE at < now() - interval '2 hours'`)
	return 0, err
}
