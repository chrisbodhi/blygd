package reader

import (
	"context"
	"time"
)

// Queues counts the reader's work, for the operator's log line (App.Run):
// the origins someone subscribes to, how many of those are past due for a
// poll by more than late and not being polled (the poller is behind or
// stopped), how many are failing, and the subscription imports waiting.
// An import is late only at the head of its user's line: the worker takes
// one entry per user per batch, so a long list is due all at once and
// worked off one by one, and its tail waiting is not the worker failing.
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
			(SELECT count(*) FROM (SELECT DISTINCT ON (user_id) next_attempt_at FROM subscription_imports
				WHERE status = 'pending' ORDER BY user_id, next_attempt_at, id) head
			WHERE head.next_attempt_at < now() - $1::float8 * interval '1 second')`, late.Seconds()).Scan(&imports, &importsLate); err != nil {
		return nil, err
	}
	return map[string]int64{
		"polled": origins, "poll_late": pollLate, "poll_failing": failing,
		"import": imports, "import_late": importsLate,
	}, nil
}
