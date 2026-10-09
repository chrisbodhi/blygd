package studio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/chrisbodhi/blygd/internal/blyg"
	"github.com/chrisbodhi/blygd/internal/fetch"
)

// Imports is the one way imported bytes reach plane 1: a read-only lookup,
// implemented by internal/reader, that hands the studio the snapshots a
// user's subscriptions hold (§10.2 step 2). The studio never queries the
// mirror itself (planes_test.go), and nothing it gets here is rendered
// except as a baked transclusion, a link, or a frozen citation.
type Imports interface {
	// Snapshots returns, for each id, every imported copy reachable from
	// the user's subscriptions, usable or not.
	Snapshots(ctx context.Context, userID int64, ids []string) (map[string][]Snapshot, error)
}

// Snapshot is one imported copy of an id.
type Snapshot struct {
	Origin      string // identity origin (§12.2), never the manifest's site
	ID          string
	Kind        string // fragment or thread
	Version     int    // the version ContentHTML is; a retained pin's after withdrawal
	ContentHTML string
	Page        string // absolute (§5.8), or the reference convention
	Pinned      bool
	Author      json.RawMessage // the source's own, passed through (§5.5)
	SourceTitle string          // the origin's title as we hold it, for cited.source
	// Unusable is why §10.2 step 2 refuses this copy (withdrawn with
	// nothing retained, or an L0 wrapper), or "" when it may be quoted.
	Unusable string
}

// unpublishable is a publish refusal that lists every problem at once.
func unpublishable(problems []blyg.Problem) error {
	reasons := make([]string, 0, len(problems))
	for _, p := range problems {
		reasons = append(reasons, p.At+": "+p.Reason)
	}
	return &Error{Code: "invalid", Msg: "cannot publish: " + strings.Join(reasons, "; "), Problems: problems}
}

// referencedIDs lists the ids a draft names anywhere, cheaply. Over-reporting
// is harmless: it only decides which imports are looked up.
func referencedIDs(md string) []string {
	seen := map[string]bool{}
	var out []string
	for i := 0; i+2 < len(md); i++ {
		if md[i] != '[' || md[i+1] != '[' || i+28 > len(md) {
			continue
		}
		if id := md[i+2 : i+28]; blyg.ValidID(id) && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// resolver resolves ids for one publish, in the publish transaction:
// local first, then the imports fetched before it (§10.2).
type resolver struct {
	ctx     context.Context
	tx      pgx.Tx
	origin  *Origin
	self    string
	imports map[string][]Snapshot
	quoted  map[string]*Snapshot // remote quotes, for their cited (§5.9)
}

func (r *resolver) resolve(id string, use blyg.Use) (*blyg.Target, error) {
	var (
		kind, page *string
		version    int
		withdrawn  bool
		html       *string
		pinned     bool
	)
	err := r.tx.QueryRow(r.ctx, `SELECT i.authored_kind, i.version, i.withdrawn, i.page, v.content_html, v.pinned_at IS NOT NULL
		FROM items i LEFT JOIN item_versions v ON v.item_id = i.id AND v.version = i.version
		WHERE i.origin_id = $1 AND i.id = $2`, r.origin.ID, id).Scan(&kind, &version, &withdrawn, &page, &html, &pinned)
	switch {
	case err == nil:
		return r.local(id, use, *kind, version, withdrawn, page, html, pinned)
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	case use == blyg.UseSource:
		return nil, &blyg.Unresolved{Reason: "a generation source must be one of this origin's published items (§5.7 rule 1)"}
	}
	return r.imported(id)
}

func (r *resolver) local(id string, use blyg.Use, kind string, version int, withdrawn bool, page, html *string, pinned bool) (*blyg.Target, error) {
	switch {
	case version == 0:
		return nil, &blyg.Unresolved{Reason: "item is a draft, not published"}
	case withdrawn:
		return nil, &blyg.Unresolved{Reason: "item is withdrawn"}
	}
	if use == blyg.UseDirective && kind == "thread" {
		if id == r.self {
			return nil, &blyg.Unresolved{Reason: "a thread cannot transclude itself"}
		}
		loop, err := r.closureContains(id, r.self)
		if err != nil {
			return nil, err
		}
		if loop {
			return nil, &blyg.Unresolved{Reason: "that thread already quotes this one (§10.2 local closure check)"}
		}
	}
	return &blyg.Target{ID: id, Kind: kind, Version: version, ContentHTML: *html, Page: r.origin.BaseURL + *page, Pinned: pinned}, nil
}

// closureContains walks the stored local transclusions of start's latest
// version, and theirs, looking for want (§10.2). Remote entries are not
// walked: harmless by snapshot, and unknowable in general.
func (r *resolver) closureContains(start, want string) (bool, error) {
	seen := map[string]bool{}
	queue := []string{start}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		if id == want {
			return true, nil
		}
		var raw []byte
		err := r.tx.QueryRow(r.ctx, `SELECT v.transclusions FROM items i
			JOIN item_versions v ON v.item_id = i.id AND v.version = i.version
			WHERE i.origin_id = $1 AND i.id = $2`, r.origin.ID, id).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return false, err
		}
		var refs []blyg.Reference
		if raw != nil {
			if err := json.Unmarshal(raw, &refs); err != nil {
				return false, err
			}
		}
		for _, ref := range refs {
			if ref.Origin == "" && !seen[ref.ID] {
				queue = append(queue, ref.ID)
			}
		}
	}
	return false, nil
}

