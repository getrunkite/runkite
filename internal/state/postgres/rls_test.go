package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
)

// TestRLS_MissingAppFilterStillIsolates proves that with WithRLS(true), a
// SELECT that forgets WHERE tenant_id still cannot read another tenant's
// rows — Postgres RLS is the safety net under the application filter.
func TestRLS_MissingAppFilterStillIsolates(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set — skipping Postgres RLS test")
	}

	ctx := context.Background()
	s, err := New(ctx, dsn, WithRLS(true))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := s.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	sys := tenant.SystemContext(ctx)
	if err := s.TruncateAll(sys); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	now := time.Now().UTC()
	acme := tenant.WithContext(ctx, "acme")
	thread := &models.Thread{
		ThreadID:  "rls-thread-acme",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.CreateThread(acme, thread); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	run := &models.Run{
		RunID:     "rls-run-acme",
		ThreadID:  thread.ThreadID,
		AgentID:   "agent-1",
		Status:    models.RunStatusPending,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.CreateRun(acme, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Adversarial: query with beta context and NO tenant_id predicate.
	beta := tenant.WithContext(ctx, "beta")
	var n int
	err = s.pool.QueryRow(beta,
		`SELECT COUNT(*) FROM runs WHERE run_id = $1`, run.RunID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("adversarial count: %v", err)
	}
	if n != 0 {
		t.Fatalf("RLS failed: beta saw %d row(s) for acme run without app tenant filter", n)
	}

	err = s.pool.QueryRow(sys,
		`SELECT COUNT(*) FROM runs WHERE run_id = $1`, run.RunID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("system count: %v", err)
	}
	if n != 1 {
		t.Fatalf("system context should see acme run, got count=%d", n)
	}

	err = s.pool.QueryRow(acme,
		`SELECT COUNT(*) FROM runs WHERE run_id = $1`, run.RunID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("acme count: %v", err)
	}
	if n != 1 {
		t.Fatalf("acme should see own run via RLS, got count=%d", n)
	}
}
