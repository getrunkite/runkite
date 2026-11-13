package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/pagecursor"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/google/uuid"
)

type connectorActionApprovals interface {
	CreatePendingAction(context.Context, *models.PendingAction) error
	SetPendingActionApproved(context.Context, string, string) error
	GetPendingAction(context.Context, string) (*models.PendingAction, error)
}

func connectorActionFixture(t *testing.T, store state.Store) (context.Context, *models.ConnectorAction) {
	t.Helper()
	ctx := tenant.WithContext(context.Background(), "actions-"+uuid.NewString())
	now := time.Now().UTC().Truncate(time.Microsecond)
	revision := pendingTargetRevision
	a := &models.ConnectorAction{TargetRevision: &revision, ID: uuid.NewString(), TenantID: tenant.FromContext(ctx), RunID: uuid.NewString(), ThreadID: uuid.NewString(), Generation: 1, AgentID: "agent", AgentVersion: 999, Principal: "alice", Connector: "fixture", Tool: "write", Transport: "json", ArgsDigest: strings.Repeat("a", 64)}
	if err := store.UpsertAgent(ctx, &models.Agent{AgentID: a.AgentID, Name: a.AgentID, Metadata: map[string]interface{}{}}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateThread(ctx, &models.Thread{ThreadID: a.ThreadID, Status: models.ThreadStatusBusy, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(ctx, &models.Run{RunID: a.RunID, ThreadID: a.ThreadID, AgentID: a.AgentID, Status: models.RunStatusPending, Metadata: map[string]interface{}{"run_manifest": map[string]interface{}{"agent_version": 7}}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if accepted, err := store.BeginRunAttempt(ctx, a.RunID, a.Generation); err != nil || !accepted {
		t.Fatalf("begin fixture attempt: %v %v", accepted, err)
	}
	return ctx, a
}

// RunConnectorActions qualifies the optional SQL lifecycle independently of the
// universal run store. Every effect assertion belongs in the API fixture too.
func RunConnectorActions(t *testing.T, store state.Store) {
	actions, ok := store.(state.ProtectedActionStore)
	if !ok {
		t.Fatal("protected action store unavailable")
	}
	approvals, ok := store.(connectorActionApprovals)
	if !ok {
		t.Fatal("approval store unavailable")
	}
	create := func(t *testing.T) (context.Context, *models.ConnectorAction) {
		ctx, a := connectorActionFixture(t, store)
		got, err := actions.CreateConnectorAction(ctx, a, uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		return ctx, got
	}
	ready := func(t *testing.T, ctx context.Context, a *models.ConnectorAction, effect string) *models.ConnectorAction {
		got, err := actions.RecordConnectorActionDecision(ctx, a.ID, a.Version, models.ConnectorActionDecision{Effect: effect, AuthorizationSource: "policy"}, uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	prepare := func(t *testing.T, ctx context.Context, a *models.ConnectorAction) *state.ConnectorActionDispatch {
		claim, err := actions.PrepareConnectorActionDispatch(ctx, a.ID, a.Version, "", "policy", time.Now().Add(time.Minute), uuid.NewString())
		if err != nil || !claim.Applied {
			t.Fatalf("prepare: %+v %v", claim, err)
		}
		return claim
	}

	t.Run("identity_cas_and_mutation_replay", func(t *testing.T) {
		ctx, input := connectorActionFixture(t, store)
		input.RunCreatedAt = time.Unix(1, 0)
		mutation := uuid.NewString()
		a, err := actions.CreateConnectorAction(ctx, input, mutation)
		if err != nil {
			t.Fatal(err)
		}
		storedRun, err := store.GetRun(ctx, input.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if !a.RunCreatedAt.Equal(storedRun.CreatedAt) || a.AgentVersion != 7 || a.Version != 1 || a.RunIncarnationID == "" || a.RunIncarnationID != storedRun.IncarnationID {
			t.Fatalf("untrusted run provenance: %+v", a)
		}
		if replay, err := actions.CreateConnectorAction(ctx, input, mutation); err != nil || replay.ID != a.ID {
			t.Fatalf("creation replay: %+v %v", replay, err)
		}
		changed := *input
		changed.Tool = "another"
		if _, err := actions.CreateConnectorAction(ctx, &changed, mutation); err == nil {
			t.Fatal("mutation accepted changed metadata")
		}
		var wins atomic.Int32
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := actions.RecordConnectorActionDecision(ctx, a.ID, a.Version, models.ConnectorActionDecision{Effect: "allow", AuthorizationSource: "policy"}, uuid.NewString()); err == nil {
					wins.Add(1)
				}
			}()
		}
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("decision winners=%d", wins.Load())
		}
		events, err := actions.ListConnectorActionEvents(ctx, a.ID, 0, 100)
		if err != nil || len(events) != 2 {
			t.Fatalf("events=%+v err=%v", events, err)
		}
		for i, e := range events {
			if e.Sequence != int64(i+1) || e.TenantID != a.TenantID {
				t.Fatalf("event order/scope=%+v", e)
			}
		}
		foreign := tenant.WithContext(context.Background(), a.TenantID+"-foreign")
		if _, err := actions.GetConnectorAction(foreign, a.ID); err == nil {
			t.Fatal("foreign action visible")
		}
		if _, err := actions.ListConnectorActionEvents(foreign, a.ID, 0, 10); err == nil {
			t.Fatal("foreign events visible")
		}
		if rows, err := actions.SearchConnectorActions(foreign, &models.ConnectorActionSearchRequest{TenantID: a.TenantID}); err != nil || len(rows) != 0 {
			t.Fatalf("foreign search=%+v %v", rows, err)
		}
	})

	t.Run("dispatch_is_single_claim_and_response_is_evidence", func(t *testing.T) {
		ctx, a := create(t)
		a = ready(t, ctx, a, "allow")
		mutation := uuid.NewString()
		deadline := time.Now().Add(time.Minute)
		claim, err := actions.PrepareConnectorActionDispatch(ctx, a.ID, a.Version, "", "policy", deadline, mutation)
		if err != nil || !claim.Applied {
			t.Fatalf("claim=%+v %v", claim, err)
		}
		replay, err := actions.PrepareConnectorActionDispatch(ctx, a.ID, a.Version, "", "policy", deadline, mutation)
		if err != nil || replay.Applied || replay.DispatchID != claim.DispatchID {
			t.Fatalf("replay supplied execution authority: %+v %v", replay, err)
		}
		if _, err := actions.PrepareConnectorActionDispatch(ctx, a.ID, a.Version, "", "policy", deadline.Add(time.Second), mutation); err == nil {
			t.Fatal("changed dispatch mutation accepted")
		}
		if _, err := actions.PrepareConnectorActionDispatch(ctx, a.ID, a.Version, "", "policy", deadline, uuid.NewString()); err == nil {
			t.Fatal("second dispatch claim accepted")
		}
		status := 200
		bytes := int64(100)
		obs := models.ConnectorActionObservation{State: models.ConnectorActionResponseObserved, ResponseKind: "rpc_result", HTTPStatus: &status, ResponseBytes: &bytes}
		finish := uuid.NewString()
		got, err := actions.FinishConnectorAction(ctx, a.ID, claim.Version, claim.DispatchID, obs, finish)
		if err != nil || got.Version != 4 || got.State != models.ConnectorActionResponseObserved {
			t.Fatalf("response=%+v %v", got, err)
		}
		if replay, err := actions.FinishConnectorAction(ctx, a.ID, claim.Version, claim.DispatchID, obs, finish); err != nil || replay.Version != 4 {
			t.Fatalf("finish replay=%+v %v", replay, err)
		}
		events, err := actions.ListConnectorActionEvents(ctx, a.ID, 0, 100)
		if err != nil || len(events) != 4 {
			t.Fatalf("events=%+v %v", events, err)
		}
		if _, err := actions.FinishConnectorAction(ctx, a.ID, claim.Version, claim.DispatchID, models.ConnectorActionObservation{State: models.ConnectorActionNotDispatched}, uuid.NewString()); err == nil {
			t.Fatal("dispatched action relabeled not dispatched")
		}
	})

	t.Run("concurrent_same_mutation_is_persistence_replay_only", func(t *testing.T) {
		ctx, a := create(t)
		a = ready(t, ctx, a, "allow")
		mutation, deadline := uuid.NewString(), time.Now().Add(time.Minute)
		var wins atomic.Int32
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		receipts := make(chan *state.ConnectorActionDispatch, 8)
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				claim, err := actions.PrepareConnectorActionDispatch(ctx, a.ID, a.Version, "", "policy", deadline, mutation)
				if err != nil {
					errs <- err
					return
				}
				if claim.Applied {
					wins.Add(1)
				}
				receipts <- claim
			}()
		}
		wg.Wait()
		close(errs)
		close(receipts)
		for err := range errs {
			t.Error(err)
		}
		if wins.Load() != 1 {
			t.Fatalf("new dispatch claim winners=%d", wins.Load())
		}
		id := ""
		count := 0
		for receipt := range receipts {
			count++
			if id == "" {
				id = receipt.DispatchID
			}
			if receipt.DispatchID != id || receipt.Version != 3 {
				t.Fatalf("replayed receipt changed: %+v", receipt)
			}
		}
		if count != 8 {
			t.Fatalf("receipts=%d", count)
		}
		events, err := actions.ListConnectorActionEvents(ctx, a.ID, 0, 100)
		if err != nil || len(events) != 3 {
			t.Fatalf("duplicate dispatch events=%+v %v", events, err)
		}
	})

	t.Run("approval_peek_and_atomic_consume", func(t *testing.T) {
		ctx, a := create(t)
		second := *a
		second.ID = uuid.NewString()
		b, err := actions.CreateConnectorAction(ctx, &second, uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		pending := &models.PendingAction{TargetRevision: pendingTargetRevision, ID: "pending-v2-" + uuid.NewString(), TenantID: a.TenantID, RunID: a.RunID, Generation: a.Generation, AgentID: a.AgentID, Connector: a.Connector, Tool: a.Tool, ArgsDigest: a.ArgsDigest, Status: models.PendingStatusPending}
		if err := approvals.CreatePendingAction(ctx, pending); err != nil {
			t.Fatal(err)
		}
		if err := approvals.SetPendingActionApproved(ctx, pending.ID, "reviewer"); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			peek, err := actions.PeekApprovedConnectorAction(ctx, a.ID)
			if err != nil || peek == nil || peek.ID != pending.ID {
				t.Fatalf("peek=%+v %v", peek, err)
			}
		}
		p, err := approvals.GetPendingAction(ctx, pending.ID)
		if err != nil || p.Status != models.PendingStatusApproved {
			t.Fatalf("peek consumed approval: %+v %v", p, err)
		}
		a = ready(t, ctx, a, "pending")
		b = ready(t, ctx, b, "pending")
		if _, err := actions.PrepareConnectorActionDispatch(ctx, a.ID, a.Version, "", "policy", time.Now().Add(time.Minute), uuid.NewString()); err == nil {
			t.Fatal("pending dispatched without approval")
		}
		var wins atomic.Int32
		var wg sync.WaitGroup
		for _, action := range []*models.ConnectorAction{a, b} {
			wg.Add(1)
			go func(action *models.ConnectorAction) {
				defer wg.Done()
				if _, err := actions.PrepareConnectorActionDispatch(ctx, action.ID, action.Version, pending.ID, "approved_capability", time.Now().Add(time.Minute), uuid.NewString()); err == nil {
					wins.Add(1)
				}
			}(action)
		}
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("approval dispatch winners=%d", wins.Load())
		}
		p, err = approvals.GetPendingAction(ctx, pending.ID)
		if err != nil || p.Status != models.PendingStatusConsumed {
			t.Fatalf("approval=%+v %v", p, err)
		}
	})

	t.Run("unknown_sweep_late_observation_and_retention", func(t *testing.T) {
		ctx, a := create(t)
		a = ready(t, ctx, a, "allow")
		claim := prepare(t, ctx, a)
		if count, err := actions.MarkExpiredConnectorActionsUnknown(ctx, time.Now().Add(2*time.Minute), 100); err != nil || count != 1 {
			t.Fatalf("sweep=%d %v", count, err)
		}
		got, err := actions.GetConnectorAction(ctx, a.ID)
		if err != nil || got.State != models.ConnectorActionOutcomeUnknown {
			t.Fatalf("unknown=%+v %v", got, err)
		}
		events, err := actions.ListConnectorActionEvents(ctx, a.ID, 0, 100)
		if err != nil || events[len(events)-1].Observer != "runkite_reconciler" {
			t.Fatalf("false observed evidence=%+v %v", events, err)
		}
		if count, err := actions.PruneConnectorActions(ctx, time.Now().Add(time.Hour), 100); err != nil || count != 0 {
			t.Fatalf("unknown pruned=%d %v", count, err)
		}
		if count, err := actions.MarkExpiredConnectorActionsUnknown(ctx, time.Now().Add(3*time.Minute), 100); err != nil || count != 0 {
			t.Fatalf("repeat sweep=%d %v", count, err)
		}
		if accepted, err := store.BeginRunAttempt(ctx, a.RunID, 2); err != nil || !accepted {
			t.Fatalf("reclaim=%v %v", accepted, err)
		}
		got, err = actions.FinishConnectorAction(ctx, a.ID, claim.Version, claim.DispatchID, models.ConnectorActionObservation{State: models.ConnectorActionResponseObserved, ResponseKind: "rpc_error"}, uuid.NewString())
		if err != nil || got.Generation != 1 {
			t.Fatalf("late original response=%+v %v", got, err)
		}
		if err := store.DeleteRun(ctx, a.RunID); err != nil {
			t.Fatal(err)
		}
		if _, err := actions.GetConnectorAction(ctx, a.ID); err != nil {
			t.Fatalf("run deletion erased evidence: %v", err)
		}
		if count, err := actions.PruneConnectorActions(ctx, time.Now().Add(time.Hour), 100); err != nil || count != 1 {
			t.Fatalf("explicit prune=%d %v", count, err)
		}
		if _, err := actions.GetConnectorAction(ctx, a.ID); err == nil {
			t.Fatal("resolved action survived explicit retention")
		}
	})

	t.Run("stale_dispatch_and_recreated_run", func(t *testing.T) {
		ctx, a := create(t)
		a = ready(t, ctx, a, "allow")
		if _, err := store.BeginRunAttempt(ctx, a.RunID, 2); err != nil {
			t.Fatal(err)
		}
		if _, err := actions.PrepareConnectorActionDispatch(ctx, a.ID, a.Version, "", "policy", time.Now().Add(time.Minute), uuid.NewString()); err == nil {
			t.Fatal("stale attempt received dispatch authority")
		}
		if err := store.DeleteRun(ctx, a.RunID); err != nil {
			t.Fatal(err)
		}
		now := a.RunCreatedAt
		if err := store.CreateRun(ctx, &models.Run{RunID: a.RunID, ThreadID: a.ThreadID, AgentID: a.AgentID, Status: models.RunStatusPending, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.BeginRunAttempt(ctx, a.RunID, 1); err != nil {
			t.Fatal(err)
		}
		if _, err := actions.PrepareConnectorActionDispatch(ctx, a.ID, a.Version, "", "policy", time.Now().Add(time.Minute), uuid.NewString()); err == nil {
			t.Fatal("old action bound to recreated run ID")
		}
	})

	t.Run("approval_authority_does_not_transfer_to_recreated_run", func(t *testing.T) {
		ctx, original := create(t)
		pending := &models.PendingAction{TargetRevision: pendingTargetRevision, ID: uuid.NewString(), TenantID: original.TenantID, AgentID: original.AgentID, RunID: original.RunID, Generation: original.Generation, Connector: original.Connector, Tool: original.Tool, ArgsDigest: original.ArgsDigest}
		if err := approvals.CreatePendingAction(ctx, pending); err != nil {
			t.Fatal(err)
		}
		if pending.RunIncarnationID == "" || pending.RunIncarnationID != original.RunIncarnationID {
			t.Fatalf("approval lacks durable run binding: %+v", pending)
		}
		if err := approvals.SetPendingActionApproved(ctx, pending.ID, "reviewer"); err != nil {
			t.Fatal(err)
		}
		unreviewed := *pending
		unreviewed.ID, unreviewed.Status = uuid.NewString(), models.PendingStatusPending
		if err := approvals.CreatePendingAction(ctx, &unreviewed); err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteRun(ctx, original.RunID); err != nil {
			t.Fatal(err)
		}
		legacy := *pending
		legacy.ID, legacy.RunIncarnationID, legacy.Status = uuid.NewString(), "", models.PendingStatusPending
		if err := approvals.CreatePendingAction(ctx, &legacy); err != nil {
			t.Fatal(err)
		}
		if legacy.RunIncarnationID != "" {
			t.Fatal("legacy fixture acquired authority without a run")
		}
		if err := approvals.SetPendingActionApproved(ctx, legacy.ID, "legacy-reviewer"); err != nil {
			t.Fatal(err)
		}
		if err := store.CreateRun(ctx, &models.Run{RunID: original.RunID, ThreadID: original.ThreadID, AgentID: original.AgentID, Status: models.RunStatusPending, CreatedAt: original.RunCreatedAt, UpdatedAt: original.RunCreatedAt}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.BeginRunAttempt(ctx, original.RunID, original.Generation); err != nil {
			t.Fatal(err)
		}
		input := *original
		input.ID = uuid.NewString()
		replacement, err := actions.CreateConnectorAction(ctx, &input, uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		replacement = ready(t, ctx, replacement, "pending")
		if got, err := actions.PeekApprovedConnectorAction(ctx, replacement.ID); err != nil || got != nil {
			t.Fatalf("old approval transferred into replacement: %+v %v", got, err)
		}
		if _, err := actions.PrepareConnectorActionDispatch(ctx, replacement.ID, replacement.Version, pending.ID, "approved_capability", time.Now().Add(time.Minute), uuid.NewString()); err == nil {
			t.Fatal("replacement consumed original run approval")
		}
		consumer := store.(interface {
			ConsumeApprovedAction(context.Context, string, string, string, int64, string, string, string, string) (string, error)
		})
		if id, err := consumer.ConsumeApprovedAction(ctx, replacement.TenantID, replacement.AgentID, replacement.RunID, replacement.Generation, replacement.Connector, replacement.Tool, replacement.ArgsDigest, pendingTargetRevision); err != nil || id != "" {
			t.Fatalf("legacy consume transferred approval: %q %v", id, err)
		}
		fresh := *pending
		fresh.ID, fresh.RunIncarnationID, fresh.Status = uuid.NewString(), "", models.PendingStatusPending
		if err := approvals.CreatePendingAction(ctx, &fresh); err != nil {
			t.Fatal(err)
		}
		if fresh.ID == pending.ID || fresh.ID == unreviewed.ID || fresh.RunIncarnationID != replacement.RunIncarnationID {
			t.Fatalf("new review reused old authority: %+v", fresh)
		}
		if err := approvals.SetPendingActionApproved(ctx, unreviewed.ID, "late-reviewer"); err == nil {
			t.Fatal("review of an earlier run's pending action was accepted")
		}
		old, err := approvals.GetPendingAction(ctx, pending.ID)
		if err != nil || old.Status != models.PendingStatusApproved {
			t.Fatalf("old approval was consumed: %+v %v", old, err)
		}
	})

	t.Run("original_receipt_survives_run_deletion", func(t *testing.T) {
		ctx, a := create(t)
		a = ready(t, ctx, a, "allow")
		claim := prepare(t, ctx, a)
		if err := store.DeleteRun(ctx, a.RunID); err != nil {
			t.Fatal(err)
		}
		got, err := actions.FinishConnectorAction(ctx, a.ID, claim.Version, claim.DispatchID, models.ConnectorActionObservation{State: models.ConnectorActionResponseObserved, ResponseKind: "rpc_result"}, uuid.NewString())
		if err != nil || got.State != models.ConnectorActionResponseObserved || !got.RunCreatedAt.Equal(a.RunCreatedAt) || got.Generation != a.Generation {
			t.Fatalf("deleted-run observation=%+v %v", got, err)
		}
	})

	t.Run("explicit_deny_and_break_glass_provenance", func(t *testing.T) {
		ctx, a := create(t)
		a = ready(t, ctx, a, "deny")
		if _, err := actions.PrepareConnectorActionDispatch(ctx, a.ID, a.Version, "", "policy", time.Now().Add(time.Minute), uuid.NewString()); err == nil {
			t.Fatal("deny dispatched")
		}
		ctx, b := create(t)
		b, err := actions.RecordConnectorActionDecision(ctx, b.ID, b.Version, models.ConnectorActionDecision{Effect: "deny", AuthorizationSource: "break_glass", BreakGlassID: "window"}, uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		claim, err := actions.PrepareConnectorActionDispatch(ctx, b.ID, b.Version, "", "break_glass", time.Now().Add(time.Minute), uuid.NewString())
		if err != nil || !claim.Applied {
			t.Fatalf("break glass=%+v %v", claim, err)
		}
		b, err = actions.GetConnectorAction(ctx, b.ID)
		if err != nil || b.Decision != "deny" || b.BreakGlassID != "window" || b.AuthorizationSource != "break_glass" {
			t.Fatalf("raw decision lost=%+v %v", b, err)
		}
	})

	t.Run("pagination_and_typed_metadata", func(t *testing.T) {
		ctx, a := create(t)
		b := *a
		b.ID = uuid.NewString()
		if _, err := actions.CreateConnectorAction(ctx, &b, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		first, err := actions.SearchConnectorActions(ctx, &models.ConnectorActionSearchRequest{Limit: 1})
		if err != nil || len(first) != 1 {
			t.Fatalf("page=%+v %v", first, err)
		}
		second, err := actions.SearchConnectorActions(ctx, &models.ConnectorActionSearchRequest{Limit: 1, Cursor: pagecursor.EncodeTime(first[0].CreatedAt, first[0].ID)})
		if err != nil || len(second) != 1 || second[0].ID == first[0].ID {
			t.Fatalf("second page=%+v %v", second, err)
		}
		raw, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]interface{}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"args", "arguments", "request_body", "response_body", "credentials", "access_token"} {
			if _, ok := body[field]; ok {
				t.Fatalf("payload field retained: %s", field)
			}
		}
		foreign := tenant.WithContext(context.Background(), a.TenantID+"other")
		_, err = actions.RecordConnectorActionDecision(foreign, a.ID, a.Version, models.ConnectorActionDecision{Effect: "allow"}, uuid.NewString())
		var missing *state.ErrNotFound
		if !errors.As(err, &missing) {
			t.Fatalf("foreign mutation=%v", err)
		}
	})
}