func (r *resolver) imported(id string) (*blyg.Target, error) {
	var usable []Snapshot
	reason := ""
	for _, s := range r.imports[id] {
		if s.Unusable != "" {
			reason = s.Unusable
			continue
		}
		usable = append(usable, s)
	}
	switch {
	case len(usable) > 1:
		origins := make([]string, len(usable))
		for i, s := range usable {
			origins[i] = s.Origin
		}
		// §10.2: the directive names an identity; the publisher never
		// picks an origin on the author's behalf.
		return nil, &blyg.Unresolved{Reason: "ambiguous: imported from more than one origin (" + strings.Join(origins, ", ") + ")"}
	case len(usable) == 0 && reason != "":
		return nil, &blyg.Unresolved{Reason: reason}
	case len(usable) == 0:
		return nil, &blyg.Unresolved{Reason: "unknown item: not published here and not in any subscription"}
	}
	s := usable[0]
	if r.quoted == nil {
		r.quoted = map[string]*Snapshot{}
	}
	r.quoted[s.Origin+" "+s.ID] = &s
	return &blyg.Target{ID: s.ID, Kind: s.Kind, Version: s.Version, ContentHTML: s.ContentHTML,
		Origin: s.Origin, Page: s.Page, Pinned: s.Pinned}, nil
}

// citeExcerpt is the length of an excerpt the studio freezes; well inside
// citedExcerptCap.
const citeExcerpt = 80

// cited is the human half of a reference to a snapshot, frozen now (§5.9).
func cited(s *Snapshot, at time.Time) *blyg.Cited {
	c := &blyg.Cited{Source: s.SourceTitle, Excerpt: blyg.Excerpt(s.ContentHTML, citeExcerpt), URL: s.Page, Retrieved: blyg.Time(at)}
	var author struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(s.Author, &author) == nil {
		c.Author = author.Name // §5.5: the source's own, passed through
	}
	return c
}

// stubAgreement applies §10.6 rule 3: when the body transcludes the stub's
// target, stub_of.version is the version baked; otherwise it keeps the
// version set when the stub was created.
func stubAgreement(raw json.RawMessage, own string, refs []blyg.Reference) (json.RawMessage, error) {
	var stub blyg.StubOf
	if err := json.Unmarshal(raw, &stub); err != nil {
		return nil, fmt.Errorf("stored stub_of is malformed: %w", err)
	}
	if stub.URL == "" {
		for _, t := range refs {
			origin := t.Origin
			if origin == "" {
				origin = own
			}
			if t.ID == stub.ID && blyg.SameOrigin(origin, stub.Origin) {
				stub.Version = t.Version
				break
			}
		}
	}
	return json.Marshal(stub)
}

