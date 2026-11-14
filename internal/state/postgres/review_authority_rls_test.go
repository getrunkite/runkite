package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/conformance"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/google/uuid"
)

func TestRLS_ApprovalHistoryAndActionDeliveries(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN is not configured")
	}
	s, err := New(t.Context(), dsn, WithRLS(true))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	conformance.RunPendingReviewSecurity(t, s)
	conformance.RunActionDeliveries(t, s)
	ctx := tenant.ScopedContext(context.Background(), "review-rls-"+uuid.NewString())
	system := tenant.SystemContext(ctx)
	foreign := tenant.ScopedContext(ctx, tenant.FromContext(ctx)+"-foreign")
	expires := time.Now().UTC().Add(time.Hour)
	p := &models.PendingAction{ID: uuid.NewString(), TenantID: tenant.FromContext(ctx), AgentID: "agent", RunID: "app-owned", Connector: "fixture", Tool: "write", ArgsDigest: "digest", ExpiresAt: &expires, RequiredApprovals: 2, RequesterIdentity: "requester", ExcludeRequester: true}
	if err := s.CreatePendingAction(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPendingActionApproved(ctx, p.ID, "reviewer"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ctx  context.Context
		want int
	}{{ctx, 1}, {system, 1}, {foreign, 0}} {
		var count int
		if err := s.pool.QueryRow(tc.ctx, `SELECT COUNT(*) FROM pending_action_approvals WHERE pending_action_id=$1`, p.ID).Scan(&count); err != nil || count != tc.want {
			t.Fatalf("unfiltered vote-history RLS count=%d want=%d err=%v", count, tc.want, err)
		}
	}
	// Outbox rows deliberately do not depend on the continued existence of an
	// action. This direct fixture qualifies database RLS without an app predicate.
	id := uuid.NewString()
	now := time.Now().UTC()
	if _, err := s.pool.Exec(ctx, `INSERT INTO action_deliveries(id,sink_id,tenant_id,action_id,sequence,payload,status,available_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$8,$8)`, id, "rls-fixture", tenant.FromContext(ctx), uuid.NewString(), 1, []byte(`{"schema":"rls-fixture"}`), state.ActionDeliveryPending, now); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ctx  context.Context
		want int
	}{{ctx, 1}, {system, 1}, {foreign, 0}} {
		var count int
		if err := s.pool.QueryRow(tc.ctx, `SELECT COUNT(*) FROM action_deliveries WHERE id=$1`, id).Scan(&count); err != nil || count != tc.want {
			t.Fatalf("unfiltered delivery RLS count=%d want=%d err=%v", count, tc.want, err)
		}
	}
}
