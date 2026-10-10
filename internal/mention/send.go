package mention

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/chrisbodhi/blygd/internal/blyg"
	"github.com/chrisbodhi/blygd/internal/fetch"
)

// RetrySchedule is §15.2's RECOMMENDED backoff after a network failure, 5xx
// or 429. After the last step the mention is abandoned.
var RetrySchedule = []time.Duration{15 * time.Minute, time.Hour, 4 * time.Hour, 12 * time.Hour, 24 * time.Hour}

// Outbound is one mention to send.
type Outbound struct {
	SourceURL    string
	TargetURL    string
	TargetOrigin string // set for blyg targets: try the manifest first
}

// SendResult is the outcome of one send attempt.
type SendResult struct {
	Status   string // sent, failed, no_endpoint, retry
	Endpoint string
	Detail   string
	// RetryAfter is a 429's Retry-After: the receiver saying when to come
	// back. Zero when it said nothing.
	RetryAfter time.Duration
}

// minRetry is the soonest a refused mention is sent again, whatever the
// receiver says.
const minRetry = time.Minute

// RetryDelay is how long to wait before attempt n+1 after attempt n (from
// 1) was refused or failed. §15.2's schedule, unless the receiver asked for
// sooner with a 429's Retry-After: a receiver that refuses a pair re-sent
// within its cooldown (as the reference client does for a stub withdrawn
// moments after it was sent) should hear about the withdrawal when it said
// to, not a schedule step later. Never sooner than a minute.
func RetryDelay(n int, retryAfter time.Duration) time.Duration {
	d := RetrySchedule[n-1]
	if retryAfter > 0 && retryAfter < d {
		d = max(retryAfter, minRetry)
	}
	return d
}

var sendLimits = fetch.Limits{MaxRedirects: 3, Timeout: 10 * time.Second, MaxBody: 1 << 20}

// Send discovers the target's endpoint and delivers one mention (§15.2).
func Send(ctx context.Context, c *fetch.Client, m Outbound) SendResult {
	endpoint, res := Discover(ctx, c, m)
	if endpoint == "" {
		return res
	}
	form := url.Values{"source": {m.SourceURL}, "target": {m.TargetURL}}
	resp, err := c.PostForm(ctx, endpoint, form, sendLimits)
	if err != nil {
		return SendResult{Status: "retry", Endpoint: endpoint, Detail: err.Error()}
	}
	detail := fmt.Sprintf("HTTP %d", resp.Status)
	switch {
	case resp.Status >= 200 && resp.Status < 300:
		return SendResult{Status: "sent", Endpoint: endpoint, Detail: detail}
	case resp.Status == 429:
		return SendResult{Status: "retry", Endpoint: endpoint, Detail: detail, RetryAfter: fetch.RetryAfter(resp.Header, time.Now())}
	case resp.Status >= 500:
		return SendResult{Status: "retry", Endpoint: endpoint, Detail: detail}
	}
	// 4xx is terminal; so is a redirect, which we do not replay as a POST.
	return SendResult{Status: "failed", Endpoint: endpoint, Detail: detail}
}

// Discover finds a Webmention endpoint, in §15.2's order: the target
// origin's manifest `webmention` key; then W3C discovery on the target URL
// (Link header, then the first <link> or <a> with rel=webmention, HEAD
// first and GET if the header says nothing). When it returns no endpoint,
// res says why: no_endpoint (recorded, not retried) or retry.
func Discover(ctx context.Context, c *fetch.Client, m Outbound) (endpoint string, res SendResult) {
	if m.TargetOrigin != "" {
		manifestURL := m.TargetOrigin + "blyg.json"
		if resp, err := c.Get(ctx, manifestURL, sendLimits, "application/json"); err == nil && resp.Status == 200 {
			var man struct {
				Webmention string `json:"webmention"`
			}
			if json.Unmarshal(resp.Body, &man) == nil && strings.TrimSpace(man.Webmention) != "" {
				if ep := absHTTP(resp.FinalURL, man.Webmention); ep != "" {
					return ep, SendResult{}
				}
			}
		}
	}

	if resp, err := c.Head(ctx, m.TargetURL, sendLimits); err == nil && resp.Status < 400 {
		if ep := linkHeader(resp.Header.Values("Link"), resp.FinalURL); ep != "" {
			return ep, SendResult{}
		}
	}
	resp, err := c.Get(ctx, m.TargetURL, sendLimits, "text/html")
	if err != nil {
		return "", SendResult{Status: "retry", Detail: "discovery: " + err.Error()}
	}
	if resp.Status == 429 || resp.Status >= 500 {
		return "", SendResult{Status: "retry", Detail: fmt.Sprintf("discovery: HTTP %d", resp.Status)}
	}
	if resp.Status >= 400 {
		return "", SendResult{Status: "no_endpoint", Detail: fmt.Sprintf("target returned %d", resp.Status)}
	}
	if ep := linkHeader(resp.Header.Values("Link"), resp.FinalURL); ep != "" {
		return ep, SendResult{}
	}
	if ep := htmlEndpoint(resp.Body, resp.FinalURL); ep != "" {
		return ep, SendResult{}
	}
	return "", SendResult{Status: "no_endpoint", Detail: "no endpoint advertised"}
}

var linkPart = regexp.MustCompile(`<([^>]*)>([^,]*)`)
var relParam = regexp.MustCompile(`(?i)\brel\s*=\s*(?:"([^"]*)"|([^\s;,]+))`)

// linkHeader finds rel=webmention in HTTP Link headers.
func linkHeader(values []string, base string) string {
	for _, v := range values {
		for _, m := range linkPart.FindAllStringSubmatch(v, -1) {
			rm := relParam.FindStringSubmatch(m[2])
			if rm == nil {
				continue
			}
			rel := rm[1] + rm[2]
			if hasToken(strings.ToLower(rel), "webmention") {
				if ep := absHTTP(base, m[1]); ep != "" {
					return ep
				}
			}
		}
	}
	return ""
}

// htmlEndpoint returns the first <link> or <a> with rel=webmention. An empty
// href means the page itself (W3C Webmention §3.1.2).
func htmlEndpoint(body []byte, base string) string {
	z := html.NewTokenizer(bytes.NewReader(body))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return ""
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			if (string(name) != "link" && string(name) != "a") || !hasAttr {
				continue
			}
			var rel string
			href, hasHref := "", false
			for {
				k, v, more := z.TagAttr()
				switch string(k) {
				case "rel":
					rel = strings.ToLower(string(v))
				case "href":
					href, hasHref = string(v), true
				}
				if !more {
					break
				}
			}
			if hasHref && hasToken(rel, "webmention") {
				if ep := absHTTP(base, href); ep != "" {
					return ep
				}
			}
		}
	}
}

// absHTTP resolves ref against base and keeps it only if it is http(s).
func absHTTP(base, ref string) string {
	abs := blyg.Resolve(base, strings.TrimSpace(ref))
	u, err := url.Parse(abs)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	u.Fragment = ""
	return u.String()
}
