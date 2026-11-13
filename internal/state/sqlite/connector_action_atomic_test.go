package sqlite

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/google/uuid"
)

func TestConnectorActionEventFailureRollsBackApprovalAndSummary(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := tenant.WithContext(context.Background(), "acme")
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.UpsertAgent(ctx, &models.Agent{AgentID: "agent", Name: "agent", Metadata: map[string]interface{}{}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateThread(ctx, &models.Thread{ThreadID: "thread", Status: models.ThreadStatusBusy, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, &models.Run{RunID: "run", ThreadID: "thread", AgentID: "agent", Status: models.RunStatusPending, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginRunAttempt(ctx, "run", 1); err != nil {
		t.Fatal(err)
	}
	revision := testPendingTargetRevision
	a, err := s.CreateConnectorAction(ctx, &models.ConnectorAction{TargetRevision: &revision, ID: uuid.NewString(), TenantID: "acme", RunID: "run", ThreadID: "thread", Generation: 1, AgentID: "agent", Connector: "fixture", Tool: "write", ArgsDigest: strings.Repeat("a", 64)}, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_action_decision BEFORE INSERT ON connector_action_events WHEN NEW.kind='decision' BEGIN SELECT RAISE(ABORT,'fixture event failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordConnectorActionDecision(ctx, a.ID, a.Version, models.ConnectorActionDecision{Effect: "pending", AuthorizationSource: "policy"}, uuid.NewString()); err == nil {
		t.Fatal("event insert unexpectedly succeeded")
	}
	unchanged, err := s.GetConnectorAction(ctx, a.ID)
	if err != nil || unchanged.State != models.ConnectorActionReceived || unchanged.Version != 1 {
		t.Fatalf("summary escaped rolled back transaction: %+v %v", unchanged, err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TRIGGER reject_action_decision`); err != nil {
		t.Fatal(err)
	}
	a, err = s.RecordConnectorActionDecision(ctx, a.ID, a.Version, models.ConnectorActionDecision{Effect: "pending", AuthorizationSource: "policy"}, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	p := &models.PendingAction{TargetRevision: testPendingTargetRevision, ID: "pending-v2-" + uuid.NewString(), TenantID: "acme", RunID: "run", Generation: 1, AgentID: "agent", Connector: "fixture", Tool: "write", ArgsDigest: a.ArgsDigest, Status: models.PendingStatusPending}
	if err := s.CreatePendingAction(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPendingActionApproved(ctx, p.ID, "reviewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_action_dispatch BEFORE INSERT ON connector_action_events WHEN NEW.kind='dispatch_intent' BEGIN SELECT RAISE(ABORT,'fixture event failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareConnectorActionDispatch(ctx, a.ID, a.Version, p.ID, "approved_capability", time.Now().Add(time.Minute), uuid.NewString()); err == nil {
		t.Fatal("dispatch event insert unexpectedly succeeded")
	}
	unchanged, err = s.GetConnectorAction(ctx, a.ID)
	if err != nil || unchanged.State != models.ConnectorActionApprovalRequired || unchanged.Version != 2 || unchanged.DispatchID != "" {
		t.Fatalf("dispatch summary escaped rollback: %+v %v", unchanged, err)
	}
	pending, err := s.GetPendingAction(ctx, p.ID)
	if err != nil || pending.Status != models.PendingStatusApproved {
		t.Fatalf("approval escaped rollback: %+v %v", pending, err)
	}
	events, err := s.ListConnectorActionEvents(ctx, a.ID, 0, 100)
	if err != nil || len(events) != 2 {
		t.Fatalf("partial timeline: %+v %v", events, err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TRIGGER reject_action_dispatch`); err != nil {
		t.Fatal(err)
	}
	claim, err := s.PrepareConnectorActionDispatch(ctx, a.ID, a.Version, p.ID, "approved_capability", time.Now().Add(time.Minute), uuid.NewString())
	if err != nil || !claim.Applied {
		t.Fatalf("recovery: %+v %v", claim, err)
	}
}
