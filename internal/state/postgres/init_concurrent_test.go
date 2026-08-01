package postgres_test

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getrunkite/runkite/internal/state/postgres"
)

// TestInit_ConcurrentFreshDatabase is a regression test for a real race
// found via live multi-instance testing:
// starting 3 `runkite serve` replicas simultaneously against a genuinely
// fresh Postgres database (nothing created yet) produced
// "duplicate key value violates unique constraint \"pg_type_typname_nsp_index\""
// on 2 of the 3 -- CREATE TABLE IF NOT EXISTS is idempotent once the schema
// exists, but isn't by itself race-free for the very first, from-nothing
// creation when multiple sessions run it at the same instant. Init now
// wraps its DDL in a Postgres session advisory lock to serialize this; this
// test drops to a genuinely blank schema and calls Init from several
// goroutines at once, which reproduced the bug reliably before the fix.
func TestInit_ConcurrentFreshDatabase(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set — skipping Postgres conformance tests")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	defer pool.Close()

	// Reset to a genuinely blank schema -- the race this test targets only
	// manifests when the tables don't exist yet at all, not on a repeat
	// Init() against an already-initialized database.
	if _, err := pool.Exec(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public;"); err != nil {
		t.Fatalf("reset schema: %v", err)
	}

	const concurrency = 5
	var wg sync.WaitGroup
	errs := make([]error, concurrency)
	for i := range concurrency {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := postgres.New(ctx, dsn)
			if err != nil {
				errs[i] = err
				return
			}
			defer s.Close()
			errs[i] = s.Init(ctx)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent Init() call %d failed: %v", i, err)
		}
	}
}
