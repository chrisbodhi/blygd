package reader

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chrisbodhi/blygd/internal/apperr"
	"github.com/chrisbodhi/blygd/internal/classify"
	"github.com/chrisbodhi/blygd/internal/testblyg"
)

// fakeClassifier says a post is mainly about a topic when its text names
// the topic, and not about it otherwise. fail, when set, is returned
// instead.
type fakeClassifier struct {
	mu     sync.Mutex
	calls  int
	topics [][]string // topic names asked, per call
	inputs []classify.Input
	fail   error
}

func (f *fakeClassifier) ModelID() string { return "fake-1" }

func (f *fakeClassifier) Classify(_ context.Context, in classify.Input, topics []classify.Topic) (*classify.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	var names []string
	for _, t := range topics {
		names = append(names, t.Name)
	}
	f.topics = append(f.topics, names)
	f.inputs = append(f.inputs, in)
	if f.fail != nil {
		return nil, f.fail
	}
	res := &classify.Result{Answers: map[string]classify.Answer{}, InputTokens: 10}
	for _, t := range topics {
		a := classify.Answer{Probabilities: [3]float64{1, 0, 0}, Confidence: 0.9}
		if strings.Contains(strings.ToLower(in.Text), strings.ToLower(t.Name)) {
			a.Probabilities = [3]float64{0.05, 0.15, 0.8}
		} else if strings.Contains(strings.ToLower(in.Text), "in passing: "+strings.ToLower(t.Name)) {
			a.Probabilities = [3]float64{0.3, 0.6, 0.1}
		}
		a.Score = a.Probabilities[1] + 2*a.Probabilities[2]
		res.Answers[t.Hash()] = a
	}
	return res, nil
}

