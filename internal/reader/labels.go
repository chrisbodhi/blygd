package reader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/chrisbodhi/blygd/internal/batch"
	"github.com/chrisbodhi/blygd/internal/blyg"
	"github.com/chrisbodhi/blygd/internal/classify"
	"github.com/chrisbodhi/blygd/internal/events"
	"github.com/chrisbodhi/blygd/internal/tx"
)

// The labelling worker asks the classifier about imported items that a
// reader's filters cover and that hold no answer for their current text.
// It leases the poller's way: one statement claims a batch, the request
// runs outside any transaction, and a short transaction stores what came
// back. Nothing here is on the wire (protocol invariant 3), and a reader
// with no classifier configured has filters that do nothing.

// Classifier judges a post against topics. *classify.Client is one.
type Classifier interface {
	Classify(ctx context.Context, in classify.Input, topics []classify.Topic) (*classify.Result, error)
	ModelID() string
}

const (
	labelLease    = 5 * time.Minute
	labelClaimMax = time.Minute // one item: up to a request per 128 questions, each under 30 s
	labelAttempts = 4           // after this many failures a version is shown unclassified
	labelHalt     = 10 * time.Minute
)

// labelState is what the operator should know about the classifier. It is
// per process: a halt is learned again by each.
type labelState struct {
	mu          sync.Mutex
	haltedUntil time.Time
	lastError   string
	lastErrorAt time.Time
}

// ClassifierStatus says whether filtering runs, and why not.
type ClassifierStatus struct {
	Enabled     bool       `json:"enabled"`
	Model       *string    `json:"model"`
	HaltedUntil *time.Time `json:"halted_until"` // no requests until then: a key, credit or model problem
	LastError   *string    `json:"last_error"`
	LastErrorAt *time.Time `json:"last_error_at"`
	CallsToday  int        `json:"calls_today"`
	DailyCalls  int        `json:"daily_calls"` // the ceiling (BLYGD_SUBQ_DAILY_CALLS)
}

func (r *Reader) model() string {
	if r.Classifier == nil {
		return ""
	}
	return r.Classifier.ModelID()
}

func (r *Reader) labelWindow() time.Duration {
	if r.LabelWindow > 0 {
		return r.LabelWindow
	}
	return 14 * 24 * time.Hour
}

func (r *Reader) holdFor() time.Duration {
	if r.HoldFor > 0 {
		return r.HoldFor
	}
	return 10 * time.Minute
}

func (r *Reader) dailyCalls() int {
	if r.DailyCalls > 0 {
		return r.DailyCalls
	}
	return 20000
}

func (r *Reader) noteError(msg string, haltFor time.Duration) {
	r.labels.mu.Lock()
	defer r.labels.mu.Unlock()
	now := r.now()
	r.labels.lastError, r.labels.lastErrorAt = msg, now
	if haltFor > 0 {
		r.labels.haltedUntil = now.Add(haltFor)
	}
}

func (r *Reader) halted() bool {
	r.labels.mu.Lock()
	defer r.labels.mu.Unlock()
	return r.now().Before(r.labels.haltedUntil)
}

// ClassifierStatus reports the classifier's state to readers, who see
// whether their filters are running.
func (r *Reader) ClassifierStatus(ctx context.Context) (ClassifierStatus, error) {
	st := ClassifierStatus{Enabled: r.Classifier != nil, DailyCalls: r.dailyCalls()}
	if st.Enabled {
		m := r.model()
		st.Model = &m
	}
	if err := r.Pool.QueryRow(ctx, `SELECT coalesce((SELECT calls FROM classifier_usage WHERE day = ($1::timestamptz AT TIME ZONE 'UTC')::date), 0)`,
		r.now()).Scan(&st.CallsToday); err != nil {
		return st, err
	}
	r.labels.mu.Lock()
	defer r.labels.mu.Unlock()
	if r.now().Before(r.labels.haltedUntil) {
		t := r.labels.haltedUntil
		st.HaltedUntil = &t
	}
	if r.labels.lastError != "" {
		e, t := r.labels.lastError, r.labels.lastErrorAt
		st.LastError, st.LastErrorAt = &e, &t
	}
	return st, nil
}

// RunLabels classifies until ctx ends. Without a classifier it returns at
// once: filters then fold and hide nothing.
func (r *Reader) RunLabels(ctx context.Context) {
	if r.Classifier == nil {
		return
	}
	idle := r.Idle
	if idle <= 0 {
		idle = 10 * time.Second
	}
	batch.Loop(ctx, idle, r.LabelBatch, r.Log.With("worker", "label"))
}

type leasedLabel struct {
	id       int64
	attempts int
}

