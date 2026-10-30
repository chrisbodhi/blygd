package web

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/chrisbodhi/blygd/internal/account"
	"github.com/chrisbodhi/blygd/internal/events"
)

// GET /api/events is a Server-Sent Events stream of hints for the token's
// user: import progress on their subscriptions and their subscription
// imports (reader:read), changes to their own items (items:read), and
// their origin's mentions and outbound sends (mentions:read). An event
// says what changed, never the state itself, so a client re-fetches what
// it shows and a dropped stream loses nothing. Browsers read it with
// fetch(), which can send the Authorization header (EventSource cannot).

// maxStreams caps one user's concurrent streams: each holds a goroutine and
// a subscription for as long as a phone keeps a tab open.
const maxStreams = 8

// heartbeat keeps intermediaries from timing out an idle stream, and finds
// dead clients.
const heartbeat = 25 * time.Second

type streamCount struct {
	mu sync.Mutex
	n  map[int64]int
}

func (s *streamCount) acquire(user int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.n == nil {
		s.n = map[int64]int{}
	}
	if s.n[user] >= maxStreams {
		return false
	}
	s.n[user]++
	return true
}

func (s *streamCount) release(user int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n[user]--
}

func (a *API) streamEvents(w http.ResponseWriter, r *http.Request) error {
	if a.Events == nil {
		return &httpError{http.StatusServiceUnavailable, "unavailable", "events are not running"}
	}
	p := principal(r.Context())
	o, err := a.origin(r)
	if err != nil {
		return err
	}
	if !a.streams.acquire(p.UserID) {
		return &httpError{http.StatusTooManyRequests, "too_many_streams", fmt.Sprintf("at most %d open event streams per user", maxStreams)}
	}
	defer a.streams.release(p.UserID)

	rc := http.NewResponseController(w)
	// The server's WriteTimeout would end every stream after a minute.
	_ = rc.SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	ch, unsubscribe := a.Events.Subscribe()
	defer unsubscribe()
	write := func(s string) bool {
		if _, err := fmt.Fprint(w, s); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !write("retry: 3000\n: connected\n\n") {
		return nil
	}
	tick := time.NewTicker(heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return nil
		case <-tick.C:
			if !write(": ping\n\n") {
				return nil
			}
		case ev := <-ch:
			if !a.visible(r, p, o.ID, ev) {
				continue
			}
			if !write("event: " + ev.Type + "\ndata: " + string(ev.Data) + "\n\n") {
				return nil
			}
		}
	}
}

// visible says whether an event is this stream's to hear: its scopes allow
// it, and it concerns the user's own origin or one of their subscriptions.
func (a *API) visible(r *http.Request, p *account.Principal, originID int64, ev events.Event) bool {
	switch ev.Type {
	case "import":
		if !p.Can(account.ScopeReaderRead) || ev.RemoteOriginID == 0 {
			return false
		}
		return a.Reader.Subscribed(r.Context(), p.UserID, ev.RemoteOriginID)
	case "item":
		return p.Can(account.ScopeItemsRead) && ev.OriginID == originID
	case "subscribe":
		return p.Can(account.ScopeReaderRead) && ev.UserID != 0 && ev.UserID == p.UserID
	case "mention", "outbound":
		return p.Can(account.ScopeMentionsRead) && ev.OriginID == originID
	}
	return false
}
