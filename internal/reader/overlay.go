package reader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/chrisbodhi/blygd/internal/apperr"
	"github.com/chrisbodhi/blygd/internal/blyg"
	"github.com/chrisbodhi/blygd/internal/tx"
)

// ErrNotFound covers subscriptions and items the user cannot see.
var ErrNotFound = apperr.New(apperr.NotFound, "not found")

// Notice travels with every response that carries imported HTML. §14: a
// reader MUST sanitize or sandbox fetched content_html before rendering
// it, and we store verbatim, so that duty is the client's.
const Notice = "content_html, summary_html and pinned documents are the origin's verbatim bytes. Sanitize or sandbox them before rendering (§14)."

// Subscription is one user's subscription to one remote origin.
type Subscription struct {
	ID              int64           `json:"id"` // the remote origin id; scopes item ids (§13.1 rule 7)
	Kind            string          `json:"kind"`
	Origin          string          `json:"origin"`
	Title           *string         `json:"title"`
	Author          json.RawMessage `json:"author"` // manifest publication identity (§6.1): asserted, opaque (§5.5)
	DisplayTitle    *string         `json:"display_title"`
	SiteURL         *string         `json:"site_url,omitempty"`
	InBlogroll      bool            `json:"in_blogroll"`
	SiteDiscrepancy *string         `json:"site_discrepancy"`
	Lossy           bool            `json:"lossy"`
	Items           int             `json:"items"`
	Unread          int             `json:"unread"`
	Discrepancies   int             `json:"discrepancies"`
	LastPolledAt    *time.Time      `json:"last_polled_at"`
	NextPollAt      time.Time       `json:"next_poll_at"`
	PollFailures    int             `json:"poll_failures"`
	LastError       *string         `json:"last_error"`
	CreatedAt       time.Time       `json:"created_at"`
}

const subCols = `ro.id, ro.kind, ro.identity_url, ro.title,
	CASE WHEN jsonb_typeof(ro.manifest->'author') = 'object' THEN ro.manifest->'author' END,
	s.display_title, ro.site_url,
	EXISTS (SELECT 1 FROM blogroll_entries b WHERE b.origin_id = $2 AND b.remote_origin_id = ro.id),
	ro.site_discrepancy, ro.lossy,
	(SELECT count(*) FROM imported_items ii WHERE ii.remote_origin_id = ro.id AND ii.state = 'current'),
	(SELECT count(*) FROM imported_items ii LEFT JOIN reads rd ON rd.imported_item_id = ii.id AND rd.user_id = s.user_id
		WHERE ii.remote_origin_id = ro.id AND ii.state = 'current' AND (rd.version IS NULL OR rd.version < ii.version)),
	(SELECT count(*) FROM imported_items ii WHERE ii.remote_origin_id = ro.id AND ii.discrepancy IS NOT NULL),
	ro.last_polled_at, ro.next_poll_at, ro.poll_failures, ro.last_error, s.created_at`

