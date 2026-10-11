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
	"github.com/chrisbodhi/blygd/internal/events"
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
	Events   *events.Hub // GET /api/events; nil means unavailable
	Log      *slog.Logger

	streams streamCount
}

type principalKey struct{}

func principal(ctx context.Context) *account.Principal {
	p, _ := ctx.Value(principalKey{}).(*account.Principal)
	return p
}

// Handler returns the API's routes.
// route is one /api/ route: its mux pattern, the scope a token needs (""
// for any token), and its handler. The table is the single source both the
// mux and the OpenAPI coverage test read.
type route struct {
	pattern string
	scope   string
	fn      func(http.ResponseWriter, *http.Request) error
}

func (a *API) routes() []route {
	rs := []route{
		{"GET /api/me", "", a.me},
		{"GET /api/events", "", a.streamEvents},
		{"GET /api/origin", account.ScopeItemsRead, a.getOrigin},
		{"PATCH /api/origin", account.ScopeOriginWrite, a.patchOrigin},

		{"GET /api/items", account.ScopeItemsRead, a.listItems},
		{"POST /api/items", account.ScopeItemsWrite, a.createItem},
		{"GET /api/items/{id}", account.ScopeItemsRead, a.getItem},
		{"PATCH /api/items/{id}", account.ScopeItemsWrite, a.patchItem},
		{"DELETE /api/items/{id}", account.ScopeItemsWrite, a.deleteItem},
		{"POST /api/items/{id}/publish", account.ScopeItemsPublish, a.publish},
		{"POST /api/items/{id}/withdraw", account.ScopeItemsWithdraw, a.withdraw},
		{"POST /api/items/{id}/versions/{n}/pin", account.ScopeItemsPin, a.pin},

		{"GET /api/hoppers", account.ScopeHoppersRead, a.listHoppers},
		{"POST /api/hoppers", account.ScopeHoppersWrite, a.createHopper},
		{"GET /api/hoppers/{hid}", account.ScopeHoppersRead, a.getHopper},
		{"PATCH /api/hoppers/{hid}", account.ScopeHoppersWrite, a.patchHopper},
		{"DELETE /api/hoppers/{hid}", account.ScopeHoppersWrite, a.deleteHopper},
		{"POST /api/hoppers/{hid}/members", account.ScopeHoppersWrite, a.addMember},
		{"DELETE /api/hoppers/{hid}/members/{mid}", account.ScopeHoppersWrite, a.removeMember},

		{"GET /api/media", account.ScopeItemsRead, a.listMedia},
		{"POST /api/media", account.ScopeItemsWrite, a.uploadMedia},
		{"DELETE /api/media/{file}", account.ScopeItemsWrite, a.deleteMedia},

		{"GET /api/mentions", account.ScopeMentionsRead, a.listMentions},
		{"PUT /api/mentions/{mid}/approval", account.ScopeMentionsWrite, a.approveMention},
		{"DELETE /api/mentions/{mid}/approval", account.ScopeMentionsWrite, a.hideMention},
		{"GET /api/mention-blocks", account.ScopeMentionsRead, a.listBlocks},
		{"POST /api/mention-blocks", account.ScopeMentionsWrite, a.blockSource},
		{"DELETE /api/mention-blocks/{bid}", account.ScopeMentionsWrite, a.unblock},
		{"GET /api/outbound", account.ScopeMentionsRead, a.listOutbound},
	}
	rs = append(rs, a.readerRoutes()...)
	return append(rs,
		route{"GET /api/tokens", account.ScopeTokensWrite, a.listTokens},
		route{"POST /api/tokens", account.ScopeTokensWrite, a.mintToken},
		route{"DELETE /api/tokens/{tid}", account.ScopeTokensWrite, a.revokeToken},
	)
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, r := range a.routes() {
		mux.Handle(r.pattern, a.guard(r.scope, r.fn))
	}
	// The API description is public: it says what exists, never what a
	// user has (docs/PLAN.md, Operations).
	mux.HandleFunc("GET "+openAPIPath, serveOpenAPI)
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
		if len(se.Problems) > 0 {
			writeJSON(w, status, map[string]any{"error": se.Code, "message": se.Msg, "problems": se.Problems})
			return
		}
		writeError(w, status, se.Code, se.Msg)
	case errors.Is(err, hopper.ErrNotFound):
		writeError(w, 404, "not_found", "not found")
	case errors.Is(err, hopper.ErrExists):
		writeError(w, 409, "conflict", err.Error())
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		writeError(w, 409, "conflict", "already exists")
	case errors.As(err, &pgErr) && pgErr.Code == "P0001":
		// An invariant trigger refused the change (internal/db/migrations).
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
	// Listings only.
	ContentHTML        string `json:"content_html,omitempty"`
	UnpublishedChanges bool   `json:"unpublished_changes,omitempty"`
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
	for _, li := range items {
		v := viewItem(o, li.Item, nil)
		v.ContentHTML, v.UnpublishedChanges = li.ContentHTML, li.UnpublishedChanges
		out = append(out, v)
	}
	writeJSON(w, 200, map[string]any{"items": out})
	return nil
}

