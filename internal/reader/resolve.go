package reader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/chrisbodhi/blygd/internal/blyg"
	"github.com/chrisbodhi/blygd/internal/fetch"
)

// MaxResolveFetches is §12's bound on one resolution.
const MaxResolveFetches = 6

// ResolveTimeout bounds a whole resolution, which a subscribe request waits
// on: six fetches at the per-fetch limit would outlast the HTTP server.
const ResolveTimeout = 45 * time.Second

// Resolution is the output of §12.1: a blyg subscription, or a legacy L0
// feed subscription.
type Resolution struct {
	Kind string `json:"kind"` // blyg or l0
	// Identity is the subscription identity: for a blyg, the URL the
	// manifest was actually fetched from, minus blyg.json (§12.2); for L0,
	// the final feed URL.
	Identity string `json:"identity"`
	Title    string `json:"title,omitempty"`
	SiteURL  string `json:"site_url,omitempty"` // L0 channel link

	Manifest     json.RawMessage `json:"-"`
	ManifestURL  string          `json:"-"` // its final URL: where to re-read it
	ManifestETag string          `json:"-"`
	// SiteDiscrepancy is set when the manifest's self-asserted site
	// disagrees with the identity. It is surfaced and never adopted.
	SiteDiscrepancy string   `json:"site_discrepancy,omitempty"`
	Warnings        []string `json:"warnings,omitempty"`
	Trail           []string `json:"trail"`
}

// ResolveError is a failed resolution with the trail of URLs tried, which
// §12.1 step 6 says a reader SHOULD report.
type ResolveError struct {
	Reason string
	Trail  []string
}

func (e *ResolveError) Error() string { return e.Reason }

type resolver struct {
	ctx    context.Context
	f      Fetcher
	trail  []string
	probed map[string]bool
}

func (rs *resolver) get(u string) (*fetch.Response, error) {
	if len(rs.trail) >= MaxResolveFetches {
		return nil, errBudget
	}
	rs.probed[u] = true
	resp, err := rs.f.GetIf(rs.ctx, u, fetch.ReaderLimits, "application/json, application/rss+xml, application/atom+xml, application/xml;q=0.9, text/html;q=0.8, */*;q=0.5", fetch.Conditional{})
	switch {
	case err != nil:
		rs.trail = append(rs.trail, u+" → "+err.Error())
	default:
		rs.trail = append(rs.trail, fmt.Sprintf("%s → %d", u, resp.Status))
	}
	return resp, err
}

var errBudget = errors.New("resolution fetch budget exhausted")

// probe is step 2's test at one base URL: GET {base}blyg.json and keep it
// if it parses as a manifest. URLs already probed are skipped.
func (rs *resolver) probe(base string) *Resolution {
	u := base + "blyg.json"
	if rs.probed[u] {
		return nil
	}
	resp, err := rs.get(u)
	if err != nil || resp.Status != 200 {
		return nil
	}
	return manifestResolution(resp)
}

// manifestResolution builds a blyg resolution from a fetched manifest, or
// returns nil if the body is not one. A manifest's address is a path: one
// served at a URL with a query string has no identity and is out of scope
// (§12.1 step 1, decision #64).
func manifestResolution(resp *fetch.Response) *Resolution {
	m, ok := blyg.ParseManifest(resp.Body)
	if !ok {
		return nil
	}
	if u, err := url.Parse(resp.FinalURL); err != nil || u.RawQuery != "" || u.ForceQuery {
		return nil
	}
	identity, err := IdentityFromManifestURL(resp.FinalURL)
	if err != nil {
		return nil
	}
	res := &Resolution{Kind: "blyg", Identity: identity, Title: blyg.ManifestString(m, "title"),
		Manifest: json.RawMessage(resp.Body), ManifestURL: resp.FinalURL, ManifestETag: resp.Header.Get("ETag")}
	if site := blyg.ManifestString(m, "site"); site != "" && !blyg.SameOrigin(site, identity) {
		res.SiteDiscrepancy = fmt.Sprintf("manifest asserts site %s, but it was fetched from %s; the fetch origin is the identity (§12.2)", site, identity)
	}
	return res
}

// IdentityFromManifestURL is §12.2: the URL the manifest was fetched from,
// after redirects, minus its last path segment (blyg.json at the default
// location).
func IdentityFromManifestURL(final string) (string, error) {
	u, err := url.Parse(final)
	if err != nil {
		return "", err
	}
	u.RawQuery, u.Fragment = "", ""
	if i := strings.LastIndex(u.Path, "/"); i >= 0 {
		u.Path = u.Path[:i+1]
	}
	u.RawPath = ""
	return blyg.NormalizeOrigin(u.String())
}

