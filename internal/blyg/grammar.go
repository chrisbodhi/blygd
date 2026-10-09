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
const idClass = `[0-9a-hjkmnp-tv-z]{26}`

var (
	directiveLine = regexp.MustCompile(`^[ \t]*!\[\[(` + idClass + `)\]\][ \t]*$`)
	reservedLine  = regexp.MustCompile(`^[ \t]*!\[\[` + idClass + `@v\d+\]\][ \t]*$`)
	// Go has no lookbehind; a match preceded by '!' is the directive form
	// written inline, which is inert text (§10.1), and callers skip it.
	inlineLink = regexp.MustCompile(`\[\[(` + idClass + `)\]\]`)
	quoteLine  = regexp.MustCompile(`^[ \t]*>`)
	quoteMark  = regexp.MustCompile(`^[ \t]*> ?`)
)

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
func attachedQuote(lines []string, i int) (quote string, next int, ok bool) {
	j := i + 1
	var run []string
	for j < len(lines) && quoteLine.MatchString(lines[j]) {
		run = append(run, quoteMark.ReplaceAllString(lines[j], ""))
		j++
	}
	return strings.Join(run, "\n"), j, len(run) > 0
}

// directiveAt reports the directive id on a line, and the offset of its
// '!' within the line, or "" when the line is not a directive.
func directiveAt(line string) (string, int) {
	m := directiveLine.FindStringSubmatchIndex(line)
	if m == nil {
		return "", 0
	}
	return line[m[2]:m[3]], strings.IndexByte(line, '!')
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
		if id, at := directiveAt(l); id != "" && !inSpans(code, starts[i]+at) {
			return true
		}
	}
	for _, m := range inlineLink.FindAllStringIndex(md, -1) {
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
