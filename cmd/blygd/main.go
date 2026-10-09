// Command blygd is a Blygger studio: publisher, reader and hopper service in
// one binary over Postgres. `serve` also runs the mention workers and the
// reader's poller.
//
//	blygd serve                              run the server
//	blygd user add <handle> [title]          create a user, their origin and an owner token
//	blygd token mint <handle> <name> <scope>…  mint a scoped token (paste-a-token path, §16.6)
//	blygd rebuild <handle>                   re-render an origin's surface
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

const usage = `usage:
  blygd serve
  blygd user add <handle> [title]
  blygd token mint <handle> <name> <scope>...
  blygd rebuild <handle>

scopes: ` + "*, " + "items:read items:write items:publish items:withdraw items:pin hoppers:read hoppers:write mentions:read origin:write tokens:write reader:read reader:write"

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

func serve(ctx context.Context, log *slog.Logger, cfg *config.Config, pool *pgxpool.Pool, st *studio.Studio, accts *account.Accounts, fc *fetch.Client) error {
	if cfg.AllowPrivate {
		log.Warn("BLYGD_ALLOW_PRIVATE_FETCH is on: Webmention sources can reach private addresses. Development only.")
	}
	receiver := &mention.Receiver{Pool: pool, Limits: mention.DefaultRateLimits}
	rd := &reader.Reader{Pool: pool, Fetch: &reader.LocalFirst{Studio: st, Remote: fc}, Log: log}
	st.Imports = rd // §10.2 step 2: imported snapshots reach a publish only through this
	hoppers := &hopper.Store{Pool: pool}
	api := &web.API{Pool: pool, Accounts: accts, Studio: st, Hoppers: hoppers, Reader: rd, Log: log}
	surface := &web.Surface{Studio: st, Receiver: receiver, Hoppers: hoppers, Scheme: cfg.Scheme(), TrustProxy: cfg.TrustProxy, Log: log}

	workers := &mention.Workers{Pool: pool, Fetch: fc, Log: log}
	workers.Run(ctx)
	rd.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           web.Router(api, surface, log),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
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
