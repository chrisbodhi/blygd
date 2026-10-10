// Command blygd is a Blygger studio: publisher, reader and hopper service in
// one binary over Postgres. `serve` also runs the mention workers and the
// reader's poller.
//
//	blygd serve                              run the server (migrates first, unless BLYGD_MIGRATE_ON_START=false)
//	blygd migrate [status]                   apply pending schema migrations, or report them
//	blygd user add <handle> [title]          create a user, their origin and an owner token
//	blygd token mint <handle> <name> <scope>…  mint a scoped token (paste-a-token path, §16.6)
//	blygd rebuild <handle>                   re-render an origin's surface
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chrisbodhi/blygd/internal/account"
	"github.com/chrisbodhi/blygd/internal/config"
	"github.com/chrisbodhi/blygd/internal/db"
	"github.com/chrisbodhi/blygd/internal/events"
	"github.com/chrisbodhi/blygd/internal/fetch"
	"github.com/chrisbodhi/blygd/internal/hopper"
	"github.com/chrisbodhi/blygd/internal/mention"
	"github.com/chrisbodhi/blygd/internal/reader"
	"github.com/chrisbodhi/blygd/internal/studio"
	"github.com/chrisbodhi/blygd/internal/web"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "blygd:", err)
		os.Exit(1)
	}
}

var usage = `usage:
  blygd serve
  blygd migrate [status]
  blygd user add <handle> [title]
  blygd token mint <handle> <name> <scope>...
  blygd rebuild <handle>

scopes: * ` + strings.Join(account.AllScopes, " ")

func run(log *slog.Logger, args []string) error {
	if len(args) == 0 {
		args = []string{"serve"}
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if args[0] == "migrate" {
		return migrateCmd(ctx, pool, args[1:])
	}
	if err := ensureSchema(ctx, log, cfg, pool); err != nil {
		return err
	}

	fc := &fetch.Client{UserAgent: cfg.Generator() + " (+" + cfg.GeneratorURL + ")", AllowPrivate: cfg.AllowPrivate}
	st := &studio.Studio{Pool: pool, Generator: cfg.Generator(), GeneratorURL: cfg.GeneratorURL,
		Level: cfg.Level, Fetch: fc, FragmentCap: cfg.FragmentCap, MediaMax: cfg.MediaMax}
	accts := &account.Accounts{Pool: pool}

	switch {
	case args[0] == "serve":
		return serve(ctx, log, cfg, pool, st, accts, fc)
	case len(args) >= 3 && args[0] == "user" && args[1] == "add":
		title := strings.Join(args[3:], " ")
		_, originID, secret, err := accts.CreateUser(ctx, args[2], cfg.PublicURL, title)
		if err != nil {
			return err
		}
		if err := st.Rebuild(ctx, originID); err != nil {
			return err
		}
		o, _ := st.GetOrigin(ctx, originID)
		fmt.Printf("origin: %s\nowner token (shown once): %s\n", o.BaseURL, secret)
		return nil
	case len(args) >= 5 && args[0] == "token" && args[1] == "mint":
		uid, err := accts.UserByHandle(ctx, args[2])
		if err != nil {
			return fmt.Errorf("no user %q", args[2])
		}
		secret, _, err := accts.Mint(ctx, uid, args[3], args[4:])
		if err != nil {
			return err
		}
		fmt.Printf("token (shown once): %s\n", secret)
		return nil
	case len(args) == 2 && args[0] == "rebuild":
		uid, err := accts.UserByHandle(ctx, args[1])
		if err != nil {
			return fmt.Errorf("no user %q", args[1])
		}
		o, err := st.OriginForOwner(ctx, uid)
		if err != nil {
			return err
		}
		return st.Rebuild(ctx, o.ID)
	}
	return errors.New(usage)
}

// migrateCmd is `blygd migrate`: apply pending migrations, or with
// "status", report the database's version and the binary's.
func migrateCmd(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	switch {
	case len(args) == 1 && args[0] == "status":
		current, latest, err := db.Status(ctx, pool)
		if err != nil {
			return err
		}
		fmt.Printf("database at migration %d; this binary has %d\n", current, latest)
		if current < latest {
			fmt.Printf("%d pending: run blygd migrate\n", latest-current)
		}
		return nil
	case len(args) == 0:
		from, to, err := db.Migrate(ctx, pool)
		if err != nil {
			return err
		}
		if from == to {
			fmt.Printf("up to date at migration %d\n", to)
		} else {
			fmt.Printf("migrated from %d to %d\n", from, to)
		}
		return nil
	}
	return errors.New(usage)
}

// ensureSchema migrates before any other command, or, with
// BLYGD_MIGRATE_ON_START=false, refuses to run on an out-of-date schema so
// that schema changes stay a deliberate act (`blygd migrate`).
func ensureSchema(ctx context.Context, log *slog.Logger, cfg *config.Config, pool *pgxpool.Pool) error {
	if cfg.MigrateOnStart {
		from, to, err := db.Migrate(ctx, pool)
		if err != nil {
			return err
		}
		if from != to {
			log.Info("migrated", "from", from, "to", to)
		}
		return nil
	}
	current, latest, err := db.Status(ctx, pool)
	if err != nil {
		return err
	}
	if current != latest {
		return fmt.Errorf("database is at migration %d and this binary expects %d: run blygd migrate", current, latest)
	}
	return nil
}

func serve(ctx context.Context, log *slog.Logger, cfg *config.Config, pool *pgxpool.Pool, st *studio.Studio, accts *account.Accounts, fc *fetch.Client) error {
	if cfg.AllowPrivate {
		log.Warn("BLYGD_ALLOW_PRIVATE_FETCH is on: Webmention sources can reach private addresses. Development only.")
	}
	receiver := &mention.Receiver{Pool: pool, Limits: mention.DefaultRateLimits}
	rd := &reader.Reader{Pool: pool, Fetch: &reader.LocalFirst{Studio: st, Remote: fc}, Log: log,
		ManualPollFloor: time.Duration(cfg.PollFloor) * time.Second}
	st.Imports = rd // §10.2 step 2: imported snapshots reach a publish only through this
	hoppers := &hopper.Store{Pool: pool}
	hub := &events.Hub{Pool: pool, Log: log}
	go hub.Run(ctx)
	api := &web.API{Pool: pool, Accounts: accts, Studio: st, Hoppers: hoppers, Reader: rd, Events: hub, Log: log}
	surface := &web.Surface{Studio: st, Receiver: receiver, Hoppers: hoppers, Scheme: cfg.Scheme(), TrustProxy: cfg.TrustProxy, Log: log}

	workers := &mention.Workers{Pool: pool, Fetch: fc, Log: log, OnDisplayChange: st.RerenderItem}
	workers.Run(ctx)
	rd.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           web.Router(api, surface, log),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		// Requests share the process's context, so open event streams end
		// when a shutdown begins instead of holding it for its full timeout.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Listen, "public", cfg.PublicURL)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}