func scanSub(row pgx.Row) (*Subscription, error) {
	var s Subscription
	err := row.Scan(&s.ID, &s.Kind, &s.Origin, &s.Title, &s.Author, &s.DisplayTitle, &s.SiteURL, &s.InBlogroll,
		&s.SiteDiscrepancy, &s.Lossy, &s.Items, &s.Unread, &s.Discrepancies,
		&s.LastPolledAt, &s.NextPollAt, &s.PollFailures, &s.LastError, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &s, err
}

// Subscriptions lists a user's subscriptions. ownOriginID is the user's
// own origin, for the blogroll flag.
func (r *Reader) Subscriptions(ctx context.Context, userID, ownOriginID int64) ([]*Subscription, error) {
	rows, err := r.Pool.Query(ctx, `SELECT `+subCols+` FROM subscriptions s JOIN remote_origins ro ON ro.id = s.remote_origin_id
		WHERE s.user_id = $1 ORDER BY lower(coalesce(s.display_title, ro.title, ro.identity_url)), ro.id`, userID, ownOriginID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Subscription{}
	for rows.Next() {
		s, err := scanSub(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetSubscription returns one subscription.
func (r *Reader) GetSubscription(ctx context.Context, userID, ownOriginID, remoteID int64) (*Subscription, error) {
	return scanSub(r.Pool.QueryRow(ctx, `SELECT `+subCols+` FROM subscriptions s JOIN remote_origins ro ON ro.id = s.remote_origin_id
		WHERE s.user_id = $1 AND ro.id = $3`, userID, ownOriginID, remoteID))
}

// Subscribed says whether userID follows the remote origin. It answers
// no when it cannot tell, so a doubtful event is not sent.
func (r *Reader) Subscribed(ctx context.Context, userID, remoteID int64) bool {
	var ok bool
	_ = r.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM subscriptions WHERE user_id = $1 AND remote_origin_id = $2)`,
		userID, remoteID).Scan(&ok)
	return ok
}

// Subscribe resolves a URL (§12.1, network first) and then records the
// subscription. Many users share one remote origin row, and so one poll.
func (r *Reader) Subscribe(ctx context.Context, userID int64, input string) (int64, *Resolution, error) {
	// Checked before resolving, so a refused subscribe costs no fetch. Two
	// racing subscribes can pass it together; the cap is a budget, not an
	// invariant.
	max := r.MaxSubscriptions
	if max <= 0 {
		max = 1000
	}
	var n int
	if err := r.Pool.QueryRow(ctx, `SELECT count(*) FROM subscriptions WHERE user_id = $1`, userID).Scan(&n); err != nil {
		return 0, nil, err
	}
	if n >= max {
		return 0, nil, fmt.Errorf("%w: %d subscriptions", ErrTooManySubscriptions, max)
	}
	res, err := Resolve(ctx, r.Fetch, input)
	if err != nil {
		return 0, nil, err
	}
	id, err := r.attach(ctx, userID, res, nil)
	return id, res, err
}

// attach records a resolution as a remote origin and subscribes the user to
// it. With from set, the user's subscription to that origin is moved in the
// same transaction (an identity change the user confirmed, §12.2).
func (r *Reader) attach(ctx context.Context, userID int64, res *Resolution, from *int64) (int64, error) {
	var id int64
	err := tx.Run(ctx, r.Pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var manifest, siteURL, siteDisc any
		if res.Manifest != nil {
			manifest = string(res.Manifest)
		}
		if res.SiteURL != "" {
			siteURL = res.SiteURL
		}
		if res.SiteDiscrepancy != "" {
			siteDisc = res.SiteDiscrepancy
		}
		err := tx.QueryRow(ctx, `INSERT INTO remote_origins (kind, identity_url, manifest, manifest_etag, title, site_url, site_discrepancy, manifest_url)
			VALUES ($1, $2, $3, nullif($4, ''), nullif($5, ''), $6, $7, nullif($8, ''))
			ON CONFLICT (identity_url) DO UPDATE SET
				manifest = coalesce(EXCLUDED.manifest, remote_origins.manifest),
				manifest_url = coalesce(EXCLUDED.manifest_url, remote_origins.manifest_url),
				title = coalesce(EXCLUDED.title, remote_origins.title),
				site_url = coalesce(EXCLUDED.site_url, remote_origins.site_url),
				site_discrepancy = CASE WHEN EXCLUDED.manifest IS NULL THEN remote_origins.site_discrepancy ELSE EXCLUDED.site_discrepancy END
			RETURNING id`, res.Kind, res.Identity, manifest, res.ManifestETag, res.Title, siteURL, siteDisc, res.ManifestURL).Scan(&id)
		if err != nil {
			return err
		}
		var display *string
		if from != nil {
			if *from == id {
				return fmt.Errorf("%w: the new identity is the current one", ErrSameIdentity)
			}
			err := tx.QueryRow(ctx, `DELETE FROM subscriptions WHERE user_id = $1 AND remote_origin_id = $2 RETURNING display_title`,
				userID, *from).Scan(&display)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
		}
		tag, err := tx.Exec(ctx, `INSERT INTO subscriptions (user_id, remote_origin_id, display_title) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`, userID, id, display)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			// Poll soon, without tightening a schedule that failure set.
			_, err = tx.Exec(ctx, `UPDATE remote_origins SET next_poll_at = least(next_poll_at, now())
				WHERE id = $1 AND poll_failures = 0`, id)
		}
		return err
	})
	return id, err
}

// ErrSameIdentity means a move was asked for to where the subscription
// already is.
var ErrSameIdentity = apperr.New(apperr.Conflict, "same identity")

// ErrTooManySubscriptions: the user is at Reader.MaxSubscriptions.
var ErrTooManySubscriptions = apperr.New(apperr.Conflict, "subscription limit reached")

// Reresolve re-runs resolution for a subscription (§12.2: readers MAY, when
// surfaces 404 persistently). It changes nothing: adopting a new origin is
// an identity change and needs the user's confirmation through Move.
func (r *Reader) Reresolve(ctx context.Context, userID, remoteID int64, input string) (current string, res *Resolution, err error) {
	if err := r.Pool.QueryRow(ctx, `SELECT ro.identity_url FROM subscriptions s JOIN remote_origins ro ON ro.id = s.remote_origin_id
		WHERE s.user_id = $1 AND ro.id = $2`, userID, remoteID).Scan(&current); errors.Is(err, pgx.ErrNoRows) {
		return "", nil, ErrNotFound
	} else if err != nil {
		return "", nil, err
	}
	if input == "" {
		input = current
	}
	res, err = Resolve(ctx, r.Fetch, input)
	return current, res, err
}

// Move is the user-confirmed identity change (§12.2). It resolves to first,
// and the subscription moves only if to resolves to exactly the identity
// the user confirmed — so a redirect between confirmation and move cannot
// carry the subscription somewhere the user never saw.
func (r *Reader) Move(ctx context.Context, userID, remoteID int64, to string) (int64, *Resolution, error) {
	res, err := Resolve(ctx, r.Fetch, to)
	if err != nil {
		return 0, nil, err
	}
	want := to
	if res.Kind == "blyg" {
		if n, err := blyg.NormalizeOrigin(to); err == nil {
			want = n
		}
	} else if n, err := blyg.NormalizeURL(to); err == nil {
		want = n
	}
	if res.Identity != want {
		return 0, res, &ResolveError{Reason: fmt.Sprintf("%s resolves to %s; confirm that identity instead", to, res.Identity), Trail: res.Trail}
	}
	id, err := r.attach(ctx, userID, res, &remoteID)
	return id, res, err
}

// Unsubscribe removes a subscription. The mirror keeps its copy for other
// subscribers; an origin nobody subscribes to is simply no longer polled.
func (r *Reader) Unsubscribe(ctx context.Context, userID, remoteID int64) error {
	tag, err := r.Pool.Exec(ctx, `DELETE FROM subscriptions WHERE user_id = $1 AND remote_origin_id = $2`, userID, remoteID)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// SetDisplayTitle sets or clears (nil) a subscription's private title.
func (r *Reader) SetDisplayTitle(ctx context.Context, userID, remoteID int64, title *string) error {
	if title != nil {
		t := strings.TrimSpace(*title)
		if t == "" {
			title = nil
		} else {
			title = &t
		}
	}
	tag, err := r.Pool.Exec(ctx, `UPDATE subscriptions SET display_title = $3 WHERE user_id = $1 AND remote_origin_id = $2`,
		userID, remoteID, title)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// BlogrollEntry computes the §11 outline for a subscription: xmlUrl is the
// feed, htmlUrl the origin (or the page URL of a legacy feed), and the
// text the title the user sees. The studio stores a copy, so publishing
// the blogroll never reads the mirror.
func (r *Reader) BlogrollEntry(ctx context.Context, userID, remoteID int64) (*blyg.BlogrollEntry, error) {
	var kind, identity string
	var title, display, siteURL *string
	var manifest []byte
	err := r.Pool.QueryRow(ctx, `SELECT ro.kind, ro.identity_url, ro.title, s.display_title, ro.site_url, ro.manifest
		FROM subscriptions s JOIN remote_origins ro ON ro.id = s.remote_origin_id
		WHERE s.user_id = $1 AND ro.id = $2`, userID, remoteID).Scan(&kind, &identity, &title, &display, &siteURL, &manifest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	e := &blyg.BlogrollEntry{Text: identity, XMLURL: blyg.SurfaceOf(identity, manifest).FeedURL(), HTMLURL: identity}
	if kind == "l0" {
		e.XMLURL, e.HTMLURL = identity, identity
		if siteURL != nil && *siteURL != "" {
			e.HTMLURL = *siteURL
		}
	}
	switch {
	case display != nil && *display != "":
		e.Text = *display
	case title != nil && *title != "":
		e.Text = *title
	}
	return e, nil
}

// Item is one imported item as the reading API shows it.
type Item struct {
	ID             int64           `json:"-"`
	SubscriptionID int64           `json:"subscription_id"`
	Origin         string          `json:"origin"`
	Source         string          `json:"source"` // the subscription's title as the user sees it
	ItemID         string          `json:"item_id"`
	Kind           string          `json:"kind"` // fragment, thread, withdrawn, or l0
	L0             bool            `json:"l0"`   // wrapped legacy RSS (§13.6): mark it visibly
	State          string          `json:"state"`
	Version        int             `json:"version"`
	Page           *string         `json:"page"`
	ClaimedUpdated *time.Time      `json:"claimed_updated"`
	FirstSeenAt    time.Time       `json:"first_seen_at"`
	SortAt         time.Time       `json:"sort_at"`
	Read           bool            `json:"read"`
	Discrepancy    *string         `json:"discrepancy"`
	Doc            json.RawMessage `json:"doc"` // verbatim; null once withdrawn (§13.4)
	Pins           []Pin           `json:"pins,omitempty"`
}

// Pin is a retained pinned version (§13.4), shown with attribution to the
// origin's own pin file.
type Pin struct {
	Version int             `json:"version"`
	URL     string          `json:"url"`
	Doc     json.RawMessage `json:"doc"`
}

const itemCols = `ii.id, ii.remote_origin_id, ro.identity_url, coalesce(s.display_title, ro.title, ro.identity_url),
	ii.item_id, ii.kind, ii.state, ii.version, ii.page_url, ii.claimed_updated, ii.first_seen_at, ii.sort_at,
	(rd.version IS NOT NULL AND rd.version >= ii.version), ii.discrepancy, ii.doc`

func scanItem(row pgx.Row) (*Item, error) {
	var it Item
	var doc []byte
	err := row.Scan(&it.ID, &it.SubscriptionID, &it.Origin, &it.Source, &it.ItemID, &it.Kind, &it.State, &it.Version,
		&it.Page, &it.ClaimedUpdated, &it.FirstSeenAt, &it.SortAt, &it.Read, &it.Discrepancy, &doc)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if doc != nil {
		it.Doc = doc
	}
	it.L0 = it.Kind == "l0"
	return &it, err
}

// FeedQuery selects a page of the merged reading feed.
type FeedQuery struct {
	Limit          int
	Before         string // cursor from a previous page
	SubscriptionID int64  // 0 for every subscription
	UnreadOnly     bool
}

// FeedPage is one page of the reading feed.
type FeedPage struct {
	Items []*Item
	Next  string // cursor for the next page; empty at the end
	// AsOf is when the feed was read. MarkAllRead takes it back, so "mark
	// all read" covers what the feed could have shown and nothing newer.
	AsOf time.Time
}

// Feed is the merged reverse-chronological reading surface across a
// user's subscriptions, ordered by min(claimed updated, when we saw that
// version) so a skewed or future-dated origin cannot sit on top (§13.7).
// Withdrawn items are not live and are not shown (§13.4).
func (r *Reader) Feed(ctx context.Context, userID int64, q FeedQuery) (FeedPage, error) {
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 50
	}
	asOf := r.now()
	var beforeT *time.Time
	var beforeID int64
	if q.Before != "" {
		t, id, err := parseCursor(q.Before)
		if err != nil {
			return FeedPage{}, err
		}
		beforeT, beforeID = &t, id
	}
	rows, err := r.Pool.Query(ctx, `SELECT `+itemCols+`
		FROM subscriptions s
		JOIN remote_origins ro ON ro.id = s.remote_origin_id
		JOIN imported_items ii ON ii.remote_origin_id = s.remote_origin_id
		LEFT JOIN reads rd ON rd.user_id = s.user_id AND rd.imported_item_id = ii.id
		WHERE s.user_id = $1 AND ii.state = 'current'
		  AND ($2::bigint = 0 OR s.remote_origin_id = $2)
		  AND ($3::timestamptz IS NULL OR (ii.sort_at, ii.id) < ($3, $4))
		  AND (NOT $5 OR rd.version IS NULL OR rd.version < ii.version)
		ORDER BY ii.sort_at DESC, ii.id DESC LIMIT $6`,
		userID, q.SubscriptionID, beforeT, beforeID, q.UnreadOnly, q.Limit)
	if err != nil {
		return FeedPage{}, err
	}
	defer rows.Close()
	page := FeedPage{Items: []*Item{}, AsOf: asOf}
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return FeedPage{}, err
		}
		page.Items = append(page.Items, it)
	}
	if err := rows.Err(); err != nil {
		return FeedPage{}, err
	}
	if len(page.Items) == q.Limit {
		last := page.Items[len(page.Items)-1]
		page.Next = strconv.FormatInt(last.SortAt.UnixMicro(), 10) + "." + strconv.FormatInt(last.ID, 10)
	}
	return page, nil
}

func parseCursor(c string) (time.Time, int64, error) {
	ts, id, ok := strings.Cut(c, ".")
	us, err1 := strconv.ParseInt(ts, 10, 64)
	n, err2 := strconv.ParseInt(id, 10, 64)
	if !ok || err1 != nil || err2 != nil {
		return time.Time{}, 0, apperr.New(apperr.BadRequest, "bad cursor")
	}
	return time.UnixMicro(us).UTC(), n, nil
}

// GetItem returns one imported item from a subscription, with any retained
// pins. A withdrawn item carries no content, only its pins (§13.4).
func (r *Reader) GetItem(ctx context.Context, userID, remoteID int64, itemID string) (*Item, error) {
	it, err := scanItem(r.Pool.QueryRow(ctx, `SELECT `+itemCols+`
		FROM subscriptions s
		JOIN remote_origins ro ON ro.id = s.remote_origin_id
		JOIN imported_items ii ON ii.remote_origin_id = s.remote_origin_id
		LEFT JOIN reads rd ON rd.user_id = s.user_id AND rd.imported_item_id = ii.id
		WHERE s.user_id = $1 AND s.remote_origin_id = $2 AND ii.item_id = $3`, userID, remoteID, itemID))
	if err != nil {
		return nil, err
	}
	var manifest []byte
	if err := r.Pool.QueryRow(ctx, `SELECT manifest FROM remote_origins WHERE id = $1`, remoteID).Scan(&manifest); err != nil {
		return nil, err
	}
	surface := blyg.SurfaceOf(it.Origin, manifest)
	rows, err := r.Pool.Query(ctx, `SELECT version, doc FROM imported_pins WHERE remote_origin_id = $1 AND item_id = $2 ORDER BY version`,
		remoteID, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p Pin
		var doc []byte
		if err := rows.Scan(&p.Version, &doc); err != nil {
			return nil, err
		}
		p.Doc, p.URL = doc, surface.PinURL(itemID, p.Version)
		it.Pins = append(it.Pins, p)
	}
	return it, rows.Err()
}

// MarkRead marks an item read at its current version, or unread. An item
// read at v3 is unread again once v4 arrives.
func (r *Reader) MarkRead(ctx context.Context, userID, remoteID int64, itemID string, read bool) error {
	var tag interface{ RowsAffected() int64 }
	var err error
	if read {
		tag, err = r.Pool.Exec(ctx, `INSERT INTO reads (user_id, imported_item_id, version)
			SELECT s.user_id, ii.id, ii.version FROM subscriptions s
			JOIN imported_items ii ON ii.remote_origin_id = s.remote_origin_id
			WHERE s.user_id = $1 AND s.remote_origin_id = $2 AND ii.item_id = $3
			ON CONFLICT (user_id, imported_item_id) DO UPDATE SET version = EXCLUDED.version, read_at = now()`,
			userID, remoteID, itemID)
	} else {
		tag, err = r.Pool.Exec(ctx, `DELETE FROM reads rd USING imported_items ii, subscriptions s
			WHERE rd.imported_item_id = ii.id AND rd.user_id = $1 AND s.user_id = $1
			  AND s.remote_origin_id = ii.remote_origin_id AND ii.remote_origin_id = $2 AND ii.item_id = $3`,
			userID, remoteID, itemID)
		if err == nil {
			return nil // unmarking something already unread is not an error
		}
	}
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// MarkAllRead marks read, at its current version, every current item whose
// current version was first seen by asOf: what a feed read at asOf could
// have shown. A version that arrives later stays unread, even one dated
// earlier, which the feed sorts among items already read (§13.7). remoteID
// 0 means every subscription. It returns how many items it marked.
func (r *Reader) MarkAllRead(ctx context.Context, userID, remoteID int64, asOf time.Time) (int64, error) {
	if remoteID != 0 {
		var ok bool
		if err := r.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM subscriptions WHERE user_id = $1 AND remote_origin_id = $2)`,
			userID, remoteID).Scan(&ok); err != nil {
			return 0, err
		}
		if !ok {
			return 0, ErrNotFound
		}
	}
	tag, err := r.Pool.Exec(ctx, `INSERT INTO reads (user_id, imported_item_id, version)
		SELECT s.user_id, ii.id, ii.version FROM subscriptions s
		JOIN imported_items ii ON ii.remote_origin_id = s.remote_origin_id
		WHERE s.user_id = $1 AND ($2::bigint = 0 OR s.remote_origin_id = $2)
		  AND ii.state = 'current' AND ii.version_seen_at <= $3
		ON CONFLICT (user_id, imported_item_id) DO UPDATE SET version = EXCLUDED.version, read_at = now()
			WHERE reads.version < EXCLUDED.version`,
		userID, remoteID, asOf)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