// errHalted ends a batch: no request will succeed until the operator acts.
var errHalted = errors.New("classifier halted")

// LabelBatch classifies up to one batch of due items, newest first.
func (r *Reader) LabelBatch(ctx context.Context) (int, error) {
	if r.Classifier == nil || r.halted() {
		return 0, nil
	}
	now := r.now()
	var calls int
	if err := r.Pool.QueryRow(ctx, `SELECT coalesce((SELECT calls FROM classifier_usage WHERE day = ($1::timestamptz AT TIME ZONE 'UTC')::date), 0)`,
		now).Scan(&calls); err != nil {
		return 0, err
	}
	if calls >= r.dailyCalls() {
		r.noteError(fmt.Sprintf("daily ceiling of %d requests reached (BLYGD_SUBQ_DAILY_CALLS); items wait until tomorrow, UTC", r.dailyCalls()), 0)
		return 0, nil
	}
	n := r.Batch
	if n <= 0 {
		n = 10
	}
	until := now.Add(labelLease)
	// Due: a current item, recent enough, under some reader's enabled
	// filter that holds no answer for its current text, and not already
	// claimed, waiting to retry, or failed at this version.
	rows, err := r.Pool.Query(ctx, `WITH due AS (
			SELECT ii.id, ii.version, ii.content_hash FROM imported_items ii
			LEFT JOIN label_leases ll ON ll.imported_item_id = ii.id
			WHERE ii.state = 'current' AND ii.sort_at > @now::timestamptz - make_interval(secs => @window)
			  AND (ll.imported_item_id IS NULL OR ll.version <> ii.version OR ll.content_hash IS DISTINCT FROM ii.content_hash
			       OR (NOT ll.failed AND ll.next_attempt_at <= @now))
			  AND EXISTS (SELECT 1 FROM subscriptions s
				JOIN filters f ON f.user_id = s.user_id AND f.enabled
				  AND (f.remote_origin_id IS NULL OR f.remote_origin_id = s.remote_origin_id)
				WHERE s.remote_origin_id = ii.remote_origin_id
				  AND NOT EXISTS (SELECT 1 FROM item_labels l WHERE l.imported_item_id = ii.id AND l.def_hash = f.def_hash
					AND l.model = @model AND l.version = ii.version AND l.content_hash IS NOT DISTINCT FROM ii.content_hash))
			ORDER BY ii.sort_at DESC, ii.id DESC LIMIT @n)
		INSERT INTO label_leases (imported_item_id, version, content_hash, attempts, next_attempt_at, updated_at)
		SELECT id, version, content_hash, 1, @until, @now FROM due
		ON CONFLICT (imported_item_id) DO UPDATE SET
			attempts = CASE WHEN label_leases.version = EXCLUDED.version
				AND label_leases.content_hash IS NOT DISTINCT FROM EXCLUDED.content_hash THEN label_leases.attempts + 1 ELSE 1 END,
			version = EXCLUDED.version, content_hash = EXCLUDED.content_hash, failed = false,
			next_attempt_at = EXCLUDED.next_attempt_at, updated_at = EXCLUDED.updated_at
		WHERE label_leases.version <> EXCLUDED.version OR label_leases.content_hash IS DISTINCT FROM EXCLUDED.content_hash
		   OR (NOT label_leases.failed AND label_leases.next_attempt_at <= @now)
		RETURNING imported_item_id, attempts`,
		pgx.NamedArgs{"now": now, "until": until, "window": r.labelWindow().Seconds(), "model": r.model(), "n": n})
	if err != nil {
		return 0, err
	}
	leased, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (leasedLabel, error) {
		var l leasedLabel
		err := row.Scan(&l.id, &l.attempts)
		return l, err
	})
	if err != nil || len(leased) == 0 {
		return 0, err
	}
	rest, err := batch.Each(ctx, batch.Lease{Until: until, ClaimMax: labelClaimMax, Now: r.now}, leased, r.labelOne)
	if len(rest) > 0 {
		// Not started (a halt, a shutdown, the lease running out): handed
		// back with the attempt refunded.
		ids := make([]int64, len(rest))
		for i, l := range rest {
			ids[i] = l.id
		}
		wait := now
		if errors.Is(err, errHalted) {
			wait = now.Add(labelHalt)
		}
		if _, rerr := r.Pool.Exec(context.WithoutCancel(ctx), `UPDATE label_leases SET attempts = greatest(attempts - 1, 0),
			next_attempt_at = $2 WHERE imported_item_id = ANY($1)`, ids, wait); rerr != nil {
			return len(leased), errors.Join(err, rerr)
		}
	}
	if errors.Is(err, errHalted) {
		err = nil // said in ClassifierStatus; not a batch failure to log every ten seconds
	}
	return len(leased), err
}

