package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/chrisbodhi/blygd/internal/account"
	"github.com/chrisbodhi/blygd/internal/reader"
)

// The reading API: subscriptions (§12), the merged feed (§13.7), item
// reads, and read marks. Every imported byte is returned verbatim with the
// §14 notice: the client sanitizes.

func (a *API) readerRoutes(route func(pattern, scope string, fn func(http.ResponseWriter, *http.Request) error)) {
	route("GET /api/subscriptions", account.ScopeReaderRead, a.listSubscriptions)
	route("POST /api/subscriptions", account.ScopeReaderWrite, a.subscribe)
	route("GET /api/subscriptions/{sid}", account.ScopeReaderRead, a.getSubscription)
	route("PATCH /api/subscriptions/{sid}", account.ScopeReaderWrite, a.patchSubscription)
	route("DELETE /api/subscriptions/{sid}", account.ScopeReaderWrite, a.unsubscribe)
	route("POST /api/subscriptions/{sid}/resolve", account.ScopeReaderRead, a.reresolve)
	route("POST /api/subscriptions/{sid}/move", account.ScopeReaderWrite, a.moveSubscription)

	route("GET /api/reader/feed", account.ScopeReaderRead, a.readerFeed)
	route("GET /api/reader/items/{sid}/{iid}", account.ScopeReaderRead, a.readerItem)
	route("PUT /api/reader/items/{sid}/{iid}/read", account.ScopeReaderWrite, a.markRead)
	route("DELETE /api/reader/items/{sid}/{iid}/read", account.ScopeReaderWrite, a.markUnread)
}

// resolveFailed reports a failed resolution with its trail (§12.1 step 6).
func resolveFailed(w http.ResponseWriter, err error) bool {
	var re *reader.ResolveError
	if !errors.As(err, &re) {
		return false
	}
	trail := re.Trail
	if trail == nil {
		trail = []string{}
	}
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "unresolved", "message": re.Reason, "trail": trail})
	return true
}

func (a *API) readerErr(err error) error {
	switch {
	case errors.Is(err, reader.ErrNotFound):
		return &httpError{404, "not_found", "not found"}
	case errors.Is(err, reader.ErrSameIdentity):
		return &httpError{409, "conflict", err.Error()}
	}
	return err
}

func (a *API) subscriptionView(r *http.Request, sid int64) (*reader.Subscription, error) {
	o, err := a.origin(r)
	if err != nil {
		return nil, err
	}
	s, err := a.Reader.GetSubscription(r.Context(), principal(r.Context()).UserID, o.ID, sid)
	return s, a.readerErr(err)
}

func (a *API) listSubscriptions(w http.ResponseWriter, r *http.Request) error {
	o, err := a.origin(r)
	if err != nil {
		return err
	}
	subs, err := a.Reader.Subscriptions(r.Context(), principal(r.Context()).UserID, o.ID)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"subscriptions": subs})
	return nil
}

func (a *API) subscribe(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		URL string `json:"url"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if in.URL == "" {
		return badRequest("url is required")
	}
	sid, res, err := a.Reader.Subscribe(r.Context(), principal(r.Context()).UserID, in.URL)
	if resolveFailed(w, err) {
		return nil
	}
	if err != nil {
		return err
	}
	s, err := a.subscriptionView(r, sid)
	if err != nil {
		return err
	}
	writeJSON(w, 201, map[string]any{"subscription": s, "resolution": res})
	return nil
}

func (a *API) getSubscription(w http.ResponseWriter, r *http.Request) error {
	sid, err := pathInt(r, "sid")
	if err != nil {
		return err
	}
	s, err := a.subscriptionView(r, sid)
	if err != nil {
		return err
	}
	writeJSON(w, 200, s)
	return nil
}

func (a *API) patchSubscription(w http.ResponseWriter, r *http.Request) error {
	sid, err := pathInt(r, "sid")
	if err != nil {
		return err
	}
	var in struct {
		DisplayTitle json.RawMessage `json:"display_title"`
		InBlogroll   *bool           `json:"in_blogroll"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	p := principal(r.Context())
	// §11: publishing a reading choice is a deliberate public act.
	if in.InBlogroll != nil && !p.Can(account.ScopeOriginWrite) {
		return &httpError{403, "forbidden", "changing the public blogroll needs scope " + account.ScopeOriginWrite}
	}
	if _, err := a.subscriptionView(r, sid); err != nil {
		return err
	}
	if in.DisplayTitle != nil {
		var title *string
		if err := json.Unmarshal(in.DisplayTitle, &title); err != nil {
			return badRequest("display_title must be a string or null")
		}
		if err := a.Reader.SetDisplayTitle(r.Context(), p.UserID, sid, title); err != nil {
			return a.readerErr(err)
		}
	}
	if in.InBlogroll != nil {
		o, err := a.origin(r)
		if err != nil {
			return err
		}
		if *in.InBlogroll {
			// Re-sent true refreshes the published copy from the current
			// title.
			e, err := a.Reader.BlogrollEntry(r.Context(), p.UserID, sid)
			if err != nil {
				return a.readerErr(err)
			}
			err = a.Studio.SetBlogrollEntry(r.Context(), o.ID, sid, e)
			if err != nil {
				return err
			}
		} else if err := a.Studio.SetBlogrollEntry(r.Context(), o.ID, sid, nil); err != nil {
			return err
		}
	}
	s, err := a.subscriptionView(r, sid)
	if err != nil {
		return err
	}
	writeJSON(w, 200, s)
	return nil
}