func (f *fakeClassifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func filterRig(t *testing.T, items ...*testblyg.Item) (*rig, *fakeClassifier) {
	g := newRig(t)
	fc := &fakeClassifier{}
	g.r.Classifier = fc
	g.b.Items = items
	g.subscribe()
	g.poll()
	return g, fc
}

func str(s string) *string   { return &s }
func num(f float64) *float64 { return &f }
func boolp(b bool) *bool     { return &b }
func idp(n int64) *int64     { return &n }
func (g *rig) labelAll() {
	g.t.Helper()
	for range 10 {
		n, err := g.r.LabelBatch(context.Background())
		if err != nil {
			g.t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
	g.t.Fatal("labelling did not settle")
}

func (g *rig) mustFilter(in FilterInput) *Filter {
	g.t.Helper()
	f, err := g.r.CreateFilter(context.Background(), g.uid, in)
	if err != nil {
		g.t.Fatal(err)
	}
	return f
}

func byID(items []*Item) map[string]*Item {
	m := map[string]*Item{}
	for _, it := range items {
		m[it.ItemID] = it
	}
	return m
}

type labelView struct {
	FilterID  int64    `json:"filter_id"`
	Filter    string   `json:"filter"`
	Matched   *bool    `json:"matched"`
	PMentions *float64 `json:"p_mentions"`
	PMainly   *float64 `json:"p_mainly"`
	Override  *bool    `json:"override"`
}

func labels(t *testing.T, it *Item) []labelView {
	t.Helper()
	var ls []labelView
	if err := json.Unmarshal(it.Labels, &ls); err != nil {
		t.Fatalf("labels %s: %v", it.Labels, err)
	}
	return ls
}

// A filter folds or hides what matches, says which filter did it, and
// counts what it left out: nothing vanishes silently. The unfiltered feed
// shows everything, still saying what the filters make of it.
func TestFiltersFoldHideAndSay(t *testing.T) {
	now := time.Now().UTC()
	g, fc := filterRig(t,
		&testblyg.Item{ID: idA, Kind: "fragment", Version: 1, ContentMD: "The Senate passed the bill: US politics at its finest.", Updated: now.Add(-3 * time.Minute)},
		&testblyg.Item{ID: idB, Kind: "fragment", Version: 1, ContentMD: "Tomatoes, gardening and patience.", Updated: now.Add(-2 * time.Minute)},
		&testblyg.Item{ID: idC, Kind: "fragment", Version: 1, ContentMD: "A note about nothing.", Updated: now.Add(-1 * time.Minute)},
	)
	ctx := context.Background()
	politics := g.mustFilter(FilterInput{Name: str("US politics"), Action: str("hide")})
	gardening := g.mustFilter(FilterInput{Name: str("gardening")})

	// Before any answer: nothing is filtered, and each item says so.
	page, err := g.r.Feed(ctx, g.uid, FeedQuery{})
	if err != nil || len(page.Items) != 3 || page.Hidden != 0 {
		t.Fatalf("before labelling: %d items, hidden %d, %v", len(page.Items), page.Hidden, err)
	}
	if it := page.Items[0]; it.Classification != "pending" || it.Filtered != nil {
		t.Fatalf("before labelling: %s %+v", it.Classification, it.Filtered)
	}

	g.labelAll()
	if fc.count() != 3 {
		t.Fatalf("one request per item, every topic in it: %d requests", fc.count())
	}
	if len(fc.topics[0]) != 2 || fc.inputs[0].Text == "" || strings.Contains(fc.inputs[0].Text, "<p>") {
		t.Fatalf("asked %v of %+v", fc.topics[0], fc.inputs[0])
	}

	page, err = g.r.Feed(ctx, g.uid, FeedQuery{})
	if err != nil {
		t.Fatal(err)
	}
	m := byID(page.Items)
	if len(page.Items) != 2 || m[idA] != nil || page.Hidden != 1 || page.Held != 0 {
		t.Fatalf("politics not hidden and counted: %d items, hidden %d", len(page.Items), page.Hidden)
	}
	if f := m[idB].Filtered; f == nil || f.Action != "fold" || len(f.Filters) != 1 || f.Filters[0] != gardening.ID {
		t.Fatalf("gardening not folded by its filter: %+v", f)
	}
	if m[idC].Filtered != nil || m[idC].Classification != "done" {
		t.Fatalf("unrelated item: %+v %s", m[idC].Filtered, m[idC].Classification)
	}
	ls := labels(t, m[idB])
	if len(ls) != 2 || ls[1].Filter != "gardening" || ls[1].Matched == nil || !*ls[1].Matched || ls[1].PMainly == nil || *ls[1].PMainly != 0.8 {
		t.Fatalf("labels: %s", m[idB].Labels)
	}

	all, err := g.r.Feed(ctx, g.uid, FeedQuery{Unfiltered: true})
	if err != nil || len(all.Items) != 3 {
		t.Fatalf("unfiltered: %d %v", len(all.Items), err)
	}
	if f := byID(all.Items)[idA].Filtered; f == nil || f.Action != "hide" || f.Filters[0] != politics.ID {
		t.Fatalf("unfiltered view does not say what was hidden: %+v", f)
	}
	it, err := g.r.GetItem(ctx, g.uid, g.sid, idA)
	if err != nil || it.Filtered == nil || it.Filtered.Action != "hide" {
		t.Fatalf("item view: %+v %v", it, err)
	}

	// A threshold, a match or an action applies to the answers held: no
	// new request.
	if _, err := g.r.UpdateFilter(ctx, g.uid, gardening.ID, FilterInput{Threshold: num(0.9)}); err != nil {
		t.Fatal(err)
	}
	if page, _ = g.r.Feed(ctx, g.uid, FeedQuery{}); byID(page.Items)[idB].Filtered != nil {
		t.Fatal("a higher threshold did not unfold the item")
	}
	if _, err := g.r.UpdateFilter(ctx, g.uid, politics.ID, FilterInput{Action: str("fold")}); err != nil {
		t.Fatal(err)
	}
	if page, _ = g.r.Feed(ctx, g.uid, FeedQuery{}); len(page.Items) != 3 || page.Hidden != 0 {
		t.Fatal("fold instead of hide did not bring the item back")
	}
	g.labelAll()
	if fc.count() != 3 {
		t.Fatalf("threshold or action changes asked again: %d", fc.count())
	}

	// New words are a new question: asked again of every item.
	if _, err := g.r.UpdateFilter(ctx, g.uid, gardening.ID, FilterInput{Description: str("vegetable gardens")}); err != nil {
		t.Fatal(err)
	}
	g.labelAll()
	if fc.count() != 6 {
		t.Fatalf("new words not asked again: %d", fc.count())
	}
}

// An answer is about one version's words. An edit or a stealth edit
// (§13.3) waits for a new answer; a version with the same words reuses
// the old one (§13.2) without asking.
func TestAnswersFollowTheText(t *testing.T) {
	now := time.Now().UTC()
	a := &testblyg.Item{ID: idA, Kind: "fragment", Version: 1, ContentMD: "gardening notes", Updated: now}
	g, fc := filterRig(t, a)
	ctx := context.Background()
	g.mustFilter(FilterInput{Name: str("gardening")})
	g.labelAll()

	a.Version, a.ContentMD = 2, "nothing about plants"
	g.b.Publish()
	g.poll()
	if it, _ := g.r.GetItem(ctx, g.uid, g.sid, idA); it.Classification != "pending" || it.Filtered != nil {
		t.Fatalf("an answer about v1 applied to v2: %s %+v", it.Classification, it.Filtered)
	}
	g.labelAll()
	if it, _ := g.r.GetItem(ctx, g.uid, g.sid, idA); fc.count() != 2 || it.Classification != "done" || it.Filtered != nil {
		t.Fatalf("v2: %d requests, %s %+v", fc.count(), it.Classification, it.Filtered)
	}

	// The same words at a new version: reused, not asked.
	a.Version = 3
	g.b.Publish()
	g.poll()
	g.labelAll()
	if it, _ := g.r.GetItem(ctx, g.uid, g.sid, idA); fc.count() != 2 || it.Classification != "done" || it.Version != 3 {
		t.Fatalf("maintenance version: %d requests, %s v%d", fc.count(), it.Classification, it.Version)
	}

	// A stealth edit: same version, other words.
	a.ContentMD = "gardening again, quietly"
	g.b.Publish()
	g.reconcileNext()
	g.poll()
	if it, _ := g.r.GetItem(ctx, g.uid, g.sid, idA); it.Classification != "pending" {
		t.Fatalf("an answer applied across a stealth edit: %s", it.Classification)
	}
	g.labelAll()
	if it, _ := g.r.GetItem(ctx, g.uid, g.sid, idA); fc.count() != 3 || it.Filtered == nil {
		t.Fatalf("after the stealth edit: %d requests, %+v", fc.count(), it.Filtered)
	}
}

// §13.4: a withdrawn item keeps nothing derived from what it said.
func TestWithdrawalForgetsLabels(t *testing.T) {
	a := &testblyg.Item{ID: idA, Kind: "fragment", Version: 1, ContentMD: "gardening", Updated: time.Now().UTC()}
	g, _ := filterRig(t, a)
	ctx := context.Background()
	f := g.mustFilter(FilterInput{Name: str("gardening")})
	g.labelAll()
	if err := g.r.SetOverride(ctx, g.uid, g.sid, idA, f.ID, boolp(true)); err != nil {
		t.Fatal(err)
	}
	count := func(table string) int {
		var n int
		if err := g.r.Pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count("item_labels") != 1 {
		t.Fatal("no label to forget")
	}
	a.Kind, a.Version, a.ContentMD = "withdrawn", 2, ""
	g.b.Publish()
	g.poll()
	if n := count("item_labels"); n != 0 {
		t.Fatalf("withdrawn item kept %d labels", n)
	}
	if count("label_overrides") != 1 {
		t.Error("the reader's own judgment went with the item: it is theirs, like a read mark")
	}
	if it, _ := g.r.GetItem(ctx, g.uid, g.sid, idA); it.Classification != "none" || it.Filtered != nil {
		t.Fatalf("withdrawn: %s %+v", it.Classification, it.Filtered)
	}
}

// A filter that holds keeps a new post back until it is answered, for at
// most HoldFor: a spoiler shown and then folded has already spoiled.
func TestHoldUntilAnswered(t *testing.T) {
	g, _ := filterRig(t, &testblyg.Item{ID: idA, Kind: "fragment", Version: 1, ContentMD: "the twist: spoilers for Severance", Updated: time.Now().UTC()})
	ctx := context.Background()
	g.mustFilter(FilterInput{Name: str("spoilers"), Description: str("for Severance season 3"), Pending: str("hold")})
	page, _ := g.r.Feed(ctx, g.uid, FeedQuery{})
	if len(page.Items) != 0 || page.Held != 1 {
		t.Fatalf("not held: %d items, held %d", len(page.Items), page.Held)
	}
	if all, _ := g.r.Feed(ctx, g.uid, FeedQuery{Unfiltered: true}); len(all.Items) != 1 || all.Items[0].Filtered.Action != "hold" {
		t.Fatal("the unfiltered view does not show what is held")
	}
	later := time.Now().Add(g.r.holdFor() + time.Minute)
	g.r.Now = func() time.Time { return later }
	page, _ = g.r.Feed(ctx, g.uid, FeedQuery{})
	if len(page.Items) != 1 || page.Held != 0 || page.Items[0].Classification != "pending" {
		t.Fatalf("held past HoldFor: %d items, held %d", len(page.Items), page.Held)
	}
	g.r.Now = nil
	g.labelAll()
	page, _ = g.r.Feed(ctx, g.uid, FeedQuery{})
	if len(page.Items) != 1 || page.Items[0].Filtered == nil || page.Items[0].Filtered.Action != "fold" {
		t.Fatalf("answered: %+v", page.Items)
	}
}

// A reader's own judgment wins over the classifier's, either way.
func TestOverrideWins(t *testing.T) {
	g, _ := filterRig(t,
		&testblyg.Item{ID: idA, Kind: "fragment", Version: 1, ContentMD: "gardening", Updated: time.Now().UTC()},
		&testblyg.Item{ID: idB, Kind: "fragment", Version: 1, ContentMD: "compost, if you squint", Updated: time.Now().UTC()})
	ctx := context.Background()
	f := g.mustFilter(FilterInput{Name: str("gardening"), Action: str("hide")})
	g.labelAll()
	if err := g.r.SetOverride(ctx, g.uid, g.sid, idA, f.ID, boolp(false)); err != nil {
		t.Fatal(err)
	}
	if err := g.r.SetOverride(ctx, g.uid, g.sid, idB, f.ID, boolp(true)); err != nil {
		t.Fatal(err)
	}
	page, _ := g.r.Feed(ctx, g.uid, FeedQuery{})
	if len(page.Items) != 1 || page.Items[0].ItemID != idA || page.Hidden != 1 {
		t.Fatalf("overrides: %d items, hidden %d", len(page.Items), page.Hidden)
	}
	if ls := labels(t, page.Items[0]); ls[0].Override == nil || *ls[0].Override {
		t.Fatalf("override not shown: %s", page.Items[0].Labels)
	}
	if err := g.r.SetOverride(ctx, g.uid, g.sid, idA, f.ID, nil); err != nil {
		t.Fatal(err)
	}
	if page, _ = g.r.Feed(ctx, g.uid, FeedQuery{}); len(page.Items) != 0 || page.Hidden != 2 {
		t.Fatalf("forgotten override still applies: %d", len(page.Items))
	}
	if err := g.r.SetOverride(ctx, g.uid+1, g.sid, idA, f.ID, boolp(true)); !errors.Is(err, ErrNotFound) {
		t.Errorf("another user's filter: %v", err)
	}
}

// Without a classifier, filters do nothing: a reader needs no models or
// keys (protocol invariant 3), and nothing is held for an answer that
// will never come.
func TestNoClassifierNoFiltering(t *testing.T) {
	g, _ := filterRig(t, &testblyg.Item{ID: idA, Kind: "fragment", Version: 1, ContentMD: "gardening", Updated: time.Now().UTC()})
	ctx := context.Background()
	g.mustFilter(FilterInput{Name: str("gardening"), Action: str("hide"), Pending: str("hold")})
	g.labelAll()
	g.r.Classifier = nil
	page, _ := g.r.Feed(ctx, g.uid, FeedQuery{})
	if len(page.Items) != 1 || page.Items[0].Filtered != nil || page.Items[0].Classification != "none" || page.Hidden != 0 {
		t.Fatalf("filtered without a classifier: %+v", page.Items)
	}
	if n, err := g.r.LabelBatch(ctx); n != 0 || err != nil {
		t.Fatalf("labelled without a classifier: %d %v", n, err)
	}
}

// Readers who ask the same question in the same words share the answer,
// as they share the mirror.
func TestSameQuestionAskedOnce(t *testing.T) {
	g, fc := filterRig(t, &testblyg.Item{ID: idA, Kind: "fragment", Version: 1, ContentMD: "gardening", Updated: time.Now().UTC()})
	ctx := context.Background()
	g.mustFilter(FilterInput{Name: str("gardening")})
	var other int64
	if err := g.r.Pool.QueryRow(ctx, `INSERT INTO users (handle) VALUES ('other') RETURNING id`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if _, _, err := g.r.Subscribe(ctx, other, g.b.Origin()); err != nil {
		t.Fatal(err)
	}
	if _, err := g.r.CreateFilter(ctx, other, FilterInput{Name: str("  Gardening ")}); err != nil {
		t.Fatal(err)
	}
	g.labelAll()
	if fc.count() != 1 || len(fc.topics[0]) != 1 {
		t.Fatalf("asked %d times: %v", fc.count(), fc.topics)
	}
	page, _ := g.r.Feed(ctx, other, FeedQuery{})
	if len(page.Items) != 1 || page.Items[0].Filtered == nil {
		t.Fatal("the other reader's filter did not use the shared answer")
	}
}

// A filter scoped to one subscription covers only its items.
func TestFilterScope(t *testing.T) {
	g, _ := filterRig(t, &testblyg.Item{ID: idA, Kind: "fragment", Version: 1, ContentMD: "gardening", Updated: time.Now().UTC()})
	ctx := context.Background()
	if _, err := g.r.CreateFilter(ctx, g.uid, FilterInput{Name: str("gardening"), SetScope: true, Scope: idp(g.sid + 1000)}); !isKind(err, apperr.Invalid) {
		t.Fatalf("a scope that is not a subscription: %v", err)
	}
	f := g.mustFilter(FilterInput{Name: str("gardening"), SetScope: true, Scope: idp(g.sid)})
	g.labelAll()
	if page, _ := g.r.Feed(ctx, g.uid, FeedQuery{}); page.Items[0].Filtered == nil {
		t.Fatal("scoped filter did not apply to its subscription")
	}
	if _, err := g.r.CreateFilter(ctx, g.uid, FilterInput{Name: str("gardening"), SetScope: true, Scope: idp(g.sid)}); !isKind(err, apperr.Conflict) {
		t.Fatalf("duplicate filter: %v", err)
	}
	if f.SubscriptionID == nil || *f.SubscriptionID != g.sid {
		t.Fatalf("scope: %v", f.SubscriptionID)
	}
}

func isKind(err error, k apperr.Kind) bool {
	var ke apperr.Kinded
	return errors.As(err, &ke) && ke.ErrorKind() == k
}

func TestFilterValidation(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	for _, c := range []struct {
		in   FilterInput
		kind apperr.Kind
	}{
		{FilterInput{}, apperr.BadRequest},
		{FilterInput{Name: str("   ")}, apperr.BadRequest},
		{FilterInput{Name: str("Spoilers")}, apperr.Invalid},
		{FilterInput{Name: str(strings.Repeat("x", 81))}, apperr.Invalid},
		{FilterInput{Name: str("AI"), Threshold: num(1)}, apperr.BadRequest},
		{FilterInput{Name: str("AI"), Match: str("some")}, apperr.BadRequest},
		{FilterInput{Name: str("AI"), Action: str("delete")}, apperr.BadRequest},
		{FilterInput{Name: str("AI"), Pending: str("wait")}, apperr.BadRequest},
	} {
		if _, err := g.r.CreateFilter(ctx, g.uid, c.in); !isKind(err, c.kind) {
			t.Errorf("%+v: %v", c.in, err)
		}
	}
	if _, err := g.r.CreateFilter(ctx, g.uid, FilterInput{Name: str("spoilers"), Description: str("for the Expanse books")}); err != nil {
		t.Errorf("spoilers with a subject refused: %v", err)
	}
	for i := 1; i < MaxFilters; i++ {
		if _, err := g.r.CreateFilter(ctx, g.uid, FilterInput{Name: str(strings.Repeat("t", i))}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := g.r.CreateFilter(ctx, g.uid, FilterInput{Name: str("one more")}); !isKind(err, apperr.Conflict) {
		t.Errorf("over MaxFilters: %v", err)
	}
}

// Each failure means what the API says it means: a halt stops every
// request until the operator acts, a skip gives up on that version (shown
// unclassified, never held), and a retry backs off.
func TestClassifierFailures(t *testing.T) {
	g, fc := filterRig(t,
		&testblyg.Item{ID: idA, Kind: "fragment", Version: 1, ContentMD: "gardening", Updated: time.Now().UTC()},
		&testblyg.Item{ID: idB, Kind: "fragment", Version: 1, ContentMD: "more gardening", Updated: time.Now().UTC()})
	ctx := context.Background()
	// Long enough that only failing, not time, lets a held item show.
	g.r.HoldFor = 24 * time.Hour
	g.mustFilter(FilterInput{Name: str("gardening"), Pending: str("hold")})

	fc.fail = &classify.Error{Failure: classify.Halt, Status: 402, Type: "insufficient_credits", Message: "no credits"}
	if _, err := g.r.LabelBatch(ctx); err != nil {
		t.Fatal(err)
	}
	if fc.count() != 1 {
		t.Fatalf("kept asking after a halt: %d", fc.count())
	}
	st, err := g.r.ClassifierStatus(ctx)
	if err != nil || st.HaltedUntil == nil || st.LastError == nil || !strings.Contains(*st.LastError, "402") || st.CallsToday != 1 {
		t.Fatalf("status after a halt: %+v %v", st, err)
	}
	if n, _ := g.r.LabelBatch(ctx); n != 0 || fc.count() != 1 {
		t.Fatal("asked again while halted")
	}
	var attempts int
	g.r.Pool.QueryRow(ctx, `SELECT coalesce(max(attempts), 0) FROM label_leases`).Scan(&attempts)
	if attempts != 0 {
		t.Fatalf("a halt cost the items an attempt: %d", attempts)
	}

	// After the halt, a skip: that version fails, and is no longer held.
	later := time.Now().Add(labelHalt + time.Minute)
	g.r.Now = func() time.Time { return later }
	fc.fail = &classify.Error{Failure: classify.Skip, Status: 400, Message: "too long"}
	g.labelAll()
	page, _ := g.r.Feed(ctx, g.uid, FeedQuery{})
	if len(page.Items) != 2 || page.Held != 0 || page.Items[0].Classification != "failed" {
		t.Fatalf("after a skip: %d items, held %d; a failed version must show, unclassified", len(page.Items), page.Held)
	}
	asked := fc.count()
	g.labelAll()
	if fc.count() != asked {
		t.Fatal("a failed version was asked again")
	}

	// A retry backs off, and is not asked again until then.
	g.b.Items[0].Version, g.b.Items[0].ContentMD = 2, "gardening, edited"
	g.b.Publish()
	g.poll()
	fc.fail = &classify.Error{Failure: classify.Retry, Status: 503, Message: "later"}
	g.labelAll()
	if fc.count() != asked+1 {
		t.Fatalf("retry: %d requests, want %d", fc.count(), asked+1)
	}
	var next time.Time
	g.r.Pool.QueryRow(ctx, `SELECT next_attempt_at FROM label_leases WHERE NOT failed`).Scan(&next)
	if next.Sub(later) < 4*time.Minute {
		t.Fatalf("no backoff: next attempt %v", next)
	}
}

// Requests stop at the daily ceiling.
func TestDailyCeiling(t *testing.T) {
	g, fc := filterRig(t,
		&testblyg.Item{ID: idA, Kind: "fragment", Version: 1, ContentMD: "gardening", Updated: time.Now().UTC()},
		&testblyg.Item{ID: idB, Kind: "fragment", Version: 1, ContentMD: "more gardening", Updated: time.Now().UTC()})
	g.r.DailyCalls, g.r.Batch = 1, 1
	g.mustFilter(FilterInput{Name: str("gardening")})
	g.labelAll()
	if fc.count() != 1 {
		t.Fatalf("over the ceiling: %d requests", fc.count())
	}
	if st, _ := g.r.ClassifierStatus(context.Background()); st.LastError == nil || !strings.Contains(*st.LastError, "ceiling") {
		t.Fatalf("ceiling not said: %+v", st)
	}
}

// Hidden items are counted on exactly one page each, however the feed is
// paged.
func TestHiddenCountedOncePerPage(t *testing.T) {
	now := time.Now().UTC()
	var items []*testblyg.Item
	ids := []string{
		"0100000000000000000000000a", "0200000000000000000000000a", "0300000000000000000000000a",
		"0400000000000000000000000a", "0500000000000000000000000a", "0600000000000000000000000a",
	}
	for i, id := range ids {
		md := "plain"
		if i%2 == 0 {
			md = "gardening"
		}
		items = append(items, &testblyg.Item{ID: id, Kind: "fragment", Version: 1, ContentMD: md, Updated: now.Add(-time.Duration(i) * time.Minute)})
	}
	g, _ := filterRig(t, items...)
	ctx := context.Background()
	g.mustFilter(FilterInput{Name: str("gardening"), Action: str("hide")})
	g.labelAll()
	shown, hidden := 0, 0
	q := FeedQuery{Limit: 1}
	for range 10 {
		page, err := g.r.Feed(ctx, g.uid, q)
		if err != nil {
			t.Fatal(err)
		}
		shown, hidden = shown+len(page.Items), hidden+page.Hidden
		if page.Next == "" {
			break
		}
		q.Before = page.Next
	}
	if shown != 3 || hidden != 3 {
		t.Fatalf("paged: %d shown, %d hidden; want 3 and 3", shown, hidden)
	}
}
