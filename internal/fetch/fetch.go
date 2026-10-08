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
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
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
)

// Get fetches url with GET.
func (c *Client) Get(ctx context.Context, rawURL string, lim Limits, accept string) (*Response, error) {
	return c.Do(ctx, http.MethodGet, rawURL, nil, "", lim, accept)
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
	transport := &http.Transport{
		// No environment proxy: a proxy would fetch on our behalf and the
		// address check above would be checking the proxy, not the target.
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   lim.Timeout,
		ResponseHeaderTimeout: lim.Timeout,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
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

var cgnat = netip.MustParsePrefix("100.64.0.0/10")
var benchmarking = netip.MustParsePrefix("198.18.0.0/15")
var nat64 = netip.MustParsePrefix("64:ff9b::/96")

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
		benchmarking.Contains(ip),
		nat64.Contains(ip):
		return false
	}
	if ip.Is4() {
		b := ip.As4()
		if b[0] == 0 || b[0] >= 240 { // 0.0.0.0/8, 240.0.0.0/4 and broadcast
			return false
		}
	}
	return true
}
