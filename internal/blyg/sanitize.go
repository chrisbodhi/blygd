package blyg

import (
	"html/template"
	"regexp"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// pagePolicy is what our HTML pages let through. A thread's content_html
// carries other origins' HTML baked verbatim (§10.2), and the wire must
// keep those bytes, so the pages sanitize instead (§14): the same answer
// the reference client gives. The protocol's own classes and data
// attributes survive, so a bake still reads as a quote.
var pagePolicy = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.RequireNoFollowOnLinks(false)
	p.AllowAttrs("class").Matching(regexp.MustCompile(`^blyg-(transclusion|partial|tk-gen)( blyg-(transclusion|partial|tk-gen))*$`)).
		OnElements("blockquote", "span", "div")
	p.AllowAttrs("data-blyg-id", "data-blyg-version").Matching(regexp.MustCompile(`^[0-9a-z]{1,26}$`)).OnElements("blockquote")
	p.AllowAttrs("data-blyg-origin").Matching(regexp.MustCompile(`^https?://[^\s"<>]+$`)).OnElements("blockquote")
	// A math element that reached the policy is one mathText could not
	// read; it goes whole, never as loose text (§14).
	p.SkipElementsContent("math")
	return p
}()

var bakeOpenTag = regexp.MustCompile(`<blockquote class="blyg-transclusion`)

// PageHTML sanitizes content_html for one of our pages.
//
//   - Mathematics (§14, decision #105) is a second vocabulary with its own
//     parsing rules, so a math element is never passed through: it is
//     replaced by the TeX its author wrote, as text, or dropped when it
//     carries none (mathText).
//   - A baked quote is isolated directionally (§14, decision #103), so that
//     bidirectional controls in a stranger's words cannot reorder ours.
func PageHTML(contentHTML string) template.HTML {
	clean := pagePolicy.Sanitize(mathText(contentHTML))
	clean = bakeOpenTag.ReplaceAllString(clean, `<blockquote dir="auto" class="blyg-transclusion`)
	return template.HTML(clean) //nolint:gosec // sanitized above
}

// mathText replaces each math element with its application/x-tex
// annotation shown as text, in <code> (<pre><code> for display="block"),
// or with nothing when there is none: §14's third and fourth treatments.
// The element is found by an HTML5 tree parser, which reads MathML's
// foreign content as a browser does, and what replaces it is built from
// one escaped string, never copied from the input. HTML without math is
// returned untouched.
func mathText(h string) string {
	if !strings.Contains(strings.ToLower(h), "<math") {
		return h
	}
	body := &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body}
	nodes, err := html.ParseFragment(strings.NewReader(h), body)
	if err != nil {
		return ""
	}
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; {
			next := c.NextSibling
			if c.Type == html.ElementNode && c.Namespace == "math" && c.Data == "math" {
				if r := mathReplacement(c); r != nil {
					n.InsertBefore(r, c)
				}
				n.RemoveChild(c)
			} else {
				walk(c)
			}
			c = next
		}
	}
	var b strings.Builder
	for _, n := range nodes {
		wrap := &html.Node{Type: html.DocumentNode}
		wrap.AppendChild(n)
		walk(wrap)
		for c := wrap.FirstChild; c != nil; c = c.NextSibling {
			if err := html.Render(&b, c); err != nil {
				return ""
			}
		}
	}
	return b.String()
}

func mathReplacement(m *html.Node) *html.Node {
	tex := ""
	var find func(n *html.Node) bool
	find = func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.Namespace == "math" && n.Data == "annotation" && mathAttr(n, "encoding") == "application/x-tex" {
			tex = strings.TrimSpace(mathTextOf(n))
			return true
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if find(c) {
				return true
			}
		}
		return false
	}
	if !find(m) || tex == "" {
		return nil
	}
	code := &html.Node{Type: html.ElementNode, Data: "code", DataAtom: atom.Code}
	code.AppendChild(&html.Node{Type: html.TextNode, Data: tex})
	if mathAttr(m, "display") != "block" {
		return code
	}
	pre := &html.Node{Type: html.ElementNode, Data: "pre", DataAtom: atom.Pre}
	pre.AppendChild(code)
	return pre
}

func mathAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Namespace == "" && a.Key == key {
			return a.Val
		}
	}
	return ""
}

func mathTextOf(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}
