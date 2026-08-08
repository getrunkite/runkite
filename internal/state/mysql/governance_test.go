package mysql_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/mysql"
	"github.com/getrunkite/runkite/internal/tenant"
)

func TestGovernance_AuditGrantPending(t *testing.T) {
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		dsn = "runkite:runkite@tcp(127.0.0.1:3307)/runkite_test?parseTime=true"
	}
	ctx := context.Background()
	store, err := mysql.New(ctx, dsn)
	if err != nil {
		t.Skipf("mysql not available: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	sys := tenant.SystemContext(ctx)

	_ = store.DeletePolicyGrant(sys, "mysql-g1")
	_ = store.DeletePolicyGrant(sys, "mysql-g2")

	ts := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	ev := &models.AuditEvent{
		ID: "mysql-aud-1", TS: ts, TenantID: "acme", Action: "tool.call",
		Decision: "deny", ReasonCode: "policy_no_grant", RunID: "r1",
		AgentID: "sales", Connector: "sf", Tool: "updateRecord",
	}
	if err := store.WriteAuditEvent(sys, ev); err != nil {
		t.Fatalf("WriteAuditEvent: %v", err)
	}
	got, err := store.SearchAuditEvents(sys, &models.AuditSearchRequest{TenantID: "acme", RunID: "r1", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range got {
		if e.ID == "mysql-aud-1" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("audit event not found in %#v", got)
	}

	g := &models.PolicyGrant{ID: "mysql-g1", TenantID: "acme", AgentID: "sales", Connector: "sf"}
	if err := store.UpsertPolicyGrant(sys, g); err != nil {
		t.Fatalf("UpsertPolicyGrant: %v", err)
	}
	dup := &models.PolicyGrant{ID: "mysql-g2", TenantID: "acme", AgentID: "sales", Connector: "sf"}
	err = store.UpsertPolicyGrant(sys, dup)
	if _, ok := err.(*state.ErrConflict); !ok {
		t.Fatalf("want ErrConflict on duplicate key, got %v", err)
	}

	a := &models.PendingAction{
		ID: "mysql-p1", RunID: "mysql-r1", Generation: 2, TenantID: "acme", AgentID: "sales",
		Connector: "sf", Tool: "delete_repo", Status: models.PendingStatusPending,
	}
	if err := store.CreatePendingAction(sys, a); err != nil {
		t.Fatalf("CreatePendingAction: %v", err)
	}
	if err := store.SetPendingActionStatus(sys, "mysql-p1", models.PendingStatusPending, models.PendingStatusApproved); err != nil {
		t.Fatal(err)
	}
	id, err := store.ConsumeApprovedAction(sys, "mysql-r1", 2, "sf", "delete_repo")
	if err != nil || id != "mysql-p1" {
		t.Fatalf("ConsumeApprovedAction: id=%q err=%v", id, err)
	}
	id, err = store.ConsumeApprovedAction(sys, "mysql-r1", 2, "sf", "delete_repo")
	if err != nil || id != "" {
		t.Fatalf("second consume want empty, got %q err=%v", id, err)
	}
}