// citedExcerptCap is §5.9's SHOULD: an excerpt is a caption, near 200
// characters, never a second quotation channel.
const citedExcerptCap = 200

// stubOf resolves a stub's target at creation and freezes what the author
// saw: the version and the cited label (§10.6, §5.9). It returns the
// stub_of value and the body a new stub starts with.
func (s *Studio) stubOf(ctx context.Context, o *Origin, t *StubTarget) (json.RawMessage, string, error) {
	now := s.now()
	if t.URL != "" {
		if t.ID != "" || t.Origin != "" {
			return nil, "", invalid("stub_of is either {url} or {id, origin}, not both")
		}
		u, err := blyg.NormalizeURL(t.URL)
		if err != nil {
			return nil, "", invalid("stub_of.url must be an absolute http(s) URL")
		}
		out := blyg.StubOf{URL: u}
		if t.Cited != nil {
			c, err := clientCited(t.Cited, now)
			if err != nil {
				return nil, "", err
			}
			out.Cited = c
		}
		b, _ := json.Marshal(out)
		return b, "", nil
	}
	if t.Cited != nil {
		return nil, "", invalid("a blyg stub's cited is frozen by the studio from what it holds (§5.9)")
	}
	if !blyg.ValidID(t.ID) {
		return nil, "", invalid("stub_of needs a valid id, or a url")
	}
	var origin string
	if t.Origin != "" {
		norm, err := blyg.NormalizeOrigin(t.Origin)
		if err != nil {
			return nil, "", invalid("stub_of.origin must be an absolute http(s) origin")
		}
		origin = norm
	}
	var snap *Snapshot
	if origin == "" || blyg.SameOrigin(origin, o.BaseURL) {
		var kind, page, html string
		var version int
		err := s.Pool.QueryRow(ctx, `SELECT i.authored_kind, i.page, v.content_html, i.version FROM items i
			JOIN item_versions v ON v.item_id = i.id AND v.version = i.version
			WHERE i.origin_id = $1 AND i.id = $2 AND NOT i.withdrawn`, o.ID, t.ID).Scan(&kind, &page, &html, &version)
		switch {
		case err == nil:
			title := o.Title
			if title == "" {
				title = o.BaseURL
			}
			snap = &Snapshot{Origin: o.BaseURL, ID: t.ID, Kind: kind, Version: version, ContentHTML: html,
				Page: o.BaseURL + page, Author: o.Byline, SourceTitle: title}
		case !errors.Is(err, pgx.ErrNoRows):
			return nil, "", err
		case origin != "":
			return nil, "", invalid("stub_of: %s is not a published item here", t.ID)
		}
	}
	if snap == nil {
		var cands []Snapshot
		if s.Imports != nil {
			all, err := s.Imports.Snapshots(ctx, o.OwnerID, []string{t.ID})
			if err != nil {
				return nil, "", err
			}
			cands = all[t.ID]
		}
		r := &resolver{imports: map[string][]Snapshot{t.ID: filterOrigin(cands, origin)}}
		if _, err := r.imported(t.ID); err != nil {
			return nil, "", invalid("stub_of: %v", err)
		}
		for _, q := range r.quoted {
			snap = q
		}
	}
	b, _ := json.Marshal(blyg.StubOf{Origin: snap.Origin, ID: snap.ID, Version: snap.Version, Cited: cited(snap, now)})
	// The reference client starts a stub with the quote of its target
	// (§10.6 rule 2); the author may delete it.
	return b, "![[" + snap.ID + "]]\n\n", nil
}

func filterOrigin(cands []Snapshot, origin string) []Snapshot {
	if origin == "" {
		return cands
	}
	var out []Snapshot
	for _, c := range cands {
		if blyg.SameOrigin(c.Origin, origin) {
			out = append(out, c)
		}
	}
	return out
}

