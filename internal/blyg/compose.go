package blyg

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// Use says what a resolved id is for. Resolution order is the same for all
// three (§10.2); what differs is what the caller may accept.
type Use int

const (
	UseDirective Use = iota // ![[id]]: bakes a snapshot (§10.2)
	UseLink                 // [[id]]: renders an anchor and a links[] entry (§10.1, §10.3a)
	UseSource               // an id named by a [TK] instruction: a generation source (§5.7 rules 1, 8)
)

// Target is a resolved id: the snapshot the publisher holds (§10.2).
type Target struct {
	ID          string
	Kind        string // fragment or thread
	Version     int
	ContentHTML string
	Origin      string // identity origin (§12.2) when remote; "" when own (§10.3)
	Page        string // absolute permalink (§5.8)
	// Pinned reports whether Version is pinned at its origin. §10.2 asks a
	// studio to say when a directive resolves to an unpinned remote item.
	Pinned bool
	// Label is the author's label at this use (§10.1, decision #70): a
	// link's anchor text, or a directive's data-blyg-label. Resolution
	// ignores it.
	Label string
}

// Unresolved is a resolver's refusal: a publish error the author can fix,
// as opposed to a failure of the resolver itself.
type Unresolved struct{ Reason string }

func (u *Unresolved) Error() string { return u.Reason }

// Resolver resolves one id. Return *Unresolved for a publish error.
type Resolver func(id string, use Use) (*Target, error)

// GenMeta is what a client declares about one [TK] scope, by position:
// the model and when it ran (§5.7, both RECOMMENDED).
type GenMeta struct {
	Model string `json:"model,omitempty"`
	At    string `json:"at,omitempty"`
}

// Generated is one entry of the generated array (§5.7).
type Generated struct {
	Sources []GenSource `json:"sources"`
	Model   string      `json:"model,omitempty"`
	At      string      `json:"at,omitempty"`
}

// GenSource names an item version a generator drew on (§5.7 rule 1): a
// reference, origin omitted for an own item and present for another
// origin's, as in transclusions[].
type GenSource struct {
	Origin  string `json:"origin,omitempty"`
	ID      string `json:"id"`
	Version int    `json:"version"`
	Cited   *Cited `json:"cited,omitempty"`
}

// Problem is one reason a draft cannot be published as it stands.
type Problem struct {
	At     string `json:"at"`
	Reason string `json:"reason"`
}

// ComposeInput is a draft on its way to the wire.
type ComposeInput struct {
	ContentMD  string // the draft, TK grammar included
	Thread     bool
	PageURL    string // the item's absolute page: links resolve against it
	Origin     string // the publishing origin: media/… resolves against it (§5.4)
	Resolve    Resolver
	Generation []GenMeta
}

// Composed is a version ready to store: wire content_md, baked
// content_html, and the provenance that goes with them.
type Composed struct {
	ContentMD     string
	ContentHTML   string
	Transclusions []Reference // §10.3, directive order
	Quoted        []*Target   // parallel to Transclusions
	Generated     []Generated // §5.7, scope order
	Links         []Link      // §10.3a, document order
	Problems      []Problem   // non-empty means do not publish
}

// Compose turns a draft into a version. It never touches the network or a
// database except through Resolve: what gets baked is what the publisher
// already holds (§10.2).
//
// The pipeline:
//
//  1. [TK] scopes are stripped to their output (the wire content_md) and
//     located (§5.7).
//  2. A thread is split at its directives. Each directive bakes its
//     target's snapshot, or for a partial quote the selection's text, once
//     the selection is found in the target (§10.2).
//  3. The prose between directives is rendered with [[id]] links swapped
//     for sentinels, then generated spans are wrapped in blyg-tk-gen and the
//     links spliced back as absolute anchors (§10.1).
func Compose(in ComposeInput) (*Composed, error) {
	c := &composer{in: in, out: &Composed{Transclusions: []Reference{}}}
	scopes, errs := parseScopes(in.ContentMD)
	for _, e := range errs {
		c.problem("[TK]", e)
	}
	for _, s := range scopes {
		if !s.hasOutput {
			c.problem("[TK]"+clip(s.instruction, 40), "this scope has no output yet: generate it or remove it")
		}
	}
	if len(c.out.Problems) > 0 {
		return c.out, nil
	}
	stripped, spans := stripScopes(in.ContentMD, scopes)
	c.out.ContentMD = stripped
	for _, sp := range spans {
		for _, l := range strings.Split(stripped[sp.start:sp.end], "\n") {
			if id, _, _ := directiveAt(l); id != "" || reservedLine.MatchString(l) {
				// §5.7 rule 3: generated prose and quotation are disjoint.
				c.problem(strings.TrimSpace(l), "a directive inside generated text: move it outside the [TK] scope")
			}
		}
	}
	c.generated(scopes)

	text := c.annotate(stripped, spans, scopes)
	var body string
	if in.Thread {
		body = c.thread(text)
	} else {
		c.refuseDirectives(text)
		body = c.prose(text)
	}
	if c.err != nil {
		return nil, c.err
	}
	c.out.ContentHTML = body
	c.out.Links = c.linksInOrder(stripped)
	return c.out, nil
}

