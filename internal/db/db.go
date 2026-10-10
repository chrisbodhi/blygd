// Package db owns the Postgres connection pool and the schema migrations.
package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/tern/v2/migrate"
)

// Migrations are numbered SQL files, run once each, in order, by tern.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// versionTable is where tern records the current migration.
const versionTable = "schema_version"

// ErrPredatesMigrations is a database built by a blygd from before
// migrations, when the schema was applied idempotently at startup. Such a
// schema may be missing columns no migration knows to add.
var ErrPredatesMigrations = errors.New("this database predates migrations (pre-release blygd): recreate it, then start blygd")

// Open connects to Postgres. It does not migrate: callers decide (Migrate,
// or Status to refuse an out-of-date schema).
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

// Migrations returns the embedded migration files, for tests and tooling.
func Migrations() fs.FS {
	sub, _ := fs.Sub(migrationFiles, "migrations")
	return sub
}

// Migrate brings the database to the latest migration and reports the
// versions it went from and to. tern holds an advisory lock while it
// works, so two starting processes cannot race, and runs each migration in
// its own transaction. A database newer than this binary is an error.
func Migrate(ctx context.Context, pool *pgxpool.Pool) (from, to int32, err error) {
	return withMigrator(ctx, pool, func(m *migrate.Migrator) (int32, int32, error) {
		from, err := m.GetCurrentVersion(ctx)
		if err != nil {
			return 0, 0, err
		}
		if err := m.Migrate(ctx); err != nil {
			return from, from, fmt.Errorf("migrate: %w", err)
		}
		to, err := m.GetCurrentVersion(ctx)
		return from, to, err
	})
}

// Status reports the database's migration and the latest this binary has.
func Status(ctx context.Context, pool *pgxpool.Pool) (current, latest int32, err error) {
	return withMigrator(ctx, pool, func(m *migrate.Migrator) (int32, int32, error) {
		current, err := m.GetCurrentVersion(ctx)
		return current, int32(len(m.Migrations)), err
	})
}

func withMigrator(ctx context.Context, pool *pgxpool.Pool, fn func(*migrate.Migrator) (int32, int32, error)) (int32, int32, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer conn.Release()
	// Before tern creates its version table: a database with blygd's tables
	// and no version table was built the old way.
	var hasVersions, hasTables bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL, to_regclass('items') IS NOT NULL`,
		versionTable).Scan(&hasVersions, &hasTables); err != nil {
		return 0, 0, err
	}
	if hasTables && !hasVersions {
		return 0, 0, ErrPredatesMigrations
	}
	m, err := migrate.NewMigrator(ctx, conn.Conn(), versionTable)
	if err != nil {
		return 0, 0, err
	}
	if err := m.LoadMigrations(Migrations()); err != nil {
		return 0, 0, fmt.Errorf("load migrations: %w", err)
	}
	return fn(m)
}
