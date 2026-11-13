package conformance

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/google/uuid"
)

const pendingTargetRevision = "target-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const otherPendingTargetRevision = "target-v1-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// RunPendingTargetRevisionSecurity qualifies target binding in both legacy
// consume and recorded-action dispatch paths. mutate is test-only SQL used to
// represent historical rows that a fresh CreatePendingAction now refuses.
func RunPendingTargetRevisionSecurity(t *testing.T, store state.Store, mutate func(context.Context, string, string, string) error) {
	t.Helper()
	pending, ok := store.(interface {
		PendingStore
		FindOpenPendingAction(context.Context, string, string, string, int64, string, string, string, string, bool) (*models.PendingAction, error)
	})
	if !ok {
		t.Fatal("pending target contract unavailable")
	}
	actions, ok := store.(state.ProtectedActionStore)
	if !ok {
		t.Fatal("recorded action contract unavailable")
	}
	ctx, input := connectorActionFixture(t, store)
	makePending := func(revision, digest string) *models.PendingAction {
		return &models.PendingAction{ID: "pending-v2-" + uuid.NewString(), TenantID: input.TenantID, AgentID: input.AgentID, RunID: input.RunID, Generation: input.Generation, Connector: input.Connector, Tool: input.Tool, ArgsDigest: digest, TargetRevision: revision}
	}
	a := makePending(pendingTargetRevision, input.ArgsDigest)
	if err := pending.CreatePendingAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	duplicate := makePending(pendingTargetRevision, input.ArgsDigest)
	if err := pending.CreatePendingAction(ctx, duplicate); err != nil || duplicate.ID != a.ID {
		t.Fatalf("same target did not deduplicate: %v", err)
	}
	b := makePending(otherPendingTargetRevision, input.ArgsDigest)
	if err := pending.CreatePendingAction(ctx, b); err != nil || b.ID == a.ID {
		t.Fatalf("different targets merged: %v", err)
	}
	for _, row := range []*models.PendingAction{a, b} {
		found, err := pending.FindOpenPendingAction(ctx, row.TenantID, row.AgentID, row.RunID, row.Generation, row.Connector, row.Tool, row.ArgsDigest, row.TargetRevision, false)
		if err != nil || found == nil || found.ID != row.ID {
			t.Fatalf("target find returned wrong authority: %+v %v", found, err)
		}
		rows, err := pending.SearchPendingActions(ctx, &models.PendingActionSearchRequest{RunID: input.RunID, TargetRevision: row.TargetRevision})
		if err != nil || len(rows) != 1 || rows[0].ID != row.ID || rows[0].TargetRevision != row.TargetRevision {
			t.Fatalf("target search returned wrong rows: %+v %v", rows, err)
		}
	}
	if err := pending.SetPendingActionApproved(ctx, a.ID, "reviewer"); err != nil {
		t.Fatal(err)
	}
	consume := func(revision string) (string, error) {
		return pending.ConsumeApprovedAction(ctx, input.TenantID, input.AgentID, input.RunID, input.Generation, input.Connector, input.Tool, input.ArgsDigest, revision)
	}
	if id, err := consume(otherPendingTargetRevision); err != nil || id != "" {
		t.Fatalf("wrong target consumed approval: %q %v", id, err)
	}
	unchanged, err := pending.GetPendingAction(ctx, a.ID)
	if err != nil || unchanged.Status != models.PendingStatusApproved {
		t.Fatal("target mismatch burned approval")
	}
	// Recorded dispatch must reject the same mismatch under the run/approval locks.
	changed := *input
	changed.ID = uuid.NewString()
	otherRevision := otherPendingTargetRevision
	changed.TargetRevision = &otherRevision
	actionB, err := actions.CreateConnectorAction(ctx, &changed, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	actionB, err = actions.RecordConnectorActionDecision(ctx, actionB.ID, actionB.Version, models.ConnectorActionDecision{Effect: "pending", AuthorizationSource: "policy"}, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if peek, err := actions.PeekApprovedConnectorAction(ctx, actionB.ID); err != nil || peek != nil {
		t.Fatalf("wrong target peek found capability: %+v %v", peek, err)
	}
	if _, err := actions.PrepareConnectorActionDispatch(ctx, actionB.ID, actionB.Version, a.ID, "approved_capability", time.Now().Add(time.Minute), uuid.NewString()); err == nil {
		t.Fatal("recorded dispatch accepted different target")
	}
	unchanged, err = pending.GetPendingAction(ctx, a.ID)
	if err != nil || unchanged.Status != models.PendingStatusApproved {
		t.Fatal("recorded mismatch burned approval")
	}
	matching := *input
	matching.ID = uuid.NewString()
	actionA, err := actions.CreateConnectorAction(ctx, &matching, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	actionA, err = actions.RecordConnectorActionDecision(ctx, actionA.ID, actionA.Version, models.ConnectorActionDecision{Effect: "pending", AuthorizationSource: "policy"}, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if peek, err := actions.PeekApprovedConnectorAction(ctx, actionA.ID); err != nil || peek == nil || peek.ID != a.ID || peek.TargetRevision != a.TargetRevision {
		t.Fatalf("matching target peek failed: %+v %v", peek, err)
	}
	if receipt, err := actions.PrepareConnectorActionDispatch(ctx, actionA.ID, actionA.Version, a.ID, "approved_capability", time.Now().Add(time.Minute), uuid.NewString()); err != nil || !receipt.Applied {
		t.Fatalf("matching recorded dispatch: %+v %v", receipt, err)
	}
	if id, err := consume(pendingTargetRevision); err != nil || id != "" {
		t.Fatalf("recorded approval consumed twice: %q %v", id, err)
	}
	// Legacy evidence-off consumption keeps exactly the same target fence.
	if err := pending.SetPendingActionApproved(ctx, b.ID, "reviewer"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var winners atomic.Int32
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := consume(otherPendingTargetRevision)
			if err != nil {
				t.Error(err)
			}
			if id == b.ID {
				winners.Add(1)
			} else if id != "" {
				t.Error("consumed unrelated approval")
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("legacy consume winners=%d", winners.Load())
	}
	for index, revision := range []string{"", "target-v1-" + strings.Repeat("A", 64), "target-v1-" + strings.Repeat("a", 63)} {
		bad := makePending(revision, fmt.Sprintf("invalid-%d", index))
		if err := pending.CreatePendingAction(ctx, bad); err == nil {
			t.Fatal("fresh managed approval accepted invalid target")
		}
		bad.TargetRevision = pendingTargetRevision
		if err := pending.CreatePendingAction(ctx, bad); err != nil {
			t.Fatal(err)
		}
		if err := mutate(ctx, bad.ID, revision, models.PendingStatusPending); err != nil {
			t.Fatal(err)
		}
		if err := pending.SetPendingActionApproved(ctx, bad.ID, "reviewer"); err == nil {
			t.Fatal("malformed stored target acquired approval")
		}
	}
	for _, status := range []string{models.PendingStatusPending, models.PendingStatusApproved} {
		old := makePending(pendingTargetRevision, "legacy-"+status)
		old.Status = status
		if err := pending.CreatePendingAction(ctx, old); err != nil {
			t.Fatal(err)
		}
		if err := mutate(ctx, old.ID, "", status); err != nil {
			t.Fatal(err)
		}
		if status == models.PendingStatusPending {
			if err := pending.SetPendingActionApproved(ctx, old.ID, "reviewer"); err == nil {
				t.Fatal("historical empty target acquired new approval")
			}
			fresh := makePending(pendingTargetRevision, old.ArgsDigest)
			if err := pending.CreatePendingAction(ctx, fresh); err != nil || fresh.ID == old.ID {
				t.Fatalf("fresh request reused historical target: %v", err)
			}
		}
		for _, target := range []string{"", pendingTargetRevision} {
			id, err := pending.ConsumeApprovedAction(ctx, old.TenantID, old.AgentID, old.RunID, old.Generation, old.Connector, old.Tool, old.ArgsDigest, target)
			if err != nil || id != "" {
				t.Fatal("historical target authorized a new operation")
			}
		}
		read, err := pending.GetPendingAction(ctx, old.ID)
		if err != nil || read.Status != status || read.TargetRevision != "" {
			t.Fatal("historical record was guessed or consumed")
		}
	}
	expiry := time.Now().Add(time.Hour)
	embedded := makePending("untrusted-app-target", "embedded-digest")
	embedded.ExpiresAt = &expiry
	embedded.RunID = "app-owned-" + uuid.NewString()
	if err := pending.CreatePendingAction(ctx, embedded); err != nil {
		t.Fatal(err)
	}
	if embedded.TargetRevision != "" {
		t.Fatal("standalone governance acquired managed target binding")
	}
	if err := pending.SetPendingActionApproved(ctx, embedded.ID, "reviewer"); err != nil {
		t.Fatal(err)
	}
	read, err := pending.GetPendingAction(ctx, embedded.ID)
	if err != nil || read.TargetRevision != "" || read.Status != models.PendingStatusApproved {
		t.Fatal("standalone approval behavior changed")
	}
}
