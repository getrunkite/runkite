package conformance

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/google/uuid"
)

// PendingStore is the SQL approval contract, separate from non-SQL state backends.
type PendingStore interface {
	CreatePendingAction(context.Context, *models.PendingAction) error
	GetPendingAction(context.Context, string) (*models.PendingAction, error)
	SearchPendingActions(context.Context, *models.PendingActionSearchRequest) ([]*models.PendingAction, error)
	SetPendingActionApproved(context.Context, string, string) error
	ConsumeApprovedAction(context.Context, string, string, string, int64, string, string, string) (string, error)
}

// RunPendingSecurity proves isolation, concurrent dedup and deadline enforcement
// against a real SQL backend. Every invocation uses independent synthetic IDs.
func RunPendingSecurity(t *testing.T, s PendingStore) {
	t.Helper()
	ctx := tenant.SystemContext(t.Context())
	runID := "approval-security-" + uuid.NewString()
	future := time.Now().UTC().Add(time.Hour)
	makeRow := func(id, tenantID, agentID string, embedded bool) *models.PendingAction {
		row := &models.PendingAction{ID: runID + id, TenantID: tenantID, AgentID: agentID, RunID: runID, Connector: "files", Tool: "upload", ArgsDigest: "same-digest"}
		if embedded {
			row.ExpiresAt = &future
		}
		return row
	}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	ids := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			row := makeRow(fmt.Sprintf("-%d", i), "scope-a", "agent-a", true)
			if err := s.CreatePendingAction(ctx, row); err != nil {
				errs <- err
			} else {
				ids <- row.ID
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	close(ids)
	for err := range errs {
		t.Error(err)
	}
	canonical := ""
	for id := range ids {
		if canonical == "" {
			canonical = id
		}
		if id != canonical {
			t.Fatalf("concurrent pending actions differ: %q and %q", canonical, id)
		}
	}
	if canonical == "" {
		t.Fatal("no canonical pending action")
	}
	for _, row := range []*models.PendingAction{makeRow("-tenant", "scope-b", "agent-a", true), makeRow("-agent", "scope-a", "agent-b", true), makeRow("-connector", "scope-a", "agent-a", false)} {
		original := row.ID
		if err := s.CreatePendingAction(ctx, row); err != nil {
			t.Fatal(err)
		}
		if row.ID != original {
			t.Fatalf("scope merged into %s", row.ID)
		}
	}
	rows, err := s.SearchPendingActions(ctx, &models.PendingActionSearchRequest{RunID: runID})
	if err != nil || len(rows) != 4 {
		t.Fatalf("scoped rows=%d err=%v", len(rows), err)
	}
	if err := s.SetPendingActionApproved(ctx, canonical, "synthetic-ops"); err != nil {
		t.Fatal(err)
	}
	if id, err := s.ConsumeApprovedAction(ctx, "scope-a", "agent-a", runID, 0, "files", "upload", "same-digest"); err != nil || id != "" {
		t.Fatalf("embedded approval consumed as connector: %q %v", id, err)
	}
	replacement := makeRow("-replacement", "scope-a", "agent-a", true)
	if err := s.CreatePendingAction(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.ID == canonical {
		t.Fatal("approval key not released")
	}
	connectorID := runID + "-connector"
	if err := s.SetPendingActionApproved(ctx, connectorID, "synthetic-ops"); err != nil {
		t.Fatal(err)
	}
	if id, err := s.ConsumeApprovedAction(ctx, "scope-b", "agent-a", runID, 0, "files", "upload", "same-digest"); err != nil || id != "" {
		t.Fatalf("cross-tenant approval consumed: %q %v", id, err)
	}
	if id, err := s.ConsumeApprovedAction(ctx, "scope-a", "agent-a", runID, 0, "files", "upload", "same-digest"); err != nil || id != connectorID {
		t.Fatalf("connector consume: %q %v", id, err)
	}
	past := time.Now().UTC().Add(-time.Hour)
	expired := makeRow("-expired", "scope-a", "agent-a", true)
	expired.RunID += "-expired"
	expired.ExpiresAt = &past
	if err := s.CreatePendingAction(ctx, expired); err != nil {
		t.Fatal(err)
	}
	var conflict *state.ErrConflict
	if err := s.SetPendingActionApproved(ctx, expired.ID, "synthetic-ops"); !errors.As(err, &conflict) {
		t.Fatalf("overdue approval error=%v", err)
	}
	got, err := s.GetPendingAction(ctx, expired.ID)
	if err != nil || got.Status != models.PendingStatusPending {
		t.Fatalf("overdue row=%+v err=%v", got, err)
	}
}
