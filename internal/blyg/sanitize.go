package blyg

import (
	"html/template"
	"regexp"

	"github.com/microcosm-cc/bluemonday"
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
	return p
}()

// PageHTML sanitizes content_html for one of our pages.
func PageHTML(contentHTML string) template.HTML {
	return template.HTML(pagePolicy.Sanitize(contentHTML)) //nolint:gosec // sanitized above
}
