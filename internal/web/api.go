package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chrisbodhi/blygd/internal/account"
	"github.com/chrisbodhi/blygd/internal/blyg"
	"github.com/chrisbodhi/blygd/internal/hopper"
	"github.com/chrisbodhi/blygd/internal/reader"
	"github.com/chrisbodhi/blygd/internal/studio"
)

// API is the private write surface. It is host-rooted (/api/…), never under
// an origin: a studio and its API are client internals, not protocol (§15.1,
// §16.6). Every route takes a bearer token and checks one scope.
type API struct {
	Pool     *pgxpool.Pool
	Accounts *account.Accounts
	Studio   *studio.Studio
	Hoppers  *hopper.Store
	Reader   *reader.Reader
	Log      *slog.Logger
}

type principalKey struct{}

func principal(ctx context.Context) *account.Principal {
	p, _ := ctx.Value(principalKey{}).(*account.Principal)
	return p
}

// Handler returns the API's routes.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	route := func(pattern, scope string, fn func(http.ResponseWriter, *http.Request) error) {
		mux.Handle(pattern, a.guard(scope, fn))
	}
	route("GET /api/me", "", a.me)
	route("GET /api/origin", account.ScopeItemsRead, a.getOrigin)
	route("PATCH /api/origin", account.ScopeOriginWrite, a.patchOrigin)

	route("GET /api/items", account.ScopeItemsRead, a.listItems)
	route("POST /api/items", account.ScopeItemsWrite, a.createItem)
	route("GET /api/items/{id}", account.ScopeItemsRead, a.getItem)
	route("PATCH /api/items/{id}", account.ScopeItemsWrite, a.patchItem)
	route("DELETE /api/items/{id}", account.ScopeItemsWrite, a.deleteItem)
	route("POST /api/items/{id}/publish", account.ScopeItemsPublish, a.publish)
	route("POST /api/items/{id}/withdraw", account.ScopeItemsWithdraw, a.withdraw)
	route("POST /api/items/{id}/versions/{n}/pin", account.ScopeItemsPin, a.pin)

	route("GET /api/hoppers", account.ScopeHoppersRead, a.listHoppers)
	route("POST /api/hoppers", account.ScopeHoppersWrite, a.createHopper)
	route("GET /api/hoppers/{hid}", account.ScopeHoppersRead, a.getHopper)
	route("PATCH /api/hoppers/{hid}", account.ScopeHoppersWrite, a.renameHopper)
	route("DELETE /api/hoppers/{hid}", account.ScopeHoppersWrite, a.deleteHopper)
	route("POST /api/hoppers/{hid}/members", account.ScopeHoppersWrite, a.addMember)
	route("DELETE /api/hoppers/{hid}/members/{mid}", account.ScopeHoppersWrite, a.removeMember)

	route("GET /api/mentions", account.ScopeMentionsRead, a.listMentions)
	route("GET /api/outbound", account.ScopeMentionsRead, a.listOutbound)

	a.readerRoutes(route)

	route("GET /api/tokens", account.ScopeTokensWrite, a.listTokens)
	route("POST /api/tokens", account.ScopeTokensWrite, a.mintToken)
	route("DELETE /api/tokens/{tid}", account.ScopeTokensWrite, a.revokeToken)

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such route")
	})
	return mux
}

// httpError carries a status and code out of a handler.
type httpError struct {
	status int
	code   string
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func badRequest(msg string) error { return &httpError{http.StatusBadRequest, "invalid", msg} }

func (a *API) guard(scope string, fn func(http.ResponseWriter, *http.Request) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, err := a.Accounts.Authenticate(r.Context(), r.Header.Get("Authorization"))
		if errors.Is(err, account.ErrUnauthorized) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="blygd"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
			return
		}
		if err != nil {
			a.internal(w, r, err)
			return
		}
		if scope != "" && !p.Can(scope) {
			writeError(w, http.StatusForbidden, "forbidden", "token lacks scope "+scope)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), principalKey{}, p))
		if err := fn(w, r); err != nil {
			a.handleErr(w, r, err)
		}
	})
}