// linksInOrder is links[]: one entry per resolved link, in document order
// (§10.3a). Rendering visits generated blocks out of order, so the order is
// read from the wire content_md, by the same grammar substituteLinks used:
// outside code, not the directive form, and in a thread not a directive
// line or the selection attached to one.
func (c *composer) linksInOrder(md string) []Link {
	lines := strings.Split(md, "\n")
	starts := lineOffsets(lines)
	code := codeSpans(md)
	var out []Link
	for i := 0; i < len(lines); i++ {
		if c.in.Thread {
			if id, _, at := directiveAt(lines[i]); id != "" && !inSpans(code, starts[i]+at) {
				if _, next, ok := attachedQuote(lines, i); ok {
					i = next - 1
				}
				continue
			}
		}
		l := lines[i]
		for _, m := range findLinks(l) {
			if (m[0] > 0 && l[m[0]-1] == '!') || inSpans(code, starts[i]+m[0]) {
				continue
			}
			if t := c.linked[l[m[2]:m[3]]]; t != nil {
				out = append(out, Link{Origin: t.Origin, ID: t.ID})
			}
		}
	}
	return out
}

type composer struct {
	in     ComposeInput
	out    *Composed
	err    error
	linked map[string]*Target // each [[id]] resolved, by id
	blocks map[string]string  // block sentinel token → rendered generated block
	inline bool
	seq    int
}

func (c *composer) problem(at, reason string) {
	c.out.Problems = append(c.out.Problems, Problem{at, reason})
}

func (c *composer) resolve(id string, use Use, at string) (*Target, bool) {
	t, err := c.in.Resolve(id, use)
	if err != nil {
		var u *Unresolved
		if errors.As(err, &u) {
			c.problem(at, u.Reason)
		} else if c.err == nil {
			c.err = err
		}
		return nil, false
	}
	return t, true
}

// generated builds the generated array: one entry per scope, in order.
func (c *composer) generated(scopes []tkScope) {
	for i, s := range scopes {
		g := Generated{Sources: []GenSource{}}
		if s.imported {
			// Generated elsewhere: no sources declared, and no time, because
			// none is known here (§5.7 rule 7).
			g.Model = s.model
		} else {
			for _, id := range s.sourceIDs {
				// §5.7 rule 8: a source resolves like a directive.
				if t, ok := c.resolve(id, UseSource, "![["+id+"]]"); ok {
					g.Sources = append(g.Sources, GenSource{Origin: t.Origin, ID: t.ID, Version: t.Version})
				}
			}
			if i < len(c.in.Generation) {
				g.Model, g.At = c.in.Generation[i].Model, c.in.Generation[i].At
			}
		}
		c.out.Generated = append(c.out.Generated, g)
	}
}

// annotate marks generated spans in the stripped text: a block span becomes
// a one-line token rendered on its own, so a multi-paragraph block is never
// split by the surrounding document; an inline span stays in the flow
// between sentinels, so markdown around it still parses across it.
func (c *composer) annotate(stripped string, spans []genSpan, scopes []tkScope) string {
	var b strings.Builder
	last := 0
	for i, sp := range spans {
		b.WriteString(stripped[last:sp.start])
		out := stripped[sp.start:sp.end]
		if scopes[sp.scope].block {
			if c.blocks == nil {
				c.blocks = map[string]string{}
			}
			token := blockSentinel + strconv.Itoa(i) + blockSentinel
			c.blocks[token] = c.prose(out)
			b.WriteString(token)
		} else {
			c.inline = true
			b.WriteString(inlineOpen + out + inlineClose)
		}
		last = sp.end
	}
	b.WriteString(stripped[last:])
	return b.String()
}

