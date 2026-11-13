package sqlite

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state/migrate"
	"github.com/getrunkite/runkite/internal/tenant"
)

func TestTargetRevisionMigrationPreservesHistoryWithoutAuthority(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := tenant.SystemContext(context.Background())
	bk := migrate.NewSQL(s.db, migrate.SQLite)
	if err := migrate.Upgrade(ctx, bk, s.migrations()[:17], nil); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{models.PendingStatusPending, models.PendingStatusApproved} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO pending_actions(id,run_id,tenant_id,agent_id,connector,tool,status,args_digest,args,run_incarnation_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, "old-"+status, "old-run", "acme", "agent", "files", "write", status, "digest", `{"amount":123}`, "original-incarnation", formatTS(time.Now().UTC()), formatTS(time.Now().UTC())); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{models.PendingStatusPending, models.PendingStatusApproved} {
		row, err := s.GetPendingAction(ctx, "old-"+status)
		if err != nil || row.TargetRevision != "" || row.RunIncarnationID != "original-incarnation" || row.Status != status || row.ArgsDigest != "digest" {
			t.Fatalf("migration guessed or changed historical authority: %+v %v", row, err)
		}
	}
	if err := s.SetPendingActionApproved(ctx, "old-pending", "reviewer"); err == nil {
		t.Fatal("migration silently authorized old target")
	}
	if id, err := s.ConsumeApprovedAction(ctx, "acme", "agent", "old-run", 0, "files", "write", "digest", testPendingTargetRevision); err != nil || id != "" {
		t.Fatal("historical target was consumable")
	}
	if err := s.Downgrade(ctx); err == nil || !strings.Contains(err.Error(), "backup") {
		t.Fatalf("unsafe downgrade accepted: %v", err)
	}
	if version, err := bk.Current(ctx); err != nil || version != 18 {
		t.Fatalf("refused downgrade changed version: %d %v", version, err)
	}
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
}
