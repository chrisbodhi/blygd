package classify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/chrisbodhi/blygd/internal/archtest"
	"github.com/chrisbodhi/blygd/internal/fetch"
	"github.com/chrisbodhi/blygd/internal/tx"
)

// fakeSubQ answers POST /v1/systemone the way the Decisions API documents:
// one score answer per question, keyed by the question's name.
func fakeSubQ(t *testing.T, answer func(name string, q map[string]any) map[string]any) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/systemone" {
			http.Error(w, "not here", 404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			w.WriteHeader(401)
			io.WriteString(w, `{"detail":{"error_type":"authentication_error","message":"bad key"}}`)
			return
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("request is not JSON: %v", err)
		}
		got = append(got, req)
		answers := map[string]any{}
		for name, q := range req["questions"].(map[string]any) {
			answers[name] = answer(name, q.(map[string]any))
		}
		json.NewEncoder(w).Encode(map[string]any{"model": req["model"], "answers": answers,
			"usage": map[string]any{"input_tokens": 477, "output_tokens": 0}})
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func score(p0, p1, p2 float64) map[string]any {
	return map[string]any{"type": "score", "score": p1 + 2*p2,
		"legend":        map[string]any{"0": Levels[0], "1": Levels[1], "2": Levels[2]},
		"probabilities": map[string]any{"0": p0, "1": p1, "2": p2}, "confidence": 0.8}
}

func client(srv *httptest.Server) *Client {
	return &Client{Fetch: &fetch.Client{AllowPrivate: true}, BaseURL: srv.URL + "/v1", Key: "sk-test"}
}

func TestClassifyAsksScoreQuestions(t *testing.T) {
	srv, got := fakeSubQ(t, func(name string, q map[string]any) map[string]any {
		if strings.Contains(q["instructions"].(string), "politics") {
			return score(0.1, 0.2, 0.7)
		}
		return score(0.9, 0.1, 0)
	})
	politics := Topic{Name: "  US   politics ", Description: "elections, parties"}
	gardening := Topic{Name: "gardening"}
	res, err := client(srv).Classify(context.Background(), Input{Text: "The Senate voted."}, []Topic{politics, gardening})
	if err != nil {
		t.Fatal(err)
	}
	a := res.Answers[politics.Hash()]
	if a.Mainly() != 0.7 || a.Mentions() < 0.89 || a.Mentions() > 0.91 || res.InputTokens != 477 {
		t.Fatalf("politics: %+v, %d tokens", a, res.InputTokens)
	}
	if g := res.Answers[gardening.Hash()]; g.Mentions() != 0.1 {
		t.Fatalf("gardening: %+v", g)
	}

	req := (*got)[0]
	if req["model"] != DefaultModel {
		t.Errorf("model %v", req["model"])
	}
	if req["state"].(map[string]any)["text"] != "The Senate voted." {
		t.Errorf("state %v", req["state"])
	}
	qs := req["questions"].(map[string]any)
	if len(qs) != 2 {
		t.Fatalf("questions %v", qs)
	}
	for name, q := range qs {
		q := q.(map[string]any)
		if len(name) > 100 || q["type"] != "score" || len(q["criteria"].([]any)) != 3 {
			t.Errorf("question %s: %v", name, q)
		}
	}
	if q := qs[politics.key()].(map[string]any); !strings.Contains(q["instructions"].(string), "US politics?") ||
		!strings.Contains(q["instructions"].(string), "elections, parties") {
		t.Errorf("instructions not normalized or missing the description: %v", q["instructions"])
	}
}

// Two readers who wrote the same topic share one answer; a different
// description, or a new prompt, is a different question.
func TestTopicHash(t *testing.T) {
	a := Topic{Name: "AI", Description: "machine learning news"}
	if a.Hash() != (Topic{Name: " ai ", Description: "machine  learning news"}).Hash() {
		t.Error("whitespace or case changed the hash")
	}
	if a.Hash() == (Topic{Name: "AI"}).Hash() {
		t.Error("the description is not part of the hash")
	}
	if len(a.key()) > 100 {
		t.Error("question name over 100 characters")
	}
}

func TestInputClipAndHash(t *testing.T) {
	long := Input{Text: strings.Repeat("é", MaxText)} // 2 bytes each
	c := long.Clip()
	if len(c.Text) > MaxText || !strings.HasPrefix(long.Text, c.Text) || !utf8Valid(c.Text) {
		t.Fatalf("clip: %d bytes", len(c.Text))
	}
	if long.Hash() != c.Hash() {
		t.Error("the hash is not of the input as sent")
	}
	if (Input{Text: "a"}).Hash() == (Input{Title: "a", Text: ""}).Hash() {
		t.Error("title and text collide")
	}
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "�") == s }

