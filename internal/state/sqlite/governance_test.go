package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

func TestGovernance_AuditGrantPending(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := tenant.SystemContext(context.Background())
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}

	ts := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	ev := &models.AuditEvent{
		ID: "aud-1", TS: ts, TenantID: "acme", Action: "tool.call",
		Decision: "deny", ReasonCode: "policy_no_grant", RunID: "r1",
		AgentID: "sales", Connector: "sf", Tool: "updateRecord",
	}
	if err := s.WriteAuditEvent(ctx, ev); err != nil {
		t.Fatalf("WriteAuditEvent: %v", err)
	}
	got, err := s.SearchAuditEvents(ctx, &models.AuditSearchRequest{TenantID: "acme", Limit: 10})
	if err != nil || len(got) != 1 || got[0].ID != "aud-1" {
		t.Fatalf("SearchAuditEvents: %#v err=%v", got, err)
	}

	g := &models.PolicyGrant{ID: "g1", TenantID: "acme", AgentID: "sales", Connector: "sf"}
	if err := s.UpsertPolicyGrant(ctx, g); err != nil {
		t.Fatalf("UpsertPolicyGrant: %v", err)
	}
	dup := &models.PolicyGrant{ID: "g2", TenantID: "acme", AgentID: "sales", Connector: "sf"}
	err = s.UpsertPolicyGrant(ctx, dup)
	var conflict *state.ErrConflict
	if !errorsAsConflict(err, &conflict) {
		t.Fatalf("want ErrConflict on duplicate key, got %v", err)
	}

	a := &models.PendingAction{
		ID: "p1", RunID: "r1", Generation: 2, TenantID: "acme", AgentID: "sales",
		Connector: "sf", Tool: "delete_repo", Status: models.PendingStatusPending,
	}
	if err := s.CreatePendingAction(ctx, a); err != nil {
		t.Fatalf("CreatePendingAction: %v", err)
	}
	if err := s.SetPendingActionStatus(ctx, "p1", models.PendingStatusPending, models.PendingStatusApproved); err != nil {
		t.Fatal(err)
	}
	id, err := s.ConsumeApprovedAction(ctx, "r1", 2, "sf", "delete_repo")
	if err != nil || id != "p1" {
		t.Fatalf("ConsumeApprovedAction: id=%q err=%v", id, err)
	}
	id, err = s.ConsumeApprovedAction(ctx, "r1", 2, "sf", "delete_repo")
	if err != nil || id != "" {
		t.Fatalf("second consume want empty, got %q err=%v", id, err)
	}
}

func errorsAsConflict(err error, target **state.ErrConflict) bool {
	if err == nil {
		return false
	}
	c, ok := err.(*state.ErrConflict)
	if !ok {
		return false
	}
	*target = c
	return true
}
