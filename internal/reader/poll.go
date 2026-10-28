package reader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/chrisbodhi/blygd/internal/batch"
	"github.com/chrisbodhi/blygd/internal/blyg"
	"github.com/chrisbodhi/blygd/internal/events"
	"github.com/chrisbodhi/blygd/internal/fetch"
)

// Run polls until ctx ends.
func (r *Reader) Run(ctx context.Context) {
	idle := r.Idle
	if idle <= 0 {
		idle = 10 * time.Second
	}
	log := r.Log.With("worker", "poll")
	go batch.Loop(ctx, idle, r.PollBatch, log)
}

// remote is one leased remote origin.
type remote struct {
	id                 int64
	kind               string
	identity           string
	manifestURL        string
	manifestETag       string
	surface            blyg.Surface // where the manifest says the files are (§6.1)
	feedETag, feedLM   string
	indexETag, indexLM string
	newestGUID         string
	reconciledAt       *time.Time
	failures           int
}

// pollResult is what one poll learned, written back in one UPDATE.
type pollResult struct {
	failed     bool
	retryAfter time.Duration
	err        string // last_error; set on soft errors too
	more       bool   // the fetch budget ran out: come back soon
	// incomplete means a document fetch failed for a reason that may pass
	// (network, 5xx). Validators that would skip it next time are not
	// saved, so a 304 cannot hide an entry we never managed to fetch.
	incomplete bool

	feedETag, feedLM   *string
	indexETag, indexLM *string
	newestGUID         *string
	reconciled         bool
	lossy              *bool
	manifest           json.RawMessage
	manifestETag       *string
	title, siteURL     *string
	siteDiscrepancy    *string
}

// PollBatch polls up to one batch of due origins. Only origins someone
// subscribes to are polled. The lease is the mention workers' pattern: one
// UPDATE … FOR UPDATE SKIP LOCKED pushes next_poll_at forward, and the
// network work runs outside any transaction.
func (r *Reader) PollBatch(ctx context.Context) (int, error) {
	batch := r.Batch
	if batch <= 0 {
		batch = 5
	}
	leased, err := r.lease(ctx, `SELECT ro.id FROM remote_origins ro
			WHERE ro.next_poll_at <= now() AND (ro.poll_lease_until IS NULL OR ro.poll_lease_until < now())
			AND EXISTS (SELECT 1 FROM subscriptions s WHERE s.remote_origin_id = ro.id)
			ORDER BY ro.next_poll_at LIMIT $1 FOR UPDATE SKIP LOCKED`, batch)
	if err != nil {
		return 0, err
	}
	for _, o := range leased {
		if err := r.pollOne(ctx, o); err != nil {
			return len(leased), err
		}
	}
	return len(leased), nil
}

// leaseFor is how long a lease holds an origin; finish releases it early.
const leaseFor = "15 minutes"

// lease claims the origins the subquery selects: one UPDATE pushes
// next_poll_at forward and holds poll_lease_until, and the network work runs
// outside any transaction.
func (r *Reader) lease(ctx context.Context, which string, args ...any) ([]remote, error) {
	rows, err := r.Pool.Query(ctx, `UPDATE remote_origins
		SET next_poll_at = now() + interval '`+leaseFor+`', poll_lease_until = now() + interval '`+leaseFor+`'
		WHERE id IN (`+which+`)
		RETURNING id, kind, identity_url, coalesce(manifest_url, identity_url || 'blyg.json'), manifest, coalesce(manifest_etag, ''),
			coalesce(feed_etag, ''), coalesce(feed_last_modified, ''),
			coalesce(index_etag, ''), coalesce(index_last_modified, ''), coalesce(feed_newest_guid, ''), reconciled_at, poll_failures`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (remote, error) {
		var o remote
		var manifest []byte
		err := row.Scan(&o.id, &o.kind, &o.identity, &o.manifestURL, &manifest, &o.manifestETag, &o.feedETag, &o.feedLM,
			&o.indexETag, &o.indexLM, &o.newestGUID, &o.reconciledAt, &o.failures)
		o.surface = blyg.SurfaceOf(o.identity, manifest)
		return o, err
	})
}

