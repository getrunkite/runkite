package mysql_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"

	"github.com/getrunkite/runkite/internal/state/mysql"
	"github.com/getrunkite/runkite/internal/tenant"
)

// Use the native driver option against the same dedicated MySQL fixture as the
// rest of this package. Never print a DSN or connection diagnostic containing it.
func cronClaimProfileStore(t *testing.T, foundRows bool) *mysql.Store {
	t.Helper()
	cfg, err := mysqldriver.ParseDSN(testDSN())
	if err != nil {
		t.Fatal("invalid MySQL fixture DSN")
	}
	cfg.ClientFoundRows = foundRows
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	store, err := mysql.New(ctx, cfg.FormatDSN())
	if err != nil {
		if os.Getenv("MYSQL_DSN") != "" {
			t.Fatal("configured MySQL fixture unavailable for native claim regression")
		}
		t.Skip("MySQL fixture unavailable for native claim regression")
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal("MySQL fixture initialization failed")
	}
	if err := store.TruncateAll(ctx); err != nil {
		t.Fatal("MySQL fixture reset failed")
	}
	return store
}

func TestCronClaimClientFoundRowsSequentialScopes(t *testing.T) {
	for _, profile := range []struct {
		name      string
		foundRows bool
	}{{"changed_rows", false}, {"found_rows", true}} {
		t.Run(profile.name, func(t *testing.T) {
			store := cronClaimProfileStore(t, profile.foundRows)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			ctxA := tenant.ScopedContext(ctx, " tenant-a ")
			ctxB := tenant.ScopedContext(ctx, "tenant-a")
			fire := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
			claim := func(scope context.Context, name string, at time.Time, want bool) {
				t.Helper()
				won, err := store.TryClaimCronFire(scope, name, at)
				if err != nil {
					t.Fatal("cron claim returned an unexpected backend error")
				}
				if won != want {
					t.Errorf("cron claim won=%t, want %t", won, want)
				}
			}
			claim(ctxA, "daily", fire, true)
			claim(ctxA, "daily", fire, false)
			// A different location representing the same instant is still a loss.
			claim(ctxA, "daily", fire.In(time.FixedZone("fixture", 19800)), false)
			claim(ctxB, "daily", fire, true)
			claim(ctx, "daily", fire, true)
			claim(ctxA, "other", fire, true)
			claim(ctxA, "daily", fire.Add(time.Minute), true)
			claim(ctxA, "daily", fire, false)
			last, found, err := store.GetLastCronFireTime(ctxA, "daily")
			if err != nil || !found || !last.Equal(fire.Add(time.Minute)) {
				t.Fatal("lost claims changed the latest owned fire")
			}
			if err := store.ReleaseCronClaim(ctxB, "daily", fire); err != nil {
				t.Fatal("cron claim release failed")
			}
			claim(ctxA, "daily", fire, false)
			claim(ctxB, "daily", fire, true)
			if err := store.ReleaseCronClaim(ctxA, "daily", fire.Add(time.Minute)); err != nil {
				t.Fatal("cron claim release failed")
			}
			claim(ctxA, "daily", fire.Add(time.Minute), true)
			claim(ctxA, "daily", fire.Add(time.Minute), false)

			// Adjacent claim paths use different SQL shapes. Keep their native
			// positive controls under the same driver option without changing them.
			for _, want := range []bool{true, false} {
				won, err := store.TryClaimTerminalHook(ctx, "terminal-fixture")
				if err != nil || won != want {
					t.Fatal("terminal-hook claim changed under the driver profile")
				}
			}
			seedThread(t, ctx, store, "thread-fixture")
			for _, want := range []bool{true, false} {
				won, err := store.TryClaimThread(ctx, "thread-fixture")
				if err != nil || won != want {
					t.Fatal("busy-thread claim changed under the driver profile")
				}
			}
		})
	}
}

func TestCronClaimClientFoundRowsConcurrent(t *testing.T) {
	for _, profile := range []struct {
		name      string
		foundRows bool
	}{{"changed_rows", false}, {"found_rows", true}} {
		t.Run(profile.name, func(t *testing.T) {
			store := cronClaimProfileStore(t, profile.foundRows)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			ctx = tenant.ScopedContext(ctx, "tenant-a")
			fire := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
			type result struct {
				won bool
				err error
			}
			const contenders = 20
			start := make(chan struct{})
			results := make(chan result, contenders)
			for range contenders {
				go func() {
					<-start
					won, err := store.TryClaimCronFire(ctx, "daily", fire)
					results <- result{won, err}
				}()
			}
			close(start)
			wins := 0
			for range contenders {
				result := <-results
				if result.err != nil {
					t.Error("concurrent cron claim returned a backend error")
				}
				if result.won {
					wins++
				}
			}
			if wins != 1 {
				t.Fatalf("concurrent claim winners=%d, want 1", wins)
			}
		})
	}
}

func TestCronClaimClientFoundRowsPreservesBackendErrors(t *testing.T) {
	store := cronClaimProfileStore(t, true)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	fire := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	won, err := store.TryClaimCronFire(ctx, "cancelled", fire)
	if won || !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled claim was reported as a duplicate or success")
	}
	ctx, cancel = context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	won, err = store.TryClaimCronFire(ctx, "cancelled", fire)
	if err != nil || !won {
		t.Fatal("cancelled request left a durable claim")
	}
}