func (a *API) unsubscribe(w http.ResponseWriter, r *http.Request) error {
	sid, err := pathInt(r, "sid")
	if err != nil {
		return err
	}
	o, err := a.origin(r)
	if err != nil {
		return err
	}
	if err := a.Reader.Unsubscribe(r.Context(), principal(r.Context()).UserID, sid); err != nil {
		return a.readerErr(err)
	}
	// A blogroll entry for something no longer followed is a stale public
	// claim; withdrawing it only narrows what is public.
	if err := a.Studio.SetBlogrollEntry(r.Context(), o.ID, sid, nil); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func optionalURL(r *http.Request, field string) (string, error) {
	if r.ContentLength == 0 {
		return "", nil
	}
	var in map[string]string
	if err := decode(r, &in); err != nil {
		return "", err
	}
	for k := range in {
		if k != field {
			return "", badRequest("unknown field " + k)
		}
	}
	return in[field], nil
}

// reresolve re-runs resolution and reports, changing nothing (§12.2).
func (a *API) reresolve(w http.ResponseWriter, r *http.Request) error {
	sid, err := pathInt(r, "sid")
	if err != nil {
		return err
	}
	u, err := optionalURL(r, "url")
	if err != nil {
		return err
	}
	cur, res, err := a.Reader.Reresolve(r.Context(), principal(r.Context()).UserID, sid, u)
	if resolveFailed(w, err) {
		return nil
	}
	if err != nil {
		return a.readerErr(err)
	}
	writeJSON(w, 200, map[string]any{"current": cur, "resolution": res, "changed": res.Identity != cur,
		"note": "An identity change is never automatic: confirm it with POST /api/subscriptions/{id}/move (§12.2)."})
	return nil
}

// moveSubscription is the user-confirmed identity change (§12.2).
func (a *API) moveSubscription(w http.ResponseWriter, r *http.Request) error {
	sid, err := pathInt(r, "sid")
	if err != nil {
		return err
	}
	to, err := optionalURL(r, "to")
	if err != nil {
		return err
	}
	if to == "" {
		return badRequest("to is required: the identity you confirmed")
	}
	old, err := a.subscriptionView(r, sid)
	if err != nil {
		return err
	}
	p := principal(r.Context())
	nid, res, err := a.Reader.Move(r.Context(), p.UserID, sid, to)
	if resolveFailed(w, err) {
		return nil
	}
	if err != nil {
		return a.readerErr(err)
	}
	var notes []string
	if old.InBlogroll {
		o, err := a.origin(r)
		if err != nil {
			return err
		}
		if err := a.Studio.SetBlogrollEntry(r.Context(), o.ID, sid, nil); err != nil {
			return err
		}
		if p.Can(account.ScopeOriginWrite) {
			e, err := a.Reader.BlogrollEntry(r.Context(), p.UserID, nid)
			if err != nil {
				return a.readerErr(err)
			}
			if err := a.Studio.SetBlogrollEntry(r.Context(), o.ID, nid, e); err != nil {
				return err
			}
		} else {
			notes = append(notes, "the old blogroll entry was removed; adding the new one needs scope "+account.ScopeOriginWrite)
		}
	}
	s, err := a.subscriptionView(r, nid)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"subscription": s, "resolution": res, "notes": notes})
	return nil
}

func (a *API) readerFeed(w http.ResponseWriter, r *http.Request) error {
	q := reader.FeedQuery{Before: r.URL.Query().Get("before"), UnreadOnly: r.URL.Query().Get("unread") == "true"}
	if l := r.URL.Query().Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil {
			return badRequest("limit must be an integer")
		}
		q.Limit = n
	}
	if s := r.URL.Query().Get("subscription"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 {
			return badRequest("subscription must be a positive integer")
		}
		q.SubscriptionID = n
	}
	items, next, err := a.Reader.Feed(r.Context(), principal(r.Context()).UserID, q)
	if err != nil {
		if err.Error() == "bad cursor" {
			return badRequest("bad cursor")
		}
		return err
	}
	writeJSON(w, 200, map[string]any{"items": items, "next": next, "notice": reader.Notice})
	return nil
}

func (a *API) readerItem(w http.ResponseWriter, r *http.Request) error {
	sid, err := pathInt(r, "sid")
	if err != nil {
		return err
	}
	it, err := a.Reader.GetItem(r.Context(), principal(r.Context()).UserID, sid, r.PathValue("iid"))
	if err != nil {
		return a.readerErr(err)
	}
	writeJSON(w, 200, map[string]any{"item": it, "notice": reader.Notice})
	return nil
}

func (a *API) setRead(w http.ResponseWriter, r *http.Request, read bool) error {
	sid, err := pathInt(r, "sid")
	if err != nil {
		return err
	}
	if err := a.Reader.MarkRead(r.Context(), principal(r.Context()).UserID, sid, r.PathValue("iid"), read); err != nil {
		return a.readerErr(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) markRead(w http.ResponseWriter, r *http.Request) error   { return a.setRead(w, r, true) }
func (a *API) markUnread(w http.ResponseWriter, r *http.Request) error { return a.setRead(w, r, false) }
