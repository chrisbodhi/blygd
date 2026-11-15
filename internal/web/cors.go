package web

import (
	"net/http"
	"strconv"
	"time"
)

// Cross-origin clients. Any web page may call /api/, so a client can live
// on its own origin and still be one more client of the API (DESIGN.md,
// "Cross-origin clients"). That is safe because the API authenticates only
// by a bearer token in the Authorization header, never by a cookie or by
// where a request comes from: a page elsewhere acts as a user only with a
// token that user gave it. So the answer is the same for every origin,
// "*", and never carries Access-Control-Allow-Credentials.
const (
	corsMethods = "GET, POST, PUT, PATCH, DELETE"
	corsHeaders = "Authorization, Content-Type"
	// What a client must read to behave: when to come back after a 429, and
	// why a token was refused.
	corsExpose = "Retry-After, WWW-Authenticate"
	// Chromium caps a preflight cache at two hours; asking for more is a
	// promise browsers do not keep.
	corsMaxAge = 2 * time.Hour
)

// cors answers preflights for /api/ and marks every /api/ response, errors
// and 429s included, as readable from any origin. It runs before
// authentication and the token budgets, since a preflight carries no
// Authorization header. A preflight is a constant answer that touches
// nothing, cheaper than a limiter lookup, so like /healthz it is free:
// charged to an address, it would compete with every page view behind a
// shared NAT (a hackathon room), and a refused preflight hides Retry-After
// from the script, which sees only a network error.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Expose-Headers", corsExpose)
		if r.Method != http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		h.Set("Access-Control-Allow-Methods", corsMethods)
		h.Set("Access-Control-Allow-Headers", corsHeaders)
		h.Set("Access-Control-Max-Age", strconv.Itoa(int(corsMaxAge.Seconds())))
		h.Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	})
}
