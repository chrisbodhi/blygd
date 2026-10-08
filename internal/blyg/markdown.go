package blyg

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"

	"github.com/rivo/uniseg"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	"golang.org/x/net/html"
)

// RenderMarkdown renders content_md to self-contained content_html.
//
//   - Raw HTML in the markdown is escaped and shown as text, never passed
//     through (the reference client's markdown-it `html: false`; §14).
//   - Every link and image URL is made absolute against pageURL, because
//     content_html travels as the feed description, into importers' stores,
//     and baked into other origins' threads (§5.2, decision #53).
//   - Bare URLs are linkified, as the reference client does.
//
// Transclusion directives and [[id]] links (§10.1) are not handled here;
// thread resolution is Phase 3 and will run before this.
func RenderMarkdown(contentMD, pageURL string) (string, error) {
	md := goldmark.New(
		goldmark.WithExtensions(extension.Linkify),
		goldmark.WithParser(safeParser(pageURL)),
	)
	var buf bytes.Buffer
	if err := md.Convert([]byte(contentMD), &buf); err != nil {
		return "", fmt.Errorf("render markdown: %w", err)
	}
	return buf.String(), nil
}

// safeParser is goldmark's default parser minus its two raw-HTML parsers,
// so HTML in the source is ordinary text and gets escaped on output.
func safeParser(pageURL string) parser.Parser {
	var blocks []util.PrioritizedValue
	for _, v := range parser.DefaultBlockParsers() {
		if !strings.Contains(fmt.Sprintf("%T", v.Value), "htmlBlockParser") {
			blocks = append(blocks, v)
		}
	}
	var inlines []util.PrioritizedValue
	for _, v := range parser.DefaultInlineParsers() {
		if !strings.Contains(fmt.Sprintf("%T", v.Value), "rawHTMLParser") {
			inlines = append(inlines, v)
		}
	}
	return parser.NewParser(
		parser.WithBlockParsers(blocks...),
		parser.WithInlineParsers(inlines...),
		parser.WithParagraphTransformers(parser.DefaultParagraphTransformers()...),
		parser.WithASTTransformers(util.Prioritized(absoluteURLs{base: pageURL}, 100)),
	)
}

// absoluteURLs rewrites link and image destinations to absolute URLs.
type absoluteURLs struct{ base string }

func (a absoluteURLs) Transform(doc *ast.Document, _ text.Reader, _ parser.Context) {
	base, err := url.Parse(a.base)
	if err != nil || a.base == "" {
		return
	}
	abs := func(dest []byte) []byte {
		ref, err := url.Parse(string(dest))
		if err != nil {
			return dest
		}
		return []byte(base.ResolveReference(ref).String())
	}
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.Link:
			n.Destination = abs(n.Destination)
		case *ast.Image:
			n.Destination = abs(n.Destination)
		}
		return ast.WalkContinue, nil
	})
}

// PlainText is the text content of rendered HTML on one line: tags
// stripped, entities decoded, block boundaries turned into spaces, all
// whitespace collapsed. Inline tags add nothing, so "<strong>x</strong>."
// reads "x.". Used for excerpts.
func PlainText(contentHTML string) string {
	z := html.NewTokenizer(strings.NewReader(contentHTML))
	var b strings.Builder
	for {
		switch z.Next() {
		case html.ErrorToken:
			return strings.Join(strings.Fields(b.String()), " ")
		case html.TextToken:
			b.Write(z.Text())
		case html.StartTagToken, html.EndTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			if blockTags[string(name)] {
				b.WriteByte(' ')
			}
		}
	}
}

var blockTags = map[string]bool{
	"p": true, "div": true, "br": true, "hr": true, "li": true, "ul": true, "ol": true,
	"blockquote": true, "pre": true, "h1": true, "h2": true, "h3": true, "h4": true,
	"h5": true, "h6": true, "table": true, "tr": true, "td": true, "th": true,
	"section": true, "article": true, "figure": true, "figcaption": true, "dl": true, "dt": true, "dd": true,
}

// Excerpt is the first n grapheme clusters of the plain text, trimmed and
// ellipsized when cut — the reference client's excerpt(), at n = 60 for
// feed titles.
func Excerpt(contentHTML string, n int) string {
	t := PlainText(contentHTML)
	if uniseg.GraphemeClusterCount(t) <= n {
		return t
	}
	var b strings.Builder
	g := uniseg.NewGraphemes(t)
	for i := 0; i < n && g.Next(); i++ {
		b.WriteString(g.Str())
	}
	return strings.TrimRight(b.String(), " \t\n") + "…"
}
