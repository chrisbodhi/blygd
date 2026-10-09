package blyg

import (
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// NormalizeOrigin returns the canonical spelling of an identity origin
// (§2, §12.2): http(s) only, scheme and host lowercased, default port
// dropped, no query or fragment, path ending in "/". The path is kept
// case-sensitive — two path-mounted blygs on one host are different origins
// (§15.4 rule 2).
func NormalizeOrigin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if err := normalizeURL(u); err != nil {
		return "", err
	}
	u.RawQuery, u.Fragment, u.RawFragment = "", "", ""
	if u.Path == "" || !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
		u.RawPath = ""
	}
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
	return nil
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
func ItemDocURL(origin, id string) string { return origin + "items/" + id + ".json" }

// PinURL is the pinned version URL (§8).
func PinURL(origin, id string, version int) string {
	return origin + "items/" + id + "/v" + strconv.Itoa(version) + ".json"
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
