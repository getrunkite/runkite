package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/conformance"
	"github.com/getrunkite/runkite/internal/state/migrate"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/google/uuid"
)

func TestConnectorActionConformance(t *testing.T) {
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Skip("MYSQL_DSN is not configured")
	}
	s, err := New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	conformance.RunConnectorActions(t, s)
	conformance.RunActionDeliveries(t, s)
	conformance.RunInsertionIdentity(t, s)
}

func TestRunIncarnationCreationPaths(t *testing.T) {
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Skip("MYSQL_DSN is not configured")
	}
	s, err := New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx := tenant.WithContext(t.Context(), "incarnation-"+uuid.NewString())
	if err := s.UpsertAgent(ctx, &models.Agent{AgentID: "agent", Name: "agent"}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, method := range []string{"normal", "admitted", "cached"} {
		t.Run(method, func(t *testing.T) {
			now := time.Now().UTC()
			thread := uuid.NewString()
			if err := s.CreateThread(ctx, &models.Thread{ThreadID: thread, Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			run := &models.Run{RunID: uuid.NewString(), ThreadID: thread, AgentID: "agent", IncarnationID: "untrusted-reused-id", Status: models.RunStatusPending, CreatedAt: now, UpdatedAt: now}
			var err error
			switch method {
			case "normal":
				err = s.CreateRun(ctx, run)
			case "admitted":
				err = s.CreateRunAdmitted(ctx, run, &state.RunAdmissionCaps{TenantConcurrent: 100})
			case "cached":
				run.Status, run.Output = models.RunStatusSuccess, []byte(`{"answer":"cached"}`)
				err = s.CreateCachedRun(ctx, run, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			stored, err := s.GetRun(ctx, run.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := uuid.Parse(stored.IncarnationID); err != nil || stored.IncarnationID == "untrusted-reused-id" || seen[stored.IncarnationID] {
				t.Fatalf("incarnation was not freshly generated: %q", stored.IncarnationID)
			}
			if method != "cached" && stored.IncarnationID != run.IncarnationID {
				t.Fatal("insertion receipt differs from the committed identity")
			}
			seen[stored.IncarnationID] = true
		})
	}
}

func TestRunIncarnationMigrationBackfillAndRetry(t *testing.T) {
	s := legacyIdentityStore(t)
	ctx := tenant.WithContext(context.Background(), "incarnation-backfill")
	now := time.Now().UTC()
	if err := s.CreateThread(ctx, &models.Thread{ThreadID: "legacy-incarnation-thread", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	// Cross the migration's 256-row batch boundary, including both case variants.
	ids := []string{"RunA", "runa"}
	for i := range 257 {
		ids = append(ids, fmt.Sprintf("legacy-incarnation-%03d", i))
	}
	// Case-sensitive identities become valid only after migration 17.
	if err := s.withSchemaLock(ctx, func(ctx context.Context, conn *sql.Conn) error {
		return migrate.Upgrade(ctx, migrate.NewSQL(conn, migrate.MySQL), s.migrations(conn)[:17], nil)
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if err := insertLegacyIdentityRun(ctx, s, &models.Run{RunID: id, ThreadID: "legacy-incarnation-thread", Status: models.RunStatusPending, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	var defaultValue string
	if err := s.db.QueryRowContext(ctx, `SELECT COLUMN_DEFAULT FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='runs' AND COLUMN_NAME='incarnation_id'`).Scan(&defaultValue); err != nil || defaultValue != "" {
		t.Fatalf("incarnation must have a deterministic constant default: %q %v", defaultValue, err)
	}
	seen := map[string]bool{}
	preserved := ""
	for _, id := range ids {
		run, err := s.GetRun(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := uuid.Parse(run.IncarnationID); err != nil || seen[run.IncarnationID] {
			t.Fatalf("backfill identity invalid or duplicated: %q", run.IncarnationID)
		}
		seen[run.IncarnationID] = true
		if id == ids[0] {
			preserved = run.IncarnationID
		}
	}
	// Simulate a partially committed backfill, then retry the migration body.
	if _, err := s.db.ExecContext(ctx, `UPDATE runs SET incarnation_id='' WHERE BINARY run_id=BINARY ?`, ids[len(ids)-1]); err != nil {
		t.Fatal(err)
	}
	if err := s.withSchemaLock(ctx, func(ctx context.Context, conn *sql.Conn) error {
		return s.upConnectorActions(ctx, conn)
	}); err != nil {
		t.Fatal(err)
	}
	first, err := s.GetRun(ctx, ids[0])
	if err != nil || first.IncarnationID != preserved {
		t.Fatalf("retry replaced existing identity: %+v %v", first, err)
	}
	last, err := s.GetRun(ctx, ids[len(ids)-1])
	if err != nil || last.IncarnationID == "" {
		t.Fatalf("retry did not finish incomplete backfill: %+v %v", last, err)
	}
}
