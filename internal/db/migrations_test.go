package db_test

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"

	"github.com/chrisbodhi/blygd/internal/db"
	"github.com/chrisbodhi/blygd/internal/testdb"
)

// Migrations are append-only: once shipped, a migration is never edited,
// because databases out there already ran it. migrations.sum records each
// file's SHA-256; a changed file fails here, and so does a new file until
// it is recorded (shasum -a 256 in internal/db/migrations).
func TestMigrationsAreAppendOnly(t *testing.T) {
	f, err := os.Open("migrations.sum")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recorded := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if fields := strings.Fields(sc.Text()); len(fields) == 2 {
			recorded[fields[1]] = fields[0]
		}
	}
	files, err := fs.Glob(db.Migrations(), "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		b, _ := fs.ReadFile(db.Migrations(), name)
		sum := sha256.Sum256(b)
		switch want, ok := recorded[name]; {
		case !ok:
			t.Errorf("%s is not in migrations.sum: record it once it is final", name)
		case want != hex.EncodeToString(sum[:]):
			t.Errorf("%s changed after it was recorded: shipped migrations are never edited; write a new one", name)
		}
		if strings.Contains(string(b), "{"+"{") {
			t.Errorf("%s contains a template action: tern parses migrations as Go templates", name)
		}
		delete(recorded, name)
	}
	for name := range recorded {
		t.Errorf("migrations.sum lists %s, which no longer exists: shipped migrations are never removed", name)
	}
}

func TestMigrate(t *testing.T) {
	pool := testdb.NewEmpty(t)
	ctx := context.Background()
	from, to, err := db.Migrate(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := fs.Glob(db.Migrations(), "*.sql")
	if from != 0 || int(to) != len(files) {
		t.Fatalf("fresh migrate: %d → %d, want 0 → %d", from, to, len(files))
	}
	if from, to, err = db.Migrate(ctx, pool); err != nil || from != to {
		t.Fatalf("second migrate must do nothing: %d → %d, %v", from, to, err)
	}
	if cur, latest, err := db.Status(ctx, pool); err != nil || cur != latest {
		t.Fatalf("status: %d of %d, %v", cur, latest, err)
	}

	// A database newer than this binary is refused, never migrated down.
	if _, err := pool.Exec(ctx, `UPDATE schema_version SET version = 999`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.Migrate(ctx, pool); err == nil || !strings.Contains(err.Error(), "999") {
		t.Fatalf("newer database: %v", err)
	}
}

// A database built before migrations, by the old apply-at-startup, may be
// missing columns: it is refused with a clear way forward.
func TestMigrateRefusesAPreMigrationDatabase(t *testing.T) {
	pool := testdb.NewEmpty(t)
	ctx := context.Background()
	first, _ := fs.ReadFile(db.Migrations(), "001_initial.sql")
	if _, err := pool.Exec(ctx, string(first)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.Migrate(ctx, pool); !errors.Is(err, db.ErrPredatesMigrations) {
		t.Fatalf("pre-migration database: %v", err)
	}
	if _, _, err := db.Status(ctx, pool); !errors.Is(err, db.ErrPredatesMigrations) {
		t.Fatalf("status of a pre-migration database: %v", err)
	}
}