func (a *API) createItem(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Kind       string             `json:"kind"`
		ContentMD  string             `json:"content_md"`
		ForkedFrom *blyg.Reference    `json:"forked_from"`
		StubOf     *studio.StubTarget `json:"stub_of"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	o, err := a.origin(r)
	if err != nil {
		return err
	}
	it, err := a.Studio.CreateDraft(r.Context(), o.ID, studio.NewDraft{Kind: in.Kind, ContentMD: in.ContentMD, ForkedFrom: in.ForkedFrom, StubOf: in.StubOf})
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
		ContentMD  *string         `json:"content_md"`
		Note       *string         `json:"note"`
		Generation json.RawMessage `json:"generation"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	o, err := a.origin(r)
	if err != nil {
		return err
	}
	it, err := a.Studio.UpdateDraft(r.Context(), o.ID, r.PathValue("id"), studio.DraftEdit{ContentMD: in.ContentMD, Note: in.Note, Generation: in.Generation})
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
	var in struct {
		Note          *string `json:"note"`
		NoteGenerated bool    `json:"note_generated"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &in); err != nil {
			return err
		}
	}
	o, err := a.origin(r)
	if err != nil {
		return err
	}
	res, err := a.Studio.Publish(r.Context(), o.ID, r.PathValue("id"), studio.PublishOptions{Note: in.Note, NoteGenerated: in.NoteGenerated})
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

// ── media ────────────────────────────────────────────────────

func (a *API) listMedia(w http.ResponseWriter, r *http.Request) error {
	o, err := a.origin(r)
	if err != nil {
		return err
	}
	ms, err := a.Studio.ListMedia(r.Context(), o)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"media": ms})
	return nil
}

// uploadMedia takes the raw bytes as the body, typed by Content-Type.
func (a *API) uploadMedia(w http.ResponseWriter, r *http.Request) error {
	o, err := a.origin(r)
	if err != nil {
		return err
	}
	limit := a.Studio.MediaMaxBytes()
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(limit)+1))
	if err != nil {
		return badRequest("reading the upload: " + err.Error())
	}
	if len(body) > limit {
		return &httpError{http.StatusRequestEntityTooLarge, "too_large", "media is over the cap of " + strconv.Itoa(limit) + " bytes"}
	}
	m, err := a.Studio.Upload(r.Context(), o, r.Header.Get("Content-Type"), body)
	if err != nil {
		return err
	}
	writeJSON(w, 201, m)
	return nil
}

func (a *API) deleteMedia(w http.ResponseWriter, r *http.Request) error {
	o, err := a.origin(r)
	if err != nil {
		return err
	}
	if err := a.Studio.DeleteMedia(r.Context(), o, "media/"+r.PathValue("file")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
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

// patchHopper renames a hopper and/or makes it public or private. Making a
// list public is a publishing act (§13.5), so it also needs origin:write,
// as the blogroll does (§11).
func (a *API) patchHopper(w http.ResponseWriter, r *http.Request) error {
	id, err := pathInt(r, "hid")
	if err != nil {
		return err
	}
	var in struct {
		Name   *string `json:"name"`
		Public *bool   `json:"public"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	p := principal(r.Context())
	if in.Public != nil && !p.Can(account.ScopeOriginWrite) {
		return &httpError{403, "forbidden", "changing whether a hopper is public needs scope " + account.ScopeOriginWrite}
	}
	if in.Name == nil && in.Public == nil {
		return badRequest("give name, public, or both")
	}
	var h *hopper.Hopper
	if in.Name != nil {
		if h, err = a.Hoppers.Rename(r.Context(), p.UserID, id, *in.Name); err != nil {
			var pgErr *pgconn.PgError
			if errors.Is(err, hopper.ErrNotFound) || errors.As(err, &pgErr) {
				return err
			}
			return badRequest(err.Error())
		}
	}
	if in.Public != nil {
		if h, err = a.Hoppers.SetPublic(r.Context(), p.UserID, id, *in.Public); err != nil {
			return err
		}
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
	SourceHost    string          `json:"source_host"`
	ApprovedAt    *time.Time      `json:"approved_at"` // shown on the target's page (§15.5)
	Blocked       bool            `json:"blocked"`
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
	// Mentions from blocked sources stay out of view unless asked for.
	withBlocked := r.URL.Query().Get("include_blocked") == "true"
	rows, err := a.Pool.Query(r.Context(), `SELECT * FROM (SELECT m.id, m.target_item_id, m.source_url, m.target_url, m.status, m.relation,
		m.source_origin, m.source_item_id, m.source_version, m.source_kind, m.source_author, m.source_page, m.failure, m.received_at, m.verified_at,
		m.source_host, m.approved_at,
		EXISTS (SELECT 1 FROM mention_blocks b WHERE b.origin_id = m.origin_id
			AND ((b.kind = 'host' AND b.value = m.source_host) OR (b.kind = 'domain' AND b.value = m.source_domain))) AS blocked
		FROM inbound_mentions m WHERE m.origin_id = $1 AND ($2 = '' OR m.status = $2)) x
		WHERE $3 OR NOT x.blocked
		ORDER BY x.received_at DESC LIMIT 200`, o.ID, status, withBlocked)
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

// approveMention shows a verified mention on its target's page. That
// publishes, so it also needs origin:write (§15.5).
func (a *API) approveMention(w http.ResponseWriter, r *http.Request) error {
	if !principal(r.Context()).Can(account.ScopeOriginWrite) {
		return &httpError{403, "forbidden", "showing a response publicly needs scope " + account.ScopeOriginWrite}
	}
	return a.setApproval(w, r, true)
}

// hideMention takes a response off the page. Narrowing what is public needs
// no more than moderation.
func (a *API) hideMention(w http.ResponseWriter, r *http.Request) error {
	return a.setApproval(w, r, false)
}

func (a *API) setApproval(w http.ResponseWriter, r *http.Request, approve bool) error {
	mid, err := pathInt(r, "mid")
	if err != nil {
		return err
	}
	o, err := a.origin(r)
	if err != nil {
		return err
	}
	if err := a.Studio.SetMentionApproval(r.Context(), o.ID, mid, approve); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) listBlocks(w http.ResponseWriter, r *http.Request) error {
	o, err := a.origin(r)
	if err != nil {
		return err
	}
	bs, err := a.Studio.ListBlocks(r.Context(), o.ID)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"blocks": bs})
	return nil
}

func (a *API) blockSource(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	o, err := a.origin(r)
	if err != nil {
		return err
	}
	b, err := a.Studio.BlockSource(r.Context(), o.ID, in.Kind, in.Value)
	if err != nil {
		return err
	}
	writeJSON(w, 201, b)
	return nil
}

func (a *API) unblock(w http.ResponseWriter, r *http.Request) error {
	bid, err := pathInt(r, "bid")
	if err != nil {
		return err
	}
	o, err := a.origin(r)
	if err != nil {
		return err
	}
	if err := a.Studio.Unblock(r.Context(), o.ID, bid); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
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
