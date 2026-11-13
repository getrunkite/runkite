package conformance

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/google/uuid"
)

// RunPendingReviewSecurity proves exact reviewer counting and revision-bound
// authority under the same real SQL transactions used by managed dispatch.
func RunPendingReviewSecurity(t *testing.T, store state.Store) {
	t.Helper()
	pending := store.(PendingStore)
	actions := store.(state.ProtectedActionStore)
	rowFor := func(a *models.ConnectorAction) *models.PendingAction {
		return &models.PendingAction{
			ID: "pending-v2-" + uuid.NewString(), TenantID: a.TenantID, AgentID: a.AgentID, RunID: a.RunID, Generation: a.Generation,
			Connector: a.Connector, Tool: a.Tool, ArgsDigest: a.ArgsDigest, TargetRevision: *a.TargetRevision, PolicyRevision: *a.PolicyRevision}
	}
	consume := func(ctx context.Context, a *models.ConnectorAction, policyRevision, toolRevision string) (string, error) {
		return pending.ConsumeApprovedAction(ctx, a.TenantID, a.AgentID, a.RunID, a.Generation, a.Connector, a.Tool, a.ArgsDigest, *a.TargetRevision, policyRevision, toolRevision)
	}
	t.Run("quorum_separation_exact_identities_and_history", func(t *testing.T) {
		ctx, input := connectorActionFixture(t, store)
		row := rowFor(input)
		row.RequiredApprovals = 3
		row.ExcludeRequester = true
		row.ApprovalCount = 99
		row.Approvals = []models.PendingActionApproval{{ReviewerIdentity: "forged"}}
		if err := pending.CreatePendingAction(ctx, row); err != nil {
			t.Fatal(err)
		}
		if row.RequesterIdentity != "alice" || row.RequiredApprovals != 3 || row.ApprovalCount != 0 || len(row.Approvals) != 0 {
			t.Fatalf("untrusted plan provenance: %+v", row)
		}
		for _, actor := range []string{"alice", "", "  "} {
			if err := pending.SetPendingActionApproved(ctx, row.ID, actor); err == nil {
				t.Fatal("self or anonymous approval accepted")
			}
		}
		if err := pending.SetPendingActionApproved(ctx, row.ID, "Alice"); err != nil {
			t.Fatal(err)
		}
		first, err := pending.GetPendingAction(ctx, row.ID)
		if err != nil || first.Status != models.PendingStatusPending || first.ApprovalCount != 1 || len(first.Approvals) != 1 {
			t.Fatalf("partial vote: %+v %v", first, err)
		}
		at := first.Approvals[0].ReviewedAt
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := pending.SetPendingActionApproved(ctx, row.ID, "Alice"); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		same, err := pending.GetPendingAction(ctx, row.ID)
		if err != nil || same.ApprovalCount != 1 || !same.Approvals[0].ReviewedAt.Equal(at) {
			t.Fatalf("duplicate identity changed vote: %+v %v", same, err)
		}
		if id, err := consume(ctx, input, pendingPolicyRevision, ""); err != nil || id != "" {
			t.Fatalf("partial quorum consumed: %q %v", id, err)
		}
		action, err := actions.CreateConnectorAction(ctx, input, uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		action, err = actions.RecordConnectorActionDecision(ctx, action.ID, action.Version, models.ConnectorActionDecision{Effect: "pending", AuthorizationSource: "policy"}, uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		if got, err := actions.PeekApprovedConnectorAction(ctx, action.ID); err != nil || got != nil {
			t.Fatalf("partial quorum supplied capability: %+v %v", got, err)
		}
		if _, err := actions.PrepareConnectorActionDispatch(ctx, action.ID, action.Version, row.ID, "approved_capability", time.Now().Add(time.Minute), uuid.NewString()); err == nil {
			t.Fatal("partial quorum dispatched")
		}
		for _, actor := range []string{"bob", "carol"} {
			wg.Add(1)
			go func(actor string) {
				defer wg.Done()
				if err := pending.SetPendingActionApproved(ctx, row.ID, actor); err != nil {
					t.Error(err)
				}
			}(actor)
		}
		wg.Wait()
		final, err := pending.GetPendingAction(ctx, row.ID)
		if err != nil || final.Status != models.PendingStatusApproved || final.ApprovalCount != 3 || len(final.Approvals) != 3 || final.DecidedBy == "" {
			t.Fatalf("quorum completion: %+v %v", final, err)
		}
		foreign := tenant.ScopedContext(ctx, input.TenantID+"-foreign")
		if _, err := pending.GetPendingAction(foreign, row.ID); err == nil {
			t.Fatal("foreign reviewer read approval history")
		}
		if err := pending.SetPendingActionApproved(foreign, row.ID, "foreign"); err == nil {
			t.Fatal("foreign reviewer voted")
		}
		if got, err := actions.PeekApprovedConnectorAction(ctx, action.ID); err != nil || got == nil || got.ID != row.ID {
			t.Fatalf("complete quorum unavailable: %+v %v", got, err)
		}
		if receipt, err := actions.PrepareConnectorActionDispatch(ctx, action.ID, action.Version, row.ID, "approved_capability", time.Now().Add(time.Minute), uuid.NewString()); err != nil || !receipt.Applied {
			t.Fatalf("quorum dispatch: %+v %v", receipt, err)
		}
		used, err := pending.GetPendingAction(ctx, row.ID)
		if err != nil || used.Status != models.PendingStatusConsumed || used.ApprovalCount != 3 || len(used.Approvals) != 3 {
			t.Fatalf("consumption erased history: %+v %v", used, err)
		}
	})
	t.Run("policy_and_optional_tool_revisions_are_exact", func(t *testing.T) {
		ctx, input := connectorActionFixture(t, store)
		policyB := "policy-v1-" + strings.Repeat("b", 64)
		toolA := "tool-v1-" + strings.Repeat("a", 64)
		toolB := "tool-v1-" + strings.Repeat("b", 64)
		row := rowFor(input)
		row.ToolRevision = toolA
		if err := pending.CreatePendingAction(ctx, row); err != nil {
			t.Fatal(err)
		}
		for _, variation := range []struct{ policy, tool string }{{policyB, toolA}, {pendingPolicyRevision, toolB}, {pendingPolicyRevision, ""}} {
			different := rowFor(input)
			different.PolicyRevision = variation.policy
			different.ToolRevision = variation.tool
			if err := pending.CreatePendingAction(ctx, different); err != nil || different.ID == row.ID {
				t.Fatalf("revision tuples merged: %v", err)
			}
		}
		if err := pending.SetPendingActionApproved(ctx, row.ID, "reviewer"); err != nil {
			t.Fatal(err)
		}
		for _, variation := range []struct{ policy, tool string }{{policyB, toolA}, {pendingPolicyRevision, toolB}, {pendingPolicyRevision, ""}} {
			if id, err := consume(ctx, input, variation.policy, variation.tool); err != nil || id != "" {
				t.Fatalf("wrong revision consumed: %q %v", id, err)
			}
			altered := *input
			altered.ID = uuid.NewString()
			altered.PolicyRevision = &variation.policy
			altered.ToolRevision = &variation.tool
			a, err := actions.CreateConnectorAction(ctx, &altered, uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			if got, err := actions.PeekApprovedConnectorAction(ctx, a.ID); err != nil || got != nil {
				t.Fatalf("wrong revision peek: %+v %v", got, err)
			}
			a, err = actions.RecordConnectorActionDecision(ctx, a.ID, a.Version, models.ConnectorActionDecision{Effect: "pending", AuthorizationSource: "policy"}, uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := actions.PrepareConnectorActionDispatch(ctx, a.ID, a.Version, row.ID, "approved_capability", time.Now().Add(time.Minute), uuid.NewString()); err == nil {
				t.Fatal("wrong revision dispatched")
			}
		}
		unchanged, err := pending.GetPendingAction(ctx, row.ID)
		if err != nil || unchanged.Status != models.PendingStatusApproved {
			t.Fatal("revision mismatch burned capability")
		}
		if id, err := consume(ctx, input, pendingPolicyRevision, toolA); err != nil || id != row.ID {
			t.Fatalf("matching revision not consumed: %q %v", id, err)
		}
	})
	t.Run("immutable_requester_plan_and_local_compatibility", func(t *testing.T) {
		ctx, input := connectorActionFixture(t, store)
		forged := rowFor(input)
		forged.RequesterIdentity = "another-person"
		if err := pending.CreatePendingAction(ctx, forged); err == nil {
			t.Fatal("requester replaced durable principal")
		}
		for _, count := range []int{-1, 6} {
			bad := rowFor(input)
			bad.RequiredApprovals = count
			if err := pending.CreatePendingAction(ctx, bad); err == nil {
				t.Fatal("invalid quorum accepted")
			}
		}
		bad := rowFor(input)
		bad.PolicyRevision = ""
		if err := pending.CreatePendingAction(ctx, bad); err == nil {
			t.Fatal("unbound policy accepted")
		}
		bad = rowFor(input)
		bad.ToolRevision = "tool-v1-bad"
		if err := pending.CreatePendingAction(ctx, bad); err == nil {
			t.Fatal("malformed tool revision accepted")
		}
		row := rowFor(input)
		if err := pending.CreatePendingAction(ctx, row); err != nil {
			t.Fatal(err)
		}
		if row.RequiredApprovals != 1 {
			t.Fatal("default quorum changed")
		}
		if err := pending.SetPendingActionApproved(ctx, row.ID, ""); err != nil {
			t.Fatalf("single-reviewer local compatibility: %v", err)
		}
		if id, err := consume(ctx, input, pendingPolicyRevision, ""); err != nil || id != row.ID {
			t.Fatalf("default-one consume: %q %v", id, err)
		}
		expiry := time.Now().Add(time.Hour)
		standalone := rowFor(input)
		standalone.RunID = "app-owned-" + uuid.NewString()
		standalone.ExpiresAt = &expiry
		standalone.RequesterIdentity = "requester"
		standalone.RequiredApprovals = 2
		standalone.ExcludeRequester = true
		if err := pending.CreatePendingAction(ctx, standalone); err != nil {
			t.Fatal(err)
		}
		if standalone.TargetRevision != "" || standalone.PolicyRevision != "" || standalone.ToolRevision != "" || standalone.RunIncarnationID != "" || standalone.RequesterIdentity != "requester" {
			t.Fatal("standalone acquired managed provenance")
		}
		if err := pending.SetPendingActionApproved(ctx, standalone.ID, "requester"); err == nil {
			t.Fatal("standalone self-approval accepted")
		}
		if err := pending.SetPendingActionApproved(ctx, standalone.ID, "a"); err != nil {
			t.Fatal(err)
		}
		if err := pending.SetPendingActionApproved(ctx, standalone.ID, "b"); err != nil {
			t.Fatal(err)
		}
		final, err := pending.GetPendingAction(ctx, standalone.ID)
		if err != nil || final.Status != models.PendingStatusApproved || final.ApprovalCount != 2 {
			t.Fatalf("standalone quorum: %+v %v", final, err)
		}
	})
}
