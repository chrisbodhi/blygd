package mention

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chrisbodhi/blygd/internal/blyg"
	"github.com/chrisbodhi/blygd/internal/fetch"
	"github.com/chrisbodhi/blygd/internal/testdb"
)

// A batch never works a claim past its lease (architecture review D5):
// with sources slower than the lease allows for the whole batch, the
// claims that fit are verified and the rest come back due at once, with
// the attempt their claim spent refunded.
func TestVerifyBatchStaysWithinItsLease(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		http.NotFound(w, r)
	}))
	defer slow.Close()

	var item string
	err := pool.QueryRow(ctx, `WITH u AS (INSERT INTO users (handle) VALUES ('alice') RETURNING id),
		o AS (INSERT INTO origins (owner_id, base_url) SELECT id, 'http://alice.test/' FROM u RETURNING id)
		INSERT INTO items (id, origin_id, authored_kind) SELECT $1, id, 'fragment' FROM o RETURNING id`, blyg.NewID()).Scan(&item)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		if _, err := pool.Exec(ctx, `INSERT INTO inbound_mentions (origin_id, target_item_id, source_url, target_url, status,
			next_attempt_at, source_host, source_domain)
			SELECT origin_id, id, $2, 'http://alice.test/items/' || id || '.json', 'pending', now(), '127.0.0.1', '127.0.0.1'
			FROM items WHERE id = $1`, item, slow.URL+"/source/"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}

	wk := &Workers{Pool: pool, Fetch: &fetch.Client{AllowPrivate: true}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		verifyLease: 2 * time.Second, verifyClaimMax: time.Second}
	start := time.Now()
	n, err := wk.VerifyBatch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Claims start at about 0, 0.4 and 0.8 s; at 1.2 s one more second would
	// overrun the 2 s lease.
	if n < 1 || n > 3 || time.Since(start) > 2*time.Second {
		t.Fatalf("worked %d claims in %v", n, time.Since(start))
	}
	var done, released, spent int
	if err := pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE status <> 'pending'),
		count(*) FILTER (WHERE status = 'pending' AND attempts = 0 AND next_attempt_at <= now()),
		count(*) FILTER (WHERE status = 'pending' AND attempts > 0)
		FROM inbound_mentions`).Scan(&done, &released, &spent); err != nil {
		t.Fatal(err)
	}
	if done != n || released != 5-n || spent != 0 {
		t.Fatalf("worked %d: %d settled, %d released and due, %d pending with an attempt spent", n, done, released, spent)
	}
}
