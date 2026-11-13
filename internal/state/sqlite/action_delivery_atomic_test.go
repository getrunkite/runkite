package sqlite

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/google/uuid"
)

func TestActionOutboxFailureRollsBackDispatchAndApproval(t *testing.T) {
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
	if err := s.CreateThread(ctx, &models.Thread{ThreadID: "thread", Status: models.ThreadStatusBusy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, &models.Run{RunID: "run", ThreadID: "thread", AgentID: "agent", Status: models.RunStatusPending, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginRunAttempt(ctx, "run", 1); err != nil {
		t.Fatal(err)
	}
	target, policyRevision := testPendingTargetRevision, testPendingPolicyRevision
	input := &models.ConnectorAction{ID: uuid.NewString(), ExportSinkID: "siem.primary", TenantID: "acme", AgentID: "agent", RunID: "run", ThreadID: "thread", Generation: 1, Connector: "fixture", Tool: "write", ArgsDigest: strings.Repeat("a", 64), TargetRevision: &target, PolicyRevision: &policyRevision}
	a, err := s.CreateConnectorAction(ctx, input, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	a, err = s.RecordConnectorActionDecision(ctx, a.ID, a.Version, models.ConnectorActionDecision{Effect: "pending", AuthorizationSource: "policy"}, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	pending := &models.PendingAction{ID: uuid.NewString(), TenantID: "acme", AgentID: "agent", RunID: "run", Generation: 1, Connector: "fixture", Tool: "write", ArgsDigest: input.ArgsDigest, TargetRevision: target, PolicyRevision: policyRevision, RequiredApprovals: 2}
	if err := s.CreatePendingAction(ctx, pending); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []string{"one", "two"} {
		if err := s.SetPendingActionApproved(ctx, pending.ID, actor); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_outbox_intent BEFORE INSERT ON action_deliveries WHEN NEW.sequence=3 BEGIN SELECT RAISE(ABORT,'fixture outbox insert failure'); END`); err != nil {
		t.Fatal(err)
	}
	mutation := uuid.NewString()
	deadline := time.Now().Add(time.Minute)
	if _, err := s.PrepareConnectorActionDispatch(ctx, a.ID, a.Version, pending.ID, "approved_capability", deadline, mutation); err == nil {
		t.Fatal("dispatch accepted failed outbox write")
	}
	unchanged, err := s.GetConnectorAction(ctx, a.ID)
	if err != nil || unchanged.Version != 2 || unchanged.State != models.ConnectorActionApprovalRequired || unchanged.DispatchID != "" {
		t.Fatalf("action escaped outbox rollback: %+v %v", unchanged, err)
	}
	p, err := s.GetPendingAction(ctx, pending.ID)
	if err != nil || p.Status != models.PendingStatusApproved || p.ApprovalCount != 2 {
		t.Fatalf("approval burned on outbox failure: %+v %v", p, err)
	}
	events, err := s.ListConnectorActionEvents(ctx, a.ID, 0, 100)
	if err != nil || len(events) != 2 {
		t.Fatal("event escaped outbox rollback")
	}
	rows, err := s.SearchActionDeliveries(ctx, nil)
	if err != nil || len(rows) != 2 {
		t.Fatal("partial outbox escaped transaction")
	}
	if _, err := s.db.ExecContext(ctx, `DROP TRIGGER reject_outbox_intent`); err != nil {
		t.Fatal(err)
	}
	receipt, err := s.PrepareConnectorActionDispatch(ctx, a.ID, a.Version, pending.ID, "approved_capability", deadline, mutation)
	if err != nil || !receipt.Applied {
		t.Fatalf("safe persistence recovery: %+v %v", receipt, err)
	}
	replay, err := s.PrepareConnectorActionDispatch(ctx, a.ID, a.Version, pending.ID, "approved_capability", deadline, mutation)
	if err != nil || replay.Applied || replay.DispatchID != receipt.DispatchID {
		t.Fatal("persistence replay gained effect authority")
	}
	rows, err = s.SearchActionDeliveries(ctx, &state.ActionDeliverySearchRequest{SinkID: input.ExportSinkID})
	if err != nil || len(rows) != 3 {
		t.Fatal("mutation replay duplicated outbox delivery")
	}
	// A failed outbox create also leaves no action or event to be mistaken for
	// accepted evidence. Existing rows remain available for investigation.
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_outbox_create BEFORE INSERT ON action_deliveries WHEN NEW.sequence=1 BEGIN SELECT RAISE(ABORT,'fixture outbox create failure'); END`); err != nil {
		t.Fatal(err)
	}
	input.ID = uuid.NewString()
	if _, err := s.CreateConnectorAction(ctx, input, uuid.NewString()); err == nil {
		t.Fatal("action accepted failed initial outbox write")
	}
	if _, err := s.GetConnectorAction(ctx, input.ID); err == nil {
		t.Fatal("failed outbox action survived rollback")
	}
}
