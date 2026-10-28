// Package mention implements §15: receiving Webmentions with structural
// verification, and sending them for a publisher's remote references.
package mention

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/net/html"

	"github.com/chrisbodhi/blygd/internal/blyg"
	"github.com/chrisbodhi/blygd/internal/fetch"
)

// Outcome is the result of verifying one claimed mention (§15.4).
type Outcome struct {
	Status    string // verified, failed, gone
	Transient bool   // the network said nothing; retry rather than conclude
	Reason    string

	Relation      string // stub, transclusion, fork, source, link
	Class         string // response or notice (§15.5), with Relation
	SourceOrigin  string
	SourceID      string
	SourceVersion int
	SourceKind    string
	SourceAuthor  json.RawMessage
	SourcePage    string
}

// Target is the receiving side of a verification.
type Target struct {
	Origin string // this blyg's identity origin
	ItemID string
	// IsPinned reports whether version v of the target is pinned here. Only
	// the receiver can say, so the fork check costs no fetch (§15.4 rule 4).
	IsPinned func(v int) bool
}

// sourceDoc is the subset of an item document verification reads. Unknown
// members are ignored (§13.1 rule 4).
type sourceDoc struct {
	Blyg          json.RawMessage `json:"blyg"`
	ID            string          `json:"id"`
	Kind          string          `json:"kind"`
	Origin        string          `json:"origin"`
	Version       int             `json:"version"`
	Page          string          `json:"page"`
	Author        json.RawMessage `json:"author"`
	ContentMD     string          `json:"content_md"`
	ContentHTML   string          `json:"content_html"`
	StubOf        json.RawMessage `json:"stub_of"`
	Transclusions []identity      `json:"transclusions"`
	ForkedFrom    *identity       `json:"forked_from"`
	Generated     []struct {
		Sources []identity `json:"sources"`
	} `json:"generated"`
	Links []identity `json:"links"`
}

// identity is a reference's machine-readable identity and nothing else
// (§5.9). Decoding only these members is what keeps a malformed `cited` or
// `selector` from deciding a verification: §15.4 must not consult them.
type identity struct {
	Origin  string `json:"origin"`
	ID      string `json:"id"`
	Version int    `json:"version"`
}

func failed(format string, a ...any) Outcome {
	return Outcome{Status: "failed", Reason: fmt.Sprintf(format, a...)}
}

func transient(format string, a ...any) Outcome {
	return Outcome{Status: "failed", Transient: true, Reason: fmt.Sprintf(format, a...)}
}

// fetchFailure classifies a fetch error. Bounds being exceeded and refused
// addresses are conclusions about the source; anything else is the network.
func fetchFailure(what string, err error) Outcome {
	switch {
	case errors.Is(err, fetch.ErrTooLarge), errors.Is(err, fetch.ErrTooManyHops),
		errors.Is(err, fetch.ErrBlockedAddr), errors.Is(err, fetch.ErrNotHTTP):
		return failed("%s: %v", what, err)
	}
	return transient("%s: %v", what, err)
}

