package sqlite

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/google/uuid"
)

func TestStrictPendingAuthorityRequiresDurableVerifiedRequesterAtEveryTransition(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := tenant.ScopedContext(context.Background(), "acme")
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.UpsertAgent(ctx, &models.Agent{AgentID: "agent", Name: "agent"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateThread(ctx, &models.Thread{ThreadID: "thread", Status: models.ThreadStatusBusy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	metadata := func(verified bool) []byte {
		value, err := json.Marshal(map[string]any{"run_manifest": models.RunManifest{Principal: &models.RunManifestPrincipal{Identity: "requester", VerifiedIdentity: verified}}})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	var initial map[string]interface{}
	if err := json.Unmarshal(metadata(true), &initial); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, &models.Run{RunID: "run", ThreadID: "thread", AgentID: "agent", Status: models.RunStatusPending, Metadata: initial, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginRunAttempt(ctx, "run", 1); err != nil {
		t.Fatal(err)
	}
	row := &models.PendingAction{ID: uuid.NewString(), TenantID: "acme", AgentID: "agent", RunID: "run", Generation: 1, Connector: "fixture", Tool: "write", ArgsDigest: strings.Repeat("a", 64), TargetRevision: testPendingTargetRevision, PolicyRevision: testPendingPolicyRevision, ExcludeRequester: true}
	if err := s.CreatePendingAction(ctx, row); err != nil {
		t.Fatal(err)
	}
	setProof := func(verified bool) {
		t.Helper()
		// A legacy stored manifest can contain the same display identity without
		// the new server-owned proof. It must not inherit strict authority.
		current, err := s.GetRun(ctx, "run")
		if err != nil {
			t.Fatal(err)
		}
		current.Metadata["run_manifest"] = models.RunManifest{Principal: &models.RunManifestPrincipal{Identity: "requester", VerifiedIdentity: verified}}
		encoded, err := json.Marshal(current.Metadata)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE runs SET metadata=? WHERE run_id=?`, encoded, "run"); err != nil {
			t.Fatal(err)
		}
	}
	setProof(false)
	if err := s.SetPendingActionApproved(ctx, row.ID, "reviewer"); err == nil {
		t.Fatal("legacy requester provenance accepted a vote")
	}
	setProof(true)
	if err := s.SetPendingActionApproved(ctx, row.ID, "reviewer"); err != nil {
		t.Fatal(err)
	}
	target, revision := testPendingTargetRevision, testPendingPolicyRevision
	a, err := s.CreateConnectorAction(ctx, &models.ConnectorAction{ID: uuid.NewString(), TenantID: "acme", AgentID: "agent", RunID: "run", ThreadID: "thread", Generation: 1, Connector: "fixture", Tool: "write", ArgsDigest: row.ArgsDigest, TargetRevision: &target, PolicyRevision: &revision}, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	a, err = s.RecordConnectorActionDecision(ctx, a.ID, a.Version, models.ConnectorActionDecision{Effect: "pending", AuthorizationSource: "policy"}, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	setProof(false)
	if id, err := s.ConsumeApprovedAction(ctx, "acme", "agent", "run", 1, "fixture", "write", row.ArgsDigest, target, revision, ""); err == nil || id != "" {
		t.Fatalf("legacy requester provenance consumed authority: %q %v", id, err)
	}
	if found, err := s.PeekApprovedConnectorAction(ctx, a.ID); err == nil || found != nil {
		t.Fatalf("legacy requester provenance supplied a capability: %+v %v", found, err)
	}
	if _, err := s.PrepareConnectorActionDispatch(ctx, a.ID, a.Version, row.ID, "approved_capability", now.Add(time.Minute), uuid.NewString()); err == nil {
		t.Fatal("legacy requester provenance committed dispatch")
	}
	stored, err := s.GetPendingAction(ctx, row.ID)
	if err != nil || stored.Status != models.PendingStatusApproved || stored.ApprovalCount != 1 {
		t.Fatalf("rejected provenance consumed or rewrote history: %+v %v", stored, err)
	}
}
