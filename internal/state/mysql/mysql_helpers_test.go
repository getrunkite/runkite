package mysql_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state/mysql"
)

// Shared test helpers for this package's supplementary (non-conformance)
// test files -- concurrency races, dialect-specific quirks, and the
// AgentSchema interface gap that conformance itself doesn't cover on
// any backend yet. See mysql_test.go's doc comment for why this package
// carries both a conformance-suite wiring test AND these targeted ones.

func testDSN() string {
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		dsn = "runkite:runkite@tcp(127.0.0.1:3307)/runkite_test?parseTime=true"
	}
	return dsn
}

func newTestStore(t *testing.T) *mysql.Store {
	t.Helper()
	ctx := context.Background()
	s, err := mysql.New(ctx, testDSN())
	if err != nil {
		t.Skipf("mysql not available: %v", err)
	}
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.TruncateAll(ctx); err != nil {
		t.Fatalf("TruncateAll: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seedThread is a small helper -- every run/checkpoint in this package
// has a FK to threads(thread_id) ON DELETE CASCADE, same as Postgres/
// SQLite, so tests exercising those need a thread row to exist first.
func seedThread(t *testing.T, ctx context.Context, s interface {
	CreateThread(context.Context, *models.Thread) error
}, threadID string) {
	t.Helper()
	now := time.Now().UTC()
	if err := s.CreateThread(ctx, &models.Thread{ThreadID: threadID, Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seedThread(%s): %v", threadID, err)
	}
}
