// Package testdb gives each test a fresh Postgres database, migrated. Tests that need it skip unless BLYGD_TEST_DATABASE_URL is set to
// a database the test user may CREATE DATABASE from (see the Makefile).
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chrisbodhi/blygd/internal/db"
)

// New returns a pool on a new, migrated database that is dropped when the
// test ends.
func New(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := NewEmpty(t)
	if _, _, err := db.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// NewEmpty returns a pool on a new database with nothing in it, dropped
// when the test ends.
func NewEmpty(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv("BLYGD_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("BLYGD_TEST_DATABASE_URL not set; skipping database test")
	}
	ctx := context.Background()
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "blygd_t_" + hex.EncodeToString(b[:])

	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	conn.Close(ctx)

	cfg, err := pgxpool.ParseConfig(admin)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if c, err := pgx.Connect(context.Background(), admin); err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			c.Close(context.Background())
		}
	})
	return pool
}