type labelTopic struct {
	hash  string
	topic classify.Topic
}

type heldLabel struct {
	version     int
	contentHash *string
	inputHash   string
}

// labelOne classifies one leased item for every question its readers ask
// that has no answer for its current text.
func (r *Reader) labelOne(ctx context.Context, l leasedLabel) error {
	var remoteID int64
	var kind, state string
	var version int
	var contentHash *string
	var doc []byte
	err := r.Pool.QueryRow(ctx, `SELECT remote_origin_id, kind, state, version, content_hash, doc FROM imported_items WHERE id = $1`, l.id).
		Scan(&remoteID, &kind, &state, &version, &contentHash, &doc)
	if err != nil {
		return err
	}
	if state != "current" || doc == nil {
		_, err := r.Pool.Exec(ctx, `DELETE FROM label_leases WHERE imported_item_id = $1`, l.id)
		return err
	}
	model := r.model()
	rows, err := r.Pool.Query(ctx, `SELECT DISTINCT ON (f.def_hash) f.def_hash, f.name, f.description
		FROM subscriptions s JOIN filters f ON f.user_id = s.user_id AND f.enabled
		  AND (f.remote_origin_id IS NULL OR f.remote_origin_id = s.remote_origin_id)
		WHERE s.remote_origin_id = $1 ORDER BY f.def_hash, f.id`, remoteID)
	if err != nil {
		return err
	}
	topics, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (labelTopic, error) {
		var t labelTopic
		err := row.Scan(&t.hash, &t.topic.Name, &t.topic.Description)
		return t, err
	})
	if err != nil {
		return err
	}
	rows, err = r.Pool.Query(ctx, `SELECT def_hash, version, content_hash, input_hash FROM item_labels
		WHERE imported_item_id = $1 AND model = $2`, l.id, model)
	if err != nil {
		return err
	}
	held := map[string]heldLabel{}
	for rows.Next() {
		var h string
		var hl heldLabel
		if err := rows.Scan(&h, &hl.version, &hl.contentHash, &hl.inputHash); err != nil {
			rows.Close()
			return err
		}
		held[h] = hl
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	in := labelInput(kind, doc)
	inputHash := in.Hash()
	var reuse []string // answers about the same words, at an older version (§13.2)
	var ask []labelTopic
	for _, t := range topics {
		h, ok := held[t.hash]
		switch {
		case ok && h.version == version && eqHash(h.contentHash, contentHash):
		case ok && h.inputHash == inputHash:
			reuse = append(reuse, t.hash)
		default:
			ask = append(ask, t)
		}
	}

	answers := map[string]classify.Answer{}
	for start := 0; start < len(ask); start += classify.MaxTopics {
		chunk := ask[start:min(start+classify.MaxTopics, len(ask))]
		ts := make([]classify.Topic, len(chunk))
		for i, t := range chunk {
			ts[i] = t.topic
		}
		res, err := r.Classifier.Classify(ctx, in, ts)
		tokens := 0
		if res != nil {
			tokens = res.InputTokens
		}
		if uerr := r.countCall(ctx, tokens); uerr != nil {
			return uerr
		}
		if err != nil {
			return r.labelFailed(ctx, l, err)
		}
		for h, a := range res.Answers {
			answers[h] = a
		}
	}

	return tx.Run(ctx, r.Pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		// Stored only if the item is still at the text that was judged.
		var still bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM imported_items WHERE id = $1 AND state = 'current'
			AND version = $2 AND content_hash IS NOT DISTINCT FROM $3)`, l.id, version, contentHash).Scan(&still); err != nil {
			return err
		}
		if !still {
			_, err := tx.Exec(ctx, `UPDATE label_leases SET next_attempt_at = $2 WHERE imported_item_id = $1`, l.id, r.now())
			return err
		}
		if len(reuse) > 0 {
			if _, err := tx.Exec(ctx, `UPDATE item_labels SET version = $3, content_hash = $4
				WHERE imported_item_id = $1 AND model = $2 AND def_hash = ANY($5) AND input_hash = $6`,
				l.id, model, version, contentHash, reuse, inputHash); err != nil {
				return err
			}
		}
		for h, a := range answers {
			probs, _ := json.Marshal(a.Probabilities)
			if _, err := tx.Exec(ctx, `INSERT INTO item_labels (imported_item_id, def_hash, model, version, content_hash, input_hash,
					p_mentions, p_mainly, score, confidence, probabilities, classified_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
				ON CONFLICT (imported_item_id, def_hash, model) DO UPDATE SET version = EXCLUDED.version,
					content_hash = EXCLUDED.content_hash, input_hash = EXCLUDED.input_hash, p_mentions = EXCLUDED.p_mentions,
					p_mainly = EXCLUDED.p_mainly, score = EXCLUDED.score, confidence = EXCLUDED.confidence,
					probabilities = EXCLUDED.probabilities, classified_at = EXCLUDED.classified_at`,
				l.id, h, model, version, contentHash, inputHash, clamp01(a.Mentions()), clamp01(a.Mainly()), a.Score, a.Confidence,
				string(probs), r.now()); err != nil {
				return err
			}
		}
		// Done: the lease goes, so a filter added later finds the item due.
		if _, err := tx.Exec(ctx, `DELETE FROM label_leases WHERE imported_item_id = $1`, l.id); err != nil {
			return err
		}
		var itemID string
		if err := tx.QueryRow(ctx, `SELECT item_id FROM imported_items WHERE id = $1`, l.id).Scan(&itemID); err != nil {
			return err
		}
		// Heard on commit by the origin's subscribers: a held or folded
		// item may now show differently.
		return events.Notify(ctx, tx, "label", 0, remoteID, map[string]any{"subscription_id": remoteID, "item_id": itemID})
	})
}

// labelFailed records a failed request by what it means (classify.Failure).
func (r *Reader) labelFailed(ctx context.Context, l leasedLabel, err error) error {
	var ce *classify.Error
	if !errors.As(err, &ce) {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err() // shutting down: batch.Each hands it back unspent
	}
	now := r.now()
	msg := ce.Error()
	switch ce.Failure {
	case classify.Halt:
		r.noteError(msg, labelHalt)
		if _, err := r.Pool.Exec(ctx, `UPDATE label_leases SET attempts = greatest(attempts - 1, 0), next_attempt_at = $2,
			error = $3, updated_at = $4 WHERE imported_item_id = $1`, l.id, now.Add(labelHalt), msg, now); err != nil {
			return err
		}
		r.Log.Error("classifier halted", "err", msg, "for", labelHalt)
		return errHalted
	case classify.Skip:
		_, err := r.Pool.Exec(ctx, `UPDATE label_leases SET failed = true, error = $2, updated_at = $3 WHERE imported_item_id = $1`,
			l.id, msg, now)
		return err
	}
	r.noteError(msg, 0)
	if l.attempts >= labelAttempts {
		_, err := r.Pool.Exec(ctx, `UPDATE label_leases SET failed = true, error = $2, updated_at = $3 WHERE imported_item_id = $1`,
			l.id, msg, now)
		return err
	}
	// 5 minutes, then 20, then 80, or longer if the service says so;
	// failure never tightens the pace (§12.3's etiquette, kept here too).
	wait := time.Duration(5<<(2*(l.attempts-1))) * time.Minute
	wait = max(wait, ce.RetryAfter)
	_, err = r.Pool.Exec(ctx, `UPDATE label_leases SET next_attempt_at = $2, error = $3, updated_at = $4 WHERE imported_item_id = $1`,
		l.id, now.Add(wait), msg, now)
	return err
}

// countCall adds one request to today's usage.
func (r *Reader) countCall(ctx context.Context, tokens int) error {
	_, err := r.Pool.Exec(context.WithoutCancel(ctx), `INSERT INTO classifier_usage (day, calls, input_tokens)
		VALUES (($1::timestamptz AT TIME ZONE 'UTC')::date, 1, $2)
		ON CONFLICT (day) DO UPDATE SET calls = classifier_usage.calls + 1, input_tokens = classifier_usage.input_tokens + EXCLUDED.input_tokens`,
		r.now(), tokens)
	return err
}

// labelInput is what the classifier sees of an item: its text, never its
// author. Imported HTML is only read for its text here; nothing renders it.
func labelInput(kind string, doc []byte) classify.Input {
	if kind == "l0" {
		var d l0Doc
		_ = json.Unmarshal(doc, &d)
		return classify.Input{Title: d.Title, Text: blyg.PlainText(d.SummaryHTML)}.Clip()
	}
	var d struct {
		ContentMD   string `json:"content_md"`
		ContentHTML string `json:"content_html"`
	}
	_ = json.Unmarshal(doc, &d)
	// The rendered text includes baked quotes (§10.2), where a spoiler
	// can hide as well as anywhere.
	text := blyg.PlainText(d.ContentHTML)
	if text == "" {
		text = d.ContentMD
	}
	return classify.Input{Text: text}.Clip()
}

func eqHash(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func clamp01(p float64) float64 { return min(max(p, 0), 1) }