func (a *API) handleErr(w http.ResponseWriter, r *http.Request, err error) {
	var he *httpError
	var se *studio.Error
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &he):
		writeError(w, he.status, he.code, he.msg)
	case errors.As(err, &se):
		status := map[string]int{"not_found": 404, "invalid": 422, "conflict": 409}[se.Code]
		if status == 0 {
			status = 500
		}
		writeError(w, status, se.Code, se.Msg)
	case errors.Is(err, hopper.ErrNotFound):
		writeError(w, 404, "not_found", "not found")
	case errors.Is(err, hopper.ErrExists):
		writeError(w, 409, "conflict", err.Error())
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		writeError(w, 409, "conflict", "already exists")
	case errors.As(err, &pgErr) && pgErr.Code == "P0001":
		// An invariant trigger refused the change (schema.sql).
		writeError(w, 409, "conflict", pgErr.Message)
	default:
		a.internal(w, r, err)
	}
}

func (a *API) internal(w http.ResponseWriter, r *http.Request, err error) {
	a.Log.Error("api", "method", r.Method, "path", r.URL.Path, "err", err)
	writeError(w, 500, "internal", "internal error")
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := blyg.Marshal(v)
	if err != nil {
		http.Error(w, "encoding error", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(body) //nolint:errcheck
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest("request body: " + err.Error())
	}
	return nil
}

func (a *API) origin(r *http.Request) (*studio.Origin, error) {
	return a.Studio.OriginForOwner(r.Context(), principal(r.Context()).UserID)
}

func pathInt(r *http.Request, name string) (int64, error) {
	n, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || n <= 0 {
		return 0, badRequest(name + " must be a positive integer")
	}
	return n, nil
}

// ── me & origin ──────────────────────────────────────────────

func (a *API) me(w http.ResponseWriter, r *http.Request) error {
	p := principal(r.Context())
	o, err := a.origin(r)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"handle": p.Handle, "scopes": p.Scopes, "origin": o.BaseURL})
	return nil
}

type originView struct {
	Origin         string          `json:"origin"`
	Title          string          `json:"title"`
	Description    string          `json:"description"`
	Author         json.RawMessage `json:"author"`
	Byline         json.RawMessage `json:"byline"`
	AcceptMentions bool            `json:"accept_mentions"`
}

func viewOrigin(o *studio.Origin) originView {
	return originView{o.BaseURL, o.Title, o.Description, o.Author, o.Byline, o.AcceptMentions}
}

func (a *API) getOrigin(w http.ResponseWriter, r *http.Request) error {
	o, err := a.origin(r)
	if err != nil {
		return err
	}
	writeJSON(w, 200, viewOrigin(o))
	return nil
}

