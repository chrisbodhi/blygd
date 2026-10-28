package blyg

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Surface is where an origin serves its files: the manifest locates the
// surface (§6.1, §4). `feed` and `items` are authoritative URLs, and the
// `item` and `pin` templates, in RFC 6570 level-1 form, replace
// {origin}items/{id}.json and {origin}items/{id}/v{n}.json. Absent keys
// mean the defaults, which are the 0.3 paths. Only the manifest's own
// filename is fixed (§12.1).
type Surface struct {
	Origin string // the identity origin (§12.2)
	Feed   string // as the manifest wrote it; "" is the default
	Items  string
	Item   string
	Pin    string
}

// Default surface paths (§4).
const (
	DefaultFeed  = "feed.xml"
	DefaultItems = "items/index.json"
	DefaultItem  = "items/{id}.json"
	DefaultPin   = "items/{id}/v{n}.json"
)

// SurfaceOf reads a surface from manifest bytes. A manifest that does not
// parse, or keys that are not strings, leave the defaults.
func SurfaceOf(origin string, manifest []byte) Surface {
	s := Surface{Origin: origin}
	var m map[string]json.RawMessage
	if json.Unmarshal(manifest, &m) != nil {
		return s
	}
	s.Feed = strings.TrimSpace(ManifestString(m, "feed"))
	s.Items = strings.TrimSpace(ManifestString(m, "items"))
	s.Item = strings.TrimSpace(ManifestString(m, "item"))
	s.Pin = strings.TrimSpace(ManifestString(m, "pin"))
	return s
}

// FeedURL is the feed's absolute URL.
func (s Surface) FeedURL() string { return s.at(or(s.Feed, DefaultFeed)) }

// IndexURL is the archive index's absolute URL.
func (s Surface) IndexURL() string { return s.at(or(s.Items, DefaultItems)) }

// ItemURL is an item document's absolute URL: the `item` template, or the
// default, expanded with the id.
func (s Surface) ItemURL(id string) string {
	return s.at(expand(or(s.Item, DefaultItem), map[string]string{"id": id}))
}

// PinURL is a pinned version document's absolute URL, from the `pin`
// template or the default.
func (s Surface) PinURL(id string, n int) string {
	return s.at(expand(or(s.Pin, DefaultPin), map[string]string{"id": id, "n": strconv.Itoa(n)}))
}

// Templated reports whether the surface relocates item documents.
func (s Surface) Templated() bool { return s.Item != "" && s.Item != DefaultItem }

// at makes an origin-relative value absolute; an absolute one is taken as
// given. Values may carry a query string: they are expanded or fetched,
// never resolved against (§6.1).
func (s Surface) at(v string) string { return Resolve(s.Origin, v) }

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// expand is RFC 6570 level 1: {var} becomes the value with everything but
// unreserved characters percent-encoded. An unknown variable expands to
// nothing, as the RFC says of an undefined one.
func expand(tmpl string, vars map[string]string) string {
	var b strings.Builder
	for {
		open := strings.IndexByte(tmpl, '{')
		if open < 0 {
			break
		}
		end := strings.IndexByte(tmpl[open:], '}')
		if end < 0 {
			break
		}
		b.WriteString(tmpl[:open])
		b.WriteString(escapeUnreserved(vars[tmpl[open+1:open+end]]))
		tmpl = tmpl[open+end+1:]
	}
	b.WriteString(tmpl)
	return b.String()
}

func escapeUnreserved(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		c := v[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
