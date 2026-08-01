package pgvector_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getrunkite/runkite/internal/vectorstore"
	"github.com/getrunkite/runkite/internal/vectorstore/conformance"
	"github.com/getrunkite/runkite/internal/vectorstore/pgvector"
)

const testDimensions = 3

// newTestStore returns a freshly-truncated pgvector store for one test.
// Skips (not fails) if POSTGRES_DSN is unset, same convention as
// internal/state/postgres's own tests -- this suite needs a real
// pgvector-capable Postgres (see docker-compose.test.yml), not something
// SQLite could stand in for.
func newTestStore(t *testing.T) vectorstore.VectorStore {
	t.Helper()
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set — skipping pgvector tests")
	}
	ctx := context.Background()

	// Drop first, not just truncate: vector_items' embedding column
	// width is fixed at CREATE TABLE time (see pgvector.go's Init doc
	// comment) and IF NOT EXISTS never revisits it, so a stale table
	// left over from a different dimensions value (another test binary,
	// or a manually-run example against this same shared test database)
	// would otherwise make every Upsert/Search in this suite fail with a
	// dimension-mismatch error that has nothing to do with the code
	// under test.
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New for setup: %v", err)
	}
	if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS vector_items"); err != nil {
		pool.Close()
		t.Fatalf("drop vector_items: %v", err)
	}
	pool.Close()

	s, err := pgvector.New(ctx, dsn, testDimensions)
	if err != nil {
		t.Fatalf("pgvector.New: %v", err)
	}
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestPgvectorStore(t *testing.T) {
	conformance.RunVectorStoreSuite(t, newTestStore)
}