// Each documented status means one thing for the work.
func TestFailures(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   Failure
	}{
		{400, `{"detail":{"error_type":"context_length_exceeded","message":"too long"}}`, Skip},
		{422, `{"detail":{"error_type":"validation_error","message":"questions/x: bad"}}`, Skip},
		{401, `{"detail":{"error_type":"authentication_error","message":"bad key"}}`, Halt},
		{402, `{"detail":{"error_type":"insufficient_credits","message":"no credits"}}`, Halt},
		{403, `{"detail":{"error_type":"model_not_allowed","message":"no"}}`, Halt},
		{404, `not found`, Halt},
		{302, ``, Halt},
		{429, `{"detail":{"error_type":"rate_limited","message":"slow down"}}`, Retry},
		{503, `{"detail":{"error_type":"unavailable","message":"later"}}`, Retry},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c.status == 429 {
				w.Header().Set("Retry-After", "7")
			}
			if c.status == 302 {
				w.Header().Set("Location", "https://elsewhere.example/")
			}
			w.WriteHeader(c.status)
			io.WriteString(w, c.body)
		}))
		_, err := client(srv).Classify(context.Background(), Input{Text: "x"}, []Topic{{Name: "AI"}})
		srv.Close()
		var e *Error
		if !errors.As(err, &e) || e.Failure != c.want || e.Status != c.status {
			t.Errorf("%d: %v, want %v", c.status, err, c.want)
			continue
		}
		if c.status == 429 && e.RetryAfter != 7*time.Second {
			t.Errorf("429: Retry-After %v", e.RetryAfter)
		}
		if strings.HasPrefix(c.body, "{") && (e.Type == "" || e.Message == "") {
			t.Errorf("%d: detail not read: %+v", c.status, e)
		}
	}
}

// An answer that is not what was asked is the service's fault, so it is
// tried again rather than giving up on the post.
func TestMalformedAnswerRetries(t *testing.T) {
	srv, _ := fakeSubQ(t, func(string, map[string]any) map[string]any {
		return map[string]any{"type": "score", "score": 1, "probabilities": map[string]any{"0": 0.5, "1": 0.5}, "confidence": 1}
	})
	_, err := client(srv).Classify(context.Background(), Input{Text: "x"}, []Topic{{Name: "AI"}})
	var e *Error
	if !errors.As(err, &e) || e.Failure != Retry {
		t.Fatalf("got %v", err)
	}
}

func TestNoNetworkInsideATransaction(t *testing.T) {
	srv, got := fakeSubQ(t, func(string, map[string]any) map[string]any { return score(1, 0, 0) })
	err := tx.Run(context.Background(), fakeDB{}, pgx.TxOptions{}, func(ctx context.Context, _ pgx.Tx) error {
		_, err := client(srv).Classify(ctx, Input{Text: "x"}, []Topic{{Name: "AI"}})
		return err
	})
	if !errors.Is(err, fetch.ErrInTx) || len(*got) != 0 {
		t.Fatalf("classified inside a transaction: %v", err)
	}
}

// fakeTx is enough of a transaction for tx.Run to mark a context.
type fakeTx struct{ pgx.Tx }

func (fakeTx) Commit(context.Context) error   { return nil }
func (fakeTx) Rollback(context.Context) error { return nil }

type fakeDB struct{}

func (fakeDB) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) { return fakeTx{}, nil }

func TestTopicCount(t *testing.T) {
	c := &Client{Fetch: &fetch.Client{}}
	if _, err := c.Classify(context.Background(), Input{}, nil); err == nil {
		t.Error("no topics accepted")
	}
	if _, err := c.Classify(context.Background(), Input{}, make([]Topic, MaxTopics+1)); err == nil {
		t.Error("too many topics accepted")
	}
}

// The classifier holds no SQL: what it answers is stored by the reader,
// which owns the tables and their plane.
func TestClassifyHoldsNoSQL(t *testing.T) {
	archtest.Check(t, ".", archtest.Rule{
		Banned: regexp.MustCompile(`\b(FROM|JOIN|INTO|UPDATE)\s+[a-z_]+\b`),
		Why:    "SQL in the classifier: store answers in internal/reader",
	})
}
