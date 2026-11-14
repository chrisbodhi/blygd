package reader

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/chrisbodhi/blygd/internal/apperr"
	"github.com/chrisbodhi/blygd/internal/classify"
)

// Filters are a reader's topics, in their own words, and what happens to
// posts about them (DESIGN.md "Filters"). They are plane 3: private, per
// user, never on the wire (protocol invariant 3). A classifier judges each
// imported item against them (labels.go); the feed folds or hides what
// matches, and says why.

// MaxFilters caps one user's filters. Every filter on an origin is one
// question in the request that classifies its items, and a request asks
// at most classify.MaxTopics.
const MaxFilters = 32

// Filter is one of a reader's topics.
type Filter struct {
	ID             int64   `json:"id"`
	Name           string  `json:"name"`
	Description    string  `json:"description"`
	Match          string  `json:"match"`     // mentions or mainly
	Threshold      float64 `json:"threshold"` // a match is a probability at or above it
	Action         string  `json:"action"`    // fold or hide
	Pending        string  `json:"pending"`   // show or hold, before an answer arrives
	SubscriptionID *int64  `json:"subscription_id"`
	Enabled        bool    `json:"enabled"`
	// AutoTune lets each of the reader's ratings move Threshold to the one
	// that agrees with the most ratings (tune.go). Setting a threshold by
	// hand turns it off.
	AutoTune  bool       `json:"auto_tune"`
	TunedAt   *time.Time `json:"tuned_at"` // when ratings last moved the threshold
	Ratings   Ratings    `json:"ratings"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// FilterInput creates a filter, or changes the members that are set.
type FilterInput struct {
	Name        *string
	Description *string
	Match       *string
	Threshold   *float64
	Action      *string
	Pending     *string
	Enabled     *bool
	AutoTune    *bool
	// SetScope changes the subscription the filter applies to, to Scope,
	// or to every subscription when Scope is nil.
	SetScope bool
	Scope    *int64
}

const filterCols = `id, name, description, match, threshold, action, pending, remote_origin_id, enabled,
	auto_tune, tuned_at, created_at, updated_at`

func scanFilter(row pgx.Row) (*Filter, error) {
	var f Filter
	err := row.Scan(&f.ID, &f.Name, &f.Description, &f.Match, &f.Threshold, &f.Action, &f.Pending, &f.SubscriptionID,
		&f.Enabled, &f.AutoTune, &f.TunedAt, &f.CreatedAt, &f.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &f, err
}

// bareSpoilers is a topic too vague to judge: spoilers for what?
var bareSpoilers = regexp.MustCompile(`^(plot )?spoilers?$`)

// validate checks a filter as it would be stored.
func validateFilter(f *Filter) error {
	t := classify.Topic{Name: f.Name, Description: f.Description}.Normalize()
	f.Name, f.Description = t.Name, t.Description
	switch {
	case f.Name == "":
		return apperr.New(apperr.BadRequest, "name is required: the topic, in your words")
	case len([]rune(f.Name)) > 80:
		return apperr.New(apperr.Invalid, "name is at most 80 characters; say more in description")
	case len([]rune(f.Description)) > 500:
		return apperr.New(apperr.Invalid, "description is at most 500 characters")
	case bareSpoilers.MatchString(strings.ToLower(f.Name)) && f.Description == "":
		return apperr.New(apperr.Invalid, `say what the spoilers are for, as in "spoilers for Severance season 3"`)
	case f.Match != "mentions" && f.Match != "mainly":
		return apperr.New(apperr.BadRequest, "match is mentions or mainly")
	case !(f.Threshold > 0 && f.Threshold < 1):
		return apperr.New(apperr.BadRequest, "threshold is between 0 and 1")
	case f.Action != "fold" && f.Action != "hide":
		return apperr.New(apperr.BadRequest, "action is fold or hide")
	case f.Pending != "show" && f.Pending != "hold":
		return apperr.New(apperr.BadRequest, "pending is show or hold")
	}
	return nil
}

func (in FilterInput) apply(f *Filter) {
	if in.Name != nil {
		f.Name = *in.Name
	}
	if in.Description != nil {
		f.Description = *in.Description
	}
	if in.Match != nil {
		f.Match = *in.Match
	}
	if in.Threshold != nil {
		f.Threshold = *in.Threshold
	}
	if in.Action != nil {
		f.Action = *in.Action
	}
	if in.Pending != nil {
		f.Pending = *in.Pending
	}
	if in.Enabled != nil {
		f.Enabled = *in.Enabled
	}
	if in.AutoTune != nil {
		f.AutoTune = *in.AutoTune
	} else if in.Threshold != nil {
		f.AutoTune = false // a threshold set by hand is the reader's, until they say otherwise
	}
	if in.SetScope {
		f.SubscriptionID = in.Scope
	}
}

func (r *Reader) checkScope(ctx context.Context, userID int64, scope *int64) error {
	if scope != nil && !r.Subscribed(ctx, userID, *scope) {
		return apperr.New(apperr.Invalid, "subscription_id is not one of your subscriptions")
	}
	return nil
}

func filterErr(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return apperr.New(apperr.Conflict, "you already have this filter, in these words, for this subscription")
	}
	return err
}

// Filters lists userID's filters, oldest first, each with its ratings.
func (r *Reader) Filters(ctx context.Context, userID int64) ([]*Filter, error) {
	rows, err := r.Pool.Query(ctx, `SELECT `+filterCols+` FROM filters WHERE user_id = $1 ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	fs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*Filter, error) { return scanFilter(row) })
	if err != nil {
		return nil, err
	}
	return fs, r.withRatings(ctx, userID, fs...)
}

