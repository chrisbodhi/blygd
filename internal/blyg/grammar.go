package blyg

import (
	"html"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// The bracket grammar of §10.1. It is permanent protocol surface: a line
// that is only `![[id]]` is a transclusion directive, `![[id@vN]]` on its
// own line is reserved, and `[[id]]` anywhere outside code is a plain link.
// Either form may carry a label after the id, `[[id | text]]` (decision
// #70). A line ends at LF or CRLF alike (§10.1): callers split on LF, so a
// line may carry its CR, and the CR stays in content_md.
const idClass = `[0-9a-hjkmnp-tv-z]{26}`

var (
	reservedLine = regexp.MustCompile(`^[ \t]*!\[\[` + idClass + `@v\d+\]\][ \t]*\r?$`)
	quoteLine    = regexp.MustCompile(`^[ \t]*>`)
	quoteMark    = regexp.MustCompile(`^[ \t]*> ?`)
)

// findLinks finds the inline bracket form, labels included: `[[id]]`, or
// `[[id | label]]` with the label running to the first `]]` on its line
// (§10.1). Each match is {start, end, idStart, idEnd, labelStart,
// labelEnd}, the label pair -1 when there is none; the label is untrimmed.
// A match preceded by '!' is the directive form written inline, which is
// inert text (§10.1), and callers skip it. Brackets whose first 26
// characters are not an id, or with anything but a pipe after the id, are
// literal.
func findLinks(s string) [][]int {
	var out [][]int
	for i := 0; ; {
		j := strings.Index(s[i:], "[[")
		if j < 0 {
			return out
		}
		at := i + j
		i = at + 1
		idStart, idEnd := at+2, at+2+IDLen
		if idEnd > len(s) || !ValidID(s[idStart:idEnd]) {
			continue
		}
		if strings.HasPrefix(s[idEnd:], "]]") {
			out = append(out, []int{at, idEnd + 2, idStart, idEnd, -1, -1})
			i = idEnd + 2
			continue
		}
		k := idEnd
		for k < len(s) && (s[k] == ' ' || s[k] == '\t') {
			k++
		}
		if k >= len(s) || s[k] != '|' {
			continue
		}
		line := s[k+1:]
		if nl := strings.IndexByte(line, '\n'); nl >= 0 {
			line = line[:nl]
		}
		if end := strings.Index(line, "]]"); end >= 0 {
			out = append(out, []int{at, k + 1 + end + 2, idStart, idEnd, k + 1, k + 1 + end})
			i = k + 1 + end + 2
		}
	}
}

// linkLabel is a findLinks match's label, trimmed; "" when it has none. An
// empty label is no label (§10.1).
func linkLabel(s string, m []int) string {
	if m[4] < 0 {
		return ""
	}
	return strings.TrimSpace(s[m[4]:m[5]])
}

// span is a half-open byte range.
type span struct{ start, end int }

func inSpans(ss []span, pos int) bool {
	for _, s := range ss {
		if pos >= s.start && pos < s.end {
			return true
		}
	}
	return false
}

// codeSpans finds where the bracket grammar is inert: inside code spans and
// code blocks (§10.1, decision #54). It asks the renderer's own parser, so
// fences in lists and blockquotes are found exactly as they will render. A
// block's range runs from the start of each of its lines, so a whole line
// inside code is covered whatever its indentation.
func codeSpans(src string) []span {
	b := []byte(src)
	doc := goldmark.New(goldmark.WithParser(safeParser("", ""))).Parser().Parse(text.NewReader(b))
	var out []span
	lines := func(n ast.Node) {
		for i := 0; i < n.Lines().Len(); i++ {
			seg := n.Lines().At(i)
			out = append(out, span{lineStart(src, seg.Start), seg.Stop})
		}
	}
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.FencedCodeBlock:
			if n.Info != nil {
				out = append(out, span{lineStart(src, n.Info.Segment.Start), n.Info.Segment.Stop})
			}
			lines(n)
			return ast.WalkSkipChildren, nil
		case *ast.CodeBlock:
			lines(n)
			return ast.WalkSkipChildren, nil
		case *ast.CodeSpan:
			for c := n.FirstChild(); c != nil; c = c.NextSibling() {
				if t, ok := c.(*ast.Text); ok {
					out = append(out, span{t.Segment.Start, t.Segment.Stop})
				}
			}
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	return out
}

func lineStart(s string, pos int) int {
	return strings.LastIndexByte(s[:pos], '\n') + 1
}

// lineOffsets returns the byte offset at which each line starts.
func lineOffsets(lines []string) []int {
	out := make([]int, len(lines))
	at := 0
	for i, l := range lines {
		out[i] = at
		at += len(l) + 1
	}
	return out
}

// attachedQuote returns the markdown of the blockquote attached to the
// directive at lines[i] (§10.1: no blank line between), with each line's
// marker and at most one following space removed, and the index of the
// first line after it. quote is "" and ok false when nothing is attached.
//
// The blockquote's extent is CommonMark's, lazy continuation included
// (§10.1, decision #76): a line without its own `>` belongs to the quote
// when the renderer's parser reads it as part of the same blockquote.
func attachedQuote(lines []string, i int) (quote string, next int, ok bool) {
	j := i + 1
	var raw, run []string
	for j < len(lines) {
		l := lines[j]
		switch {
		case quoteLine.MatchString(l):
		case len(run) > 0 && strings.TrimSpace(l) != "" && oneBlockquote(append(raw, l)):
		default:
			return strings.Join(run, "\n"), j, len(run) > 0
		}
		raw = append(raw, l)
		run = append(run, quoteMark.ReplaceAllString(l, ""))
		j++
	}
	return strings.Join(run, "\n"), j, len(run) > 0
}

// oneBlockquote reports whether lines parse as a single blockquote, which
// is to say the last line is a lazy continuation of the quote before it.
func oneBlockquote(lines []string) bool {
	src := []byte(strings.Join(lines, "\n"))
	doc := goldmark.New(goldmark.WithParser(safeParser("", ""))).Parser().Parse(text.NewReader(src))
	first := doc.FirstChild()
	return first != nil && first.Kind() == ast.KindBlockquote && first.NextSibling() == nil
}

// directiveAt reports the directive id on a line, its label, and the offset
// of its '!' within the line; the id is "" when the line is not a
// directive.
func directiveAt(line string) (id, label string, at int) {
	id, label, ok := labelledDirective(line)
	if !ok {
		return "", "", 0
	}
	return id, label, strings.IndexByte(line, '!')
}

// HasThreadGrammar reports whether content_md contains a live directive,
// reserved directive, or [[id]] link outside code.
func HasThreadGrammar(md string) bool {
	code := codeSpans(md)
	lines := strings.Split(md, "\n")
	starts := lineOffsets(lines)
	for i, l := range lines {
		if reservedLine.MatchString(l) && !inSpans(code, starts[i]+strings.IndexByte(l, '!')) {
			return true
		}
		if id, _, at := directiveAt(l); id != "" && !inSpans(code, starts[i]+at) {
			return true
		}
	}
	for _, m := range findLinks(md) {
		if (m[0] == 0 || md[m[0]-1] != '!') && !inSpans(code, m[0]) {
			return true
		}
	}
	return false
}

// ── selection text (§10.2) ──────────────────────────────────────────────

var (
	scriptStyle   = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>|<style[^>]*>.*?</style>`)
	blockBoundary = regexp.MustCompile(`(?i)</(?:p|h[1-6]|li|blockquote|pre|div|tr|section|article)>|<br\s*/?>`)
	anyTag        = regexp.MustCompile(`<[^>]+>`)
)

// SelectionText is §10.2's text content of rendered HTML, the haystack and
// the needle of the partial-quote check: tags stripped, entities decoded,
// whitespace collapsed within a block, block boundaries kept as line
// breaks, empty blocks dropped. It matches the reference client's
// selectionText, because any reader may re-run the check and two
// normalizers that differ by one space verify differently.
//
// Boundaries are marked before whitespace collapses, so a newline the author
// typed inside a paragraph is a space and only a block end is a break.
func SelectionText(contentHTML string) string {
	h := scriptStyle.ReplaceAllString(contentHTML, " ")
	h = blockBoundary.ReplaceAllString(h, "\x00")
	h = html.UnescapeString(anyTag.ReplaceAllString(h, ""))
	var out []string
	for _, seg := range strings.Split(h, "\x00") {
		if seg = strings.Join(strings.Fields(seg), " "); seg != "" {
			out = append(out, seg)
		}
	}
	return strings.Join(out, "\n")
}

// selectorContext is how much text either side of a match the selector
// records (§10.3: prefix and suffix are short).
const selectorContext = 32

// Selector is the W3C text-quote shape of a partial transclusion (§10.3).
type Selector struct {
	Exact  string `json:"exact"`
	Prefix string `json:"prefix,omitempty"`
	Suffix string `json:"suffix,omitempty"`
}

// LocateSelection finds selection in the target's text content and
// describes where, or returns nil when it is not there. Context comes from
// the first match: arbitrary but deterministic, and only a hint.
func LocateSelection(targetHTML, selection string) *Selector {
	hay := SelectionText(targetHTML)
	at := strings.Index(hay, selection)
	if at < 0 {
		return nil
	}
	before := []rune(hay[:at])
	after := []rune(hay[at+len(selection):])
	if len(before) > selectorContext {
		before = before[len(before)-selectorContext:]
	}
	if len(after) > selectorContext {
		after = after[:selectorContext]
	}
	return &Selector{Exact: selection, Prefix: string(before), Suffix: string(after)}
}

// escapeHTML escapes the five characters the reference client escapes.
func escapeHTML(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;").Replace(s)
}

// labelledDirective reads a transclusion directive line with or without a
// label (§10.1, decision #70): `![[id]]` or `![[id | label]]`, surrounding
// whitespace allowed. The id is the 26 characters after the brackets; the
// label is everything after the pipe up to the first `]]`, which must end
// the line, so a label may hold a pipe but never `]]`.
func labelledDirective(line string) (id, label string, ok bool) {
	t := strings.TrimRight(strings.TrimLeft(line, " \t"), " \t\r")
	if len(t) < 31 || !strings.HasPrefix(t, "![[") || !ValidID(t[3:29]) {
		return "", "", false
	}
	id, rest := t[3:29], t[29:]
	if rest == "]]" {
		return id, "", true
	}
	rest = strings.TrimLeft(rest, " \t")
	end := strings.Index(rest, "]]")
	if !strings.HasPrefix(rest, "|") || end != len(rest)-2 {
		return "", "", false
	}
	return id, strings.TrimSpace(rest[1:end]), true
}

// HasOwnWords reports whether a document has words of its own once its
// transclusion directives, labels included, and each partial selection
// attached to one are set aside. A stub or transclusion from a document
// without them is a repost (§15.4, decision #80).
//
// How far a partial selection runs is read from the bake, as fork
// flattening reads it: a document composed before CommonMark's extent was
// the rule selected less, and its lazy lines are the author's. A blockquote
// after a whole bake is the author's quotation (decision #62). When the
// bakes do not line up with the directives, §10.1's extent applies.
func HasOwnWords(contentMD, contentHTML string) bool {
	quotes := topLevelQuotes(parseFragment(contentHTML))
	code := codeSpans(contentMD)
	lines := strings.Split(contentMD, "\n")
	starts := lineOffsets(lines)
	live := func(i int) bool {
		_, _, ok := labelledDirective(lines[i])
		return ok && !inSpans(code, starts[i]+strings.IndexByte(lines[i], '!'))
	}
	directives := 0
	for i := range lines {
		if live(i) {
			directives++
		}
	}
	q := 0
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if live(i) {
			switch {
			case len(quotes) != directives:
				_, next, _ := attachedQuote(lines, i)
				i = next - 1
			case hasClass(quotes[q], "blyg-partial"):
				i = partialEnd(lines, i, quotes[q]) - 1
			}
			q++
			continue
		}
		if strings.TrimSpace(l) != "" {
			return true
		}
	}
	return false
}