func (a *API) patchOrigin(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Title          *string         `json:"title"`
		Description    *string         `json:"description"`
		Author         json.RawMessage `json:"author"`
		Byline         json.RawMessage `json:"byline"`
		AcceptMentions *bool           `json:"accept_mentions"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	o, err := a.origin(r)
	if err != nil {
		return err
	}
	o, err = a.Studio.UpdateOrigin(r.Context(), o.ID, studio.OriginSettings{
		Title: in.Title, Description: in.Description, Author: in.Author, Byline: in.Byline, AcceptMentions: in.AcceptMentions,
	})
	if err != nil {
		return err
	}
	writeJSON(w, 200, viewOrigin(o))
	return nil
}

// ── items ────────────────────────────────────────────────────

type itemView struct {
	*studio.Item
	State    string            `json:"state"`
	URL      string            `json:"url,omitempty"`
	Versions []*studio.Version `json:"versions,omitempty"`
}

func viewItem(o *studio.Origin, it *studio.Item, vs []*studio.Version) itemView {
	v := itemView{Item: it, State: it.State(), Versions: vs}
	if it.Page != nil {
		v.URL = o.BaseURL + *it.Page
	}
	return v
}

func (a *API) listItems(w http.ResponseWriter, r *http.Request) error {
	o, err := a.origin(r)
	if err != nil {
		return err
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := a.Studio.ListItems(r.Context(), o.ID, r.URL.Query().Get("state"), limit)
	if err != nil {
		return err
	}
	out := make([]itemView, 0, len(items))
	for _, it := range items {
		out = append(out, viewItem(o, it, nil))
	}
	writeJSON(w, 200, map[string]any{"items": out})
	return nil
}

func (a *API) createItem(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Kind       string          `json:"kind"`
		ContentMD  string          `json:"content_md"`
		ForkedFrom *blyg.Reference `json:"forked_from"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	o, err := a.origin(r)
	if err != nil {
		return err
	}
	it, err := a.Studio.CreateDraft(r.Context(), o.ID, studio.NewDraft{Kind: in.Kind, ContentMD: in.ContentMD, ForkedFrom: in.ForkedFrom})
	if err != nil {
		return err
	}
	writeJSON(w, 201, viewItem(o, it, nil))
	return nil
}

func (a *API) getItem(w http.ResponseWriter, r *http.Request) error {
	o, err := a.origin(r)
	if err != nil {
		return err
	}
	it, vs, err := a.Studio.GetItem(r.Context(), o.ID, r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, 200, viewItem(o, it, vs))
	return nil
}

