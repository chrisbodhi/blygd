package blyg

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Fork flattening (§5.6 rule 6, decision #57). A fork copies the pinned
// *document*, not its composition: a directive or [[id]] copied as written
// would re-resolve in the forker's context, and the fork would descend from
// bytes nobody pinned. So:
//
//   - the source's own prose is copied byte-exact from the pinned content_md;
//   - each baked quote becomes an ordinary markdown blockquote of its baked
//     content, recursively, closed by an attribution line; partiality is
//     read from the blyg-partial class, never from > lines (decision #62);
//   - each [[id]] becomes an ordinary link to the page the pinned bake
//     linked (decision #63);
//   - no blyg-transclusion survives, and nothing enters transclusions[];
//   - generated spans come back as [TK]impyrt …[/TK], so a fork cannot
//     launder a disclosure away (§5.7 rule 7).
//
// When the prose cannot be matched to the bake exactly, the whole fork is
// rebuilt from the pinned HTML: byte-exactness is given up before a
// disclosure is. Mirrors the reference client's fork-flatten.ts.

// ForkSource is the pinned document a fork copies.
type ForkSource struct {
	Origin      string // the source's identity origin
	ContentMD   string
	ContentHTML string
	Generated   []Generated
}

// QuoteLink returns where an attribution line links for a quoted item: its
// page where the publisher can compute it, else its item document.
type QuoteLink func(origin, id string) string

// FlattenFork builds a fork's content_md. exact reports whether the own
// prose was copied byte-exact.
func FlattenFork(src ForkSource, link QuoteLink) (md string, exact bool) {
	f := &flattener{link: link}
	tree := parseFragment(src.ContentHTML)
	quotes := topLevelQuotes(tree)
	anchors := ownAnchors(tree)
	fallback := func() (string, bool) { return f.htmlToMarkdown(tree, src.Origin), false }

	lines := strings.Split(src.ContentMD, "\n")
	starts := lineOffsets(lines)
	code := codeSpans(src.ContentMD)
	var out []string
	q, a := 0, 0
	for i := 0; i < len(lines); i++ {
		if id, at := directiveAt(lines[i]); id != "" && !inSpans(code, starts[i]+at) {
			if q >= len(quotes) {
				return fallback()
			}
			baked := quotes[q]
			q++
			out = append(out, f.flattenQuote(baked, src.Origin))
			if hasClass(baked, "blyg-partial") {
				i = partialEnd(lines, i, baked) - 1
			}
			if i+1 < len(lines) && strings.TrimSpace(lines[i+1]) != "" {
				// What follows was a separate block in the pinned rendering
				// (directives split the thread). Adjacent to a blockquote it
				// would join it by lazy continuation, or merge with the
				// author's own quotation.
				out = append(out, "")
			}
			continue
		}
		line, unmatched := lines[i], false
		var b strings.Builder
		last := 0
		for _, m := range inlineLink.FindAllStringSubmatchIndex(line, -1) {
			if (m[0] > 0 && line[m[0]-1] == '!') || inSpans(code, starts[i]+m[0]) {
				continue
			}
			id := line[m[2]:m[3]]
			for a < len(anchors) && !strings.Contains(nodeAttr(anchors[a], "href"), id) {
				a++
			}
			if a >= len(anchors) {
				unmatched = true
				break
			}
			b.WriteString(line[last:m[0]])
			b.WriteString(f.inline([]*html.Node{anchors[a]}))
			a++
			last = m[1]
		}
		if unmatched {
			return fallback()
		}
		b.WriteString(line[last:])
		out = append(out, b.String())
	}
	if q != len(quotes) {
		return fallback()
	}
	md = strings.Join(out, "\n")

	cursor := 0
	for i, span := range ownGenerated(tree) {
		var text string
		if span.DataAtom == atom.Div {
			text = strings.Join(f.blocks(children(span), src.Origin), "\n\n")
		} else {
			text = f.inline(children(span))
		}
		at := indexFrom(md, text, cursor)
		if text == "" || at < 0 {
			return fallback()
		}
		model := ""
		if i < len(src.Generated) {
			model = src.Generated[i].Model
		}
		wrapped := impyrt(model, text)
		md = md[:at] + wrapped + md[at+len(text):]
		cursor = at + len(wrapped)
	}
	return md, true
}

func impyrt(model, text string) string {
	if model != "" && !strings.ContainsAny(model, " \t\n=") {
		return "[TK]impyrt " + model + "=" + text + "[/TK]"
	}
	return "[TK]impyrt=" + text + "[/TK]"
}

type flattener struct{ link QuoteLink }

func parseFragment(h string) []*html.Node {
	nodes, err := html.ParseFragment(strings.NewReader(h), &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body})
	if err != nil {
		return nil
	}
	return nodes
}