func (r *Reader) pollOne(ctx context.Context, o remote) error {
	var res pollResult
	if o.kind == "l0" {
		res = r.pollL0(ctx, o)
	} else {
		res = r.pollBlyg(ctx, o)
	}
	if err := r.finish(ctx, o, res); err != nil {
		return err
	}
	r.notifyImport(ctx, o, res)
	return nil
}

// notifyImport tells every subscriber's open studio that a poll finished:
// how many live items the mirror now holds, and whether a backfill has more
// to fetch (the next batch follows in a minute). Progress is never stored;
// a client that missed it sees the counts on its next fetch.
func (r *Reader) notifyImport(ctx context.Context, o remote, res pollResult) {
	var items int
	_ = r.Pool.QueryRow(ctx, `SELECT count(*) FROM imported_items WHERE remote_origin_id = $1 AND state = 'current'`, o.id).Scan(&items)
	data := map[string]any{"subscription_id": o.id, "items": items, "more": res.more && !res.failed}
	if res.failed {
		data["error"] = res.err
	}
	if err := events.Notify(ctx, r.Pool, "import", 0, o.id, data); err != nil && r.Log != nil {
		r.Log.Warn("events: notify", "type", "import", "err", err)
	}
}

// PollNow polls one origin immediately, whatever its schedule or backoff.
// It is for tests and tools; users go through PollFor, which keeps §12.3.
func (r *Reader) PollNow(ctx context.Context, remoteID int64) error {
	leased, err := r.lease(ctx, `SELECT id FROM remote_origins WHERE id = $1 FOR UPDATE`, remoteID)
	if err != nil || len(leased) == 0 {
		return err
	}
	return r.pollOne(ctx, leased[0])
}

// TooSoon refuses a user-asked poll, and says when one may run.
type TooSoon struct {
	Until  time.Time
	Reason string
}

func (e *TooSoon) Error() string { return e.Reason }

// DefaultManualPollFloor is the least time between polls of one origin that
// a user may ask for: the mirror is shared, so one tenant must not be able
// to hammer an origin on everyone's behalf.
const DefaultManualPollFloor = time.Minute

func (r *Reader) manualPollFloor() time.Duration {
	if r.ManualPollFloor > 0 {
		return r.ManualPollFloor
	}
	return DefaultManualPollFloor
}

