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

// TestRLS_TableListMatchesTenantColumns keeps rlsTables aligned with every
// public table that has a tenant_id column (minus known intentional skips).
//
// rlsTables may list tables created outside this package (e.g. vector_items
// from pgvector Init). Those are optional here: if the table is absent,
// ensureRLS already skips it. If it is present (CI runs pgvector + postgres
// against one shared DSN), it must be listed — otherwise FORCE RLS never
// covers it after a full deploy that enables vectors.
func TestRLS_TableListMatchesTenantColumns(t *testing.T) {
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
	rows, err := s.pool.Query(sys, `
		SELECT table_name FROM information_schema.columns
		WHERE table_schema = 'public' AND column_name = 'tenant_id'
		ORDER BY 1`)
	if err != nil {
		t.Fatalf("information_schema: %v", err)
	}
	defer rows.Close()

	// Tables with tenant_id that we intentionally do not FORCE-RLS
	// (document here if adding another exception).
	skip := map[string]bool{
		// none today — terminal_hook_claims has no tenant_id
	}
	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if skip[name] {
			continue
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{}
	for _, name := range rlsTables {
		want[name] = true
		// Listed but not present: optional (created by another package).
		// Present without tenant_id would be a schema bug — have[] only
		// contains tenant_id tables, so a listed existing table missing
		// from have means the column disappeared.
		if !have[name] {
			var exists bool
			err := s.pool.QueryRow(sys,
				`SELECT EXISTS (
					SELECT 1 FROM information_schema.tables
					WHERE table_schema = 'public' AND table_name = $1
				)`, name,
			).Scan(&exists)
			if err != nil {
				t.Fatalf("exists %s: %v", name, err)
			}
			if exists {
				t.Errorf("rlsTables lists %q which exists but has no tenant_id column", name)
			}
			continue
		}
	}
	for name := range have {
		if !want[name] {
			t.Errorf("table %q has tenant_id but is missing from rlsTables", name)
		}
	}
}

// TestRLS_DisableClearsStickyFORCE proves flipping WithRLS(false) on Init
// drops FORCE so a non-BYPASS login is not left deny-all after opt-out.
func TestRLS_DisableClearsStickyFORCE(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set — skipping Postgres RLS test")
	}

	ctx := context.Background()
	on, err := New(ctx, dsn, WithRLS(true))
	if err != nil {
		t.Fatalf("connect on: %v", err)
	}
	if err := on.Init(ctx); err != nil {
		t.Fatalf("init on: %v", err)
	}
	_ = on.Close()

	off, err := New(ctx, dsn, WithRLS(false))
	if err != nil {
		t.Fatalf("connect off: %v", err)
	}
	t.Cleanup(func() { _ = off.Close() })
	if err := off.Init(ctx); err != nil {
		t.Fatalf("init off: %v", err)
	}

	sys := tenant.SystemContext(ctx)
	var n int
	err = off.pool.QueryRow(sys,
		`SELECT COUNT(*) FROM pg_policies WHERE policyname = 'runkite_tenant_isolation'`,
	).Scan(&n)
	if err != nil {
		t.Fatalf("count policies: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 runkite policies after disable, got %d", n)
	}

	err = off.pool.QueryRow(sys, `
		SELECT COUNT(*) FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relname = 'runs' AND c.relforcerowsecurity`,
	).Scan(&n)
	if err != nil {
		t.Fatalf("force check: %v", err)
	}
	if n != 0 {
		t.Fatalf("runs still has FORCE ROW LEVEL SECURITY after disable")
	}
}
