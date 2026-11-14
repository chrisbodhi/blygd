package web

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chrisbodhi/blygd/internal/account"
	"github.com/chrisbodhi/blygd/internal/config"
	"github.com/chrisbodhi/blygd/internal/events"
	"github.com/chrisbodhi/blygd/internal/fetch"
	"github.com/chrisbodhi/blygd/internal/hopper"
	"github.com/chrisbodhi/blygd/internal/mention"
	"github.com/chrisbodhi/blygd/internal/reader"
	"github.com/chrisbodhi/blygd/internal/studio"
)

// App is one blygd instance, wired. It is the composition root: `serve`,
// the other commands and the end-to-end tests all build it here, so the
// tests exercise the graph production runs (architecture review P4).
type App struct {
	Fetch    *fetch.Client
	Studio   *studio.Studio
	Accounts *account.Accounts
	Reader   *reader.Reader
	Hoppers  *hopper.Store
	Mentions *mention.Store
	Hub      *events.Hub
	Receiver *mention.Receiver
	Workers  *mention.Workers
	API      *API
	Surface  *Surface
	Handler  http.Handler
	Log      *slog.Logger
	// QueueEvery is how often Run logs the depth of the background queues
	// (a minute when zero), and QueueLate how far past due a queued row
	// must be to count as late (15 minutes when zero): longer than any
	// lease, so a late row means a worker is behind or stopped.
	QueueEvery, QueueLate time.Duration
}

// New wires an instance. It starts nothing: Run does.
func New(cfg *config.Config, pool *pgxpool.Pool, log *slog.Logger) *App {
	fc := &fetch.Client{UserAgent: cfg.Generator() + " (+" + cfg.GeneratorURL + ")", AllowPrivate: cfg.AllowPrivate}
	st := &studio.Studio{Pool: pool, Generator: cfg.Generator(), GeneratorURL: cfg.GeneratorURL,
		Level: cfg.Level, Fetch: fc, FragmentCap: cfg.FragmentCap, MediaMax: cfg.MediaMax}
	accts := &account.Accounts{Pool: pool, Origins: st}
	rd := &reader.Reader{Pool: pool, Fetch: fc, Log: log,
		ManualPollFloor: time.Duration(cfg.PollFloor) * time.Second}
	st.Imports = rd // §10.2 step 2: imported snapshots reach a publish only through this
	hoppers := &hopper.Store{Pool: pool}
	mentions := &mention.Store{Pool: pool}
	hub := &events.Hub{Pool: pool, Log: log}
	receiver := &mention.Receiver{Pool: pool, Limits: mention.DefaultRateLimits}
	api := &API{Pool: pool, Accounts: accts, Studio: st, Hoppers: hoppers, Mentions: mentions, Reader: rd, Events: hub, Log: log}
	if cfg.RateLimits {
		api.Limits = NewLimits(cfg.TrustProxy)
	}
	surface := &Surface{Studio: st, Receiver: receiver, Hoppers: hoppers, Scheme: cfg.Scheme(), TrustProxy: cfg.TrustProxy, Log: log}
	// Same-instance reads, forks and mentions are answered in-process, from
	// the surface any remote client would see (and outside its budgets).
	fc.Local, fc.LocalBase = surface, cfg.PublicURL
	return &App{
		Fetch: fc, Studio: st, Accounts: accts, Reader: rd, Hoppers: hoppers, Mentions: mentions, Hub: hub, Receiver: receiver,
		Workers: &mention.Workers{Pool: pool, Fetch: fc, Log: log, Display: st.ChangeDisplay},
		API:     api, Surface: surface, Handler: Router(api, surface, log), Log: log,
	}
}

// Run runs the background work, the event hub, the mention workers, the
// poller and the subscription importer, until ctx ends, and returns once
// all of it has stopped: no batch is mid-query when the caller closes the
// pool (review P5).
func (a *App) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { a.Hub.Run(ctx) })
	wg.Go(func() { a.Workers.Run(ctx) })
	wg.Go(func() { a.Reader.Run(ctx) })
	wg.Go(func() { a.Reader.RunImports(ctx) })
	wg.Go(func() { a.logQueues(ctx) })
	wg.Wait()
}

// logQueues logs the depth of every background queue once a minute, as one
// line: what is waiting, and what is late. A late row means a worker is
// behind or stopped, so a line with one is a warning, which is what an
// operator watching the journal (`journalctl -u blygd -p warning`) sees.
func (a *App) logQueues(ctx context.Context) {
	every := a.QueueEvery
	if every <= 0 {
		every = time.Minute
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			level, attrs := a.queueLine(ctx)
			a.Log.Log(ctx, level, "queues", attrs...)
		}
	}
}

// queueLine reads the queues each package keeps, and says how loudly to
// log them.
func (a *App) queueLine(ctx context.Context) (slog.Level, []any) {
	late := a.QueueLate
	if late <= 0 {
		late = 15 * time.Minute
	}
	level := slog.LevelInfo
	var attrs []any
	for _, q := range []func(context.Context, time.Duration) (map[string]int64, error){a.Workers.Queues, a.Reader.Queues} {
		counts, err := q(ctx, late)
		if err != nil {
			return slog.LevelWarn, []any{"err", err}
		}
		keys := make([]string, 0, len(counts))
		for k := range counts {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			attrs = append(attrs, k, counts[k])
			if strings.HasSuffix(k, "_late") && counts[k] > 0 {
				level = slog.LevelWarn
			}
		}
	}
	return level, attrs
}
