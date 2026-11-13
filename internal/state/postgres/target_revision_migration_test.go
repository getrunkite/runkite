package postgres

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state/migrate"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Like audit migration tests, this fixture owns POSTGRES_DSN's dedicated test
// schema. Build the historical schema explicitly; never erase a migration stamp
// while leaving newer authority-bearing columns installed.
func TestTargetRevisionMigrationPreservesHistoryWithoutAuthority(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not configured")
	}
	ctx := tenant.SystemContext(context.Background())
	s, err := New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = s.withSchemaLock(ctx, func(ctx context.Context, conn *pgxpool.Conn) error {
		if err := s.dropSchemaLocked(ctx, conn); err != nil {
			return err
		}
		if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS schema_migrations`); err != nil {
			return err
		}
		return migrate.Upgrade(ctx, migrate.NewPgx(conn), s.migrations(conn)[:17], nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{models.PendingStatusPending, models.PendingStatusApproved} {
		if _, err := s.pool.Exec(ctx, `INSERT INTO pending_actions(id,run_id,tenant_id,agent_id,connector,tool,status,args_digest,args,run_incarnation_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, "old-"+status, "old-run", "acme", "agent", "files", "write", status, "digest", []byte(`{"amount":123}`), "original-incarnation"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{models.PendingStatusPending, models.PendingStatusApproved} {
		row, err := s.GetPendingAction(ctx, "old-"+status)
		if err != nil || row.TargetRevision != "" || row.RunIncarnationID != "original-incarnation" || row.Status != status || row.ArgsDigest != "digest" {
			t.Fatalf("migration guessed or changed authority: %+v %v", row, err)
		}
	}
	if err := s.SetPendingActionApproved(ctx, "old-pending", "reviewer"); err == nil {
		t.Fatal("historical target could be approved")
	}
	if id, err := s.ConsumeApprovedAction(ctx, "acme", "agent", "old-run", 0, "files", "write", "digest", testPendingTargetRevision); err != nil || id != "" {
		t.Fatal("historical target was consumed")
	}
	if err := s.Downgrade(ctx); err == nil || !strings.Contains(err.Error(), "backup") {
		t.Fatalf("unsafe downgrade: %v", err)
	}
	if version, err := migrate.NewPgx(s.pool).Current(ctx); err != nil || version != 18 {
		t.Fatalf("refused downgrade changed version: %d %v", version, err)
	}
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
}
