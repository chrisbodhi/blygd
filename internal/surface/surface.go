// Package surface locates another origin's publication surface: it reads
// the origin's manifest and says where its feed, index, item documents and
// pinned versions live (§4, §6.1). It touches the network and nothing else,
// with no database and no state beyond a caller's Cache, so plane 1 may use
// it without reaching the import mirror.
package surface

import (
	"context"
	"encoding/json"

	"github.com/chrisbodhi/blygd/internal/blyg"
	"github.com/chrisbodhi/blygd/internal/fetch"
)

// Getter is the one way Locate reads a URL. *fetch.Client satisfies it.
type Getter interface {
	Get(ctx context.Context, rawURL string, lim fetch.Limits, accept string) (*fetch.Response, error)
}

// Located is what one read of an origin's manifest found. Its Surface is
// always usable: the manifest's when one was read, else the defaults.
type Located struct {
	blyg.Surface
	// Status is the manifest fetch's HTTP status, 0 when the fetch itself
	// failed.
	Status int
	// Manifest is nil unless the fetch returned 200 and the body parses as
	// a manifest (§12.1 step 2).
	Manifest map[string]json.RawMessage
	// URL is where the manifest was read, after redirects.
	URL string
}

// Found reports whether a manifest was read.
func (l Located) Found() bool { return l.Manifest != nil }

// Transient reports whether the read failed in a way that may pass: no
// response at all, a 429 or a 5xx.
func (l Located) Transient() bool { return l.Status == 0 || l.Status == 429 || l.Status >= 500 }

// Locate reads {origin}blyg.json, the one name the protocol fixes (§4).
// err is the transport error, if any; the Located is returned either way.
func Locate(ctx context.Context, g Getter, origin string, lim fetch.Limits) (Located, error) {
	if n, err := blyg.NormalizeOrigin(origin); err == nil {
		origin = n
	}
	l := Located{Surface: blyg.Surface{Origin: origin}}
	resp, err := g.Get(ctx, origin+"blyg.json", lim, "application/json")
	if err != nil {
		return l, err
	}
	l.Status, l.URL = resp.Status, resp.FinalURL
	if resp.Status != 200 {
		return l, nil
	}
	if m, ok := blyg.ParseManifest(resp.Body); ok {
		l.Manifest, l.Surface = m, blyg.SurfaceOf(origin, resp.Body)
	}
	return l, nil
}

// Cache locates each origin once, for a caller that may meet the same
// origin many times in one operation (a fork quoting one origin often). It
// is not safe for concurrent use, and is meant to live for one call.
type Cache struct {
	G     Getter
	Lim   fetch.Limits
	found map[string]Located
}

// Locate is Locate, remembered by origin. A transport failure is
// remembered too: one call does not retry an origin that is down.
func (c *Cache) Locate(ctx context.Context, origin string) Located {
	if c.found == nil {
		c.found = map[string]Located{}
	}
	if l, ok := c.found[origin]; ok {
		return l
	}
	l, _ := Locate(ctx, c.G, origin, c.Lim)
	c.found[origin] = l
	return l
}
