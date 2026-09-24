package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

// CountRunsSince's simulation-exclude predicate is a Postgres JSONB ->>
// fragment, distinct from SQLite's json_extract -- only SQLite had a test
// for this before. Confirms the same three-row shape (missing key / bool
// true / string "true") behaves identically on real JSONB semantics.
func TestCountRunsSince_ExcludesSimulation(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set")
	}
	s, err := New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.TruncateAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := tenant.WithContext(context.Background(), "sim-verify-count")
	now := time.Now().UTC()
	if err := s.CreateThread(ctx, &models.Thread{ThreadID: "simct1", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		id   string
		meta map[string]interface{}
	}{
		{"sim-missing", map[string]interface{}{"other": "x"}},
		{"sim-bool", map[string]interface{}{"simulation": true}},
		{"sim-str", map[string]interface{}{"simulation": "true"}},
		{"sim-false", map[string]interface{}{"simulation": false}},
	}
	for _, row := range rows {
		if err := s.CreateRun(ctx, &models.Run{
			RunID: row.id, ThreadID: "simct1", AgentID: "echo", Status: models.RunStatusPending,
			Metadata: row.meta, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.CountRunsSince(context.Background(), "sim-verify-count", "echo", now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("CountRunsSince = %d, want 2 (missing-key + explicit false)", n)
	}
}

// CreateRunAdmitted's countSince closure is a separate code path from
// CountRunsSince (different function, same SQL fragment appended) -- this
// exercises the actual daily-admission decision a simulation run interacts
// with, not just the reporting query.
func TestCreateRunAdmitted_DailyExcludesSimulation(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set")
	}
	s, err := New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.TruncateAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := tenant.WithContext(context.Background(), "sim-verify-admit")
	now := time.Now().UTC()
	if err := s.CreateThread(ctx, &models.Thread{ThreadID: "simat1", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	caps := &state.RunAdmissionCaps{AgentDaily: 1, Now: now}

	// Three sim creates against a daily cap of 1 -- none should trip it.
	for i := 0; i < 3; i++ {
		run := &models.Run{
			RunID: "sim-admit-sim-" + string(rune('a'+i)), ThreadID: "simat1", AgentID: "echo",
			Status: models.RunStatusPending, Metadata: map[string]interface{}{"simulation": true},
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateRunAdmitted(ctx, run, caps); err != nil {
			t.Fatalf("sim create %d should not hit daily cap: %v", i, err)
		}
	}

	real1 := &models.Run{RunID: "sim-admit-real-1", ThreadID: "simat1", AgentID: "echo", Status: models.RunStatusPending, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateRunAdmitted(ctx, real1, caps); err != nil {
		t.Fatalf("first real create should be admitted: %v", err)
	}

	real2 := &models.Run{RunID: "sim-admit-real-2", ThreadID: "simat1", AgentID: "echo", Status: models.RunStatusPending, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}
	err = s.CreateRunAdmitted(ctx, real2, caps)
	if err == nil {
		t.Fatal("second real create should be denied by daily cap")
	}
	if _, ok := err.(*state.ErrAdmissionLimitExceeded); !ok {
		t.Fatalf("want ErrAdmissionLimitExceeded, got %v (%T)", err, err)
	}
}
