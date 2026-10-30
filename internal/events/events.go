// Package events carries "something changed" hints from the workers to
// open client streams (GET /api/events). An event is a hint, not data: a
// client re-fetches what it shows, so a dropped connection loses nothing.
//
// Events travel through Postgres LISTEN/NOTIFY, so any process can send
// one and every process's streams hear it. Nothing is stored: progress is
// transient, and a client that missed it asks the API for the state.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Channel is the NOTIFY channel.
const Channel = "blygd_events"

// Event is one hint. Exactly one of OriginID (an own origin: items and
// mentions), RemoteOriginID (an import, shared by every subscriber) or
// UserID (one user's private reading state) says who may hear it; Data is
// what the client receives.
type Event struct {
	Type           string          `json:"type"` // import, item, mention, outbound, subscribe
	OriginID       int64           `json:"origin_id,omitempty"`
	RemoteOriginID int64           `json:"remote_origin_id,omitempty"`
	UserID         int64           `json:"user_id,omitempty"`
	Data           json.RawMessage `json:"data"`
}

// Execer is a pool, a connection or a transaction. A NOTIFY sent inside a
// transaction is delivered when it commits, and not at all if it rolls back.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Notify sends an event. A failure is logged by the caller at most: an
// event is a hint, and nothing depends on its delivery.
func Notify(ctx context.Context, db Execer, typ string, originID, remoteOriginID int64, data any) error {
	return send(ctx, db, Event{Type: typ, OriginID: originID, RemoteOriginID: remoteOriginID}, data)
}

// NotifyUser sends an event only userID's streams may hear.
func NotifyUser(ctx context.Context, db Execer, typ string, userID int64, data any) error {
	return send(ctx, db, Event{Type: typ, UserID: userID}, data)
}

func send(ctx context.Context, db Execer, e Event, data any) error {
	d, err := json.Marshal(data)
	if err != nil {
		return err
	}
	e.Data = d
	ev, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if len(ev) > 7900 {
		return errors.New("event too large for NOTIFY")
	}
	_, err = db.Exec(ctx, `SELECT pg_notify($1, $2)`, Channel, string(ev))
	return err
}

// Hub listens on one connection and fans events out to subscribers.
type Hub struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger

	mu        sync.Mutex
	subs      map[chan Event]struct{}
	readyOnce sync.Once
	ready     chan struct{}
}

func (h *Hub) readyCh() chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ready == nil {
		h.ready = make(chan struct{})
	}
	return h.ready
}

// Ready is closed once the hub first listens, so an event sent after it is
// heard.
func (h *Hub) Ready() <-chan struct{} { return h.readyCh() }

// Subscribe returns a channel of every event and a function that ends the
// subscription. A subscriber that falls behind loses events rather than
// slowing the others: they are hints.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[chan Event]struct{}{}
	}
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

func (h *Hub) publish(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- ev:
		default: // slow subscriber: drop
		}
	}
}

// Run listens until ctx ends, reconnecting after a lost connection.
func (h *Hub) Run(ctx context.Context) {
	for ctx.Err() == nil {
		err := h.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		if h.Log != nil {
			h.Log.Warn("events: listener lost; reconnecting", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (h *Hub) listen(ctx context.Context) error {
	pooled, err := h.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	// LISTEN state lives on the connection, so it leaves the pool for good.
	conn := pooled.Hijack()
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{Channel}.Sanitize()); err != nil {
		return err
	}
	ready := h.readyCh()
	h.readyOnce.Do(func() { close(ready) })
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		var ev Event
		if json.Unmarshal([]byte(n.Payload), &ev) == nil {
			h.publish(ev)
		}
	}
}
