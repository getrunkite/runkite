package sqlite_test

import (
	"context"
	"testing"

	"github.com/runkite/runkite/internal/state"
	"github.com/runkite/runkite/internal/state/conformance"
	"github.com/runkite/runkite/internal/state/sqlite"
)

func TestSQLiteStore(t *testing.T) {
	conformance.RunStoreSuite(t, func(t *testing.T) state.Store {
		s, err := sqlite.New("")  // in-memory for tests
		if err != nil {
			t.Fatalf("create sqlite store: %v", err)
		}
		if err := s.Init(context.Background()); err != nil {
			t.Fatalf("init sqlite store: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}