// refuseDirectives reports live directives in a fragment. Only threads
// transclude (§10); rendering one as literal text would surprise the
// author, so it is a publish error.
func (c *composer) refuseDirectives(md string) {
	code := codeSpans(md)
	lines := strings.Split(md, "\n")
	starts := lineOffsets(lines)
	for i, l := range lines {
		if id, _, at := directiveAt(l); id != "" && !inSpans(code, starts[i]+at) {
			c.problem(strings.TrimSpace(l), "only threads transclude (§10): publish this as a thread, or use [["+id+"]] to link")
		} else if reservedLine.MatchString(l) && !inSpans(code, starts[i]+strings.IndexByte(l, '!')) {
			c.problem(strings.TrimSpace(l), "explicit versions (![[id@vN]]) are reserved in 0.3 (§10.1)")
		}
	}
}

// thread walks a thread line by line, baking each directive.
func (c *composer) thread(md string) string {
	lines := strings.Split(md, "\n")
	starts := lineOffsets(lines)
	code := codeSpans(md)
	var parts, prose []string
	flush := func() {
		if len(prose) > 0 {
			parts = append(parts, c.prose(strings.Join(prose, "\n")))
			prose = nil
		}
	}
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if reservedLine.MatchString(l) && !inSpans(code, starts[i]+strings.IndexByte(l, '!')) {
			flush()
			c.problem(strings.TrimSpace(l), "explicit versions (![[id@vN]]) are reserved in 0.3 (§10.1)")
			continue
		}
		id, label, at := directiveAt(l)
		if id == "" || inSpans(code, starts[i]+at) {
			prose = append(prose, l)
			continue
		}
		flush()
		// The attached quote belongs to the directive whether or not the
		// target resolves, so it never falls through as the author's own
		// quotation.
		quote, next, partial := attachedQuote(lines, i)
		i = next - 1
		directive := strings.TrimSpace(l)
		resolved, ok := c.resolve(id, UseDirective, directive)
		if !ok {
			continue
		}
		t := &Target{}
		*t = *resolved
		t.Label = label
		snapshot := t.ContentHTML
		if t.Origin != "" {
			// Defensive (§5.2): a remote snapshot should already be
			// self-contained, but must not resolve against us once baked.
			snapshot = AbsolutizeHTML(snapshot, t.Origin)
		}
		ref := Reference{Origin: t.Origin, ID: t.ID, Version: t.Version}
		if partial {
			rendered, err := RenderMarkdownIn(quote, c.in.PageURL, c.in.Origin)
			if err != nil {
				c.err = err
				continue
			}
			sel := SelectionText(rendered)
			if sel == "" {
				c.problem(directive, "the attached blockquote is empty")
				continue
			}
			loc := LocateSelection(snapshot, sel)
			if loc == nil {
				c.problem(directive, fmt.Sprintf("the quoted passage is not in version %d of %s (§10.2)", t.Version, id))
				continue
			}
			ref.Selector = loc
			parts = append(parts, BakePartial(t, loc.Exact))
		} else {
			parts = append(parts, BakeWhole(t, snapshot))
		}
		c.out.Transclusions = append(c.out.Transclusions, ref)
		c.out.Quoted = append(c.out.Quoted, t)
	}
	flush()
	return strings.Join(parts, "\n")
}

func bakeOpen(class string, t *Target) string {
	s := `<blockquote class="` + class + `" data-blyg-id="` + t.ID + `" data-blyg-version="` + strconv.Itoa(t.Version) + `"`
	if t.Origin != "" {
		// Remote sources only, so an own-origin bake is byte-identical to
		// its 0.2 form (§10.2).
		s += ` data-blyg-origin="` + escapeHTML(t.Origin) + `"`
	}
	if t.Label != "" {
		// The quoter's name for the item, verbatim and escaped, for an
		// attribution line and nothing else (§10.2, decision #81).
		s += ` data-blyg-label="` + escapeHTML(t.Label) + `"`
	}
	return s + ">\n"
}

// BakeWhole wraps a snapshot as a transclusion (§10.2): a bare blockquote
// with data attributes and no link inside.
func BakeWhole(t *Target, snapshot string) string {
	return bakeOpen("blyg-transclusion", t) + snapshot + "\n</blockquote>"
}

// BakePartial bakes a partial quote: the selection's plain text, one <p>
// per line, escaped (§10.2).
func BakePartial(t *Target, selection string) string {
	var ps []string
	for _, line := range strings.Split(selection, "\n") {
		ps = append(ps, "<p>"+escapeHTML(line)+"</p>")
	}
	return bakeOpen("blyg-transclusion blyg-partial", t) + strings.Join(ps, "\n") + "\n</blockquote>"
}

