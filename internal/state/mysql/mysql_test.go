package mysql_test

import (
	"context"
	"os"
	"testing"

	"github.com/sharanharsoor/runkite/internal/state"
	"github.com/sharanharsoor/runkite/internal/state/conformance"
	"github.com/sharanharsoor/runkite/internal/state/mysql"
)

// TestMySQLStore runs the full shared conformance suite (internal/state/
// conformance) against a real MySQL 8.4 container -- same wiring
// pattern as postgres_test.go/mongo_test.go. This is now the primary
// coverage for this package; the other _test.go files in this package
// hold ONLY what conformance doesn't already cover (concurrency races
// conformance's single-threaded suite can't exercise, MySQL-dialect-
// specific quirks, and one interface gap -- AgentSchema -- conformance
// itself doesn't test on ANY backend yet).
func TestMySQLStore(t *testing.T) {
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		dsn = "runkite:runkite@tcp(127.0.0.1:3307)/runkite_test?parseTime=true"
	}

	ctx := context.Background()
	setupStore, err := mysql.New(ctx, dsn)
	if err != nil {
		t.Skipf("mysql not available: %v", err)
	}
	if err := setupStore.Init(ctx); err != nil {
		t.Fatalf("init mysql store: %v", err)
	}
	setupStore.Close()

	conformance.RunStoreSuite(t, func(t *testing.T) state.Store {
		ctx := context.Background()
		s, err := mysql.New(ctx, dsn)
		if err != nil {
			t.Fatalf("connect to mysql: %v", err)
		}
		if err := s.TruncateAll(ctx); err != nil {
			t.Fatalf("truncate: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}
