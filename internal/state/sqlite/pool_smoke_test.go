package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
)

// These were written while MaxOpenConns was briefly 8 (see sqlite.go's
// New() doc comment for why that was tried and reverted -- the fixed
// DSN alone, at MaxOpenConns(1), turned out faster than DSN+pool=8).
// Kept and still meaningful at pool=1: TestPool_allConnectionsGetPragmas
// now confirms the single connection retains its pragmas across
// repeated concurrent checkouts (goroutines queue for the same
// connection rather than each getting a distinct one), and
// TestPool_concurrentUpsertsNoBusy still confirms concurrent Go-level
// callers never see a SQLITE_BUSY error, just serialized through Go's
// own connection checkout -- both real properties worth a permanent
// regression test regardless of pool size.
func TestPool_allConnectionsGetPragmas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pool.db")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	var wg sync.WaitGroup
	errCh := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := s.db.Conn(context.Background())
			if err != nil {
				errCh <- err.Error()
				return
			}
			defer conn.Close()
			var jm string
			var bt, fk int
			if err := conn.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&jm); err != nil {
				errCh <- err.Error()
				return
			}
			_ = conn.QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&bt)
			_ = conn.QueryRowContext(context.Background(), "PRAGMA foreign_keys").Scan(&fk)
			if jm != "wal" || bt != 5000 || fk != 1 {
				errCh <- fmt.Sprintf("jm=%s bt=%d fk=%d", jm, bt, fk)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Errorf("pooled conn: %s", e)
	}
}

func TestPool_concurrentUpsertsNoBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "busy.db")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	const workers = 64
	const per = 40
	var errs atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for j := 0; j < per; j++ {
				id := fmt.Sprintf("a-%d-%d", w, j)
				ctx := tenant.WithContext(context.Background(), "default")
				if err := s.UpsertAgent(ctx, &models.Agent{AgentID: id, Name: id}); err != nil {
					errs.Add(1)
					if errs.Load() <= 3 {
						t.Logf("upsert err: %v", err)
					}
				}
			}
		}(w)
	}
	wg.Wait()
	if n := errs.Load(); n != 0 {
		t.Fatalf("got %d SQLITE errors under concurrent upserts (pool=8 should be 0 with WAL+busy_timeout)", n)
	}
	t.Logf("2560 upserts ok in %s", time.Since(start))
}
