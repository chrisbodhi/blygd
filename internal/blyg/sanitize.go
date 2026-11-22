package blyg

import (
	"html/template"
	"regexp"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
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
	// A list numbered from 3 reads from 3: goldmark writes start for our
	// own lists, and a whole quote of someone's carries theirs verbatim
	// (§10.2), so a page that dropped it renumbered both from 1. §16.15
	// (twentieth revision; ruled, not yet in force) notes a whole quote
	// already needs it, and asks the same of a reader's sanitizer.
	p.AllowAttrs("start").Matching(regexp.MustCompile(`^-?[0-9]{1,9}$`)).OnElements("ol")
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
// What replaces it is built from one escaped string, never copied from the
// input, and every byte outside a math element is kept as it was. HTML
// without math is returned untouched.
//
// The extent of an element is read with the tokenizer, not a tree parser,
// so that damage stays inside the stranger's own bytes: an unclosed
// <math> runs to its </math>, or to the end tag of any element that was
// open outside it (the </p> or </blockquote> of the bake that holds it),
// whichever comes first, and never past (review of PR #5). Inside, no tag
// starts raw text, as in foreign content. Where this reading and a
// browser's still differ (raw text inside <mtext>, an HTML integration
// point), the difference is only in how much of the stranger's math is
// dropped: nothing is passed on unsanitized, because bluemonday sanitizes
// every byte that is kept.
func mathText(h string) string {
	if !strings.Contains(strings.ToLower(h), "<math") {
		return h
	}
	z := html.NewTokenizer(strings.NewReader(h))
	var out strings.Builder
	var open []string // elements open outside math, innermost last
	inOpen := func(name string) bool {
		for i := len(open) - 1; i >= 0; i-- {
			if open[i] == name {
				return true
			}
		}
		return false
	}
	pop := func(name string) {
		for i := len(open) - 1; i >= 0; i-- {
			if open[i] == name {
				open = open[:i]
				return
			}
		}
	}
	var (
		depth       int // > 0 while skipping a math element
		display     bool
		tex         strings.Builder
		inTeX, have bool
	)
	emit := func() {
		if t := strings.TrimSpace(tex.String()); t != "" {
			code := "<code>" + html.EscapeString(t) + "</code>"
			if display {
				code = "<pre>" + code + "</pre>"
			}
			out.WriteString(code)
		}
		depth, display, inTeX, have = 0, false, false, false
		tex.Reset()
	}
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			if depth > 0 {
				emit()
			}
			return out.String()
		}
		raw := z.Raw()
		tok := z.Token()
		if depth == 0 {
			switch {
			case tt == html.StartTagToken && tok.Data == "math":
				depth = 1
				for _, a := range tok.Attr {
					if a.Namespace == "" && a.Key == "display" && a.Val == "block" {
						display = true
					}
				}
				continue
			case tt == html.StartTagToken && !voidElements[tok.Data]:
				open = append(open, tok.Data)
			case tt == html.EndTagToken:
				pop(tok.Data)
			}
			out.Write(raw)
			continue
		}
		// Inside a math element.
		switch tt {
		case html.StartTagToken:
			// MathML is foreign content, where a browser reads style,
			// script, textarea, title, xmp, plaintext and the rest as
			// ordinary elements, never as raw text; read as raw text here,
			// they would run the skip past every end tag to the end of
			// the input (review of PR #5). The tree parser's own hook.
			z.NextIsNotRawText()
			switch {
			case tok.Data == "math":
				depth++
			case tok.Data == "annotation" && !have:
				for _, a := range tok.Attr {
					if a.Namespace == "" && a.Key == "encoding" && a.Val == "application/x-tex" {
						inTeX, have = true, true
					}
				}
			}
		case html.EndTagToken:
			switch {
			case tok.Data == "annotation":
				inTeX = false
			case tok.Data == "math":
				if depth--; depth == 0 {
					emit()
				}
			case inOpen(tok.Data):
				// An element open outside the math closes here: so does
				// the math, and the end tag is the outside's own.
				emit()
				pop(tok.Data)
				out.Write(raw)
			}
		case html.TextToken:
			if inTeX {
				tex.WriteString(tok.Data)
			}
		}
	}
}

// voidElements never have an end tag, so they are never open.
var voidElements = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true, "img": true, "input": true,
	"link": true, "meta": true, "param": true, "source": true, "track": true, "wbr": true,
}
