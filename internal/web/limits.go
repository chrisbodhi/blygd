package web

import (
	"context"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chrisbodhi/blygd/internal/limit"
)

// Cost classes for /api/ routes. A route's class picks its budget, and
// openapi.json states it on each operation as x-blygd-cost, so a tool (or
// an agent) can see which calls are dear before it makes them.
const (
	costDefault = "default"
	costFetch   = "fetch"  // turns one request into outbound fetches
	costExport  = "export" // renders the whole origin in memory
	costUpload  = "upload" // large bodies, or a whole-origin re-render
	costStream  = "stream" // long-lived; capped by open streams instead
)

// routeCosts lists every route that is not costDefault. Keys must be
// routes; TestRouteCostsMatchOpenAPI checks both directions.
var routeCosts = map[string]string{
	"POST /api/subscriptions":               costFetch, // §12.1 resolution
	"POST /api/subscriptions/{sid}/resolve": costFetch,
	"POST /api/subscriptions/{sid}/move":    costFetch,
	"POST /api/subscriptions/{sid}/poll":    costFetch,
	"POST /api/subscriptions/imports":       costFetch, // a url is fetched, and may resolve (§12.1)
	"POST /api/items":                       costFetch, // a fork or stub target is fetched
	"POST /api/items/{id}/publish":          costFetch, // remote pins are checked first
	"GET /api/origin/export":                costExport,
	"POST /api/media":                       costUpload,
	"PATCH /api/origin":                     costUpload, // re-renders every item
	"GET /api/events":                       costStream,
}

func routeCost(pattern string) string {
	if c, ok := routeCosts[pattern]; ok {
		return c
	}
	return costDefault
}

// Limits are the request budgets. A nil *Limits limits nothing, which is
// what tests that are not about budgets get.
type Limits struct {
	// TrustProxy takes the client address from the last X-Forwarded-For
	// entry, the one the proxy in front appended. Set it only behind one.
	TrustProxy bool

	token    map[string]*limit.Limiter // per cost class, per token
	user     map[string]*limit.Limiter // per cost class, per user: twice a token's
	surface  *limit.Limiter            // per address: origin files, hoppers, studio
	mention  *limit.Limiter            // per address: POST {origin}webmention
	authFail *limit.Limiter            // per address: failed bearer attempts
}

// Budgets are a token's, per cost class; a user's are twice these, across
// all their tokens.
var Budgets = map[string]limit.Rate{
	costDefault: {PerMinute: 120, Burst: 60},
	costFetch:   {PerMinute: 12, Burst: 12},
	costExport:  {PerMinute: 2, Burst: 2},
	costUpload:  {PerMinute: 30, Burst: 20},
}

// Per-address budgets for requests without a token.
var (
	SurfaceBudget  = limit.Rate{PerMinute: 600, Burst: 300}
	MentionBudget  = limit.Rate{PerMinute: 30, Burst: 30} // on top of §15.3's per-source caps
	AuthFailBudget = limit.Rate{PerMinute: 10, Burst: 30}
)

const maxLimitKeys = 100_000

// NewLimits builds the limiters from Budgets and the per-address budgets.
func NewLimits(trustProxy bool) *Limits {
	l := &Limits{TrustProxy: trustProxy, token: map[string]*limit.Limiter{}, user: map[string]*limit.Limiter{}}
	for cost, r := range Budgets {
		l.token[cost] = limit.New(r, maxLimitKeys)
		l.user[cost] = limit.New(limit.Rate{PerMinute: 2 * r.PerMinute, Burst: 2 * r.Burst}, maxLimitKeys)
	}
	l.surface = limit.New(SurfaceBudget, maxLimitKeys)
	l.mention = limit.New(MentionBudget, maxLimitKeys)
	l.authFail = limit.New(AuthFailBudget, maxLimitKeys)
	return l
}

// SetNow points every limiter at a clock, for tests.
func (l *Limits) SetNow(now func() time.Time) {
	for _, m := range []map[string]*limit.Limiter{l.token, l.user} {
		for _, lim := range m {
			lim.Now = now
		}
	}
	l.surface.Now, l.mention.Now, l.authFail.Now = now, now, now
}

// clientAddr is the address a budget is charged to: the peer, or behind a
// trusted proxy the address it saw. Public IPv6 is taken per /64, the
// block one subscriber usually holds; loopback and private addresses
// (a tailnet puts every device in one /64) are kept whole.
func clientAddr(r *http.Request, trustProxy bool) string {
	host := r.RemoteAddr
	if trustProxy {
		if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
			parts := strings.Split(xff[len(xff)-1], ",")
			host = strings.TrimSpace(parts[len(parts)-1])
		}
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}
	if ip.To4() == nil && ip.IsGlobalUnicast() && !ip.IsPrivate() {
		return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
	return ip.String()
}

func (l *Limits) addr(r *http.Request) string {
	if l == nil {
		return clientAddr(r, false)
	}
	return clientAddr(r, l.TrustProxy)
}

// authBlocked reports whether addr has spent its failed-attempt budget.
func (l *Limits) authBlocked(addr string) (bool, time.Duration) {
	if l == nil {
		return false, 0
	}
	wait := l.authFail.Wait(addr)
	return wait > 0, wait
}

// authFailed charges a failed bearer attempt to addr.
func (l *Limits) authFailed(addr string) {
	if l != nil {
		_, _ = l.authFail.Allow(addr)
	}
}

// spend charges one request of a cost class to a token and its user.
func (l *Limits) spend(cost string, tokenID, userID int64) (bool, time.Duration) {
	if l == nil || cost == costStream {
		return true, 0
	}
	if ok, wait := l.token[cost].Allow(strconv.FormatInt(tokenID, 10)); !ok {
		return false, wait
	}
	return l.user[cost].Allow(strconv.FormatInt(userID, 10))
}

// public charges an unauthenticated request to its address: the surface
// budget, or the mention budget for a POST to a webmention endpoint.
func (l *Limits) public(r *http.Request) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	lim := l.surface
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/webmention") {
		lim = l.mention
	}
	return lim.Allow(l.addr(r))
}

func retryAfter(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Max(1, math.Ceil(wait.Seconds())))))
}

// reqInfo is what the request log learns from deeper handlers.
type reqInfo struct{ tokenID int64 }

type reqInfoKey struct{}

func withReqInfo(r *http.Request) (*http.Request, *reqInfo) {
	ri := &reqInfo{}
	return r.WithContext(context.WithValue(r.Context(), reqInfoKey{}, ri)), ri
}

func noteToken(ctx context.Context, tokenID int64) {
	if ri, ok := ctx.Value(reqInfoKey{}).(*reqInfo); ok {
		ri.tokenID = tokenID
	}
}
