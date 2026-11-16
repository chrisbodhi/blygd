package blyg

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

// NormalizeOrigin returns the one spelling of an identity origin (§2,
// §12.2, decision #103): ASCII only, http(s), scheme and host lowercased,
// an internationalized host as its A-label, no default port, the path
// percent-encoded from its UTF-8 bytes with uppercase hex digits, a final
// "/", and no query or fragment. That is what a WHATWG parser writes, so a
// URL from the network is put in this form before it is compared with a
// written origin. The path's case is never folded — two path-mounted blygs
// on one host are different origins (§15.4 rule 2) — and an escape is never
// decoded: %7E stays %7E.
func NormalizeOrigin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if err := normalizeURL(u); err != nil {
		return "", err
	}
	u.RawQuery, u.Fragment, u.RawFragment = "", "", ""
	// The final "/" goes onto the escaped path, so an escape survives it:
	// rebuilding from the decoded path would turn %7E into ~ and %2F into
	// a separator (review of PR #5).
	p := upperEscapes(u.EscapedPath())
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	if u.Path, err = url.PathUnescape(p); err != nil {
		return "", err
	}
	u.RawPath = p
	return u.String(), nil
}

// NormalizeURL canonicalizes scheme, host and port of an absolute http(s)
// URL, leaving path and query as given and dropping the fragment.
func NormalizeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if err := normalizeURL(u); err != nil {
		return "", err
	}
	u.Fragment, u.RawFragment = "", ""
	return u.String(), nil
}

func normalizeURL(u *url.URL) error {
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("not an http(s) URL")
	}
	if u.Host == "" || u.Opaque != "" {
		return errors.New("not an absolute URL")
	}
	if u.User != nil {
		return errors.New("URLs with credentials are not accepted")
	}
	host := strings.ToLower(u.Hostname())
	if !isASCII(host) {
		ascii, err := idna.Lookup.ToASCII(host)
		if err != nil {
			return fmt.Errorf("host %q: %w", host, err)
		}
		host = ascii
	}
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if strings.Contains(host, ":") { // IPv6 literal
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	u.Host = host
	if u.Path == "" {
		u.Path = "/"
		u.RawPath = ""
	}
	// One spelling of the path: every escape's hex in uppercase, non-ASCII
	// escaped from its UTF-8 bytes (EscapedPath does that much), nothing
	// decoded.
	if p := upperEscapes(u.EscapedPath()); p != u.EscapedPath() {
		u.RawPath = p
	}
	return nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func upperEscapes(p string) string {
	if !strings.Contains(p, "%") {
		return p
	}
	b := []byte(p)
	for i := 0; i+2 < len(b); i++ {
		if b[i] == '%' {
			b[i+1], b[i+2] = upperHex(b[i+1]), upperHex(b[i+2])
			i += 2
		}
	}
	return string(b)
}

func upperHex(c byte) byte {
	if c >= 'a' && c <= 'f' {
		return c - 'a' + 'A'
	}
	return c
}

// SameOrigin compares two origin strings after normalization (§15.4 rule 4:
// "compares the document's asserted origin string against the receiver's own
// identity origin, normalized").
func SameOrigin(a, b string) bool {
	na, err1 := NormalizeOrigin(a)
	nb, err2 := NormalizeOrigin(b)
	return err1 == nil && err2 == nil && na == nb
}

// ItemDocURL is the live document URL for an item at an origin.
func ItemDocURL(origin, id string) string { return origin + ItemPath(id) }

// PinURL is the pinned version URL (§8).
func PinURL(origin, id string, version int) string {
	return origin + PinPath(id, version)
}

// PinnedPage is the optional rendered page of a pinned version (§8.4):
// the item's permalink plus a version segment. page is origin-relative.
func PinnedPage(page string, version int) string {
	return page + "v" + strconv.Itoa(version) + "/"
}

var pinnedPagePath = regexp.MustCompile(`^[ft]/` + idClass + `/v[1-9][0-9]*/$`)

// IsPinnedPage reports whether an origin-relative path is a pinned
// version page under the f/{id}/ and t/{id}/ convention blygd uses.
func IsPinnedPage(rel string) bool { return pinnedPagePath.MatchString(rel) }

// DefaultPage is the reference convention for a permalink (§5.8): f/{id}/
// for fragments, t/{id}/ for threads.
func DefaultPage(kind, id string) string {
	if kind == "thread" {
		return "t/" + id + "/"
	}
	return "f/" + id + "/"
}

// Resolve resolves ref against base, returning ref unchanged if either fails
// to parse.
func Resolve(base, ref string) string {
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}