func (a *API) patchItem(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		ContentMD *string `json:"content_md"`
		Note      *string `json:"note"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	o, err := a.origin(r)
	if err != nil {
		return err
	}
	it, err := a.Studio.UpdateDraft(r.Context(), o.ID, r.PathValue("id"), in.ContentMD, in.Note)
	if errors.Is(err, pgx.ErrNoRows) {
		return &httpError{404, "not_found", "item not found"}
	}
	if err != nil {
		return err
	}
	writeJSON(w, 200, viewItem(o, it, nil))
	return nil
}

func (a *API) deleteItem(w http.ResponseWriter, r *http.Request) error {
	o, err := a.origin(r)
	if err != nil {
		return err
	}
	if err := a.Studio.DeleteDraft(r.Context(), o.ID, r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

type noteBody struct {
	Note *string `json:"note"`
}

func optionalNote(r *http.Request) (*string, error) {
	var in noteBody
	if r.ContentLength == 0 {
		return nil, nil
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return in.Note, nil
}

func (a *API) publish(w http.ResponseWriter, r *http.Request) error {
	note, err := optionalNote(r)
	if err != nil {
		return err
	}
	o, err := a.origin(r)
	if err != nil {
		return err
	}
	res, err := a.Studio.Publish(r.Context(), o.ID, r.PathValue("id"), note)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"item": viewItem(o, res.Item, nil), "version": res.Version, "warnings": res.Warnings})
	return nil
}

func (a *API) withdraw(w http.ResponseWriter, r *http.Request) error {
	note, err := optionalNote(r)
	if err != nil {
		return err
	}
	o, err := a.origin(r)
	if err != nil {
		return err
	}
	res, err := a.Studio.Withdraw(r.Context(), o.ID, r.PathValue("id"), note)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"item": viewItem(o, res.Item, nil), "version": res.Version})
	return nil
}

func (a *API) pin(w http.ResponseWriter, r *http.Request) error {
	n, err := pathInt(r, "n")
	if err != nil {
		return err
	}
	o, err := a.origin(r)
	if err != nil {
		return err
	}
	v, err := a.Studio.Pin(r.Context(), o.ID, r.PathValue("id"), int(n))
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"version": v, "url": blyg.PinURL(o.BaseURL, r.PathValue("id"), int(n))})
	return nil
}

// ── hoppers ──────────────────────────────────────────────────

func (a *API) listHoppers(w http.ResponseWriter, r *http.Request) error {
	hs, err := a.Hoppers.List(r.Context(), principal(r.Context()).UserID)
	if err != nil {
		return err
	}
	if hs == nil {
		hs = []*hopper.Hopper{}
	}
	writeJSON(w, 200, map[string]any{"hoppers": hs})
	return nil
}

func (a *API) createHopper(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	h, err := a.Hoppers.Create(r.Context(), principal(r.Context()).UserID, in.Name)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return err
		}
		return badRequest(err.Error())
	}
	writeJSON(w, 201, h)
	return nil
}

func (a *API) getHopper(w http.ResponseWriter, r *http.Request) error {
	id, err := pathInt(r, "hid")
	if err != nil {
		return err
	}
	h, members, err := a.Hoppers.Get(r.Context(), principal(r.Context()).UserID, id)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"hopper": h, "members": members})
	return nil
}

func (a *API) renameHopper(w http.ResponseWriter, r *http.Request) error {
	id, err := pathInt(r, "hid")
	if err != nil {
		return err
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	h, err := a.Hoppers.Rename(r.Context(), principal(r.Context()).UserID, id, in.Name)
	if err != nil {
		if errors.Is(err, hopper.ErrNotFound) {
			return err
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return err
		}
		return badRequest(err.Error())
	}
	writeJSON(w, 200, h)
	return nil
}

func (a *API) deleteHopper(w http.ResponseWriter, r *http.Request) error {
	id, err := pathInt(r, "hid")
	if err != nil {
		return err
	}
	if err := a.Hoppers.Delete(r.Context(), principal(r.Context()).UserID, id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) addMember(w http.ResponseWriter, r *http.Request) error {
	id, err := pathInt(r, "hid")
	if err != nil {
		return err
	}
	var in struct {
		OwnItemID string `json:"own_item_id"`
		Imported  *struct {
			Origin         string `json:"origin"`
			SubscriptionID int64  `json:"subscription_id"`
			ID             string `json:"id"`
		} `json:"imported"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	uid := principal(r.Context()).UserID
	var mid int64
	switch {
	case in.OwnItemID != "" && in.Imported == nil:
		mid, err = a.Hoppers.AddOwn(r.Context(), uid, id, in.OwnItemID)
	case in.OwnItemID == "" && in.Imported != nil && in.Imported.SubscriptionID > 0:
		mid, err = a.Hoppers.AddImportedBySubscription(r.Context(), uid, id, in.Imported.SubscriptionID, in.Imported.ID)
	case in.OwnItemID == "" && in.Imported != nil:
		origin, nerr := blyg.NormalizeOrigin(in.Imported.Origin)
		if nerr != nil {
			return badRequest("imported.origin must be an http(s) origin")
		}
		mid, err = a.Hoppers.AddImported(r.Context(), uid, id, origin, in.Imported.ID)
	default:
		return badRequest("give exactly one of own_item_id or imported")
	}
	if err != nil {
		return err
	}
	writeJSON(w, 201, map[string]any{"id": mid})
	return nil
}

