// Package classify asks SubQ's Decisions API whether a post is about a
// reader's topics. It is the reader's private filtering (DESIGN.md
// "Filters"): never on the wire, and off without a key, because a reader
// needs no models or keys (protocol invariant 3).
//
// The API (https://docs.subq.ai, Decisions API) takes a state and up to 128
// named questions, and answers each with probabilities, never text. Each
// topic is one score question with three levels: not about it, mentions it
// in passing, mainly about it. The caller keeps the probabilities and
// decides with its own thresholds, as the API's docs advise ("Set
// thresholds from your own data").
//
// This package holds no SQL and knows nothing of the planes: it turns an
// input and topics into answers, through internal/fetch, outside any
// transaction.
package classify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chrisbodhi/blygd/internal/fetch"
)

// Defaults for the SubQ Decisions API.
const (
	DefaultBaseURL = "https://api.subq.ai/v1"
	DefaultModel   = "subq-snap-1.1"
)

// MaxTopics is the most questions one request may ask (Decisions API:
// "1–128 named noul, choice, or score questions").
const MaxTopics = 128

// MaxText bounds the text sent for one post, in bytes. A fragment is at
// most 2,000 characters (§5.3); a thread with its quotes can run longer,
// and nothing past this is needed to tell what a post is about. It keeps
// the request far inside the model's 384K-token window, whose overrun is a
// 400.
const MaxText = 32 << 10

// promptVersion is part of every topic's hash. Change it whenever the
// question's wording or levels change, so every stored answer, asked the
// old way, is asked again.
const promptVersion = "score3/v1"

// Levels are the score question's levels, lowest first. Index 1 or above
// is "mentions it"; index 2 is "mainly about it".
var Levels = []string{"Not about it", "Mentions it in passing", "Mainly about it"}

// Limits bound one request: an answer is small, and the credential is
// never carried through a redirect.
var Limits = fetch.Limits{MaxRedirects: 0, Timeout: 30 * time.Second, MaxBody: 1 << 20}

// Topic is one thing a reader wants to filter, in their own words.
type Topic struct {
	Name        string // "US politics"
	Description string // optional: what counts, what does not
}

// Normalize trims the topic's words and collapses their whitespace, so
// two readers who wrote the same topic share one answer.
func (t Topic) Normalize() Topic {
	return Topic{Name: strings.Join(strings.Fields(t.Name), " "), Description: strings.Join(strings.Fields(t.Description), " ")}
}

// Hash names a topic as asked: its normalized words and the question's
// wording. Stored answers are keyed by it, so editing a topic's words, or
// the prompt, asks again; changing a threshold does not.
func (t Topic) Hash() string {
	n := t.Normalize()
	sum := sha256.Sum256([]byte(promptVersion + "\x00" + strings.ToLower(n.Name) + "\x00" + n.Description))
	return hex.EncodeToString(sum[:])
}

// key is the question's name in a request: at most 100 characters.
func (t Topic) key() string { return "t_" + t.Hash()[:32] }

func (t Topic) instructions() string {
	n := t.Normalize()
	s := fmt.Sprintf("How much is this post (its title, if any, and its text) about this topic: %s?", n.Name)
	if n.Description != "" {
		s += " What the reader means by it: " + n.Description
	}
	return s
}

// Input is one post as the model sees it: what it says, never who wrote
// it, so the author's name cannot sway the answer.
type Input struct {
	Title string `json:"title,omitempty"`
	Text  string `json:"text"`
}

// Clip cuts the text to MaxText at a character boundary.
func (in Input) Clip() Input {
	if len(in.Text) <= MaxText {
		return in
	}
	n := MaxText
	for n > 0 && !utf8.RuneStart(in.Text[n]) {
		n--
	}
	in.Text = in.Text[:n]
	return in
}

// Hash names the input as sent. A version whose text did not change (a
// maintenance republish, §13.2) has the same hash, and its answers are
// reused rather than asked again.
func (in Input) Hash() string {
	b, _ := json.Marshal(in.Clip())
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Answer is the model's judgment of one topic for one input.
type Answer struct {
	// Probabilities of each level, by its index in Levels.
	Probabilities [3]float64
	Score         float64 // probability-weighted level, 0 to 2
	Confidence    float64
}

// Mentions is the probability the post at least mentions the topic.
func (a Answer) Mentions() float64 { return a.Probabilities[1] + a.Probabilities[2] }

// Mainly is the probability the post is mainly about the topic.
func (a Answer) Mainly() float64 { return a.Probabilities[2] }

// Result is one request's answers, by topic hash.
type Result struct {
	Answers     map[string]Answer
	InputTokens int
}

// Failure says what a failed request means for the work.
type Failure int

const (
	// Retry: the service or the network may recover (429, 5xx, a timeout).
	Retry Failure = iota + 1
	// Skip: this input will never classify as asked (400: over the token
	// limit, or rejected; 422: a request it calls invalid). Retrying will
	// not change it.
	Skip
	// Halt: nothing will classify until the operator acts (401 bad key,
	// 402 no credits, 403 model not allowed). Stop asking.
	Halt
)

func (f Failure) String() string {
	switch f {
	case Retry:
		return "retry"
	case Skip:
		return "skip"
	case Halt:
		return "halt"
	}
	return "unknown"
}

// Error is a failed request.
type Error struct {
	Failure    Failure
	Status     int    // 0 when no response came back
	Type       string // detail.error_type
	Message    string
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return "classifier: " + e.Message
	}
	if e.Type != "" {
		return fmt.Sprintf("classifier: HTTP %d %s: %s", e.Status, e.Type, e.Message)
	}
	return fmt.Sprintf("classifier: HTTP %d: %s", e.Status, e.Message)
}

