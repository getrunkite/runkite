package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/runkite/runkite/internal/state"
	"github.com/runkite/runkite/internal/state/conformance"
	"github.com/runkite/runkite/internal/state/postgres"
)

func TestPostgresStore(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set — skipping Postgres conformance tests")
	}

	// Create once, init schema
	ctx := context.Background()
	setupStore, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	if err := setupStore.Init(ctx); err != nil {
		t.Fatalf("init postgres store: %v", err)
	}
	setupStore.Close()

	conformance.RunStoreSuite(t, func(t *testing.T) state.Store {
		ctx := context.Background()
		s, err := postgres.New(ctx, dsn)
		if err != nil {
			t.Fatalf("connect to postgres: %v", err)
		}
		// Clean slate for each subtest
		if err := s.TruncateAll(ctx); err != nil {
			t.Fatalf("truncate: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}
