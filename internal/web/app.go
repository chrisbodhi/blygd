package web

import (
	"context"
	"log/slog"
	"net/http"
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
		API:     api, Surface: surface, Handler: Router(api, surface, log),
	}
}

// Run runs the background work, the event hub, the mention workers and
// the poller, until ctx ends, and returns once all of it has stopped: no
// batch is mid-query when the caller closes the pool (review P5).
func (a *App) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { a.Hub.Run(ctx) })
	wg.Go(func() { a.Workers.Run(ctx) })
	wg.Go(func() { a.Reader.Run(ctx) })
	wg.Wait()
}
