package reader

import (
	"context"
	"time"
)

// Queues counts the reader's work, for the operator's log line (App.Run):
// the origins someone subscribes to, how many of those are past due for a
// poll by more than late and not being polled (the poller is behind or
// stopped), how many are failing, and the subscription imports waiting.
// A user's imports are late only when their earliest pending entry has
// been due for longer than late and nothing in their list has been claimed
// or finished in that time. The worker takes one entry per user per batch
// and every entry of an import is due at once, so a long list worked off
// steadily always has an entry due since it was made; that is the worker
// working, and only a list nobody has touched is the worker stuck.
func (r *Reader) Queues(ctx context.Context, late time.Duration) (map[string]int64, error) {
	var origins, pollLate, failing, imports, importsLate int64
	if err := r.Pool.QueryRow(ctx, `SELECT
			count(*),
			count(*) FILTER (WHERE next_poll_at < now() - $1::float8 * interval '1 second'
				AND (poll_lease_until IS NULL OR poll_lease_until < now())),
			count(*) FILTER (WHERE poll_failures > 0)
		FROM remote_origins ro
		WHERE EXISTS (SELECT 1 FROM subscriptions s WHERE s.remote_origin_id = ro.id)`, late.Seconds()).Scan(&origins, &pollLate, &failing); err != nil {
		return nil, err
	}
	if err := r.Pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM subscription_imports WHERE status = 'pending'),
			(SELECT count(*) FROM (SELECT min(next_attempt_at) FILTER (WHERE status = 'pending') AS head,
					max(updated_at) AS touched
				FROM subscription_imports GROUP BY user_id) u
			WHERE u.head < now() - $1::float8 * interval '1 second'
				AND u.touched < now() - $1::float8 * interval '1 second')`, late.Seconds()).Scan(&imports, &importsLate); err != nil {
		return nil, err
	}
	return map[string]int64{
		"polled": origins, "poll_late": pollLate, "poll_failing": failing,
		"import": imports, "import_late": importsLate,
	}, nil
}
