package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/chrisbodhi/blygd/internal/classify"
	"github.com/chrisbodhi/blygd/internal/mention"
)

// fakeSubQ is SubQ's Decisions API as its docs describe it (POST
// /v1/systemone, bearer key, score answers keyed by question name). A
// post is mainly about a topic when its text names the topic's first word.
type fakeSubQ struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []string
}

func newFakeSubQ(t *testing.T) *fakeSubQ {
	f := &fakeSubQ{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer sk-test" {
			w.WriteHeader(401)
			io.WriteString(w, `{"detail":{"error_type":"authentication_error","message":"no"}}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, string(b))
		f.mu.Unlock()
		var req struct {
			State     struct{ Text string } `json:"state"`
			Questions map[string]struct {
				Instructions string `json:"instructions"`
			} `json:"questions"`
		}
		json.Unmarshal(b, &req)
		answers := map[string]any{}
		for name, q := range req.Questions {
			topic := strings.TrimSuffix(strings.SplitN(q.Instructions, "topic: ", 2)[1], "?")
			topic = strings.Fields(topic)[0]
			p := []float64{0.9, 0.1, 0}
			if strings.Contains(strings.ToLower(req.State.Text), strings.ToLower(topic)) {
				p = []float64{0.05, 0.1, 0.85}
			}
			answers[name] = map[string]any{"type": "score", "score": p[1] + 2*p[2], "confidence": 0.8,
				"legend":        map[string]string{"0": classify.Levels[0], "1": classify.Levels[1], "2": classify.Levels[2]},
				"probabilities": map[string]float64{"0": p[0], "1": p[1], "2": p[2]}}
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "subq-snap-1.1", "answers": answers,
			"usage": map[string]int{"input_tokens": 120, "output_tokens": 0}})
	}))
	t.Cleanup(f.Close)
	return f
}

func (e *env) withClassifier(f *fakeSubQ) {
	e.reader.Classifier = &classify.Client{Fetch: e.fetch, BaseURL: f.URL + "/v1", Key: "sk-test"}
}

func (e *env) label() {
	e.t.Helper()
	for range 5 {
		n, err := e.reader.LabelBatch(context.Background())
		if err != nil {
			e.t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
}

// Filters end to end, through the real SubQ client against a fake of its
// API: every filter operation, the feed's hidden count and filters=off,
// a reader's own judgment, and the classifier status.
func TestFiltersEndToEnd(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	subq := newFakeSubQ(t)
	alice, bob := e.user("alice"), e.user("bob")

	// No classifier yet: the API says so, and filters do nothing.
	r := bob.api("GET", "/filters", nil)
	expect(t, r, 200)
	if c := r.json(t)["classifier"].(map[string]any); c["enabled"] != false {
		t.Fatalf("classifier: %v", c)
	}
	e.withClassifier(subq)

	politics := alice.draft(t, map[string]any{"content_md": "Politics: the Senate voted again."})
	alice.publish(t, politics, 200)
	garden := alice.draft(t, map[string]any{"content_md": "The garden is all tomatoes."})
	alice.publish(t, garden, 200)
	sid := bob.subscribe(t, alice.origin)
	e.poll(sid)

	expect(t, bob.api("POST", "/filters", map[string]any{"name": "spoilers"}), 422)
	expect(t, bob.api("POST", "/filters", map[string]any{"name": "x", "colour": "red"}), 400)
	expect(t, bob.api("POST", "/filters", map[string]any{"name": "x", "subscription_id": 0}), 400)
	r = bob.api("POST", "/filters", map[string]any{"name": "politics", "action": "hide"})
	expect(t, r, 201)
	fid := itoa(int(r.json(t)["id"].(float64)))
	expect(t, bob.api("POST", "/filters", map[string]any{"name": "Politics "}), 409)
	expect(t, bob.api("GET", "/filters/"+fid, nil), 200)
	expect(t, alice.api("GET", "/filters/"+fid, nil), 404)
	expect(t, bob.api("PATCH", "/filters/"+fid, map[string]any{"subscription_id": sid}), 200)
	r = bob.api("PATCH", "/filters/"+fid, map[string]any{"subscription_id": nil, "threshold": 0.6})
	expect(t, r, 200)
	if f := r.json(t); f["subscription_id"] != nil || f["threshold"] != 0.6 {
		t.Fatalf("patched: %v", f)
	}

	e.label()
	if len(subq.bodies) != 2 {
		t.Fatalf("%d requests for 2 items", len(subq.bodies))
	}
	for _, b := range subq.bodies {
		if strings.Contains(b, alice.origin) || strings.Contains(b, "Alice") {
			t.Fatalf("the classifier was told who wrote it: %s", b)
		}
	}

	r = bob.api("GET", "/reader/feed", nil)
	expect(t, r, 200)
	page := r.json(t)
	if items := page["items"].([]any); len(items) != 1 || page["hidden"] != float64(1) {
		t.Fatalf("feed: %d items, hidden %v", len(items), page["hidden"])
	}
	all := feedItems(t, bob, "?filters=off")
	if len(all) != 2 {
		t.Fatalf("filters=off: %d items", len(all))
	}
	for _, it := range all {
		f, _ := it["filtered"].(map[string]any)
		if (it["item_id"] == politics) != (f != nil && f["action"] == "hide") || it["classification"] != "done" {
			t.Errorf("filters=off: %s filtered %v, %v", it["item_id"], f, it["classification"])
		}
	}
	expect(t, bob.api("GET", "/reader/feed?filters=maybe", nil), 400)

	r = bob.api("GET", "/reader/items/"+itoa(sid)+"/"+politics, nil)
	expect(t, r, 200)
	labels := r.json(t)["item"].(map[string]any)["labels"].([]any)
	if l := labels[0].(map[string]any); l["matched"] != true || l["p_mainly"] != 0.85 {
		t.Fatalf("labels: %v", labels)
	}

	// Bob's own judgment wins, and can be taken back.
	expect(t, bob.api("PUT", "/reader/items/"+itoa(sid)+"/"+politics+"/filters/"+fid, map[string]any{}), 400)
	expect(t, bob.api("PUT", "/reader/items/"+itoa(sid)+"/"+politics+"/filters/"+fid, map[string]any{"matched": false}), 204)
	if n := len(feedItems(t, bob, "")); n != 2 {
		t.Fatalf("override: %d items", n)
	}
	expect(t, bob.api("DELETE", "/reader/items/"+itoa(sid)+"/"+politics+"/filters/"+fid, nil), 204)
	if n := len(feedItems(t, bob, "")); n != 1 {
		t.Fatalf("override forgotten: %d items", n)
	}
	expect(t, alice.api("PUT", "/reader/items/"+itoa(sid)+"/"+politics+"/filters/"+fid, map[string]any{"matched": true}), 404)

	r = bob.api("GET", "/filters", nil)
	expect(t, r, 200)
	c := r.json(t)["classifier"].(map[string]any)
	if c["enabled"] != true || c["model"] != classify.DefaultModel || c["calls_today"] != float64(2) {
		t.Fatalf("classifier: %v", c)
	}

	expect(t, bob.api("DELETE", "/filters/"+fid, nil), 204)
	expect(t, bob.api("GET", "/filters/"+fid, nil), 404)
	if n := len(feedItems(t, bob, "")); n != 2 {
		t.Fatalf("filter deleted: %d items", n)
	}
}

// A bad key halts the classifier, and readers can see why.
func TestClassifierHaltIsVisible(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	subq := newFakeSubQ(t)
	e.reader.Classifier = &classify.Client{Fetch: e.fetch, BaseURL: subq.URL + "/v1", Key: "sk-wrong"}
	alice, bob := e.user("alice"), e.user("bob")
	id := alice.draft(t, map[string]any{"content_md": "Politics."})
	alice.publish(t, id, 200)
	sid := bob.subscribe(t, alice.origin)
	e.poll(sid)
	expect(t, bob.api("POST", "/filters", map[string]any{"name": "politics", "pending": "hold"}), 201)
	e.label()
	r := bob.api("GET", "/filters", nil)
	expect(t, r, 200)
	c := r.json(t)["classifier"].(map[string]any)
	if c["halted_until"] == nil || !strings.Contains(c["last_error"].(string), "401") {
		t.Fatalf("halt not visible: %v", c)
	}
}