// prose renders markdown with its [[id]] links resolved and its generated
// spans wrapped.
func (c *composer) prose(md string) string {
	text, links := c.substituteLinks(md)
	h, err := RenderMarkdownIn(text, c.in.PageURL, c.in.Origin)
	if err != nil {
		if c.err == nil {
			c.err = err
		}
		return ""
	}
	h = c.wrapGenerated(h)
	return spliceLinks(h, links)
}

// ── [[id]] links (§10.1) ────────────────────────────────────────────────

type linkSub struct {
	anchor  string // the <a> that replaces the token
	label   string // escaped anchor text, where an anchor cannot go
	literal string // the author's [[id]]
}

// substituteLinks swaps each live [[id]] for a sentinel token. A link that
// does not resolve is a publish error (§10.1) and stays literal. The anchor
// text is the author's label when there is one.
func (c *composer) substituteLinks(md string) (string, map[string]linkSub) {
	code := codeSpans(md)
	subs := map[string]linkSub{}
	var b strings.Builder
	last := 0
	for _, m := range findLinks(md) {
		if (m[0] > 0 && md[m[0]-1] == '!') || inSpans(code, m[0]) {
			continue
		}
		literal := md[m[0]:m[1]]
		t, ok := c.resolve(md[m[2]:m[3]], UseLink, literal)
		if !ok {
			continue
		}
		if c.linked == nil {
			c.linked = map[string]*Target{}
		}
		c.linked[t.ID] = t
		b.WriteString(md[last:m[0]])
		token := linkSentinel + strconv.Itoa(c.seq) + linkSentinel
		c.seq++
		text := linkLabel(md, m)
		if text == "" {
			text = anchorText(t)
		}
		label := escapeHTML(text)
		subs[token] = linkSub{
			anchor:  `<a href="` + escapeHTML(t.Page) + `">` + label + `</a>`,
			label:   label,
			literal: literal,
		}
		b.WriteString(token)
		last = m[1]
	}
	b.WriteString(md[last:])
	return b.String(), subs
}

var leadingHeading = regexp.MustCompile(`(?is)^\s*<h[1-6]\b[^>]*>(.*?)</h[1-6]>`)

// anchorText labels a link. Items are titleless (§5.3), so an id would be
// the one label sure to mean nothing: a leading heading is used when there
// is one, else a short quoted excerpt. Presentation, frozen at publish.
func anchorText(t *Target) string {
	if m := leadingHeading.FindStringSubmatch(t.ContentHTML); m != nil {
		if s := Excerpt(m[1], 80); s != "" {
			return s
		}
	}
	if s := Excerpt(t.ContentHTML, 60); s != "" {
		return "“" + s + "”"
	}
	return "a " + t.Kind
}

var (
	tagPattern     = regexp.MustCompile(`<[^>]*>`)
	linkToken      = regexp.MustCompile(linkSentinel + `(\d+)` + linkSentinel)
	encodedLinkTok = regexp.MustCompile(pctEncoded(linkSentinel) + `(\d+)` + pctEncoded(linkSentinel))
)

// splitTags splits HTML into alternating text and tag parts.
func splitTags(h string) []string {
	var parts []string
	last := 0
	for _, m := range tagPattern.FindAllStringIndex(h, -1) {
		parts = append(parts, h[last:m[0]], h[m[0]:m[1]])
		last = m[1]
	}
	return append(parts, h[last:])
}