func children(n *html.Node) []*html.Node {
	var out []*html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		out = append(out, c)
	}
	return out
}

func nodeAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hasClass(n *html.Node, class string) bool {
	return n.Type == html.ElementNode && strings.Contains(" "+strings.Join(strings.Fields(nodeAttr(n, "class")), " ")+" ", " "+class+" ")
}

func isQuote(n *html.Node) bool { return hasClass(n, "blyg-transclusion") }
func isGen(n *html.Node) bool   { return hasClass(n, "blyg-tk-gen") }

// collect walks elements in document order. take reports whether to keep a
// node; descend whether to look inside one that was not kept.
func collect(nodes []*html.Node, take, descend func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	for _, n := range nodes {
		if n.Type != html.ElementNode {
			continue
		}
		if take(n) {
			out = append(out, n)
		} else if descend(n) {
			out = append(out, collect(children(n), take, descend)...)
		}
	}
	return out
}

func always(*html.Node) bool     { return true }
func notQuote(n *html.Node) bool { return !isQuote(n) }

// topLevelQuotes are the baked quotes in document order; nested ones belong
// to their parent.
func topLevelQuotes(nodes []*html.Node) []*html.Node { return collect(nodes, isQuote, always) }

// ownGenerated and ownAnchors are the source's own: a quote's are its own.
func ownGenerated(nodes []*html.Node) []*html.Node {
	return collect(nodes, func(n *html.Node) bool { return !isQuote(n) && isGen(n) }, notQuote)
}

func ownAnchors(nodes []*html.Node) []*html.Node {
	return collect(nodes, func(n *html.Node) bool { return !isQuote(n) && n.DataAtom == atom.A }, notQuote)
}

func textOf(nodes []*html.Node) string {
	var b strings.Builder
	for _, n := range nodes {
		switch {
		case n.Type == html.TextNode:
			b.WriteString(n.Data)
		case n.DataAtom == atom.Br:
			b.WriteString("\n")
		case n.Type == html.ElementNode:
			b.WriteString(textOf(children(n)))
		}
	}
	return b.String()
}

var (
	mdSpecial   = regexp.MustCompile("([\\\\`*_\\[\\]<])")
	softNewline = regexp.MustCompile(`\s*\n\s*`)
)

func escapeMD(s string) string { return mdSpecial.ReplaceAllString(s, `\$1`) }

// inline converts inline HTML to markdown.
func (f *flattener) inline(nodes []*html.Node) string {
	var b strings.Builder
	for _, n := range nodes {
		if n.Type == html.TextNode {
			b.WriteString(softNewline.ReplaceAllString(escapeMD(n.Data), " "))
			continue
		}
		if n.Type != html.ElementNode {
			continue
		}
		inner := func() string { return f.inline(children(n)) }
		switch n.DataAtom {
		case atom.Em, atom.I:
			b.WriteString("*" + inner() + "*")
		case atom.Strong, atom.B:
			b.WriteString("**" + inner() + "**")
		case atom.Code:
			t := textOf(children(n))
			fence := "`"
			if strings.Contains(t, "`") {
				fence = "``"
			}
			b.WriteString(fence + t + fence)
		case atom.A:
			b.WriteString("[" + inner() + "](" + nodeAttr(n, "href") + ")")
		case atom.Img:
			b.WriteString("![" + escapeMD(nodeAttr(n, "alt")) + "](" + nodeAttr(n, "src") + ")")
		case atom.Br:
			b.WriteString("  \n")
		case atom.Span:
			if isGen(n) {
				b.WriteString(impyrt("", inner()))
			} else {
				b.WriteString(inner())
			}
		default:
			b.WriteString(inner())
		}
	}
	return b.String()
}

var flattenBlocks = map[atom.Atom]bool{
	atom.P: true, atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true,
	atom.Ul: true, atom.Ol: true, atom.Li: true, atom.Pre: true, atom.Blockquote: true, atom.Div: true,
	atom.Hr: true, atom.Table: true, atom.Figure: true, atom.Section: true,
}

