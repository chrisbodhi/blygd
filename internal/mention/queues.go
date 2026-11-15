package mention

import (
	"context"
	"time"
)

// Queues counts the mention work waiting, for the operator's log line
// (App.Run): claims to verify and mentions to send, and how many of each
// are past due by more than late, which means the workers are behind or
// stopped. Sends that gave up in the last 24 hours are counted too, since
// nothing else says so, over a window so the number reads on its own.
func (wk *Workers) Queues(ctx context.Context, late time.Duration) (map[string]int64, error) {
	var verify, verifyLate, send, sendLate, gaveUp int64
	if err := wk.Pool.QueryRow(ctx, `SELECT
			count(*) FILTER (WHERE status = 'pending'),
			count(*) FILTER (WHERE status = 'pending' AND next_attempt_at < now() - $1::float8 * interval '1 second')
		FROM inbound_mentions`, late.Seconds()).Scan(&verify, &verifyLate); err != nil {
		return nil, err
	}
	if err := wk.Pool.QueryRow(ctx, `SELECT
			count(*) FILTER (WHERE status = 'pending'),
			count(*) FILTER (WHERE status = 'pending' AND next_attempt_at < now() - $1::float8 * interval '1 second'),
			count(*) FILTER (WHERE status = 'abandoned' AND updated_at > now() - interval '24 hours')
		FROM outbound_mentions`, late.Seconds()).Scan(&send, &sendLate, &gaveUp); err != nil {
		return nil, err
	}
	return map[string]int64{
		"verify": verify, "verify_late": verifyLate,
		"send": send, "send_late": sendLate, "send_abandoned_24h": gaveUp,
	}, nil
}

// AbandonSends gives up every mention an origin has yet to send, recording
// why. Suspending a user calls it: their queued stubs and quotes would
// otherwise keep reaching the people they were suspended over, with
// retries. A withdrawal queued after this (a takedown) re-sends as usual,
// so each target still learns the item is gone (§15.7).
func (s *Store) AbandonSends(ctx context.Context, originID int64, why string) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE outbound_mentions SET status = 'abandoned', last_result = $2, updated_at = now()
		WHERE origin_id = $1 AND status = 'pending'`, originID, why)
	return tag.RowsAffected(), err
}
