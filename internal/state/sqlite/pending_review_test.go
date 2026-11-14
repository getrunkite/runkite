package sqlite

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state/migrate"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/google/uuid"
)

func TestPendingReviewMigrationPreservesHistoryAndRequiresFreshAuthority(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := tenant.SystemContext(context.Background())
	bk := migrate.NewSQL(s.db, migrate.SQLite)
	if err := migrate.Upgrade(ctx, bk, s.migrations()[:18], nil); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{models.PendingStatusPending, models.PendingStatusApproved} {
		_, err := s.db.ExecContext(ctx, `INSERT INTO pending_actions(id,run_id,tenant_id,agent_id,connector,tool,status,args_digest,args,run_incarnation_id,target_revision,decided_by,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, "old-"+status, "old-run", "acme", "agent", "files", "write", status, "digest", `{"amount":123}`, "old-incarnation", testPendingTargetRevision, "historical-reviewer", formatTS(time.Now().UTC()), formatTS(time.Now().UTC()))
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{models.PendingStatusPending, models.PendingStatusApproved} {
		row, err := s.GetPendingAction(ctx, "old-"+status)
		if err != nil {
			t.Fatal(err)
		}
		if row.Status != status || row.DecidedBy != "historical-reviewer" || row.PolicyRevision != "" || row.RequesterIdentity != "" || row.RequiredApprovals != 1 || row.ApprovalCount != 0 || len(row.Approvals) != 0 {
			t.Fatalf("migration fabricated authority: %+v", row)
		}
	}
	if err := s.SetPendingActionApproved(ctx, "old-pending", "reviewer"); err == nil {
		t.Fatal("old target-only approval re-authorized")
	}
	if id, err := s.ConsumeApprovedAction(ctx, "acme", "agent", "old-run", 0, "files", "write", "digest", testPendingTargetRevision, testPendingPolicyRevision, ""); err != nil || id != "" {
		t.Fatal("old target-only approval consumed")
	}
	if err := s.Downgrade(ctx); err == nil || !strings.Contains(err.Error(), "backup") {
		t.Fatalf("unsafe downgrade accepted: %v", err)
	}
	if version, err := bk.Current(ctx); err != nil || version != s.migrations()[len(s.migrations())-1].Version {
		t.Fatal("failed downgrade changed schema version")
	}
}

func TestPendingVoteFailureRollsBackAndStatusAloneCannotDispatch(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := tenant.ScopedContext(context.Background(), "acme")
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.UpsertAgent(ctx, &models.Agent{AgentID: "agent", Name: "agent"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateThread(ctx, &models.Thread{ThreadID: "thread", Status: models.ThreadStatusBusy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, &models.Run{RunID: "run", ThreadID: "thread", AgentID: "agent", Status: models.RunStatusPending, Metadata: map[string]interface{}{"run_manifest": map[string]interface{}{"principal": map[string]interface{}{"identity": "requester", "verified_identity": true}}}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginRunAttempt(ctx, "run", 1); err != nil {
		t.Fatal(err)
	}
	row := &models.PendingAction{ID: uuid.NewString(), TenantID: "acme", AgentID: "agent", RunID: "run", Generation: 1, Connector: "fixture", Tool: "write", ArgsDigest: strings.Repeat("a", 64), TargetRevision: testPendingTargetRevision, PolicyRevision: testPendingPolicyRevision, RequiredApprovals: 2, ExcludeRequester: true}
	if err := s.CreatePendingAction(ctx, row); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_vote_completion BEFORE UPDATE ON pending_actions WHEN NEW.status='approved' BEGIN SELECT RAISE(ABORT,'fixture vote commit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPendingActionApproved(ctx, row.ID, "one"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPendingActionApproved(ctx, row.ID, "two"); err == nil {
		t.Fatal("vote completion failure accepted")
	}
	got, err := s.GetPendingAction(ctx, row.ID)
	if err != nil || got.Status != models.PendingStatusPending || got.ApprovalCount != 1 {
		t.Fatalf("failed vote escaped transaction: %+v %v", got, err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TRIGGER reject_vote_completion`); err != nil {
		t.Fatal(err)
	}
	// A caller using the old generic status method cannot manufacture quorum.
	if err := s.SetPendingActionStatus(ctx, row.ID, models.PendingStatusPending, models.PendingStatusApproved); err != nil {
		t.Fatal(err)
	}
	if id, err := s.ConsumeApprovedAction(ctx, "acme", "agent", "run", 1, "fixture", "write", row.ArgsDigest, testPendingTargetRevision, testPendingPolicyRevision, ""); err != nil || id != "" {
		t.Fatalf("status-only legacy dispatch acquired authority: %q %v", id, err)
	}
	target, policyRevision := testPendingTargetRevision, testPendingPolicyRevision
	a, err := s.CreateConnectorAction(ctx, &models.ConnectorAction{ID: uuid.NewString(), TenantID: "acme", AgentID: "agent", RunID: "run", ThreadID: "thread", Generation: 1, Connector: "fixture", Tool: "write", ArgsDigest: row.ArgsDigest, TargetRevision: &target, PolicyRevision: &policyRevision}, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	a, err = s.RecordConnectorActionDecision(ctx, a.ID, a.Version, models.ConnectorActionDecision{Effect: "pending", AuthorizationSource: "policy"}, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if p, err := s.PeekApprovedConnectorAction(ctx, a.ID); err != nil || p != nil {
		t.Fatalf("status-only peek acquired authority: %+v %v", p, err)
	}
	if _, err := s.PrepareConnectorActionDispatch(ctx, a.ID, a.Version, row.ID, "approved_capability", time.Now().Add(time.Minute), uuid.NewString()); err == nil {
		t.Fatal("status-only recorded dispatch acquired authority")
	}
}
