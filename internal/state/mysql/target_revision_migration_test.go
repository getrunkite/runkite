package mysql

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state/migrate"
	"github.com/getrunkite/runkite/internal/tenant"
)

func TestTargetRevisionMigrationPreservesHistoryWithoutAuthority(t *testing.T) {
	s := legacyIdentityStore(t)
	ctx := tenant.SystemContext(context.Background())
	err := s.withSchemaLock(ctx, func(ctx context.Context, conn *sql.Conn) error {
		return migrate.Upgrade(ctx, migrate.NewSQL(conn, migrate.MySQL), s.migrations(conn)[:18], nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{models.PendingStatusPending, models.PendingStatusApproved} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO pending_actions(id,run_id,tenant_id,agent_id,connector,tool,status,args_digest,args,run_incarnation_id) VALUES(?,?,?,?,?,?,?,?,?,?)`, "old-"+status, "old-run", "acme", "agent", "files", "write", status, "digest", `{"amount":123}`, "original-incarnation"); err != nil {
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
	if id, err := s.ConsumeApprovedAction(ctx, "acme", "agent", "old-run", 0, "files", "write", "digest", "target-v1-"+strings.Repeat("a", 64)); err != nil || id != "" {
		t.Fatal("historical target was consumed")
	}
	if err := s.Downgrade(ctx); err == nil || !strings.Contains(err.Error(), "backup") {
		t.Fatalf("unsafe downgrade: %v", err)
	}
	if version, err := migrate.NewSQL(s.db, migrate.MySQL).Current(ctx); err != nil || version != 19 {
		t.Fatalf("refused downgrade changed version: %d %v", version, err)
	}
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
}
