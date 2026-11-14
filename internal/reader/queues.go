package reader

import (
	"context"
	"time"
)

// Queues counts the reader's work, for the operator's log line (App.Run):
// the origins someone subscribes to, how many of those are past due for a
// poll by more than late and not being polled (the poller is behind or
// stopped), how many are failing, and the subscription imports waiting,
// with how many of those are late.
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
			count(*) FILTER (WHERE status = 'pending'),
			count(*) FILTER (WHERE status = 'pending' AND next_attempt_at < now() - $1::float8 * interval '1 second')
		FROM subscription_imports`, late.Seconds()).Scan(&imports, &importsLate); err != nil {
		return nil, err
	}
	return map[string]int64{
		"polled": origins, "poll_late": pollLate, "poll_failing": failing,
		"import": imports, "import_late": importsLate,
	}, nil
}
