package conformance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/google/uuid"
)

type pendingScopeStore interface {
	PendingStore
	SetPendingActionStatus(context.Context, string, string, string) error
}

// RunPendingTenantScope tests the actual SQL predicates, including mutation
// failures. A request filter cannot turn a scoped context into global authority.
func RunPendingTenantScope(t *testing.T, s pendingScopeStore) {
	t.Helper()
	system := tenant.SystemContext(t.Context())
	a := tenant.ScopedContext(system, "review-a-"+uuid.NewString())
	b := tenant.ScopedContext(system, "review-b-"+uuid.NewString())
	expiry := time.Now().UTC().Add(time.Hour)
	rows := []*models.PendingAction{
		{ID: uuid.NewString(), RunID: uuid.NewString(), TenantID: tenant.FromContext(a), AgentID: "agent", Connector: "files", Tool: "write", ArgsDigest: "digest", ExpiresAt: &expiry},
		{ID: uuid.NewString(), RunID: uuid.NewString(), TenantID: tenant.FromContext(b), AgentID: "agent", Connector: "files", Tool: "write", ArgsDigest: "digest", ExpiresAt: &expiry},
	}
	for _, row := range rows {
		if err := s.CreatePendingAction(system, row); err != nil {
			t.Fatal(err)
		}
	}
	var missing *state.ErrNotFound
	if _, err := s.GetPendingAction(a, rows[1].ID); !errors.As(err, &missing) {
		t.Fatalf("foreign GET error=%v", err)
	}
	if err := s.SetPendingActionApproved(a, rows[1].ID, "foreign-reviewer"); err == nil {
		t.Fatal("foreign approval succeeded")
	}
	if err := s.SetPendingActionStatus(a, rows[1].ID, models.PendingStatusPending, models.PendingStatusDenied); err == nil {
		t.Fatal("foreign denial succeeded")
	}
	untouched, err := s.GetPendingAction(b, rows[1].ID)
	if err != nil || untouched.Status != models.PendingStatusPending || untouched.DecidedBy != "" {
		t.Fatalf("foreign mutation changed row=%+v err=%v", untouched, err)
	}
	found, err := s.SearchPendingActions(a, &models.PendingActionSearchRequest{TenantID: tenant.FromContext(b), Limit: 100})
	if err != nil || len(found) != 1 || found[0].ID != rows[0].ID {
		t.Fatalf("request filter widened tenant: rows=%+v err=%v", found, err)
	}
	if err := s.SetPendingActionApproved(a, rows[0].ID, "reviewer-a"); err != nil {
		t.Fatal(err)
	}
	approved, err := s.GetPendingAction(system, rows[0].ID)
	if err != nil || approved.Status != models.PendingStatusApproved || approved.DecidedBy != "reviewer-a" {
		t.Fatalf("own approval failed: row=%+v err=%v", approved, err)
	}
	if err := s.SetPendingActionStatus(b, rows[1].ID, models.PendingStatusPending, models.PendingStatusDenied); err != nil {
		t.Fatal(err)
	}
	denied, err := s.GetPendingAction(system, rows[1].ID)
	if err != nil || denied.Status != models.PendingStatusDenied {
		t.Fatalf("own denial failed: row=%+v err=%v", denied, err)
	}
	if err := s.SetPendingActionStatus(system, rows[1].ID, models.PendingStatusDenied, models.PendingStatusPending); err != nil {
		t.Fatalf("explicit system authority lost: %v", err)
	}
}
