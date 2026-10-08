// Package reader is the import side: §12 resolution, the §13 importer and
// poller over the shared mirror (plane 2), and the per-user reading overlay
// (plane 3). It never writes surface_files and never reads another
// tenant's plane-1 tables: a same-instance origin is read through its
// public surface like any other (docs/PLAN.md, Phase 2 item 8).
package reader

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/chrisbodhi/blygd/internal/blyg"
	"github.com/chrisbodhi/blygd/internal/fetch"
	"github.com/chrisbodhi/blygd/internal/studio"
)

// Fetcher is the one way the reader reads a URL. *fetch.Client satisfies
// it; so does LocalFirst.
type Fetcher interface {
	GetIf(ctx context.Context, rawURL string, lim fetch.Limits, accept string, cond fetch.Conditional) (*fetch.Response, error)
}

// LocalFirst serves URLs inside this instance's own origins from their
// stored surface files, and sends everything else to Remote.
//
// A same-instance subscription therefore sees exactly the bytes a remote
// reader would: the rendered item documents, index and feed, never the
// tables behind them. Joining another tenant's items directly would skip
// the watermark and withdrawal rules (§13.3–13.4) for local origins only.
type LocalFirst struct {
	Studio *studio.Studio
	Remote Fetcher
}

// GetIf implements Fetcher.
func (l *LocalFirst) GetIf(ctx context.Context, rawURL string, lim fetch.Limits, accept string, cond fetch.Conditional) (*fetch.Response, error) {
	if l.Studio != nil {
		if u, err := url.Parse(rawURL); err == nil {
			u.RawQuery, u.ForceQuery = "", false
			if norm, err := blyg.NormalizeURL(u.String()); err == nil {
				o, err := l.Studio.OriginForURL(ctx, norm)
				switch {
				case err == nil:
					return l.serve(ctx, o, norm, lim, cond)
				case !studio.IsCode(err, "not_found"):
					return nil, err
				}
			}
		}
	}
	return l.Remote.GetIf(ctx, rawURL, lim, accept, cond)
}

func (l *LocalFirst) serve(ctx context.Context, o *studio.Origin, u string, lim fetch.Limits, cond fetch.Conditional) (*fetch.Response, error) {
	rel, _ := studio.TrimOrigin(o.BaseURL, u)
	f, err := l.Studio.GetFile(ctx, o.ID, rel)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	if f == nil {
		return &fetch.Response{FinalURL: u, Status: http.StatusNotFound, Header: h}, nil
	}
	h.Set("Content-Type", f.ContentType)
	h.Set("ETag", f.ETag)
	if cond.ETag != "" && cond.ETag == f.ETag {
		return &fetch.Response{FinalURL: u, Status: http.StatusNotModified, Header: h}, nil
	}
	if int64(len(f.Body)) > lim.MaxBody {
		return nil, fetch.ErrTooLarge
	}
	return &fetch.Response{FinalURL: u, Status: http.StatusOK, Header: h, Body: f.Body}, nil
}

// now is the reader's clock, overridable in tests.
func (r *Reader) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}
