package blyg

import (
	"strings"

	"golang.org/x/net/html"
)

// MediaTypes are the media types blygd accepts, with the extension each is
// stored under. Images only: SVG is left out because it can carry script.
var MediaTypes = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
	"image/gif":  "gif",
	"image/webp": "webp",
}

// MediaRef is one of an origin's media objects referenced from content_html.
type MediaRef struct {
	Path string // origin-relative: media/….
	Alt  string // the first <img> alt text that references it
}

// MediaRefs lists the origin's media that rendered HTML references, in
// order of first reference: every href or src under {origin}media/. Baked
// quotes count too: the bytes are this thread's to keep serving.
func MediaRefs(contentHTML, origin string) []MediaRef {
	prefix := origin + "media/"
	z := html.NewTokenizer(strings.NewReader(contentHTML))
	var out []MediaRef
	seen := map[string]int{}
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			return out
		}
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			continue
		}
		tok := z.Token()
		alt, ref := "", ""
		for _, a := range tok.Attr {
			switch a.Key {
			case "href", "src":
				if strings.HasPrefix(a.Val, prefix) {
					ref = "media/" + strings.SplitN(strings.TrimPrefix(a.Val, prefix), "?", 2)[0]
				}
			case "alt":
				alt = a.Val
			}
		}
		if ref == "" {
			continue
		}
		if i, ok := seen[ref]; ok {
			if out[i].Alt == "" && tok.Data == "img" {
				out[i].Alt = alt
			}
			continue
		}
		if tok.Data != "img" {
			alt = ""
		}
		seen[ref] = len(out)
		out = append(out, MediaRef{Path: ref, Alt: alt})
	}
}
