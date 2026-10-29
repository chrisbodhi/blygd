// Package fetch is the only way blygd talks to the outside web. Every
// request it makes is to a URL someone else chose — a Webmention source, a
// target's endpoint, a feed being subscribed to — so it is bounded on every
// axis and refuses private-network addresses (§14).
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/chrisbodhi/blygd/internal/tx"
)

// Limits bound one fetch. The §15.4 RECOMMENDED values are the defaults.
type Limits struct {
	MaxRedirects int
	Timeout      time.Duration
	MaxBody      int64
}

// VerifyLimits are §15.4's RECOMMENDED bounds: 3 redirects, 5 s, 1 MB.
var VerifyLimits = Limits{MaxRedirects: 3, Timeout: 5 * time.Second, MaxBody: 1 << 20}

// Client fetches with a guarded dialer.
type Client struct {
	UserAgent string
	// AllowPrivate permits loopback and private addresses. Tests and local
	// development only: in production it turns every Webmention endpoint
	// into a probe of your own network.
	AllowPrivate bool
	// Local answers requests for this instance's own URLs, those under
	// LocalBase's scheme and host, in-process: one tenant reading,
	// forking or mentioning another gets exactly what a remote client
	// would, without leaving the process. Behind a private reverse proxy
	// the dial would be refused, and a public round trip is waste. Set
	// both or neither.
	Local     http.Handler
	LocalBase string
}

// Response is a fully read, bounded response.
type Response struct {
	FinalURL string
	Status   int
	Header   http.Header
	Body     []byte
}

var (
	ErrTooLarge    = errors.New("response body exceeds limit")
	ErrTooManyHops = errors.New("too many redirects")
	ErrBlockedAddr = errors.New("refusing to connect to a non-public address")
	ErrNotHTTP     = errors.New("only http and https URLs are fetched")
	// ErrInTx is a fetch from inside a database transaction, which holds
	// locks for as long as a stranger's server cares to take (CLAUDE.md).
	ErrInTx = errors.New("refusing to fetch inside a database transaction: fetch first, then open it")
)

// ReaderLimits bound the import side's fetches: §12.2's RECOMMENDED 5
// redirects, and room for a large archive index (§6.2 has no window).
var ReaderLimits = Limits{MaxRedirects: 5, Timeout: 20 * time.Second, MaxBody: 16 << 20}

// Conditional carries the validators of a conditional GET (§12.3).
type Conditional struct {
	ETag         string // sent as If-None-Match
	LastModified string // sent as If-Modified-Since
}

// Get fetches url with GET.
func (c *Client) Get(ctx context.Context, rawURL string, lim Limits, accept string) (*Response, error) {
	return c.Do(ctx, http.MethodGet, rawURL, nil, "", lim, accept)
}

// GetIf fetches url with a conditional GET. A 304 comes back as a Response
// with Status 304 and no body.
func (c *Client) GetIf(ctx context.Context, rawURL string, lim Limits, accept string, cond Conditional) (*Response, error) {
	h := http.Header{}
	if cond.ETag != "" {
		h.Set("If-None-Match", cond.ETag)
	}
	if cond.LastModified != "" {
		h.Set("If-Modified-Since", cond.LastModified)
	}
	return c.do(ctx, http.MethodGet, rawURL, nil, "", lim, accept, h)
}

// RetryAfter parses a Retry-After header (seconds or an HTTP date) relative
// to now. It returns 0 when the header is absent or unparseable.
func RetryAfter(h http.Header, now time.Time) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

// Head fetches url with HEAD.
func (c *Client) Head(ctx context.Context, rawURL string, lim Limits) (*Response, error) {
	return c.Do(ctx, http.MethodHead, rawURL, nil, "", lim, "")
}

// PostForm sends an application/x-www-form-urlencoded POST. Redirects are not
// followed: Go would replay a 301/302 as a GET, which is not a Webmention.
func (c *Client) PostForm(ctx context.Context, rawURL string, form url.Values, lim Limits) (*Response, error) {
	lim.MaxRedirects = 0
	return c.Do(ctx, http.MethodPost, rawURL, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", lim, "")
}

// Do performs one bounded request.
func (c *Client) Do(ctx context.Context, method, rawURL string, body io.Reader, contentType string, lim Limits, accept string) (*Response, error) {
	return c.do(ctx, method, rawURL, body, contentType, lim, accept, nil)
}

func (c *Client) do(ctx context.Context, method, rawURL string, body io.Reader, contentType string, lim Limits, accept string, extra http.Header) (*Response, error) {
	if tx.Active(ctx) {
		return nil, ErrInTx
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, ErrNotHTTP
	}
	ctx, cancel := context.WithTimeout(ctx, lim.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, vs := range extra {
		req.Header[k] = vs
	}

	hc := c.httpClient(lim)
	resp, err := hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) && errors.Is(ue.Err, ErrTooManyHops) {
			return nil, ErrTooManyHops
		}
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, lim.MaxBody+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(data)) > lim.MaxBody {
		return nil, ErrTooLarge
	}
	return &Response{FinalURL: resp.Request.URL.String(), Status: resp.StatusCode, Header: resp.Header, Body: data}, nil
}

