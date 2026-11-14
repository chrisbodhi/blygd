package web

import (
	"encoding/json"
	"net/http"

	"github.com/chrisbodhi/blygd/internal/account"
	"github.com/chrisbodhi/blygd/internal/reader"
)

// Filters: a reader's topics and what happens to posts about them, judged
// by the classifier the operator configured (DESIGN.md "Filters"). They
// are private reading state, under the reader scopes, and never on the
// wire.

func (a *API) filterRoutes() []route {
	return []route{
		{"GET /api/filters", account.ScopeReaderRead, a.listFilters},
		{"POST /api/filters", account.ScopeReaderWrite, a.createFilter},
		{"GET /api/filters/{fid}", account.ScopeReaderRead, a.getFilter},
		{"PATCH /api/filters/{fid}", account.ScopeReaderWrite, a.patchFilter},
		{"DELETE /api/filters/{fid}", account.ScopeReaderWrite, a.deleteFilter},
		{"PUT /api/reader/items/{sid}/{iid}/filters/{fid}", account.ScopeReaderWrite, a.setOverride},
		{"DELETE /api/reader/items/{sid}/{iid}/filters/{fid}", account.ScopeReaderWrite, a.clearOverride},
	}
}

// filterBody is a filter as a client sends it. subscription_id is raw so
// that absent (unchanged) and null (every subscription) differ.
type filterBody struct {
	Name           *string         `json:"name"`
	Description    *string         `json:"description"`
	Match          *string         `json:"match"`
	Threshold      *float64        `json:"threshold"`
	Action         *string         `json:"action"`
	Pending        *string         `json:"pending"`
	Enabled        *bool           `json:"enabled"`
	AutoTune       *bool           `json:"auto_tune"`
	SubscriptionID json.RawMessage `json:"subscription_id"`
}

func (b filterBody) input() (reader.FilterInput, error) {
	in := reader.FilterInput{Name: b.Name, Description: b.Description, Match: b.Match, Threshold: b.Threshold,
		Action: b.Action, Pending: b.Pending, Enabled: b.Enabled, AutoTune: b.AutoTune}
	if b.SubscriptionID != nil {
		in.SetScope = true
		if err := json.Unmarshal(b.SubscriptionID, &in.Scope); err != nil || (in.Scope != nil && *in.Scope <= 0) {
			return in, badRequest("subscription_id must be a positive integer or null")
		}
	}
	return in, nil
}

func (a *API) listFilters(w http.ResponseWriter, r *http.Request) error {
	fs, err := a.Reader.Filters(r.Context(), principal(r.Context()).UserID)
	if err != nil {
		return err
	}
	st, err := a.Reader.ClassifierStatus(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"filters": fs, "classifier": st})
	return nil
}

func (a *API) createFilter(w http.ResponseWriter, r *http.Request) error {
	var b filterBody
	if err := decode(r, &b); err != nil {
		return err
	}
	in, err := b.input()
	if err != nil {
		return err
	}
	f, err := a.Reader.CreateFilter(r.Context(), principal(r.Context()).UserID, in)
	if err != nil {
		return err
	}
	writeJSON(w, 201, f)
	return nil
}

func (a *API) getFilter(w http.ResponseWriter, r *http.Request) error {
	fid, err := pathInt(r, "fid")
	if err != nil {
		return err
	}
	f, err := a.Reader.GetFilter(r.Context(), principal(r.Context()).UserID, fid)
	if err != nil {
		return err
	}
	writeJSON(w, 200, f)
	return nil
}

func (a *API) patchFilter(w http.ResponseWriter, r *http.Request) error {
	fid, err := pathInt(r, "fid")
	if err != nil {
		return err
	}
	var b filterBody
	if err := decode(r, &b); err != nil {
		return err
	}
	in, err := b.input()
	if err != nil {
		return err
	}
	f, err := a.Reader.UpdateFilter(r.Context(), principal(r.Context()).UserID, fid, in)
	if err != nil {
		return err
	}
	writeJSON(w, 200, f)
	return nil
}

func (a *API) deleteFilter(w http.ResponseWriter, r *http.Request) error {
	fid, err := pathInt(r, "fid")
	if err != nil {
		return err
	}
	if err := a.Reader.DeleteFilter(r.Context(), principal(r.Context()).UserID, fid); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// setOverride records the reader's rating of an item under one of their
// filters: {"matched": true} or false. It wins over the classifier for
// that item, may tune the filter, and answers with the filter as the
// rating left it.
func (a *API) setOverride(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Matched *bool `json:"matched"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if in.Matched == nil {
		return badRequest("matched is required: true or false")
	}
	return a.override(w, r, in.Matched)
}

func (a *API) clearOverride(w http.ResponseWriter, r *http.Request) error {
	return a.override(w, r, nil)
}

func (a *API) override(w http.ResponseWriter, r *http.Request, matched *bool) error {
	sid, err := pathInt(r, "sid")
	if err != nil {
		return err
	}
	fid, err := pathInt(r, "fid")
	if err != nil {
		return err
	}
	f, err := a.Reader.SetOverride(r.Context(), principal(r.Context()).UserID, sid, r.PathValue("iid"), fid, matched)
	if err != nil {
		return err
	}
	writeJSON(w, 200, f)
	return nil
}
