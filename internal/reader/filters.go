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
	ID             int64     `json:"id"`
	Name           string    `json:"name"`
	Description    string    `json:"description"`
	Match          string    `json:"match"`     // mentions or mainly
	Threshold      float64   `json:"threshold"` // a match is a probability at or above it
	Action         string    `json:"action"`    // fold or hide
	Pending        string    `json:"pending"`   // show or hold, before an answer arrives
	SubscriptionID *int64    `json:"subscription_id"`
	Enabled        bool      `json:"enabled"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
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
	// SetScope changes the subscription the filter applies to, to Scope,
	// or to every subscription when Scope is nil.
	SetScope bool
	Scope    *int64
}

const filterCols = `id, name, description, match, threshold, action, pending, remote_origin_id, enabled, created_at, updated_at`

func scanFilter(row pgx.Row) (*Filter, error) {
	var f Filter
	err := row.Scan(&f.ID, &f.Name, &f.Description, &f.Match, &f.Threshold, &f.Action, &f.Pending, &f.SubscriptionID,
		&f.Enabled, &f.CreatedAt, &f.UpdatedAt)
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

// Filters lists userID's filters, oldest first.
func (r *Reader) Filters(ctx context.Context, userID int64) ([]*Filter, error) {
	rows, err := r.Pool.Query(ctx, `SELECT `+filterCols+` FROM filters WHERE user_id = $1 ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*Filter, error) { return scanFilter(row) })
}

// GetFilter returns one of userID's filters.
func (r *Reader) GetFilter(ctx context.Context, userID, id int64) (*Filter, error) {
	return scanFilter(r.Pool.QueryRow(ctx, `SELECT `+filterCols+` FROM filters WHERE user_id = $1 AND id = $2`, userID, id))
}

// CreateFilter adds a filter. Its items are classified in the background,
// newest first, back to LabelWindow.
func (r *Reader) CreateFilter(ctx context.Context, userID int64, in FilterInput) (*Filter, error) {
	f := &Filter{Match: "mainly", Threshold: 0.5, Action: "fold", Pending: "show", Enabled: true}
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
		(user_id, name, description, def_hash, match, threshold, action, pending, remote_origin_id, enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING `+filterCols,
		userID, f.Name, f.Description, hash, f.Match, f.Threshold, f.Action, f.Pending, f.SubscriptionID, f.Enabled))
	return out, filterErr(err)
}

// UpdateFilter changes the members in is sets. New words are a new
// question, asked again of every item; a new threshold, match or action
// applies at once to the answers already held.
func (r *Reader) UpdateFilter(ctx context.Context, userID, id int64, in FilterInput) (*Filter, error) {
	f, err := r.GetFilter(ctx, userID, id)
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
	out, err := scanFilter(r.Pool.QueryRow(ctx, `UPDATE filters SET name = $3, description = $4, def_hash = $5, match = $6,
		threshold = $7, action = $8, pending = $9, remote_origin_id = $10, enabled = $11, updated_at = now()
		WHERE user_id = $1 AND id = $2 RETURNING `+filterCols,
		userID, id, f.Name, f.Description, hash, f.Match, f.Threshold, f.Action, f.Pending, f.SubscriptionID, f.Enabled))
	return out, filterErr(err)
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

// SetOverride records the reader's own judgment of an item under one of
// their filters, which wins over the classifier's; nil forgets it.
func (r *Reader) SetOverride(ctx context.Context, userID, remoteID int64, itemID string, filterID int64, matched *bool) error {
	if _, err := r.GetFilter(ctx, userID, filterID); err != nil {
		return err
	}
	var iid int64
	err := r.Pool.QueryRow(ctx, `SELECT ii.id FROM subscriptions s
		JOIN imported_items ii ON ii.remote_origin_id = s.remote_origin_id
		WHERE s.user_id = $1 AND s.remote_origin_id = $2 AND ii.item_id = $3`, userID, remoteID, itemID).Scan(&iid)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if matched == nil {
		_, err = r.Pool.Exec(ctx, `DELETE FROM label_overrides WHERE filter_id = $1 AND imported_item_id = $2`, filterID, iid)
		return err
	}
	_, err = r.Pool.Exec(ctx, `INSERT INTO label_overrides (filter_id, imported_item_id, matched) VALUES ($1, $2, $3)
		ON CONFLICT (filter_id, imported_item_id) DO UPDATE SET matched = EXCLUDED.matched, created_at = now()`,
		filterID, iid, *matched)
	return err
}