// Resolve runs §12.1 on a URL found out of band. It is deterministic,
// ordered and bounded at MaxResolveFetches fetches.
func Resolve(ctx context.Context, f Fetcher, input string) (*Resolution, error) {
	in, err := url.Parse(strings.TrimSpace(input))
	if err != nil || (in.Scheme != "http" && in.Scheme != "https") || in.Host == "" {
		return nil, &ResolveError{Reason: "not an http(s) URL"}
	}
	in.Fragment, in.RawFragment = "", ""
	inputURL, err := blyg.NormalizeURL(in.String())
	if err != nil {
		return nil, &ResolveError{Reason: err.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, ResolveTimeout)
	defer cancel()
	rs := &resolver{ctx: ctx, f: f, probed: map[string]bool{}}
	var warnings []string
	if in.Scheme == "http" {
		warnings = append(warnings, "plain http: the subscription is not protected in transit (§12.1 step 1)")
	}
	done := func(res *Resolution) (*Resolution, error) {
		res.Warnings = append(warnings, res.Warnings...)
		res.Trail = rs.trail
		return res, nil
	}

	// Step 1: normalize to the candidate origin.
	candidate, err := blyg.NormalizeOrigin(inputURL)
	if err != nil {
		return nil, &ResolveError{Reason: err.Error()}
	}
	// Step 2: direct probe.
	if res := rs.probe(candidate); res != nil {
		return done(res)
	}

	// Steps 3 and 4 read the input URL itself.
	var l0 *Resolution
	var links blyg.HTMLLinks
	if !rs.probed[inputURL] {
		if resp, err := rs.get(inputURL); err == nil && resp.Status == 200 {
			if feed, err := blyg.ParseFeed(resp.Body, resp.FinalURL); err == nil {
				// Step 3: feed upgrade through <blyg:manifest>.
				if feed.Manifest != "" {
					mu := blyg.Resolve(resp.FinalURL, feed.Manifest)
					if !rs.probed[mu] {
						if mr, err := rs.get(mu); err == nil && mr.Status == 200 {
							if res := manifestResolution(mr); res != nil {
								return done(res)
							}
						}
					}
				}
				l0 = l0Resolution(resp.FinalURL, feed)
			} else {
				// Step 4: one rel="blyg" hop. A target whose body parses as a
				// manifest is the manifest; otherwise it is an origin base URL
				// and is probed as in step 2. The target's own HTML is never
				// scanned, so there is no recursion and no loop.
				links = blyg.FindHTMLLinks(resp.Body, resp.FinalURL)
				if links.Blyg != "" && !rs.probed[links.Blyg] {
					if hr, err := rs.get(links.Blyg); err == nil && hr.Status == 200 {
						if res := manifestResolution(hr); res != nil {
							return done(res)
						}
					}
				}
				if links.Blyg != "" {
					if base, err := blyg.NormalizeOrigin(links.Blyg); err == nil {
						if res := rs.probe(base); res != nil {
							return done(res)
						}
					}
				}
			}
		}
	}

	// Step 5: conventional mounts at the input's scheme+host root.
	root := in.Scheme + "://" + in.Host + "/"
	if r, err := blyg.NormalizeOrigin(root); err == nil {
		root = r
	}
	for _, base := range []string{root + "blyg/", root} {
		if res := rs.probe(base); res != nil {
			return done(res)
		}
	}

	// Step 6: RSS fallback — the held feed, or autodiscovery on the input.
	if l0 != nil {
		return done(l0)
	}
	for _, fu := range links.Feeds {
		if rs.probed[fu] {
			continue
		}
		resp, err := rs.get(fu)
		if err != nil || resp.Status != 200 {
			break
		}
		if feed, err := blyg.ParseFeed(resp.Body, resp.FinalURL); err == nil {
			return done(l0Resolution(resp.FinalURL, feed))
		}
		break
	}
	return nil, &ResolveError{Reason: "no blyg manifest or feed found", Trail: rs.trail}
}

func l0Resolution(feedURL string, feed *blyg.ParsedFeed) *Resolution {
	id := feedURL
	if n, err := blyg.NormalizeURL(feedURL); err == nil {
		id = n
	}
	return &Resolution{Kind: "l0", Identity: id, Title: feed.Title, SiteURL: feed.Link,
		Warnings: []string{"legacy RSS: items are wrapped summaries with no versions or withdrawal (§13.6)"}}
}