func (a *API) removeMember(w http.ResponseWriter, r *http.Request) error {
	hid, err := pathInt(r, "hid")
	if err != nil {
		return err
	}
	mid, err := pathInt(r, "mid")
	if err != nil {
		return err
	}
	if err := a.Hoppers.RemoveMember(r.Context(), principal(r.Context()).UserID, hid, mid); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ── mentions ─────────────────────────────────────────────────

type mentionView struct {
	ID            int64           `json:"id"`
	TargetItemID  string          `json:"target_item_id"`
	SourceURL     string          `json:"source_url"`
	TargetURL     string          `json:"target_url"`
	Status        string          `json:"status"`
	Relation      *string         `json:"relation"`
	SourceOrigin  *string         `json:"source_origin"`
	SourceItemID  *string         `json:"source_item_id"`
	SourceVersion *int            `json:"source_version"`
	SourceKind    *string         `json:"source_kind"`
	SourceAuthor  json.RawMessage `json:"source_author"`
	SourcePage    *string         `json:"source_page"`
	Failure       *string         `json:"failure"`
	ReceivedAt    time.Time       `json:"received_at"`
	VerifiedAt    *time.Time      `json:"verified_at"`
}

func (a *API) listMentions(w http.ResponseWriter, r *http.Request) error {
	o, err := a.origin(r)
	if err != nil {
		return err
	}
	status := r.URL.Query().Get("status")
	if status != "" && !slices.Contains([]string{"pending", "verified", "failed", "gone"}, status) {
		return badRequest("unknown status")
	}
	rows, err := a.Pool.Query(r.Context(), `SELECT id, target_item_id, source_url, target_url, status, relation,
		source_origin, source_item_id, source_version, source_kind, source_author, source_page, failure, received_at, verified_at
		FROM inbound_mentions WHERE origin_id = $1 AND ($2 = '' OR status = $2)
		ORDER BY received_at DESC LIMIT 200`, o.ID, status)
	if err != nil {
		return err
	}
	ms, err := pgx.CollectRows(rows, pgx.RowToStructByPos[mentionView])
	if err != nil {
		return err
	}
	if ms == nil {
		ms = []mentionView{}
	}
	writeJSON(w, 200, map[string]any{"mentions": ms})
	return nil
}

type outboundView struct {
	ID            int64     `json:"id"`
	SourceItemID  string    `json:"source_item_id"`
	Relation      string    `json:"relation"`
	TargetURL     string    `json:"target_url"`
	TargetVersion *int      `json:"target_version"`
	Status        string    `json:"status"`
	Endpoint      *string   `json:"endpoint"`
	Attempts      int       `json:"attempts"`
	LastResult    *string   `json:"last_result"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (a *API) listOutbound(w http.ResponseWriter, r *http.Request) error {
	o, err := a.origin(r)
	if err != nil {
		return err
	}
	rows, err := a.Pool.Query(r.Context(), `SELECT id, source_item_id, relation, target_url, target_version, status,
		endpoint, attempts, last_result, updated_at FROM outbound_mentions WHERE origin_id = $1
		ORDER BY updated_at DESC LIMIT 200`, o.ID)
	if err != nil {
		return err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[outboundView])
	if err != nil {
		return err
	}
	if out == nil {
		out = []outboundView{}
	}
	writeJSON(w, 200, map[string]any{"outbound": out})
	return nil
}

// ── tokens ───────────────────────────────────────────────────

func (a *API) listTokens(w http.ResponseWriter, r *http.Request) error {
	ts, err := a.Accounts.ListTokens(r.Context(), principal(r.Context()).UserID)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"tokens": ts})
	return nil
}

func (a *API) mintToken(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	p := principal(r.Context())
	// No escalation: a token can only mint scopes it holds.
	for _, s := range in.Scopes {
		if !p.Can(s) || (s == account.ScopeAll && !p.Can(account.ScopeAll)) {
			return &httpError{403, "forbidden", "cannot grant scope " + s}
		}
	}
	secret, id, err := a.Accounts.Mint(r.Context(), p.UserID, in.Name, in.Scopes)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return err
		}
		return badRequest(err.Error())
	}
	writeJSON(w, 201, map[string]any{"id": id, "token": secret, "scopes": in.Scopes,
		"note": "This is the only time the token is shown."})
	return nil
}

func (a *API) revokeToken(w http.ResponseWriter, r *http.Request) error {
	id, err := pathInt(r, "tid")
	if err != nil {
		return err
	}
	ok, err := a.Accounts.Revoke(r.Context(), principal(r.Context()).UserID, id)
	if err != nil {
		return err
	}
	if !ok {
		return &httpError{404, "not_found", "token not found"}
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
