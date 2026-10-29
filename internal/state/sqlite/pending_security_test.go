package sqlite

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
)

func TestPendingActionsScopeAndConcurrentDedup(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := tenant.SystemContext(context.Background())
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().UTC().Add(time.Hour)
	makeRow := func(id, tenantID, agentID string, embedded bool) *models.PendingAction {
		row := &models.PendingAction{ID: id, TenantID: tenantID, AgentID: agentID, RunID: "turn-1", Connector: "files", Tool: "upload", ArgsDigest: "same-digest"}
		if embedded {
			row.ExpiresAt = &expiry
		}
		return row
	}
	var wg sync.WaitGroup
	errs := make(chan error, 24)
	ids := make(chan string, 24)
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a := makeRow(fmt.Sprintf("same-%d", i), "tenant-a", "agent-a", true)
			if err := s.CreatePendingAction(ctx, a); err != nil {
				errs <- err
			} else {
				ids <- a.ID
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	close(ids)
	for err := range errs {
		t.Error(err)
	}
	sharedID := ""
	for id := range ids {
		if sharedID == "" {
			sharedID = id
		}
		if id != sharedID {
			t.Fatalf("concurrent duplicate IDs %q and %q", sharedID, id)
		}
	}
	if sharedID == "" {
		t.Fatal("no action returned")
	}
	for _, row := range []*models.PendingAction{makeRow("other-tenant", "tenant-b", "agent-a", true), makeRow("other-agent", "tenant-a", "agent-b", true), makeRow("connector", "tenant-a", "agent-a", false)} {
		originalID := row.ID
		if err := s.CreatePendingAction(ctx, row); err != nil {
			t.Fatal(err)
		}
		if row.ID != originalID {
			t.Fatalf("scope reused %q as %q", originalID, row.ID)
		}
	}
	rows, err := s.SearchPendingActions(ctx, &models.PendingActionSearchRequest{RunID: "turn-1"})
	if err != nil || len(rows) != 4 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	if err := s.SetPendingActionApproved(ctx, sharedID, "ops"); err != nil {
		t.Fatal(err)
	}
	if id, err := s.ConsumeApprovedAction(ctx, "tenant-a", "agent-a", "turn-1", 0, "files", "upload", "same-digest"); err != nil || id != "" {
		t.Fatalf("embedded approval consumed as connector: %q %v", id, err)
	}
	next := makeRow("next-request", "tenant-a", "agent-a", true)
	if err := s.CreatePendingAction(ctx, next); err != nil {
		t.Fatal(err)
	}
	if next.ID == sharedID {
		t.Fatal("terminal key was not released")
	}
	if err := s.SetPendingActionApproved(ctx, "connector", "ops"); err != nil {
		t.Fatal(err)
	}
	for _, scope := range [][2]string{{"tenant-b", "agent-a"}, {"tenant-a", "agent-b"}} {
		if id, err := s.ConsumeApprovedAction(ctx, scope[0], scope[1], "turn-1", 0, "files", "upload", "same-digest"); err != nil || id != "" {
			t.Fatalf("other scope consumed connector: %q %v", id, err)
		}
	}
	if id, err := s.ConsumeApprovedAction(ctx, "tenant-a", "agent-a", "turn-1", 0, "files", "upload", "same-digest"); err != nil || id != "connector" {
		t.Fatalf("matching consume: %q %v", id, err)
	}
}

func TestPendingApprovalDeadlineIsAtomic(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := tenant.SystemContext(context.Background())
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-time.Hour)
	future := time.Now().UTC().Add(time.Hour)
	for _, tc := range []struct {
		id      string
		expires *time.Time
		denied  bool
	}{{"expired", &past, true}, {"future", &future, false}, {"connector", nil, false}} {
		a := &models.PendingAction{ID: tc.id, TenantID: "acme", AgentID: "agent", RunID: tc.id, Connector: "files", Tool: "upload", ExpiresAt: tc.expires}
		if err := s.CreatePendingAction(ctx, a); err != nil {
			t.Fatal(err)
		}
		err := s.SetPendingActionApproved(ctx, tc.id, "ops")
		var conflict *state.ErrConflict
		if tc.denied && !errors.As(err, &conflict) {
			t.Fatalf("expired approval error=%v", err)
		}
		if !tc.denied && err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ExpireGovernancePending(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	a, err := s.GetPendingAction(ctx, "expired")
	if err != nil || a.Status != models.PendingStatusDenied {
		t.Fatalf("expired row=%+v err=%v", a, err)
	}
}

func TestPendingDedupLegacyRowsRemainUsable(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := tenant.SystemContext(context.Background())
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	a := &models.PendingAction{ID: "legacy", TenantID: "acme", AgentID: "agent", RunID: "turn", Connector: "files", Tool: "upload"}
	if err := s.CreatePendingAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	// Existing deployment rows acquire no new key during migration.
	if _, err := s.db.ExecContext(ctx, `UPDATE pending_actions SET pending_key=NULL WHERE id=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	duplicate := *a
	duplicate.ID = "new"
	if err := s.CreatePendingAction(ctx, &duplicate); err != nil {
		t.Fatal(err)
	}
	if duplicate.ID != "legacy" {
		t.Fatalf("legacy pending row not reused: %q", duplicate.ID)
	}
}
