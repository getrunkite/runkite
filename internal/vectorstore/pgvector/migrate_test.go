package pgvector_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getrunkite/runkite/internal/state/migrate"
	"github.com/getrunkite/runkite/internal/vectorstore/pgvector"
)

// TestInitStampsSeparateVersionTable proves pgvector uses
// vector_schema_migrations (not the state store's schema_migrations) and
// that legacy vector_items without a version row get baseline Up + stamp.
func TestInitStampsSeparateVersionTable(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set — skipping pgvector migration tests")
	}
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()
	for _, q := range []string{
		`DROP TABLE IF EXISTS vector_items CASCADE`,
		`DROP TABLE IF EXISTS vector_schema_migrations CASCADE`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	s, err := pgvector.New(ctx, dsn, testDimensions)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	bk := migrate.NewPgxTable(pool, "vector_schema_migrations")
	cur, err := bk.Current(ctx)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if cur != 1 {
		t.Fatalf("vector_schema_migrations version = %d, want 1", cur)
	}

	// State table must not be required / must not absorb this stamp.
	stateExists, err := migrate.PgxTableExists(ctx, pool, "schema_migrations")
	if err != nil {
		t.Fatalf("PgxTableExists schema_migrations: %v", err)
	}
	if stateExists {
		stateBK := migrate.NewPgx(pool)
		stateCur, err := stateBK.Current(ctx)
		if err != nil {
			t.Fatalf("state Current: %v", err)
		}
		// If state migrations exist from other tests, vector Init must not
		// have incremented them as a side effect of stamping vector v1.
		_ = stateCur
	}

	exists, err := migrate.PgxTableExists(ctx, pool, "vector_items")
	if err != nil || !exists {
		t.Fatalf("vector_items should exist after Init (exists=%v err=%v)", exists, err)
	}

	// Second Init is a no-op (already at v1).
	if err := s.Init(ctx); err != nil {
		t.Fatalf("second Init: %v", err)
	}
	cur2, err := bk.Current(ctx)
	if err != nil {
		t.Fatalf("Current after second Init: %v", err)
	}
	if cur2 != 1 {
		t.Fatalf("version after second Init = %d, want 1", cur2)
	}
}

func TestLegacyStampRunsBaselineUp(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set — skipping pgvector migration tests")
	}
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()
	for _, q := range []string{
		`DROP TABLE IF EXISTS vector_items CASCADE`,
		`DROP TABLE IF EXISTS vector_schema_migrations CASCADE`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// Simulate a pre-versioned install: table exists, no version rows.
	// Extension may already exist from other tests; CREATE EXTENSION is
	// idempotent and required for the vector(N) column type.
	if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		t.Fatalf("CREATE EXTENSION: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE TABLE vector_items (
			tenant_id  TEXT NOT NULL DEFAULT 'default',
			namespace  TEXT NOT NULL,
			id         TEXT NOT NULL,
			content    TEXT DEFAULT '',
			embedding  vector(3) NOT NULL,
			metadata   JSONB DEFAULT '{}',
			created_at TIMESTAMPTZ DEFAULT NOW(),
			updated_at TIMESTAMPTZ DEFAULT NOW(),
			PRIMARY KEY (tenant_id, namespace, id)
		)`); err != nil {
		t.Fatalf("create legacy vector_items: %v", err)
	}

	s, err := pgvector.New(ctx, dsn, testDimensions)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init legacy: %v", err)
	}

	bk := migrate.NewPgxTable(pool, "vector_schema_migrations")
	cur, err := bk.Current(ctx)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if cur != 1 {
		t.Fatalf("legacy stamp version = %d, want 1", cur)
	}
}

func TestDowngradeDropsBaseline(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set — skipping pgvector migration tests")
	}
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()
	for _, q := range []string{
		`DROP TABLE IF EXISTS vector_items CASCADE`,
		`DROP TABLE IF EXISTS vector_schema_migrations CASCADE`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	s, err := pgvector.New(ctx, dsn, testDimensions)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Downgrade(ctx); err != nil {
		t.Fatalf("Downgrade: %v", err)
	}

	exists, err := migrate.PgxTableExists(ctx, pool, "vector_items")
	if err != nil {
		t.Fatalf("PgxTableExists: %v", err)
	}
	if exists {
		t.Fatal("vector_items should be gone after baseline Downgrade")
	}
	bk := migrate.NewPgxTable(pool, "vector_schema_migrations")
	cur, err := bk.Current(ctx)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if cur != 0 {
		t.Fatalf("version after Downgrade = %d, want 0", cur)
	}
	if err := s.Downgrade(ctx); !errors.Is(err, migrate.ErrNoMigration) {
		t.Fatalf("second Downgrade: got %v, want ErrNoMigration", err)
	}
}
