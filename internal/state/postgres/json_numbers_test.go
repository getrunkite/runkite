package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/conformance"
	"github.com/getrunkite/runkite/internal/state/postgres"
)

func TestPostgresExactJSONNumbers(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set")
	}
	conformance.RunExactJSONNumberTests(t, func(t *testing.T) state.Store {
		ctx := context.Background()
		s, err := postgres.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		if err := s.Init(ctx); err != nil {
			t.Fatal(err)
		}
		if err := s.TruncateAll(ctx); err != nil {
			t.Fatal(err)
		}
		return s
	})
}