// Client calls the Decisions API.
type Client struct {
	Fetch   *fetch.Client
	BaseURL string // default DefaultBaseURL
	Key     string
	Model   string // default DefaultModel
	Now     func() time.Time
}

// ModelID is the model answers come from. Stored answers are keyed by it,
// so a new model asks again.
func (c *Client) ModelID() string {
	if c.Model != "" {
		return c.Model
	}
	return DefaultModel
}

func (c *Client) endpoint() string {
	b := c.BaseURL
	if b == "" {
		b = DefaultBaseURL
	}
	return strings.TrimRight(b, "/") + "/systemone"
}

type question struct {
	Type         string   `json:"type"`
	Instructions string   `json:"instructions"`
	Criteria     []string `json:"criteria"`
}

type request struct {
	Model     string              `json:"model"`
	State     Input               `json:"state"`
	Questions map[string]question `json:"questions"`
}

type scoreAnswer struct {
	Type          string             `json:"type"`
	Score         *float64           `json:"score"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    *float64           `json:"confidence"`
}

type response struct {
	Answers map[string]scoreAnswer `json:"answers"`
	Usage   struct {
		InputTokens int `json:"input_tokens"`
	} `json:"usage"`
}

type errorBody struct {
	Detail struct {
		ErrorType string `json:"error_type"`
		Message   string `json:"message"`
	} `json:"detail"`
}

// Classify asks how much in is about each topic, in one request. It must
// not run inside a transaction: internal/fetch refuses it there.
func (c *Client) Classify(ctx context.Context, in Input, topics []Topic) (*Result, error) {
	if len(topics) == 0 || len(topics) > MaxTopics {
		return nil, fmt.Errorf("classify: %d topics; one request asks 1 to %d", len(topics), MaxTopics)
	}
	req := request{Model: c.ModelID(), State: in.Clip(), Questions: map[string]question{}}
	byKey := map[string]string{} // question name → topic hash
	for _, t := range topics {
		req.Questions[t.key()] = question{Type: "score", Instructions: t.instructions(), Criteria: Levels}
		byKey[t.key()] = t.Hash()
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	h := http.Header{"Authorization": {"Bearer " + c.Key}}
	resp, err := c.Fetch.DoWith(ctx, http.MethodPost, c.endpoint(), bytes.NewReader(body), "application/json", Limits, "application/json", h)
	if err != nil {
		if errors.Is(err, fetch.ErrInTx) {
			return nil, err // a bug at the call site, not a service failure
		}
		return nil, &Error{Failure: Retry, Message: err.Error()}
	}
	if resp.Status != http.StatusOK {
		return nil, c.failure(resp)
	}
	var out response
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, &Error{Failure: Retry, Status: resp.Status, Message: "response is not JSON: " + err.Error()}
	}
	res := &Result{Answers: map[string]Answer{}, InputTokens: out.Usage.InputTokens}
	for key, hash := range byKey {
		a, err := parseAnswer(out.Answers[key])
		if err != nil {
			// The service answered, but not what was asked: it may answer
			// properly next time, so this is no reason to give up on the input.
			return nil, &Error{Failure: Retry, Status: resp.Status, Message: fmt.Sprintf("answer %s: %v", key, err)}
		}
		res.Answers[hash] = a
	}
	return res, nil
}

func parseAnswer(s scoreAnswer) (Answer, error) {
	var a Answer
	if s.Type != "score" {
		return a, fmt.Errorf("type %q, want score", s.Type)
	}
	if s.Score == nil || s.Confidence == nil || len(s.Probabilities) != len(Levels) {
		return a, errors.New("missing score, confidence or probabilities")
	}
	for i := range Levels {
		p, ok := s.Probabilities[fmt.Sprint(i)]
		if !ok || !unit(p) {
			return a, fmt.Errorf("probability of level %d missing or outside 0..1", i)
		}
		a.Probabilities[i] = p
	}
	if !unit(*s.Confidence) || *s.Score < 0 || *s.Score > float64(len(Levels)-1) || math.IsNaN(*s.Score) {
		return a, errors.New("score or confidence out of range")
	}
	a.Score, a.Confidence = *s.Score, *s.Confidence
	return a, nil
}

func unit(p float64) bool { return p >= 0 && p <= 1 && !math.IsNaN(p) }

// failure maps a non-200 response to what it means for the work, by the
// API's documented statuses.
func (c *Client) failure(resp *fetch.Response) *Error {
	var eb errorBody
	_ = json.Unmarshal(resp.Body, &eb)
	e := &Error{Status: resp.Status, Type: eb.Detail.ErrorType, Message: eb.Detail.Message}
	if e.Message == "" {
		e.Message = http.StatusText(resp.Status)
	}
	if len(e.Message) > 300 {
		e.Message = e.Message[:300] + "…"
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	switch {
	case resp.Status == http.StatusBadRequest, resp.Status == http.StatusUnprocessableEntity:
		e.Failure = Skip
	case resp.Status == http.StatusUnauthorized, resp.Status == http.StatusPaymentRequired, resp.Status == http.StatusForbidden:
		e.Failure = Halt
	case resp.Status == http.StatusTooManyRequests, resp.Status >= 500:
		e.Failure = Retry
		e.RetryAfter = fetch.RetryAfter(resp.Header, now())
	case resp.Status >= 300 && resp.Status < 500:
		// No redirect is followed with the key on it, and any other 4xx
		// (a 404 from a wrong BLYGD_SUBQ_URL) says the same of every
		// request: the operator's to fix.
		e.Failure = Halt
	default:
		e.Failure = Retry
	}
	return e
}
