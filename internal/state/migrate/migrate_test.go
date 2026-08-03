package migrate_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/getrunkite/runkite/internal/state/migrate"
)

func openMem(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func testMigrations(db *sql.DB, marker *int) []migrate.Migration {
	return []migrate.Migration{
		{
			Version: 1,
			Name:    "baseline",
			Up: func(ctx context.Context) error {
				_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS agents (id TEXT PRIMARY KEY)`)
				if err == nil {
					*marker = 1
				}
				return err
			},
			Down: func(ctx context.Context) error {
				_, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS agents`)
				if err == nil {
					*marker = 0
				}
				return err
			},
		},
		{
			Version: 2,
			Name:    "add_widgets",
			Up: func(ctx context.Context) error {
				_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS widgets (id TEXT PRIMARY KEY)`)
				if err == nil {
					*marker = 2
				}
				return err
			},
			Down: func(ctx context.Context) error {
				_, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS widgets`)
				if err == nil {
					*marker = 1
				}
				return err
			},
		},
	}
}

func TestUpgradeAndDowngrade(t *testing.T) {
	db := openMem(t)
	bk := migrate.NewSQL(db, migrate.SQLite)
	marker := 0
	migrations := testMigrations(db, &marker)
	ctx := context.Background()

	if err := migrate.Upgrade(ctx, bk, migrations, nil); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	cur, err := bk.Current(ctx)
	if err != nil || cur != 2 {
		t.Fatalf("current after upgrade = %d, %v; want 2", cur, err)
	}
	if marker != 2 {
		t.Fatalf("marker = %d, want 2", marker)
	}

	if err := migrate.Downgrade(ctx, bk, migrations); err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	cur, err = bk.Current(ctx)
	if err != nil || cur != 1 {
		t.Fatalf("current after one downgrade = %d, %v; want 1", cur, err)
	}
	if marker != 1 {
		t.Fatalf("marker = %d, want 1", marker)
	}

	if err := migrate.Upgrade(ctx, bk, migrations, nil); err != nil {
		t.Fatalf("re-upgrade: %v", err)
	}
	cur, _ = bk.Current(ctx)
	if cur != 2 {
		t.Fatalf("current after re-upgrade = %d, want 2", cur)
	}
}

func TestStampLegacyRunsUpThenStamps(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	// Minimal legacy schema: agents present (trips the probe) and a
	// threads table missing a column that Up self-heals.
	if _, err := db.ExecContext(ctx, `CREATE TABLE agents (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("seed agents: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE threads (thread_id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("seed threads: %v", err)
	}
	bk := migrate.NewSQL(db, migrate.SQLite)
	upCalls := 0
	migrations := []migrate.Migration{{
		Version: 1,
		Name:    "baseline",
		Up: func(ctx context.Context) error {
			upCalls++
			// Idempotent self-heal, same class as real baseline ADD COLUMN.
			_, err := db.ExecContext(ctx, `ALTER TABLE threads ADD COLUMN version INTEGER NOT NULL DEFAULT 1`)
			if err != nil && !containsDupCol(err) {
				return err
			}
			return nil
		},
		Down: func(ctx context.Context) error { return nil },
	}}
	legacy := func(ctx context.Context) (bool, error) {
		return migrate.TableExists(ctx, db, migrate.SQLite, "agents")
	}
	if err := migrate.Upgrade(ctx, bk, migrations, legacy); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if upCalls != 1 {
		t.Fatalf("legacy path must run Up exactly once, ran %d times", upCalls)
	}
	cur, _ := bk.Current(ctx)
	if cur != 1 {
		t.Fatalf("current = %d, want 1", cur)
	}
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('threads') WHERE name = 'version'`).Scan(&n)
	if err != nil || n != 1 {
		t.Fatalf("threads.version after legacy upgrade: n=%d err=%v", n, err)
	}
}

func containsDupCol(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate column name")
}

func TestDowngradeAtZero(t *testing.T) {
	db := openMem(t)
	bk := migrate.NewSQL(db, migrate.SQLite)
	marker := 0
	migrations := testMigrations(db, &marker)[:1]
	err := migrate.Downgrade(context.Background(), bk, migrations)
	if !errors.Is(err, migrate.ErrNoMigration) {
		t.Fatalf("want ErrNoMigration, got %v", err)
	}
}

func TestIdempotentSecondUpgrade(t *testing.T) {
	db := openMem(t)
	bk := migrate.NewSQL(db, migrate.SQLite)
	marker := 0
	migrations := testMigrations(db, &marker)
	ctx := context.Background()
	if err := migrate.Upgrade(ctx, bk, migrations, nil); err != nil {
		t.Fatal(err)
	}
	marker = -1 // Up must not run again
	if err := migrate.Upgrade(ctx, bk, migrations, nil); err != nil {
		t.Fatal(err)
	}
	if marker != -1 {
		t.Fatalf("second upgrade re-ran Up (marker=%d)", marker)
	}
}
