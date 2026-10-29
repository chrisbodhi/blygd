package mention

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/net/publicsuffix"

	"github.com/chrisbodhi/blygd/internal/blyg"
	"github.com/chrisbodhi/blygd/internal/events"
	"github.com/chrisbodhi/blygd/internal/studio"
	"github.com/chrisbodhi/blygd/internal/tx"
)

// RateLimits are §15.3 step 2's three caps, per hour. They are
// instance-wide: verification fetches cost this machine, so spreading claims
// across tenants must not raise the ceiling.
type RateLimits struct {
	PerHost   int // RECOMMENDED 60
	PerDomain int // registrable domain; looser than per-host
	Global    int
}

// DefaultRateLimits follow §15.3's recommendation for the per-host cap; the
// others are this receiver's choice.
var DefaultRateLimits = RateLimits{PerHost: 60, PerDomain: 240, Global: 2000}

// Receiver is the §15.3 endpoint at {origin}webmention.
type Receiver struct {
	Pool   *pgxpool.Pool
	Limits RateLimits
}

const maxFormBytes = 16 << 10

// ServeOrigin handles a request to an origin's endpoint.
func (rc *Receiver) ServeOrigin(w http.ResponseWriter, r *http.Request, o *studio.Origin) {
	if !o.AcceptMentions {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Webmention endpoint: POST source and target, form-encoded.", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not read form body", http.StatusBadRequest)
		return
	}
	status, msg := rc.accept(r.Context(), o, r.PostForm.Get("source"), r.PostForm.Get("target"))
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "3600")
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	io.WriteString(w, msg+"\n") //nolint:errcheck
}

