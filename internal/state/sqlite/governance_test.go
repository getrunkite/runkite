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
	id, err := s.ConsumeApprovedAction(ctx, "r1", 2, "sf", "delete_repo", "")
	if err != nil || id != "p1" {
		t.Fatalf("ConsumeApprovedAction: id=%q err=%v", id, err)
	}
	id, err = s.ConsumeApprovedAction(ctx, "r1", 2, "sf", "delete_repo", "")
	if err != nil || id != "" {
		t.Fatalf("second consume want empty, got %q err=%v", id, err)
	}
}

func TestPendingAction_ArgsDigestBound(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := tenant.SystemContext(context.Background())
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}

	low := &models.PendingAction{
		ID: "p-low", RunID: "r-digest", Generation: 1, TenantID: "acme", AgentID: "payroll",
		Connector: "bank", Tool: "transfer", Status: models.PendingStatusPending,
		ArgsDigest: "digest-500", Args: map[string]any{"amount": float64(500)},
	}
	high := &models.PendingAction{
		ID: "p-high", RunID: "r-digest", Generation: 1, TenantID: "acme", AgentID: "payroll",
		Connector: "bank", Tool: "transfer", Status: models.PendingStatusPending,
		ArgsDigest: "digest-5m", Args: map[string]any{"amount": float64(5_000_000)},
	}
	if err := s.CreatePendingAction(ctx, low); err != nil {
		t.Fatal(err)
	}
	if err := s.CreatePendingAction(ctx, high); err != nil {
		t.Fatal(err)
	}

	got, err := s.FindOpenPendingAction(ctx, "r-digest", 1, "bank", "transfer", "digest-500")
	if err != nil || got == nil || got.ID != "p-low" {
		t.Fatalf("FindOpen low: %#v err=%v", got, err)
	}
	got, err = s.FindOpenPendingAction(ctx, "r-digest", 1, "bank", "transfer", "digest-5m")
	if err != nil || got == nil || got.ID != "p-high" {
		t.Fatalf("FindOpen high: %#v err=%v", got, err)
	}
	got, err = s.FindOpenPendingAction(ctx, "r-digest", 1, "bank", "transfer", "digest-other")
	if err != nil || got != nil {
		t.Fatalf("FindOpen other want nil, got %#v err=%v", got, err)
	}

	if err := s.SetPendingActionApproved(ctx, "p-low", "admin@acme"); err != nil {
		t.Fatal(err)
	}
	approved, err := s.GetPendingAction(ctx, "p-low")
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != models.PendingStatusApproved || approved.DecidedBy != "admin@acme" {
		t.Fatalf("approved row: %+v", approved)
	}

	id, err := s.ConsumeApprovedAction(ctx, "r-digest", 1, "bank", "transfer", "digest-5m")
	if err != nil || id != "" {
		t.Fatalf("consume 5m against 500-approved want empty, got %q err=%v", id, err)
	}
	still, err := s.GetPendingAction(ctx, "p-low")
	if err != nil || still.Status != models.PendingStatusApproved {
		t.Fatalf("500 row must stay approved, got %+v err=%v", still, err)
	}

	id, err = s.ConsumeApprovedAction(ctx, "r-digest", 1, "bank", "transfer", "digest-500")
	if err != nil || id != "p-low" {
		t.Fatalf("consume 500: id=%q err=%v", id, err)
	}
	highRow, err := s.GetPendingAction(ctx, "p-high")
	if err != nil || highRow.Status != models.PendingStatusPending {
		t.Fatalf("high row must stay pending, got %+v err=%v", highRow, err)
	}

	legacy := &models.PendingAction{
		ID: "p-legacy", RunID: "r-legacy", Generation: 1, TenantID: "acme", AgentID: "payroll",
		Connector: "bank", Tool: "transfer", Status: models.PendingStatusPending,
	}
	if err := s.CreatePendingAction(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPendingActionStatus(ctx, "p-legacy", models.PendingStatusPending, models.PendingStatusApproved); err != nil {
		t.Fatal(err)
	}
	id, err = s.ConsumeApprovedAction(ctx, "r-legacy", 1, "bank", "transfer", "digest-500")
	if err != nil || id != "p-legacy" {
		t.Fatalf("legacy empty digest should consume once, id=%q err=%v", id, err)
	}
	id, err = s.ConsumeApprovedAction(ctx, "r-legacy", 1, "bank", "transfer", "digest-500")
	if err != nil || id != "" {
		t.Fatalf("legacy second consume want empty, got %q err=%v", id, err)
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
