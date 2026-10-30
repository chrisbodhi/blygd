package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/chrisbodhi/blygd/internal/account"
	"github.com/chrisbodhi/blygd/internal/reader"
)

// The reading API: subscriptions (§12), the merged feed (§13.7), item
// reads, and read marks. Every imported byte is returned verbatim with the
// §14 notice: the client sanitizes.

func (a *API) readerRoutes() []route {
	return []route{
		{"GET /api/subscriptions", account.ScopeReaderRead, a.listSubscriptions},
		{"POST /api/subscriptions", account.ScopeReaderWrite, a.subscribe},
		{"GET /api/subscriptions/{sid}", account.ScopeReaderRead, a.getSubscription},
		{"PATCH /api/subscriptions/{sid}", account.ScopeReaderWrite, a.patchSubscription},
		{"DELETE /api/subscriptions/{sid}", account.ScopeReaderWrite, a.unsubscribe},
		{"POST /api/subscriptions/{sid}/resolve", account.ScopeReaderRead, a.reresolve},
		{"POST /api/subscriptions/{sid}/move", account.ScopeReaderWrite, a.moveSubscription},
		{"POST /api/subscriptions/{sid}/poll", account.ScopeReaderWrite, a.pollSubscription},
		{"POST /api/subscriptions/imports", account.ScopeReaderWrite, a.importSubscriptions},
		{"GET /api/subscriptions/imports", account.ScopeReaderRead, a.listImports},
		{"DELETE /api/subscriptions/imports", account.ScopeReaderWrite, a.clearImports},

		{"GET /api/reader/feed", account.ScopeReaderRead, a.readerFeed},
		{"GET /api/reader/items/{sid}/{iid}", account.ScopeReaderRead, a.readerItem},
		{"PUT /api/reader/items/{sid}/{iid}/read", account.ScopeReaderWrite, a.markRead},
		{"DELETE /api/reader/items/{sid}/{iid}/read", account.ScopeReaderWrite, a.markUnread},
		{"POST /api/reader/read", account.ScopeReaderWrite, a.markAllRead},
	}
}

func (a *API) subscriptionView(r *http.Request, sid int64) (*reader.Subscription, error) {
	o, err := a.origin(r)
	if err != nil {
		return nil, err
	}
	s, err := a.Reader.GetSubscription(r.Context(), principal(r.Context()).UserID, o.ID, sid)
	return s, err
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

// importSubscriptions queues an OPML list, sent or fetched, for the
// importer to resolve entry by entry.
func (a *API) importSubscriptions(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		OPML string `json:"opml"`
		URL  string `json:"url"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	uid := principal(r.Context()).UserID
	var res *reader.ImportResult
	var err error
	switch {
	case (in.OPML == "") == (in.URL == ""):
		return badRequest("send one of opml or url")
	case in.OPML != "":
		res, err = a.Reader.ImportOPML(r.Context(), uid, []byte(in.OPML))
	default:
		res, err = a.Reader.ImportFrom(r.Context(), uid, in.URL)
	}
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, res)
	return nil
}

func (a *API) listImports(w http.ResponseWriter, r *http.Request) error {
	es, err := a.Reader.Imports(r.Context(), principal(r.Context()).UserID, r.URL.Query().Get("status"))
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"imports": es})
	return nil
}

func (a *API) clearImports(w http.ResponseWriter, r *http.Request) error {
	n, err := a.Reader.ClearImports(r.Context(), principal(r.Context()).UserID, r.URL.Query().Get("status"))
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"deleted": n})
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
			return err
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
				return err
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
		return err
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
	if err != nil {
		return err
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
	if err != nil {
		return err
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
				return err
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

// pollSubscription polls one subscription now, within §12.3's etiquette,
// and returns it as it stands after the poll.
func (a *API) pollSubscription(w http.ResponseWriter, r *http.Request) error {
	sid, err := pathInt(r, "sid")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	err = a.Reader.PollFor(ctx, principal(r.Context()).UserID, sid)
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
	page, err := a.Reader.Feed(r.Context(), principal(r.Context()).UserID, q)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"items": page.Items, "next": page.Next, "as_of": page.AsOf, "notice": reader.Notice})
	return nil
}

// markAllRead marks read what a feed read at as_of could have shown, for
// one subscription or all of them.
func (a *API) markAllRead(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		AsOf           *time.Time `json:"as_of"`
		SubscriptionID int64      `json:"subscription_id"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if in.AsOf == nil {
		return badRequest("as_of is required: send the one from the feed you showed")
	}
	if in.SubscriptionID < 0 {
		return badRequest("subscription_id must be a positive integer")
	}
	n, err := a.Reader.MarkAllRead(r.Context(), principal(r.Context()).UserID, in.SubscriptionID, *in.AsOf)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"marked": n})
	return nil
}

func (a *API) readerItem(w http.ResponseWriter, r *http.Request) error {
	sid, err := pathInt(r, "sid")
	if err != nil {
		return err
	}
	it, err := a.Reader.GetItem(r.Context(), principal(r.Context()).UserID, sid, r.PathValue("iid"))
	if err != nil {
		return err
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
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) markRead(w http.ResponseWriter, r *http.Request) error   { return a.setRead(w, r, true) }
func (a *API) markUnread(w http.ResponseWriter, r *http.Request) error { return a.setRead(w, r, false) }