// blocks converts a run of nodes to markdown blocks.
func (f *flattener) blocks(nodes []*html.Node, origin string) []string {
	var out []string
	var run []*html.Node
	flush := func() {
		if t := strings.TrimSpace(f.inline(run)); t != "" {
			out = append(out, t)
		}
		run = nil
	}
	for _, n := range nodes {
		if n.Type != html.ElementNode || !flattenBlocks[n.DataAtom] {
			if n.Type == html.TextNode && strings.TrimSpace(n.Data) == "" && len(run) == 0 {
				continue
			}
			run = append(run, n)
			continue
		}
		flush()
		out = append(out, f.block(n, origin)...)
	}
	flush()
	return out
}

// partialEnd returns the index of the first line after the blockquote
// attached to the partial directive at lines[i]. Its extent is CommonMark's
// (§10.1, decision #76), but a document composed before that rule, or by a
// client that ends the quote at the first line without `>`, selected less:
// the bake is what was selected, so lazy lines are the quote's only when
// the bake holds them, and otherwise they are the author's prose.
func partialEnd(lines []string, i int, baked *html.Node) int {
	quote, next, _ := attachedQuote(lines, i)
	marked := i + 1
	for marked < next && quoteLine.MatchString(lines[marked]) {
		marked++
	}
	if marked == next {
		return next
	}
	var bake strings.Builder
	for _, c := range children(baked) {
		_ = html.Render(&bake, c)
	}
	if rendered, err := RenderMarkdown(quote, ""); err == nil && SelectionText(rendered) == SelectionText(bake.String()) {
		return next
	}
	return marked
}

func quoteMD(md string) string {
	lines := strings.Split(md, "\n")
	for i, l := range lines {
		if l == "" {
			lines[i] = ">"
		} else {
			lines[i] = "> " + l
		}
	}
	return strings.Join(lines, "\n")
}

func (f *flattener) block(n *html.Node, origin string) []string {
	if isQuote(n) {
		return []string{f.flattenQuote(n, origin)}
	}
	switch n.DataAtom {
	case atom.P:
		if t := strings.TrimSpace(f.inline(children(n))); t != "" {
			return []string{t}
		}
		return nil
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		level := int(n.Data[1] - '0')
		return []string{strings.Repeat("#", level) + " " + strings.TrimSpace(f.inline(children(n)))}
	case atom.Hr:
		return []string{"---"}
	case atom.Pre:
		t := strings.TrimSuffix(textOf(children(n)), "\n")
		fence := "```"
		if strings.Contains(t, "```") {
			fence = "~~~"
		}
		return []string{fence + "\n" + t + "\n" + fence}
	case atom.Blockquote:
		return []string{quoteMD(strings.Join(f.blocks(children(n), origin), "\n\n"))}
	case atom.Ul, atom.Ol:
		var lines []string
		i := 0
		for _, li := range children(n) {
			if li.DataAtom != atom.Li {
				continue
			}
			i++
			marker := "- "
			if n.DataAtom == atom.Ol {
				marker = strconv.Itoa(i) + ". "
			}
			body := strings.Join(f.blocks(children(li), origin), "\n\n")
			lines = append(lines, marker+strings.ReplaceAll(body, "\n", "\n"+strings.Repeat(" ", len(marker))))
		}
		return []string{strings.Join(lines, "\n")}
	case atom.Div:
		if isGen(n) {
			return []string{impyrt("", strings.Join(f.blocks(children(n), origin), "\n\n"))}
		}
	}
	return f.blocks(children(n), origin)
}

// flattenQuote is one baked quote as a plain blockquote with an
// attribution line; nested quotes nest. A nested quote with no
// data-blyg-origin was baked by the layer around it.
func (f *flattener) flattenQuote(n *html.Node, origin string) string {
	if o := nodeAttr(n, "data-blyg-origin"); o != "" {
		origin = o
	}
	id, version := nodeAttr(n, "data-blyg-id"), nodeAttr(n, "data-blyg-version")
	body := strings.Join(f.blocks(children(n), origin), "\n\n")
	host := origin
	if u, err := url.Parse(origin); err == nil && u.Host != "" {
		host = u.Host
	}
	label := host
	if version != "" {
		label += " · v" + version
	}
	attribution := "— quoted from " + label
	if id != "" {
		attribution = "— quoted from [" + label + "](" + f.link(origin, id) + ")"
	}
	return quoteMD(body + "\n\n" + attribution)
}

// htmlToMarkdown is the whole pinned HTML as markdown: the fallback that
// never loses a disclosure.
func (f *flattener) htmlToMarkdown(tree []*html.Node, origin string) string {
	return strings.Join(f.blocks(tree, origin), "\n\n") + "\n"
}
