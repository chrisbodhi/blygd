// Package reader is the import side: §12 resolution, the §13 importer and
// poller over the shared mirror (plane 2), and the per-user reading overlay
// (plane 3). It never writes surface_files and never reads another
// tenant's plane-1 tables: a same-instance origin is read through its
// public surface like any other (docs/PLAN.md, Phase 2 item 8).
package reader

import (
	"context"
	"time"

	"github.com/chrisbodhi/blygd/internal/fetch"
)

// Fetcher is the one way the reader reads a URL. *fetch.Client satisfies
// it, and answers this instance's own origins in-process (fetch.Client's
// Local): a same-instance subscription sees exactly the bytes a remote
// reader would, the rendered item documents, index and feed, never the
// tables behind them. Joining another tenant's items directly would skip
// the watermark and withdrawal rules (§13.3–13.4) for local origins only.
type Fetcher interface {
	GetIf(ctx context.Context, rawURL string, lim fetch.Limits, accept string, cond fetch.Conditional) (*fetch.Response, error)
}

// now is the reader's clock, overridable in tests.
func (r *Reader) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}
