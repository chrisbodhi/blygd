package reader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/chrisbodhi/blygd/internal/apperr"
	"github.com/chrisbodhi/blygd/internal/batch"
	"github.com/chrisbodhi/blygd/internal/blyg"
	"github.com/chrisbodhi/blygd/internal/events"
	"github.com/chrisbodhi/blygd/internal/fetch"
	"github.com/chrisbodhi/blygd/internal/tx"
)

// A subscription import turns an OPML list, a blogroll (§11) or another
// reader's export, into one queued row per entry. A worker resolves the
// rows in the background through Subscribe, so the subscription cap and
// §12.1 resolution apply unchanged; a long list neither holds a request
// open nor spends the user's subscribe budget. Each entry resolves its own
// xmlUrl, which upgrades to the blyg it belongs to through the feed (§11).

// MaxImportEntries caps one import, as MaxSubscriptions caps the result.
const MaxImportEntries = 1000

// importAttempts is how often an entry is tried before it fails for good.
const importAttempts = 3

// importLease holds a leased entry while it resolves; ResolveTimeout bounds
// the work, well inside it.
const importLease = "5 minutes"

// ImportEntry is one queued entry of a subscription import.
type ImportEntry struct {
	ID             int64     `json:"id"`
	URL            string    `json:"url"`
	Title          *string   `json:"title"`
	Status         string    `json:"status"` // pending, subscribed, failed
	SubscriptionID *int64    `json:"subscription_id"`
	Error          *string   `json:"error"`
	Attempts       int       `json:"attempts"`
	NextAttemptAt  time.Time `json:"next_attempt_at"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// ImportResult says what an import queued.
type ImportResult struct {
	Entries int `json:"entries"` // entries with an http(s) URL
	Queued  int `json:"queued"`  // new, or failed before and queued again
}

// ImportOPML queues the entries of an OPML document for userID. An entry
// already queued or subscribed is left alone; one that failed is tried
// again.
func (r *Reader) ImportOPML(ctx context.Context, userID int64, opml []byte) (*ImportResult, error) {
	entries, err := blyg.ParseOPML(opml)
	if err != nil {
		return nil, apperr.New(apperr.Invalid, "not an OPML document")
	}
	var urls, titles []string
	for _, e := range entries {
		u := e.XMLURL
		if u == "" {
			u = e.HTMLURL
		}
		if p, err := url.Parse(u); err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
			continue
		}
		urls, titles = append(urls, u), append(titles, e.Text)
	}
	if len(urls) > MaxImportEntries {
		return nil, apperr.Errorf(apperr.Invalid, "%d entries; one import takes at most %d", len(urls), MaxImportEntries)
	}
	tag, err := r.Pool.Exec(ctx, `INSERT INTO subscription_imports (user_id, url, title)
		SELECT $1, u, nullif(t, '') FROM unnest($2::text[], $3::text[]) AS e(u, t)
		ON CONFLICT (user_id, url) DO UPDATE SET status = 'pending', error = NULL, attempts = 0,
			next_attempt_at = now(), updated_at = now(), title = coalesce(EXCLUDED.title, subscription_imports.title)
			WHERE subscription_imports.status = 'failed'`,
		userID, urls, titles)
	if err != nil {
		return nil, err
	}
	return &ImportResult{Entries: len(urls), Queued: int(tag.RowsAffected())}, nil
}

// ImportFrom fetches a list to import: an OPML document at input, or the
// blogroll a blyg's manifest names (§6.1, §11) when input is a blyg.
func (r *Reader) ImportFrom(ctx context.Context, userID int64, input string) (*ImportResult, error) {
	resp, err := r.get(ctx, strings.TrimSpace(input), fetch.Conditional{}, "text/x-opml, application/xml;q=0.9, */*;q=0.5")
	if err == nil && resp.Status == 200 {
		if _, perr := blyg.ParseOPML(resp.Body); perr == nil {
			return r.ImportOPML(ctx, userID, resp.Body)
		}
	}
	res, err := Resolve(ctx, r.Fetch, input)
	if err != nil {
		return nil, err
	}
	var m struct {
		Blogroll string `json:"blogroll"`
	}
	if res.Kind != "blyg" || json.Unmarshal(res.Manifest, &m) != nil || m.Blogroll == "" {
		return nil, apperr.Errorf(apperr.Invalid, "%s is not OPML, and names no blogroll", input)
	}
	opmlURL := blyg.Resolve(res.Identity, m.Blogroll)
	resp, err = r.get(ctx, opmlURL, fetch.Conditional{}, "text/x-opml, application/xml;q=0.9")
	if err != nil {
		return nil, apperr.Errorf(apperr.Invalid, "the blogroll at %s: %v", opmlURL, err)
	}
	if resp.Status != 200 {
		return nil, apperr.Errorf(apperr.Invalid, "the blogroll at %s: HTTP %d", opmlURL, resp.Status)
	}
	return r.ImportOPML(ctx, userID, resp.Body)
}

const importCols = `id, url, title, status, remote_origin_id, error, attempts, next_attempt_at, created_at, updated_at`

// Imports lists userID's queued entries, oldest first. status filters to
// pending, subscribed or failed; empty means all.
func (r *Reader) Imports(ctx context.Context, userID int64, status string) ([]*ImportEntry, error) {
	if status != "" && status != "pending" && status != "subscribed" && status != "failed" {
		return nil, apperr.Errorf(apperr.BadRequest, "unknown status %q", status)
	}
	rows, err := r.Pool.Query(ctx, `SELECT `+importCols+` FROM subscription_imports
		WHERE user_id = $1 AND ($2 = '' OR status = $2) ORDER BY created_at, id`, userID, status)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*ImportEntry, error) {
		var e ImportEntry
		err := row.Scan(&e.ID, &e.URL, &e.Title, &e.Status, &e.SubscriptionID, &e.Error, &e.Attempts, &e.NextAttemptAt, &e.CreatedAt, &e.UpdatedAt)
		return &e, err
	})
}

// ClearImports forgets userID's entries with the given status, or all of
// them. It changes no subscription, and a pending entry it forgets is
// never tried.
func (r *Reader) ClearImports(ctx context.Context, userID int64, status string) (int64, error) {
	if status != "" && status != "pending" && status != "subscribed" && status != "failed" {
		return 0, apperr.Errorf(apperr.BadRequest, "unknown status %q", status)
	}
	tag, err := r.Pool.Exec(ctx, `DELETE FROM subscription_imports WHERE user_id = $1 AND ($2 = '' OR status = $2)`, userID, status)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// RunImports resolves queued entries until ctx ends.
func (r *Reader) RunImports(ctx context.Context) {
	idle := r.Idle
	if idle <= 0 {
		idle = 10 * time.Second
	}
	batch.Loop(ctx, idle, r.ImportBatch, r.Log.With("worker", "import"))
}

type leasedImport struct {
	id, userID int64
	url        string
	title      string
	attempts   int
}

// ImportBatch resolves up to one batch of due entries, at most one per
// user, so one long import cannot hold back another user's. The lease is
// the poller's: one UPDATE pushes next_attempt_at forward, and resolution
// runs outside any transaction.
func (r *Reader) ImportBatch(ctx context.Context) (int, error) {
	n := r.Batch
	if n <= 0 {
		n = 5
	}
	rows, err := r.Pool.Query(ctx, `UPDATE subscription_imports
		SET next_attempt_at = now() + interval '`+importLease+`', attempts = attempts + 1, updated_at = now()
		WHERE id IN (SELECT id FROM (SELECT DISTINCT ON (user_id) id, next_attempt_at FROM subscription_imports
				WHERE status = 'pending' AND next_attempt_at <= now() ORDER BY user_id, next_attempt_at, id) due
			ORDER BY next_attempt_at, id LIMIT $1)
		  AND status = 'pending' AND next_attempt_at <= now()
		RETURNING id, user_id, url, coalesce(title, ''), attempts`, n)
	if err != nil {
		return 0, err
	}
	leased, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (leasedImport, error) {
		var e leasedImport
		err := row.Scan(&e.id, &e.userID, &e.url, &e.title, &e.attempts)
		return e, err
	})
	if err != nil {
		return 0, err
	}
	errs := make([]error, len(leased))
	var wg sync.WaitGroup
	for i, e := range leased {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = r.resolveImport(ctx, e)
		}()
	}
	wg.Wait()
	return len(leased), errors.Join(errs...)
}

// resolveImport subscribes one entry and records how it went. The entry's
// title becomes the subscription's display title when it says something
// the origin's own title does not, and the user has not named it already.
func (r *Reader) resolveImport(ctx context.Context, e leasedImport) error {
	rctx, cancel := context.WithTimeout(ctx, ResolveTimeout)
	sid, res, err := r.Subscribe(rctx, e.userID, e.url)
	cancel()
	if err != nil && ctx.Err() != nil {
		// Shutting down is not the entry's failure: hand it back unspent.
		_, err := r.Pool.Exec(context.WithoutCancel(ctx), `UPDATE subscription_imports
			SET attempts = attempts - 1, next_attempt_at = now() WHERE id = $1`, e.id)
		return err
	}
	reason := ""
	if err != nil {
		// Kept short: it is stored, and it rides in the event, which NOTIFY
		// caps; an oversized one would roll the outcome back with it.
		reason = err.Error()
		if len(reason) > 500 {
			reason = reason[:500] + "…"
		}
	}
	return tx.Run(ctx, r.Pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		ev := map[string]any{"id": e.id}
		switch {
		case err == nil:
			if _, err := tx.Exec(ctx, `UPDATE subscription_imports SET status = 'subscribed', remote_origin_id = $2,
				error = NULL, updated_at = now() WHERE id = $1`, e.id, sid); err != nil {
				return err
			}
			if e.title != "" && e.title != res.Title {
				if _, err := tx.Exec(ctx, `UPDATE subscriptions SET display_title = $3
					WHERE user_id = $1 AND remote_origin_id = $2 AND display_title IS NULL`, e.userID, sid, e.title); err != nil {
					return err
				}
			}
			ev["status"], ev["subscription_id"] = "subscribed", sid
		case e.attempts >= importAttempts || errors.Is(err, ErrTooManySubscriptions):
			if _, err := tx.Exec(ctx, `UPDATE subscription_imports SET status = 'failed', error = $2, updated_at = now()
				WHERE id = $1`, e.id, reason); err != nil {
				return err
			}
			ev["status"], ev["error"] = "failed", reason
		default:
			// Try again later: 5 minutes, then 20 (§12.3: failure never
			// tightens the pace).
			wait := fmt.Sprintf("%d minutes", 5<<(2*(e.attempts-1)))
			if _, err := tx.Exec(ctx, `UPDATE subscription_imports SET error = $2, updated_at = now(),
				next_attempt_at = now() + $3::interval WHERE id = $1`, e.id, reason, wait); err != nil {
				return err
			}
			ev["status"], ev["error"] = "pending", reason
		}
		// Sent on commit: the user's open clients see the list move.
		return events.NotifyUser(ctx, tx, "subscribe", e.userID, ev)
	})
}