// spliceLinks puts anchors where tokens were, but only where an anchor is
// valid: inside another link's text the label goes in as text, inside a
// URL or a tag the author's literal goes back, and no sentinel survives.
func spliceLinks(h string, subs map[string]linkSub) string {
	if len(subs) == 0 {
		return h
	}
	get := func(seq string) (linkSub, bool) {
		s, ok := subs[linkSentinel+seq+linkSentinel]
		return s, ok
	}
	h = encodedLinkTok.ReplaceAllStringFunc(h, func(whole string) string {
		if s, ok := get(encodedLinkTok.FindStringSubmatch(whole)[1]); ok {
			return url.PathEscape(s.literal)
		}
		return whole
	})
	var b strings.Builder
	inAnchor, inCode := 0, 0
	anchorTag := ""
	for _, part := range splitTags(h) {
		isTag := strings.HasPrefix(part, "<")
		if strings.Contains(part, linkSentinel) {
			part = linkToken.ReplaceAllStringFunc(part, func(whole string) string {
				s, ok := get(linkToken.FindStringSubmatch(whole)[1])
				switch {
				case !ok:
					return whole
				case isTag || inCode > 0:
					return escapeHTML(s.literal)
				case inAnchor > 0:
					// An autolink's text is its URL: keep it reading as one.
					if strings.Contains(anchorTag, url.PathEscape(s.literal)) {
						return escapeHTML(s.literal)
					}
					return s.label
				}
				return s.anchor
			})
		}
		if isTag {
			lower := strings.ToLower(part)
			switch {
			case strings.HasPrefix(lower, "<a ") || lower == "<a>":
				inAnchor++
				anchorTag = part
			case lower == "</a>":
				inAnchor = max(0, inAnchor-1)
			case strings.HasPrefix(lower, "<code"):
				inCode++
			case lower == "</code>":
				inCode = max(0, inCode-1)
			}
		}
		b.WriteString(part)
	}
	return b.String()
}

// ── generated spans (§5.7) ──────────────────────────────────────────────

var blockTagName = regexp.MustCompile(`(?i)^</?(p|li|ul|ol|h[1-6]|blockquote|pre|div|table|tr|td|th|hr)\b`)

// wrapGenerated wraps generated spans: each block token's paragraph becomes
// <div class="blyg-tk-gen">, and inline sentinels become
// <span class="blyg-tk-gen">. An inline span that crosses a block boundary
// is closed before it and reopened after, so the HTML stays well nested.
// Where a span cannot go (inside a tag or a URL) the marker is dropped;
// generated[] still discloses the span.
func (c *composer) wrapGenerated(h string) string {
	for token, block := range c.blocks {
		p := "<p>" + token + "</p>"
		if strings.Contains(h, p) {
			h = strings.Replace(h, p, `<div class="blyg-tk-gen">`+block+`</div>`, 1)
		} else if strings.Contains(h, token) {
			c.problem("[TK]", "a generated block could not be placed: leave a blank line before and after it")
		}
	}
	if !c.inline || !strings.ContainsAny(h, inlineOpen+inlineClose) {
		return h
	}
	h = strings.NewReplacer(pctEncoded(inlineOpen), "", pctEncoded(inlineClose), "").Replace(h)
	strip := strings.NewReplacer(inlineOpen, "", inlineClose, "")
	var b strings.Builder
	open, pending := false, false
	for _, part := range splitTags(h) {
		if strings.HasPrefix(part, "<") {
			if open && blockTagName.MatchString(part) {
				b.WriteString("</span>")
				open, pending = false, true
			}
			b.WriteString(strip.Replace(part))
			continue
		}
		for part != "" {
			i := strings.IndexAny(part, inlineOpen+inlineClose)
			seg := part
			if i >= 0 {
				seg = part[:i]
			}
			if pending && strings.TrimSpace(seg) != "" {
				b.WriteString(`<span class="blyg-tk-gen">`)
				open, pending = true, false
			}
			b.WriteString(seg)
			if i < 0 {
				break
			}
			if strings.HasPrefix(part[i:], inlineOpen) {
				pending = true
			} else {
				if open {
					b.WriteString("</span>")
				}
				open, pending = false, false
			}
			part = part[i+len(inlineOpen):] // all sentinels are 3 bytes
		}
	}
	if open {
		b.WriteString("</span>")
	}
	return b.String()
}

// ── helpers ─────────────────────────────────────────────────────────────

// AbsolutizeHTML resolves every relative href and src against base. Tags
// it does not change keep their bytes.
func AbsolutizeHTML(h, base string) string {
	b, err := url.Parse(base)
	if err != nil || !b.IsAbs() {
		return h
	}
	z := html.NewTokenizer(strings.NewReader(h))
	var out strings.Builder
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			return out.String()
		}
		raw := string(z.Raw())
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			out.WriteString(raw)
			continue
		}
		tok := z.Token()
		changed := false
		for i, a := range tok.Attr {
			if a.Namespace != "" || (a.Key != "href" && a.Key != "src") {
				continue
			}
			ref, err := url.Parse(strings.TrimSpace(a.Val))
			if err != nil || ref.IsAbs() {
				continue
			}
			tok.Attr[i].Val = b.ResolveReference(ref).String()
			changed = true
		}
		if changed {
			out.WriteString(tok.String())
		} else {
			out.WriteString(raw)
		}
	}
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