// PollFor polls one of the user's subscriptions now, if etiquette allows
// (§12.3): never while the origin is backing off after failures (polling
// must not tighten on failure), never within ManualPollFloor of the last
// poll, and never beside a poll already running.
func (r *Reader) PollFor(ctx context.Context, userID, remoteID int64) error {
	floor := fmt.Sprintf("%d seconds", int(r.manualPollFloor().Seconds()))
	leased, err := r.lease(ctx, `SELECT ro.id FROM remote_origins ro
			JOIN subscriptions s ON s.remote_origin_id = ro.id AND s.user_id = $2
			WHERE ro.id = $1
			AND (ro.poll_failures = 0 OR ro.next_poll_at <= now())
			AND (ro.last_polled_at IS NULL OR ro.last_polled_at <= now() - $3::interval)
			AND (ro.poll_lease_until IS NULL OR ro.poll_lease_until < now())
			FOR UPDATE OF ro SKIP LOCKED`, remoteID, userID, floor)
	if err != nil {
		return err
	}
	if len(leased) == 1 {
		return r.pollOne(ctx, leased[0])
	}
	// Say why not.
	var failures int
	var next time.Time
	var last, leaseUntil *time.Time
	err = r.Pool.QueryRow(ctx, `SELECT ro.poll_failures, ro.next_poll_at, ro.last_polled_at, ro.poll_lease_until
		FROM remote_origins ro JOIN subscriptions s ON s.remote_origin_id = ro.id AND s.user_id = $2
		WHERE ro.id = $1`, remoteID, userID).Scan(&failures, &next, &last, &leaseUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	now := r.now()
	switch {
	case leaseUntil != nil && leaseUntil.After(now):
		return &TooSoon{Until: now.Add(10 * time.Second), Reason: "a poll of this origin is already running"}
	case failures > 0 && next.After(now):
		return &TooSoon{Until: next, Reason: fmt.Sprintf("the origin is backing off after %d failed polls; polling must not tighten on failure (§12.3)", failures)}
	case last != nil:
		return &TooSoon{Until: last.Add(r.manualPollFloor()), Reason: fmt.Sprintf("polled less than %s ago", r.manualPollFloor())}
	}
	return &TooSoon{Until: now.Add(10 * time.Second), Reason: "a poll of this origin is already running"}
}

func strp(s string) *string { return &s }

func (r *Reader) get(ctx context.Context, u string, cond fetch.Conditional, accept string) (*fetch.Response, error) {
	return r.Fetch.GetIf(ctx, u, fetch.ReaderLimits, accept, cond)
}

// httpFailure turns a non-200, non-304 status into a failed poll, honoring
// Retry-After on 429 and 503 (§12.3).
func (r *Reader) httpFailure(what string, resp *fetch.Response) pollResult {
	res := pollResult{failed: true, err: fmt.Sprintf("%s returned %d", what, resp.Status)}
	if resp.Status == 429 || resp.Status == 503 {
		res.retryAfter = fetch.RetryAfter(resp.Header, r.now())
	}
	if resp.Status == 404 || resp.Status == 410 {
		res.err += "; if this persists the blyg may have moved: re-resolve it (§12.2)"
	}
	return res
}

// pollBlyg is §13.2: the feed is a cheap trigger, the archive index is the
// reconciliation surface, and only fetched item documents advance state.
// Every URL comes from the manifest's surface (§4, §6.1): the defaults when
// it names none.
func (r *Reader) pollBlyg(ctx context.Context, o remote) pollResult {
	var res pollResult
	budget := r.maxFetches()
	local, err := r.localVersions(ctx, o.id)
	if err != nil {
		return pollResult{failed: true, err: err.Error()}
	}

	// ── the trigger
	reconcile := o.reconciledAt == nil || r.now().Sub(*o.reconciledAt) >= r.reconcileEvery()
	feedURL := o.surface.FeedURL()
	feedResp, err := r.get(ctx, feedURL, fetch.Conditional{ETag: o.feedETag, LastModified: o.feedLM},
		"application/rss+xml, application/xml;q=0.9")
	if err != nil {
		return pollResult{failed: true, err: "feed " + feedURL + ": " + err.Error()}
	}
	want := map[string]bool{} // ids whose documents to fetch
	switch feedResp.Status {
	case 304:
	case 200:
		res.feedETag, res.feedLM = strp(feedResp.Header.Get("ETag")), strp(feedResp.Header.Get("Last-Modified"))
		feed, err := blyg.ParseFeed(feedResp.Body, feedResp.FinalURL)
		if err != nil {
			// An unparseable feed skips the trigger and goes straight to
			// an index diff (§13.2).
			reconcile = true
			res.err = "the feed did not parse; reconciled from the index"
			break
		}
		inWindow := false
		for i, e := range feed.Entries {
			if i == 0 && e.GUID != "" {
				res.newestGUID = strp(e.GUID)
			}
			if o.newestGUID != "" && e.GUID == o.newestGUID {
				inWindow = true
			}
			// An unusable entry is skipped on its own (§13.2).
			if !blyg.ValidID(e.BlygID) || !knownKind(e.BlygKind) || e.BlygVersion < 1 {
				continue
			}
			if v, ok := local[e.BlygID]; !ok || e.BlygVersion > v {
				want[e.BlygID] = true
			}
		}
		// The newest entry we saw last time has left the window: we may
		// have missed some. That is not an error path, just an index diff.
		if o.newestGUID != "" && !inWindow {
			reconcile = true
		}
	default:
		return r.httpFailure("feed "+feedURL, feedResp)
	}

	for id := range want {
		if budget <= 0 {
			res.more = true
			break
		}
		if stop := r.fetchItem(ctx, o, id, &budget, &res); stop {
			res.feedETag, res.feedLM = nil, nil
			return res
		}
	}
	if res.more || res.incomplete {
		// Some triggered documents are still unfetched: keep the old
		// validators so the next poll reads the feed in full again.
		res.feedETag, res.feedLM = nil, nil
	}

	// ── reconciliation
	if reconcile && !res.more {
		r.refreshManifest(ctx, &o, &res)
		r.reconcile(ctx, o, local, want, &budget, &res)
		if !res.more && !res.failed {
			r.recheck(ctx, o, &budget, &res)
		}
	}
	return res
}

// recheck re-fetches the few live items fetched longest ago. A stealth edit
// (§13.3) changes content without a version bump, so neither the feed nor
// the index shows it; only a fetched document can. This rolls through the
// archive a handful of documents per reconciliation, which keeps the cost
// to the origin small.
func (r *Reader) recheck(ctx context.Context, o remote, budget *int, res *pollResult) {
	n := min(r.recheckPerReconcile(), *budget)
	if n <= 0 {
		return
	}
	rows, err := r.Pool.Query(ctx, `SELECT item_id FROM imported_items
		WHERE remote_origin_id = $1 AND state = 'current' AND kind IN ('fragment', 'thread')
		  AND fetched_at < now() - interval '1 minute'
		ORDER BY fetched_at LIMIT $2`, o.id, n)
	if err != nil {
		res.err = err.Error()
		return
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		res.err = err.Error()
		return
	}
	for _, id := range ids {
		if stop := r.fetchItem(ctx, o, id, budget, res); stop {
			return
		}
	}
}

// reconcile diffs the archive index against local state (§13.2). The index
// validators are saved only when the diff completes, so a diff cut short
// by the fetch budget resumes on the next poll instead of trusting a 304.
func (r *Reader) reconcile(ctx context.Context, o remote, local map[string]int, done map[string]bool, budget *int, res *pollResult) {
	indexURL := o.surface.IndexURL()
	resp, err := r.get(ctx, indexURL, fetch.Conditional{ETag: o.indexETag, LastModified: o.indexLM}, "application/json")
	if err != nil {
		res.err = "archive index " + indexURL + ": " + err.Error()
		return
	}
	switch resp.Status {
	case 304:
		res.reconciled = true
		return
	case 404, 410:
		// A nonconforming publisher: fall back to the feed window and say
		// the subscription is lossy (§13.2).
		res.lossy = new(bool)
		*res.lossy = true
		res.err = "the archive index " + indexURL + " is missing: following the feed window only, so items can be missed (§13.2)"
		return
	case 200:
	default:
		if resp.Status == 429 || resp.Status == 503 {
			res.retryAfter = fetch.RetryAfter(resp.Header, r.now())
		}
		res.err = fmt.Sprintf("the archive index %s returned %d", indexURL, resp.Status)
		return
	}
	var idx struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(resp.Body, &idx); err != nil {
		res.err = "the archive index did not parse: " + err.Error()
		return
	}
	for _, raw := range idx.Items {
		var e blyg.IndexEntry
		if json.Unmarshal(raw, &e) != nil || !blyg.ValidID(e.ID) || !knownKind(e.Kind) || e.Version < 1 {
			continue // skipped individually (§13.2)
		}
		if done[e.ID] {
			continue
		}
		if v, ok := local[e.ID]; ok && v == e.Version {
			continue
		}
		if *budget <= 0 {
			res.more = true
			return
		}
		if stop := r.fetchItem(ctx, o, e.ID, budget, res); stop {
			return
		}
	}
	res.lossy = new(bool)
	if res.incomplete {
		return // diff again next poll rather than trust a 304
	}
	res.indexETag, res.indexLM = strp(resp.Header.Get("ETag")), strp(resp.Header.Get("Last-Modified"))
	res.reconciled = true
}

// refreshManifest re-reads the manifest where resolution found it, during
// reconciliation, for its title, the §12.2 site check and the surface the
// rest of the poll reads (§6.1). It never changes the identity.
func (r *Reader) refreshManifest(ctx context.Context, o *remote, res *pollResult) {
	resp, err := r.get(ctx, o.manifestURL, fetch.Conditional{ETag: o.manifestETag}, "application/json")
	if err != nil || resp.Status == 304 {
		return
	}
	if resp.Status != 200 || resp.FinalURL != o.manifestURL {
		res.err = fmt.Sprintf("the manifest %s returned %d from %s; if this persists the blyg may have moved: re-resolve it (§12.2)",
			o.manifestURL, resp.Status, resp.FinalURL)
		return
	}
	mr := manifestResolution(resp)
	if mr == nil {
		res.err = "the manifest " + o.manifestURL + " no longer parses as a manifest"
		return
	}
	res.manifest, res.manifestETag = mr.Manifest, strp(mr.ManifestETag)
	res.title, res.siteDiscrepancy = strp(mr.Title), strp(mr.SiteDiscrepancy)
	o.surface = blyg.SurfaceOf(o.identity, mr.Manifest)
}

// fetchItem fetches and applies one item document, then any pins it newly
// names. It returns true when the origin asked us to stop (429/503).
func (r *Reader) fetchItem(ctx context.Context, o remote, id string, budget *int, res *pollResult) bool {
	*budget--
	u := o.surface.ItemURL(id)
	resp, err := r.get(ctx, u, fetch.Conditional{}, "application/json")
	if err != nil {
		res.err, res.incomplete = u+": "+err.Error(), true
		return false
	}
	switch {
	case resp.Status == 429 || resp.Status == 503:
		*res = mergeFailure(*res, r.httpFailure(u, resp))
		res.incomplete = true
		return true
	case resp.Status != 200:
		// A poll failure never alters stored state (§13.2). A 4xx for an
		// id the origin lists is its defect, and retrying every poll would
		// not fix it; a 5xx may pass.
		res.err = fmt.Sprintf("%s returned %d", u, resp.Status)
		res.incomplete = resp.Status >= 500
		return false
	}
	ar, err := r.ApplyDocument(ctx, o.id, o.surface, id, resp.Body, resp.FinalURL)
	if err != nil {
		res.err = err.Error()
		return false
	}
	if len(ar.Pinned) == 0 {
		return false
	}
	have, err := r.retainedPins(ctx, o.id, id)
	if err != nil {
		res.err = err.Error()
		return false
	}
	for _, v := range ar.Pinned {
		if have[v] {
			continue
		}
		if *budget <= 0 {
			res.more = true
			return false
		}
		*budget--
		pu := o.surface.PinURL(id, v)
		presp, err := r.get(ctx, pu, fetch.Conditional{}, "application/json")
		if err != nil {
			res.err = pu + ": " + err.Error()
			continue
		}
		if presp.Status == 429 || presp.Status == 503 {
			*res = mergeFailure(*res, r.httpFailure(pu, presp))
			return true
		}
		if presp.Status != 200 {
			r.note(ctx, o.id, id, fmt.Sprintf("changelog marks v%d pinned, but %s returned %d (§8 rule 1)", v, pu, presp.Status))
			continue
		}
		if err := r.ApplyPin(ctx, o.id, o.surface, id, v, presp.Body, presp.FinalURL); err != nil {
			res.err = err.Error()
		}
	}
	return false
}

func mergeFailure(into, f pollResult) pollResult {
	into.failed, into.err, into.retryAfter = true, f.err, f.retryAfter
	return into
}

func (r *Reader) note(ctx context.Context, remoteID int64, itemID, msg string) {
	_, _ = r.Pool.Exec(ctx, `UPDATE imported_items SET discrepancy = $3, discrepancy_at = now()
		WHERE remote_origin_id = $1 AND item_id = $2`, remoteID, itemID, msg)
}

func (r *Reader) localVersions(ctx context.Context, remoteID int64) (map[string]int, error) {
	rows, err := r.Pool.Query(ctx, `SELECT item_id, version FROM imported_items WHERE remote_origin_id = $1`, remoteID)
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for rows.Next() {
		var id string
		var v int
		if err := rows.Scan(&id, &v); err != nil {
			rows.Close()
			return nil, err
		}
		out[id] = v
	}
	return out, rows.Err()
}

func (r *Reader) retainedPins(ctx context.Context, remoteID int64, itemID string) (map[int]bool, error) {
	rows, err := r.Pool.Query(ctx, `SELECT version FROM imported_pins WHERE remote_origin_id = $1 AND item_id = $2`, remoteID, itemID)
	if err != nil {
		return nil, err
	}
	vs, err := pgx.CollectRows(rows, pgx.RowTo[int])
	out := map[int]bool{}
	for _, v := range vs {
		out[v] = true
	}
	return out, err
}

// pollL0 polls a legacy feed (§13.6). There is no index to reconcile
// against; the window is all there is.
func (r *Reader) pollL0(ctx context.Context, o remote) pollResult {
	resp, err := r.get(ctx, o.identity, fetch.Conditional{ETag: o.feedETag, LastModified: o.feedLM},
		"application/rss+xml, application/atom+xml, application/xml;q=0.9")
	if err != nil {
		return pollResult{failed: true, err: err.Error()}
	}
	switch resp.Status {
	case 304:
		return pollResult{}
	case 200:
	default:
		return r.httpFailure("feed", resp)
	}
	feed, err := blyg.ParseFeed(resp.Body, resp.FinalURL)
	if err != nil {
		return pollResult{failed: true, err: "feed did not parse"}
	}
	if _, err := r.ApplyL0(ctx, o.id, o.identity, feed); err != nil {
		return pollResult{failed: true, err: err.Error()}
	}
	res := pollResult{feedETag: strp(resp.Header.Get("ETag")), feedLM: strp(resp.Header.Get("Last-Modified"))}
	if feed.Title != "" {
		res.title = strp(feed.Title)
	}
	if feed.Link != "" {
		res.siteURL = strp(feed.Link)
	}
	return res
}

// maxBackoff caps how long repeated failure can push an origin out.
const maxBackoff = 24 * time.Hour

// nextDelay is §12.3: exponential backoff on repeated failure, Retry-After
// honored, and never a shorter interval because something failed.
func (r *Reader) nextDelay(res pollResult, failures int) time.Duration {
	base := r.pollInterval()
	if res.more && !res.failed {
		return time.Minute
	}
	if !res.failed {
		return base
	}
	d := time.Duration(float64(base) * math.Pow(2, float64(min(failures, 10))))
	if d > maxBackoff {
		d = maxBackoff
	}
	if res.retryAfter > d {
		d = res.retryAfter
	}
	return max(d, base)
}

// finish writes one poll's outcome back.
func (r *Reader) finish(ctx context.Context, o remote, res pollResult) error {
	failures := 0
	if res.failed {
		failures = o.failures + 1
	}
	delay := r.nextDelay(res, failures)
	var errText *string
	if res.err != "" {
		errText = &res.err
	}
	var manifest any
	if res.manifest != nil {
		manifest = string(res.manifest)
	}
	var siteDisc any
	if res.siteDiscrepancy != nil {
		siteDisc = *res.siteDiscrepancy
	}
	_, err := r.Pool.Exec(ctx, `UPDATE remote_origins SET
		poll_lease_until = NULL,
		next_poll_at = now() + make_interval(secs => $2),
		poll_failures = $3,
		last_polled_at = now(),
		last_error = $4,
		feed_etag = coalesce($5, feed_etag), feed_last_modified = coalesce($6, feed_last_modified),
		index_etag = coalesce($7, index_etag), index_last_modified = coalesce($8, index_last_modified),
		feed_newest_guid = coalesce($9, feed_newest_guid),
		reconciled_at = CASE WHEN $10 THEN now() ELSE reconciled_at END,
		lossy = coalesce($11, lossy),
		manifest = coalesce($12::jsonb, manifest), manifest_etag = coalesce($13, manifest_etag),
		title = coalesce(nullif($14, ''), title), site_url = coalesce($15, site_url),
		site_discrepancy = CASE WHEN $12::jsonb IS NULL THEN site_discrepancy ELSE nullif($16, '') END
		WHERE id = $1`,
		o.id, delay.Seconds(), failures, errText,
		res.feedETag, res.feedLM, res.indexETag, res.indexLM, res.newestGUID,
		res.reconciled, res.lossy, manifest, res.manifestETag, res.title, res.siteURL, siteDisc)
	if err != nil {
		return fmt.Errorf("record poll of %s: %w", o.identity, err)
	}
	if res.failed && r.Log != nil {
		r.Log.Warn("poll failed", "origin", o.identity, "failures", failures, "next_in", delay, "err", res.err)
	}
	return nil
}
