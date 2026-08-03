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

func TestMigrations_StampLegacy(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	// Simulate a pre-versioned install: create agents without schema_migrations.
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE agents (agent_id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init on legacy: %v", err)
	}
	bk := migrate.NewSQL(s.db, migrate.SQLite)
	cur, err := bk.Current(ctx)
	if err != nil || cur != 1 {
		t.Fatalf("stamped version = %d, %v; want 1", cur, err)
	}
	// agents table from the seed must still exist (Up was not re-run as a drop).
	var name string
	if err := s.db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name='agents'`).Scan(&name); err != nil {
		t.Fatalf("legacy agents table missing after stamp: %v", err)
	}
}
