package mention

import (
	"context"
	"time"
)

// Queues counts the mention work waiting, for the operator's log line
// (App.Run): claims to verify and mentions to send, and how many of each
// are past due by more than late, which means the workers are behind or
// stopped. Sends that gave up are counted too, since nothing else says so.
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
			count(*) FILTER (WHERE status = 'abandoned')
		FROM outbound_mentions`, late.Seconds()).Scan(&send, &sendLate, &gaveUp); err != nil {
		return nil, err
	}
	return map[string]int64{
		"verify": verify, "verify_late": verifyLate,
		"send": send, "send_late": sendLate, "send_abandoned": gaveUp,
	}, nil
}