// clientCited checks a client's label for a {url} stub, dates it
// (retrieved is REQUIRED, §5.9; now if absent), and holds the excerpt to a
// caption.
func clientCited(raw json.RawMessage, now time.Time) (*blyg.Cited, error) {
	var c blyg.Cited
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, invalid("stub_of.cited takes source, author, excerpt, url and retrieved, all strings (§5.9)")
	}
	if c.Retrieved == "" {
		c.Retrieved = blyg.Time(now)
	} else if t, err := time.Parse(time.RFC3339, c.Retrieved); err != nil {
		return nil, invalid("stub_of.cited.retrieved must be an RFC 3339 time")
	} else {
		c.Retrieved = blyg.Time(t)
	}
	if r := []rune(c.Excerpt); len(r) > citedExcerptCap {
		c.Excerpt = string(r[:citedExcerptCap-1]) + "…"
	}
	return &c, nil
}

// forkContent builds a fork's first draft from the pinned document it
// names (§5.6 rule 6). It reads an own pin from plane 1 and fetches any
// other, outside any transaction. It returns the draft and the source's
// kind.
func (s *Studio) forkContent(ctx context.Context, o *Origin, ref blyg.Reference) (string, string, error) {
	var src blyg.ForkSource
	var kind string
	src.Origin = ref.Origin
	if blyg.SameOrigin(ref.Origin, o.BaseURL) {
		var gen []byte
		err := s.Pool.QueryRow(ctx, `SELECT v.kind, v.content_md, v.content_html, v.generated FROM item_versions v
			JOIN items i ON i.id = v.item_id
			WHERE i.origin_id = $1 AND v.item_id = $2 AND v.version = $3 AND v.pinned_at IS NOT NULL`,
			o.ID, ref.ID, ref.Version).Scan(&kind, &src.ContentMD, &src.ContentHTML, &gen)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", invalid("forked_from names v%d of %s, which is not pinned here (§5.6 rule 2)", ref.Version, ref.ID)
		}
		if err != nil {
			return "", "", err
		}
		if gen != nil {
			_ = json.Unmarshal(gen, &src.Generated)
		}
	} else {
		if s.Fetch == nil {
			return "", "", invalid("cannot fetch the pinned source to copy; send content_md to fork without copying")
		}
		pinURL := blyg.PinURL(ref.Origin, ref.ID, ref.Version)
		resp, err := s.Fetch.Get(ctx, pinURL, fetch.VerifyLimits, "application/json")
		if err != nil {
			return "", "", invalid("could not fetch %s to copy (%v); send content_md to fork without copying", pinURL, err)
		}
		if resp.Status != 200 {
			return "", "", invalid("%s returned %d: the fork has nothing pinned to copy (§5.6 rule 2)", pinURL, resp.Status)
		}
		var doc blyg.PinDoc
		if err := json.Unmarshal(resp.Body, &doc); err != nil || doc.ID != ref.ID || doc.Version != ref.Version {
			return "", "", invalid("%s is not v%d of %s (§5.6 rule 2)", pinURL, ref.Version, ref.ID)
		}
		kind, src.ContentMD, src.ContentHTML = doc.Kind, doc.ContentMD, doc.ContentHTML
		if doc.Generated != nil {
			_ = json.Unmarshal(doc.Generated, &src.Generated)
		}
	}
	if kind != "fragment" && kind != "thread" {
		return "", "", invalid("forked_from names a %s, which has no content to copy", kind)
	}
	md, _ := blyg.FlattenFork(src, s.quoteLink(ctx, o))
	return md, kind, nil
}

// quoteLink is where a flattened quote's attribution line points: the
// quoted item's page where we hold it, else its item document (§5.6 rule 6).
func (s *Studio) quoteLink(ctx context.Context, o *Origin) blyg.QuoteLink {
	return func(origin, id string) string {
		if blyg.SameOrigin(origin, o.BaseURL) {
			var page *string
			if s.Pool.QueryRow(ctx, `SELECT page FROM items WHERE origin_id = $1 AND id = $2`, o.ID, id).Scan(&page) == nil && page != nil {
				return o.BaseURL + *page
			}
		} else if s.Imports != nil {
			if snaps, err := s.Imports.Snapshots(ctx, o.OwnerID, []string{id}); err == nil {
				for _, sn := range snaps[id] {
					if blyg.SameOrigin(sn.Origin, origin) && sn.Page != "" {
						return sn.Page
					}
				}
			}
		}
		return blyg.ItemDocURL(origin, id)
	}
}
