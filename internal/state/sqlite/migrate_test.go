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
	if err != nil || cur != 6 {
		t.Fatalf("version after Init = %d, %v; want 6", cur, err)
	}
	for _, tbl := range []string{"audit_events", "policy_grants", "pending_actions", "kill_switches"} {
		if !tableExists(t, s, tbl) {
			t.Fatalf("%s missing after Init", tbl)
		}
	}

	if err := s.Init(ctx); err != nil {
		t.Fatalf("second Init: %v", err)
	}
	cur, _ = bk.Current(ctx)
	if cur != 6 {
		t.Fatalf("version after second Init = %d, want 6", cur)
	}

	// v6→v5 (index only) then v5→v4→v3→v2→v1→0 (tables)
	if err := s.Downgrade(ctx); err != nil {
		t.Fatalf("Downgrade to 5: %v", err)
	}
	cur, _ = bk.Current(ctx)
	if cur != 5 {
		t.Fatalf("version after Downgrade = %d, want 5", cur)
	}
	wantTablesGone := []string{"kill_switches", "pending_actions", "policy_grants", "audit_events"}
	for step, wantVer := range []int{4, 3, 2, 1, 0} {
		if err := s.Downgrade(ctx); err != nil {
			t.Fatalf("Downgrade to %d: %v", wantVer, err)
		}
		cur, _ = bk.Current(ctx)
		if cur != wantVer {
			t.Fatalf("version after Downgrade = %d, want %d", cur, wantVer)
		}
		if step < 4 && tableExists(t, s, wantTablesGone[step]) {
			t.Fatalf("%s still present after Downgrade to %d", wantTablesGone[step], wantVer)
		}
	}
	if err := s.Downgrade(ctx); !errors.Is(err, migrate.ErrNoMigration) {
		t.Fatalf("extra Downgrade: want ErrNoMigration, got %v", err)
	}

	if err := s.Init(ctx); err != nil {
		t.Fatalf("re-Init after downgrade: %v", err)
	}
	cur, _ = bk.Current(ctx)
	if cur != 6 {
		t.Fatalf("version after re-Init = %d, want 6", cur)
	}
	for _, tbl := range []string{"audit_events", "policy_grants", "pending_actions", "kill_switches"} {
		if !tableExists(t, s, tbl) {
			t.Fatalf("%s missing after re-Init", tbl)
		}
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
	if err != nil || cur != 6 {
		t.Fatalf("stamped version = %d, %v; want 6", cur, err)
	}
	if !columnExists(t, s, "threads", "version") {
		t.Fatal("threads.version must exist after legacy upgrade (baseline Up self-heal)")
	}
}

func columnExists(t *testing.T, s *SQLiteStore, table, column string) bool {
	t.Helper()
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('`+table+`') WHERE name = ?`,
		column,
	).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func tableExists(t *testing.T, s *SQLiteStore, name string) bool {
	t.Helper()
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name,
	).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n > 0
}