// GetFilter returns one of userID's filters, with its ratings.
func (r *Reader) GetFilter(ctx context.Context, userID, id int64) (*Filter, error) {
	f, err := r.getFilter(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	return f, r.withRatings(ctx, userID, f)
}

func (r *Reader) getFilter(ctx context.Context, userID, id int64) (*Filter, error) {
	return scanFilter(r.Pool.QueryRow(ctx, `SELECT `+filterCols+` FROM filters WHERE user_id = $1 AND id = $2`, userID, id))
}

// withRatings fills in each filter's ratings: the reader's judgments of
// answers to its current question from the current model. Ratings of
// other words, another model, or no answer at all tune nothing.
func (r *Reader) withRatings(ctx context.Context, userID int64, fs ...*Filter) error {
	if len(fs) == 0 {
		return nil
	}
	rows, err := r.Pool.Query(ctx, `SELECT o.filter_id, o.matched, o.p_mentions, o.p_mainly
		FROM label_overrides o JOIN filters f ON f.id = o.filter_id
		WHERE f.user_id = $1 AND o.def_hash = f.def_hash AND o.model = $2
		  AND o.p_mentions IS NOT NULL AND o.p_mainly IS NOT NULL`, userID, r.model())
	if err != nil {
		return err
	}
	by := map[int64][]rating{}
	for rows.Next() {
		var id int64
		var rt rating
		if err := rows.Scan(&id, &rt.yes, &rt.pMentions, &rt.pMainly); err != nil {
			rows.Close()
			return err
		}
		by[id] = append(by[id], rt)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, f := range fs {
		f.Ratings = tune(by[f.ID], f.Match, f.Threshold)
	}
	return nil
}

// retune moves a self-tuning filter's threshold to the one its ratings
// suggest, and returns the filter as it then stands.
func (r *Reader) retune(ctx context.Context, userID, id int64) (*Filter, error) {
	f, err := r.GetFilter(ctx, userID, id)
	if err != nil || !f.AutoTune || f.Ratings.Suggested == nil {
		return f, err
	}
	if _, err := r.Pool.Exec(ctx, `UPDATE filters SET threshold = $3, tuned_at = now(), updated_at = now()
		WHERE user_id = $1 AND id = $2 AND auto_tune`, userID, id, *f.Ratings.Suggested); err != nil {
		return nil, err
	}
	return r.GetFilter(ctx, userID, id)
}

// CreateFilter adds a filter. Its items are classified in the background,
// newest first, back to LabelWindow.
func (r *Reader) CreateFilter(ctx context.Context, userID int64, in FilterInput) (*Filter, error) {
	f := &Filter{Match: "mainly", Threshold: 0.5, Action: "fold", Pending: "show", Enabled: true, AutoTune: true}
	in.apply(f)
	if err := validateFilter(f); err != nil {
		return nil, err
	}
	if err := r.checkScope(ctx, userID, f.SubscriptionID); err != nil {
		return nil, err
	}
	var n int
	if err := r.Pool.QueryRow(ctx, `SELECT count(*) FROM filters WHERE user_id = $1`, userID).Scan(&n); err != nil {
		return nil, err
	}
	if n >= MaxFilters {
		return nil, apperr.Errorf(apperr.Conflict, "you have %d filters, the most there may be", MaxFilters)
	}
	hash := classify.Topic{Name: f.Name, Description: f.Description}.Hash()
	out, err := scanFilter(r.Pool.QueryRow(ctx, `INSERT INTO filters
		(user_id, name, description, def_hash, match, threshold, action, pending, remote_origin_id, enabled, auto_tune)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING `+filterCols,
		userID, f.Name, f.Description, hash, f.Match, f.Threshold, f.Action, f.Pending, f.SubscriptionID, f.Enabled, f.AutoTune))
	return out, filterErr(err)
}

// UpdateFilter changes the members in sets. New words are a new question,
// asked again of every item, and the ratings of the old one stop counting;
// a new threshold, match or action applies at once to the answers held.
// A threshold set by hand turns tuning off unless in turns it on.
func (r *Reader) UpdateFilter(ctx context.Context, userID, id int64, in FilterInput) (*Filter, error) {
	f, err := r.getFilter(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	in.apply(f)
	if err := validateFilter(f); err != nil {
		return nil, err
	}
	if in.SetScope {
		if err := r.checkScope(ctx, userID, f.SubscriptionID); err != nil {
			return nil, err
		}
	}
	hash := classify.Topic{Name: f.Name, Description: f.Description}.Hash()
	if _, err := r.Pool.Exec(ctx, `UPDATE filters SET name = $3, description = $4, def_hash = $5, match = $6,
		threshold = $7, action = $8, pending = $9, remote_origin_id = $10, enabled = $11, auto_tune = $12, updated_at = now()
		WHERE user_id = $1 AND id = $2`,
		userID, id, f.Name, f.Description, hash, f.Match, f.Threshold, f.Action, f.Pending, f.SubscriptionID, f.Enabled, f.AutoTune); err != nil {
		return nil, filterErr(err)
	}
	return r.retune(ctx, userID, id)
}

// DeleteFilter removes a filter and the reader's judgments under it. The
// classifier's answers stay: they are about the items, and another reader
// may ask the same question.
func (r *Reader) DeleteFilter(ctx context.Context, userID, id int64) error {
	tag, err := r.Pool.Exec(ctx, `DELETE FROM filters WHERE user_id = $1 AND id = $2`, userID, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// SetOverride records the reader's rating of an item under one of their
// filters: their own judgment, which wins over the classifier's for every
// version of the item. It keeps what the classifier said of the item's
// current version, so the rating can tune the filter (tune.go), and
// returns the filter as the rating left it. nil forgets the rating.
func (r *Reader) SetOverride(ctx context.Context, userID, remoteID int64, itemID string, filterID int64, matched *bool) (*Filter, error) {
	if _, err := r.getFilter(ctx, userID, filterID); err != nil {
		return nil, err
	}
	var iid int64
	err := r.Pool.QueryRow(ctx, `SELECT ii.id FROM subscriptions s
		JOIN imported_items ii ON ii.remote_origin_id = s.remote_origin_id
		WHERE s.user_id = $1 AND s.remote_origin_id = $2 AND ii.item_id = $3`, userID, remoteID, itemID).Scan(&iid)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if matched == nil {
		_, err = r.Pool.Exec(ctx, `DELETE FROM label_overrides WHERE filter_id = $1 AND imported_item_id = $2`, filterID, iid)
	} else {
		// The answer rated is the one the reader saw: for the filter's
		// current question, the item's current version, and this model.
		_, err = r.Pool.Exec(ctx, `INSERT INTO label_overrides (filter_id, imported_item_id, matched, def_hash, model, p_mentions, p_mainly)
			SELECT f.id, ii.id, $3, f.def_hash, l.model, l.p_mentions, l.p_mainly
			FROM filters f JOIN imported_items ii ON ii.id = $2
			LEFT JOIN item_labels l ON l.imported_item_id = ii.id AND l.def_hash = f.def_hash AND l.model = $4
				AND l.version = ii.version AND l.content_hash IS NOT DISTINCT FROM ii.content_hash
			WHERE f.id = $1
			ON CONFLICT (filter_id, imported_item_id) DO UPDATE SET matched = EXCLUDED.matched, def_hash = EXCLUDED.def_hash,
				model = EXCLUDED.model, p_mentions = EXCLUDED.p_mentions, p_mainly = EXCLUDED.p_mainly, created_at = now()`,
			filterID, iid, *matched, r.model())
	}
	if err != nil {
		return nil, err
	}
	return r.retune(ctx, userID, filterID)
}