func (c *Client) httpClient(lim Limits) *http.Client {
	dialer := &net.Dialer{Timeout: lim.Timeout}
	if !c.AllowPrivate {
		// Checked on the resolved address at connect time, so DNS rebinding
		// cannot slip a private address past a check made on the name.
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return err
			}
			if !IsPublic(ip) {
				return ErrBlockedAddr
			}
			return nil
		}
	}
	var transport http.RoundTripper = &http.Transport{
		// No environment proxy: a proxy would fetch on our behalf and the
		// address check above would be checking the proxy, not the target.
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   lim.Timeout,
		ResponseHeaderTimeout: lim.Timeout,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
	}
	if c.Local != nil {
		if base, err := url.Parse(c.LocalBase); err == nil && base.Host != "" {
			transport = &local{base: base, h: c.Local, next: transport}
		}
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if lim.MaxRedirects == 0 {
				return http.ErrUseLastResponse // hand the 3xx back to the caller
			}
			if len(via) > lim.MaxRedirects {
				return ErrTooManyHops
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return ErrNotHTTP
			}
			return nil
		},
	}
}

// local serves requests for this instance in-process and passes the rest
// on. Redirects, bounds and body limits stay the client's, as for any
// response.
type local struct {
	base *url.URL
	h    http.Handler
	next http.RoundTripper
}

func (l *local) RoundTrip(req *http.Request) (*http.Response, error) {
	if !sameHost(req.URL, l.base) {
		return l.next.RoundTrip(req)
	}
	in := req.Clone(req.Context())
	in.Host, in.RequestURI, in.RemoteAddr = req.URL.Host, req.URL.RequestURI(), "127.0.0.1:0"
	if in.Body == nil {
		in.Body = http.NoBody
	}
	rec := httptest.NewRecorder()
	l.h.ServeHTTP(rec, in)
	resp := rec.Result()
	resp.Request = req
	return resp, nil
}

// sameHost compares scheme, host and port, with each scheme's default port.
func sameHost(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")
var benchmarking = netip.MustParsePrefix("198.18.0.0/15")

// blockedV6 are IPv6 ranges that are not global unicast, or that embed an
// IPv4 address a relay would forward to. The stdlib predicates above do not
// cover them.
var blockedV6 = []netip.Prefix{
	netip.MustParsePrefix("::/96"),          // IPv4-compatible (deprecated): ::127.0.0.1
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64 (RFC 6052)
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64 (RFC 8215)
	netip.MustParsePrefix("100::/64"),       // discard-only (RFC 6666)
	netip.MustParsePrefix("2001::/32"),      // Teredo
	netip.MustParsePrefix("2001:db8::/32"),  // documentation
	netip.MustParsePrefix("2002::/16"),      // 6to4
	netip.MustParsePrefix("3fff::/20"),      // documentation (RFC 9637)
	netip.MustParsePrefix("fec0::/10"),      // site-local (deprecated)
}

// IsPublic reports whether ip is a globally routable unicast address.
func IsPublic(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.Is4In6() {
		ip = netip.AddrFrom4(ip.As4())
	}
	switch {
	case !ip.IsValid(),
		ip.IsUnspecified(),
		ip.IsLoopback(),
		ip.IsPrivate(),
		ip.IsLinkLocalUnicast(),
		ip.IsLinkLocalMulticast(),
		ip.IsInterfaceLocalMulticast(),
		ip.IsMulticast(),
		cgnat.Contains(ip),
		benchmarking.Contains(ip):
		return false
	}
	if ip.Is6() {
		for _, p := range blockedV6 {
			if p.Contains(ip) {
				return false
			}
		}
	}
	if ip.Is4() {
		b := ip.As4()
		if b[0] == 0 || b[0] >= 240 { // 0.0.0.0/8, 240.0.0.0/4 and broadcast
			return false
		}
	}
	return true
}