// Verify runs §15.4 for one claim: at most two fetches, or three when the
// source origin's manifest must be read, each bounded by
// fetch.VerifyLimits. It never stores or returns source content.
func Verify(ctx context.Context, c *fetch.Client, sourceURL string, t Target) Outcome {
	const accept = "application/json, text/html;q=0.9"
	resp, err := c.Get(ctx, sourceURL, fetch.VerifyLimits, accept)
	if err != nil {
		return fetchFailure("fetching source", err)
	}
	if o, done := statusOutcome("source", resp.Status); done {
		return o
	}

	// Step 1: the source is either the item document or a page whose
	// alternate link names it.
	docURL, body := resp.FinalURL, resp.Body
	if !looksJSON(resp) {
		alt := alternateJSON(resp.Body, resp.FinalURL)
		if alt == "" {
			return failed("source is neither a blyg item document nor a page linking one")
		}
		resp2, err := c.Get(ctx, alt, fetch.VerifyLimits, "application/json")
		if err != nil {
			return fetchFailure("fetching item document", err)
		}
		if o, done := statusOutcome("item document", resp2.Status); done {
			return o
		}
		docURL, body = resp2.FinalURL, resp2.Body
	}
	var doc sourceDoc
	if err := json.Unmarshal(body, &doc); err != nil || doc.Blyg == nil {
		return failed("source document is not a blyg item document")
	}
	if !blyg.ValidID(doc.ID) || doc.Origin == "" {
		return failed("source document has no valid id and origin")
	}

	// Step 2: the document must have been fetched from exactly
	// {origin}items/{id}.json for its own origin and id — §12.2's identity
	// rule applied inbound. This is what stops a mirror speaking in a real
	// blyg's name, and why a pinned file never verifies. Under surface
	// templates (§6.1, decision #61) the asserted origin's own manifest may
	// place it elsewhere: one more fetch reads its `item` template, and only
	// an exact match is accepted.
	origin, err := blyg.NormalizeOrigin(doc.Origin)
	if err != nil {
		return failed("source origin is not an http(s) origin")
	}
	final, err := blyg.NormalizeURL(docURL)
	if err != nil {
		return failed("item document URL %s is not an http(s) URL", docURL)
	}
	if want := blyg.ItemDocURL(origin, doc.ID); final != want {
		if o, ok := templatedItem(ctx, c, origin, doc.ID, final); !ok {
			if o.Status != "" {
				return o
			}
			return failed("item document was fetched from %s, not from %s or the origin's item template", docURL, want)
		}
	}

	out := Outcome{
		SourceOrigin:  origin,
		SourceID:      doc.ID,
		SourceVersion: doc.Version,
		SourceKind:    doc.Kind,
		SourceAuthor:  doc.Author,
		SourcePage:    sourceURL,
	}
	if doc.Page != "" {
		out.SourcePage = blyg.Resolve(origin, doc.Page)
	}

	// Step 3.
	if doc.Kind == "withdrawn" {
		out.Status = "gone"
		out.Reason = "source is withdrawn"
		return out
	}

	// Step 4: relation, in order. Identity members only; `cited` and
	// `selector` are never consulted.
	us := func(o string) bool { return o != "" && blyg.SameOrigin(o, t.Origin) }
	verified := func(rel string) Outcome {
		out.Status, out.Relation, out.Class = "verified", rel, classOf(rel, doc)
		return out
	}
	if doc.StubOf != nil {
		var stub identity
		if json.Unmarshal(doc.StubOf, &stub) == nil && us(stub.Origin) && stub.ID == t.ItemID {
			return verified("stub")
		}
	}
	for _, r := range doc.Transclusions {
		if us(r.Origin) && r.ID == t.ItemID {
			return verified("transclusion")
		}
	}
	reason := "source document does not reference this item"
	if f := doc.ForkedFrom; f != nil && us(f.Origin) && f.ID == t.ItemID {
		if t.IsPinned != nil && t.IsPinned(f.Version) {
			return verified("fork")
		}
		// An unpinned fork is not a fork; a later relation may still hold.
		reason = fmt.Sprintf("forked_from names v%d, which is not pinned here", f.Version)
	}
	for _, g := range doc.Generated {
		for _, r := range g.Sources {
			if us(r.Origin) && r.ID == t.ItemID {
				return verified("source") // §5.7 rule 9
			}
		}
	}
	for _, r := range doc.Links {
		if us(r.Origin) && r.ID == t.ItemID {
			return verified("link") // §10.3a, decision #79
		}
	}
	out.Status = "failed"
	out.Reason = reason
	return out
}

// classOf is §15.5's class, which the receiver derives and no sender
// marks: a link is a notice, and so is a stub or transclusion from a
// document with no words of its own, a repost (§15.4, decision #80). The
// relation is unchanged either way; stub-ness still comes from the marker.
func classOf(relation string, doc sourceDoc) string {
	switch {
	case relation == "link":
		return "notice"
	case (relation == "stub" || relation == "transclusion") && !blyg.HasOwnWords(doc.ContentMD, doc.ContentHTML):
		return "notice"
	}
	return "response"
}

// templatedItem is §15.4 step 2 under surface templates: it reads the
// asserted origin's own manifest at {origin}blyg.json and reports whether
// its `item` template, expanded with id, is exactly final. A non-zero
// outcome says why the manifest could not be read.
func templatedItem(ctx context.Context, c *fetch.Client, origin, id, final string) (Outcome, bool) {
	resp, err := c.Get(ctx, origin+"blyg.json", fetch.VerifyLimits, "application/json")
	if err != nil {
		return fetchFailure("fetching the source origin's manifest", err), false
	}
	if o, done := statusOutcome("the source origin's manifest", resp.Status); done {
		return o, false
	}
	s := blyg.SurfaceOf(origin, resp.Body)
	if !s.Templated() {
		return Outcome{}, false
	}
	want, err := blyg.NormalizeURL(s.ItemURL(id))
	return Outcome{}, err == nil && want == final
}

// statusOutcome maps a non-200 status to an outcome.
func statusOutcome(what string, status int) (Outcome, bool) {
	switch {
	case status == 200:
		return Outcome{}, false
	case status == 429 || status >= 500:
		return transient("%s returned %d", what, status), true
	}
	return failed("%s returned %d", what, status), true
}

func looksJSON(r *fetch.Response) bool {
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.Contains(ct, "json") {
		return true
	}
	if strings.Contains(ct, "html") {
		return false
	}
	return bytes.HasPrefix(bytes.TrimSpace(r.Body), []byte("{"))
}

// alternateJSON finds <link rel="alternate" type="application/json" href>
// (§5.8) and resolves it against the page URL.
func alternateJSON(body []byte, pageURL string) string {
	z := html.NewTokenizer(bytes.NewReader(body))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return ""
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			if string(name) == "body" {
				return ""
			}
			if string(name) != "link" || !hasAttr {
				continue
			}
			var rel, typ, href string
			for {
				k, v, more := z.TagAttr()
				switch string(k) {
				case "rel":
					rel = strings.ToLower(string(v))
				case "type":
					typ = strings.ToLower(strings.TrimSpace(string(v)))
				case "href":
					href = string(v)
				}
				if !more {
					break
				}
			}
			if hasToken(rel, "alternate") && typ == "application/json" && href != "" {
				return blyg.Resolve(pageURL, href)
			}
		}
	}
}

func hasToken(list, token string) bool {
	for _, f := range strings.Fields(list) {
		if f == token {
			return true
		}
	}
	return false
}
