package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/getrunkite/runkite/internal/state/migrate"
)

func TestMigrations_UpgradeDowngradeRoundTrip(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	bk := migrate.NewSQL(s.db, migrate.SQLite)
	cur, err := bk.Current(ctx)
	if err != nil || cur != 1 {
		t.Fatalf("version after Init = %d, %v; want 1", cur, err)
	}

	if err := s.Init(ctx); err != nil {
		t.Fatalf("second Init: %v", err)
	}
	cur, _ = bk.Current(ctx)
	if cur != 1 {
		t.Fatalf("version after second Init = %d, want 1", cur)
	}

	if err := s.Downgrade(ctx); err != nil {
		t.Fatalf("Downgrade: %v", err)
	}
	cur, _ = bk.Current(ctx)
	if cur != 0 {
		t.Fatalf("version after Downgrade = %d, want 0", cur)
	}
	if err := s.Downgrade(ctx); !errors.Is(err, migrate.ErrNoMigration) {
		t.Fatalf("second Downgrade: want ErrNoMigration, got %v", err)
	}

	if err := s.Init(ctx); err != nil {
		t.Fatalf("re-Init after downgrade: %v", err)
	}
	cur, _ = bk.Current(ctx)
	if cur != 1 {
		t.Fatalf("version after re-Init = %d, want 1", cur)
	}
}

// TestMigrations_LegacyBackfillsMissingColumns reproduces the upgrade
// path every pre-migration deployment hits: agents (and a thin threads
// table) already exist with no schema_migrations row, and threads is
// missing the version column that baseline Up self-heals. Stamping
// without running Up would leave the column absent and break optimistic
// concurrency at runtime.
func TestMigrations_LegacyBackfillsMissingColumns(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	// Shape mirrors a pre-versioned install: agents already present (with
	// enough columns for later FK-bearing CREATE TABLE IF NOT EXISTS
	// statements) and threads missing the version column that baseline
	// Up self-heals via addColumnIfMissing.
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE agents (
			tenant_id TEXT NOT NULL DEFAULT 'default',
			agent_id TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (tenant_id, agent_id)
		)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE threads (
			thread_id TEXT PRIMARY KEY,
			status TEXT DEFAULT 'idle'
		)`); err != nil {
		t.Fatal(err)
	}
	if columnExists(t, s, "threads", "version") {
		t.Fatal("precondition: threads.version must be absent before upgrade")
	}

	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init on legacy schema: %v", err)
	}
	bk := migrate.NewSQL(s.db, migrate.SQLite)
	cur, err := bk.Current(ctx)
	if err != nil || cur != 1 {
		t.Fatalf("stamped version = %d, %v; want 1", cur, err)
	}
	if !columnExists(t, s, "threads", "version") {
		t.Fatal("threads.version must exist after legacy upgrade (baseline Up self-heal)")
	}
}

func columnExists(t *testing.T, s *SQLiteStore, table, column string) bool {
	t.Helper()
	var n int
	// Table name is a test constant (threads/agents), not user input.
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('`+table+`') WHERE name = ?`,
		column,
	).Scan(&n)
	if err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	return n > 0
}