// accept runs §15.3 steps 1–3 and returns the status to answer with.
func (rc *Receiver) accept(ctx context.Context, o *studio.Origin, rawSource, rawTarget string) (int, string) {
	// Step 1: syntactic checks → 400.
	source, err := blyg.NormalizeURL(rawSource)
	if err != nil {
		return 400, "source must be an absolute http(s) URL"
	}
	target, err := blyg.NormalizeURL(rawTarget)
	if err != nil {
		return 400, "target must be an absolute http(s) URL"
	}
	if source == target {
		return 400, "source and target must differ"
	}
	tu, _ := url.Parse(target)
	tu.RawQuery = ""
	rel, inside := studio.TrimOrigin(o.BaseURL, tu.String())
	if !inside {
		return 400, "target is not within this blyg"
	}
	itemID, err := rc.targetItem(ctx, o.ID, rel)
	if err != nil {
		return 500, "internal error"
	}
	if itemID == "" {
		// The endpoint exists; the claim is bad (§15.3: 400, not 404).
		return 400, "target does not name a published item"
	}

	su, _ := url.Parse(source)
	host := strings.ToLower(su.Hostname())
	domain, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		domain = host // IP literals, localhost, bare suffixes
	}

	var status int
	var msg string
	err = tx.Run(ctx, rc.Pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		// Serialize claim accounting so concurrent claims cannot overshoot.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7215)`); err != nil {
			return err
		}
		// Step 2: rate limits → 429, never queued.
		var perHost, perDomain, global int
		if err := tx.QueryRow(ctx, `SELECT
			count(*) FILTER (WHERE source_host = $1),
			count(*) FILTER (WHERE source_domain = $2),
			count(*)
			FROM mention_claims WHERE at > now() - interval '1 hour'`, host, domain).Scan(&perHost, &perDomain, &global); err != nil {
			return err
		}
		if perHost >= rc.Limits.PerHost || perDomain >= rc.Limits.PerDomain || global >= rc.Limits.Global {
			status, msg = 429, "rate limited"
			return nil
		}
		if _, err := tx.Exec(ctx, `INSERT INTO mention_claims (source_host, source_domain) VALUES ($1, $2)`, host, domain); err != nil {
			return err
		}
		// A blocked source is answered exactly like any other, so the block
		// is not revealed, and then dropped: nothing stored or fetched.
		var blocked bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM mention_blocks WHERE origin_id = $1
			AND ((kind = 'host' AND value = $2) OR (kind = 'domain' AND value = $3)))`, o.ID, host, domain).Scan(&blocked); err != nil {
			return err
		}
		if blocked {
			status, msg = 202, "Accepted; verification is asynchronous."
			return nil
		}
		// Step 3: accept → 202 and verify asynchronously. A re-sent pair
		// re-verifies rather than duplicating; what it recorded stays until
		// the new verification replaces it.
		var mid int64
		if err := tx.QueryRow(ctx, `INSERT INTO inbound_mentions
			(origin_id, target_item_id, source_url, target_url, status, next_attempt_at, source_host, source_domain)
			VALUES ($1, $2, $3, $4, 'pending', now(), $5, $6)
			ON CONFLICT (origin_id, source_url, target_url) DO UPDATE SET
				status = 'pending', attempts = 0, next_attempt_at = now(), received_at = now(),
				target_item_id = EXCLUDED.target_item_id
			RETURNING id`,
			o.ID, itemID, source, target, host, domain).Scan(&mid); err != nil {
			return err
		}
		// Sent on commit: the owner's open studios hear a mention arrived.
		if err := events.Notify(ctx, tx, "mention", o.ID, 0, map[string]any{"id": mid, "status": "pending"}); err != nil {
			return err
		}
		status, msg = 202, "Accepted; verification is asynchronous."
		return nil
	})
	if err != nil {
		return 500, "internal error"
	}
	return status, msg
}

// targetItem resolves an origin-relative path to one of this origin's
// items, by the addresses it itself serves for it (§15.3 step 1: membership,
// not location, decision #84): its document (items/{id}.json), its page, or
// a pinned version's document (items/{id}/v{n}.json) or page ({page}v{n}/).
// blygd never relocates its surface, so every such address is under the
// base URL, and the caller's prefix check refuses nothing it serves. A
// withdrawn target is fine (§15.3: people may respond to a withdrawal).
// Returns "" if none.
func (rc *Receiver) targetItem(ctx context.Context, originID int64, rel string) (string, error) {
	if m := pinDocPath.FindStringSubmatch(rel); m != nil {
		return rc.lookup(ctx, `SELECT i.id FROM items i JOIN item_versions v ON v.item_id = i.id
			WHERE i.origin_id = $1 AND i.id = $2 AND v.version = $3 AND v.pinned_at IS NOT NULL`, originID, m[1], atoi(m[2]))
	}
	if m := itemDocPath.FindStringSubmatch(rel); m != nil {
		return rc.lookup(ctx, `SELECT id FROM items WHERE origin_id = $1 AND id = $2 AND version > 0`, originID, m[1])
	}
	page := rel
	if page != "" && !strings.HasSuffix(page, "/") {
		page += "/"
	}
	if m := pinPagePath.FindStringSubmatch(page); m != nil {
		id, err := rc.lookup(ctx, `SELECT i.id FROM items i JOIN item_versions v ON v.item_id = i.id
			WHERE i.origin_id = $1 AND i.page = $2 AND v.version = $3 AND v.pinned_at IS NOT NULL`, originID, m[1], atoi(m[2]))
		if id != "" || err != nil {
			return id, err
		}
	}
	return rc.lookup(ctx, `SELECT id FROM items WHERE origin_id = $1 AND page = $2 AND version > 0`, originID, page)
}

// atoi reads a version the path patterns have already bounded.
func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// The default layout's documents (blyg.ItemPath, blyg.PinPath), which is
// the only layout blygd publishes.
var (
	itemDocPath = regexp.MustCompile(`^items/([0-9a-hjkmnp-tv-z]{26})\.json$`)
	pinDocPath  = regexp.MustCompile(`^items/([0-9a-hjkmnp-tv-z]{26})/v([1-9][0-9]{0,8})\.json$`)
	pinPagePath = regexp.MustCompile(`^(.+/)v([1-9][0-9]{0,8})/$`)
)

func (rc *Receiver) lookup(ctx context.Context, q string, args ...any) (string, error) {
	var id string
	err := rc.Pool.QueryRow(ctx, q, args...).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("target lookup: %w", err)
	}
	return id, nil
}
