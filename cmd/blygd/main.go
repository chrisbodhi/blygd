// Command blygd is a Blygger studio: publisher, reader and hopper service in
// one binary over Postgres. `serve` also runs the mention workers and the
// reader's poller.
//
//	blygd serve                              run the server (migrates first, unless BLYGD_MIGRATE_ON_START=false)
//	blygd migrate [status]                   apply pending schema migrations, or report them
//	blygd user add <handle> [title]          create a user, their origin and an owner token
//	blygd token mint <handle> <name> [--expires 30d] <scope>…  mint a scoped token (paste-a-token path, §16.6)
//	blygd rebuild <handle>                   re-render an origin's surface
//	blygd export <handle> > site.tar.gz      write an origin's static export (§15.7)
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chrisbodhi/blygd/internal/account"
	"github.com/chrisbodhi/blygd/internal/config"
	"github.com/chrisbodhi/blygd/internal/db"
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
  blygd token mint <handle> <name> [--expires 30d] <scope>...
  blygd rebuild <handle>
  blygd export <handle> > site.tar.gz

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

	app := web.New(cfg, pool, log)
	st, accts := app.Studio, app.Accounts

	switch {
	case args[0] == "serve":
		return serve(ctx, log, cfg, app)
	case len(args) >= 3 && args[0] == "user" && args[1] == "add":
		title := strings.Join(args[3:], " ")
		_, originID, secret, err := accts.CreateUser(ctx, args[2], cfg.PublicURL, title)
		if err != nil {
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
		scopes, expires, err := mintArgs(args[4:])
		if err != nil {
			return err
		}
		secret, _, err := accts.Mint(ctx, uid, args[3], scopes, expires)
		if err != nil {
			return err
		}
		fmt.Printf("token (shown once): %s\n", secret)
		if expires != nil {
			fmt.Printf("expires: %s\n", expires.Format(time.RFC3339))
		}
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
	case len(args) == 2 && args[0] == "export":
		uid, err := accts.UserByHandle(ctx, args[1])
		if err != nil {
			return fmt.Errorf("no user %q", args[1])
		}
		o, err := st.OriginForOwner(ctx, uid)
		if err != nil {
			return err
		}
		if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			return errors.New("export writes a gzipped tar to stdout: redirect it to a file")
		}
		return st.Export(ctx, o.ID, os.Stdout)
	}
	return errors.New(usage)
}

// mintArgs splits `token mint`'s trailing arguments into scopes and an
// optional --expires duration: Go's units (12h, 90m) or whole days (30d).
func mintArgs(args []string) ([]string, *time.Time, error) {
	var scopes []string
	var expires *time.Time
	for i := 0; i < len(args); i++ {
		if args[i] != "--expires" {
			scopes = append(scopes, args[i])
			continue
		}
		if i+1 == len(args) {
			return nil, nil, errors.New("--expires needs a duration, such as 30d or 12h")
		}
		i++
		d, err := parseDays(args[i])
		if err != nil || d <= 0 {
			return nil, nil, fmt.Errorf("--expires %q: want a positive duration, such as 30d or 12h", args[i])
		}
		t := time.Now().Add(d).UTC().Truncate(time.Second)
		expires = &t
	}
	if len(scopes) == 0 {
		return nil, nil, errors.New(usage)
	}
	return scopes, expires, nil
}

func parseDays(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		return time.Duration(days) * 24 * time.Hour, err
	}
	return time.ParseDuration(s)
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

func serve(ctx context.Context, log *slog.Logger, cfg *config.Config, app *web.App) error {
	if cfg.AllowPrivate {
		log.Warn("BLYGD_ALLOW_PRIVATE_FETCH is on: Webmention sources can reach private addresses. Development only.")
	}
	if !cfg.RateLimits {
		log.Warn("BLYGD_RATE_LIMITS is off: no request budgets. Test harnesses only.")
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	background := make(chan struct{})
	go func() {
		app.Run(ctx)
		close(background)
	}()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           app.Handler,
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
	var err error
	select {
	case err = <-errc:
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err = srv.Shutdown(shutdown)
	}
	// The workers stop with ctx; wait for them, so the pool closes under no
	// batch. A listener that failed on its own has not ended ctx, so end it.
	stop()
	select {
	case <-background:
	case <-time.After(15 * time.Second):
		log.Warn("background work still running after 15s; closing the pool under it")
	}
	return err
}
