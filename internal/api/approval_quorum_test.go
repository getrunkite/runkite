package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/google/uuid"
)

func quorumAPIHandler(env *reviewFixture) http.Handler {
	provider := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"requester": {Name: "requester", TenantID: "a", Permissions: []string{"admin"}},
		"vote-one":  {Name: "reviewer-one", TenantID: "a", Permissions: []string{auth.GovernanceApprovePermission}},
		// Rotating a credential for one subject must not create another vote.
		"vote-one-rotated": {Name: "reviewer-one", TenantID: "a", Permissions: []string{auth.GovernanceApprovePermission}},
		"vote-two":         {Name: "reviewer-two", TenantID: "a", Permissions: []string{"admin"}},
	})
	return auth.MiddlewareWithOpts(provider, nil, nil, auth.MiddlewareOpts{StrictPermissions: true}, env.server.Handler())
}

func quorumPolicy(env *reviewFixture, requirements policy.ApprovalRequirements, hold bool) *policy.Engine {
	cfg := policy.Config{ForceEnable: true, DefaultEffect: policy.EffectAllow,
		AuditRequired: true, Auditor: policy.NewStoreAuditor(env.store), ApprovalRequirements: requirements}
	if hold {
		cfg.MandatoryHITL = []policy.MandatoryHITLRule{{TenantID: "a", AgentID: "agent", Connector: "files", Tools: []string{"write"}}}
	}
	return policy.New(cfg)
}

func TestApprovalQuorumAPIRejectsSelfReviewAcrossAdminAndReview(t *testing.T) {
	env := newReviewFixture(t)
	requirements := policy.ApprovalRequirements{RequiredApprovals: 2, ExcludeRequester: true}
	env.server.SetPolicyEngine(quorumPolicy(env, requirements, false))
	handler := quorumAPIHandler(env)
	ctx := tenant.ScopedContext(t.Context(), "a")
	binding := &auth.RunBinding{RunID: uuid.NewString(), ThreadID: uuid.NewString(), AgentID: "agent", TenantID: "a", Generation: 1,
		User: &transport.UserContext{Identity: "requester", Permissions: []string{"admin"}}}
	seedManagedPendingFixture(t, env.store, binding, true)
	_, digest, args := policy.BindArgs(json.RawMessage(`{"amount":123}`))
	row := &models.PendingAction{ID: pendingActionIDPrefix + uuid.NewString(), RunID: binding.RunID,
		Generation: 1, TenantID: "a", AgentID: "agent", Connector: "files", Tool: "write",
		ArgsDigest: digest, Args: args, TargetRevision: currentFixtureTargetRevision(t, env.server, "files"),
		PolicyRevision: env.server.policy.Revision(), RequiredApprovals: 2, ExcludeRequester: true}
	if err := env.store.CreatePendingAction(ctx, row); err != nil {
		t.Fatal(err)
	}
	if row.RequesterIdentity != "requester" {
		t.Fatalf("requester was not derived from stored run: %+v", row)
	}
	for _, surface := range []string{"admin-api", "review-api"} {
		response := reviewRequest(handler, http.MethodPost, "/"+surface+"/pending-actions/"+row.ID+"/approve", "requester", "", nil, "")
		if response.Code != http.StatusConflict {
			t.Fatalf("self approval on %s: %d %s", surface, response.Code, response.Body.String())
		}
		stored, err := env.store.GetPendingAction(ctx, row.ID)
		if err != nil || stored.Status != models.PendingStatusPending || stored.ApprovalCount != 0 {
			t.Fatalf("self vote changed authority: row=%+v err=%v", stored, err)
		}
	}

	approve := func(surface, credential string) *models.PendingAction {
		t.Helper()
		response := reviewRequest(handler, http.MethodPost, "/"+surface+"/pending-actions/"+row.ID+"/approve", credential, "", nil, "")
		if response.Code != http.StatusOK {
			t.Fatalf("approve %s: %d %s", credential, response.Code, response.Body.String())
		}
		var result models.PendingAction
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return &result
	}
	first := approve("review-api", "vote-one")
	if first.Status != models.PendingStatusPending || first.ApprovalCount != 1 {
		t.Fatalf("one vote reached quorum: %+v", first)
	}
	duplicate := approve("review-api", "vote-one-rotated")
	if duplicate.Status != models.PendingStatusPending || duplicate.ApprovalCount != 1 {
		t.Fatalf("same identity counted twice: %+v", duplicate)
	}
	consume := func() string {
		t.Helper()
		id, err := env.store.ConsumeApprovedAction(ctx, "a", "agent", binding.RunID, 1, "files", "write", digest, row.TargetRevision, row.PolicyRevision, row.ToolRevision)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	if id := consume(); id != "" {
		t.Fatalf("partial approval consumed authority: %q", id)
	}
	second := approve("admin-api", "vote-two")
	if second.Status != models.PendingStatusApproved || second.ApprovalCount != 2 {
		t.Fatalf("two distinct reviewers did not reach quorum: %+v", second)
	}
	if id := consume(); id != row.ID {
		t.Fatalf("quorum did not mint exact capability: %q", id)
	}
	if id := consume(); id != "" {
		t.Fatalf("quorum minted reusable authority: %q", id)
	}
	stored, err := env.store.GetPendingAction(ctx, row.ID)
	if err != nil || stored.Status != models.PendingStatusConsumed || stored.ApprovalCount != 2 || len(stored.Approvals) != 2 {
		t.Fatalf("consumption lost vote history: row=%+v err=%v", stored, err)
	}
	identities := map[string]bool{}
	for _, vote := range stored.Approvals {
		identities[vote.ReviewerIdentity] = true
	}
	if !identities["reviewer-one"] || !identities["reviewer-two"] || identities["requester"] {
		t.Fatalf("wrong reviewers retained: %+v", stored.Approvals)
	}
}

func TestStandaloneApprovalPollRejectsChangedQuorumPlan(t *testing.T) {
	for _, initiallyApproved := range []bool{false, true} {
		t.Run(fmt.Sprintf("initially-approved=%v", initiallyApproved), func(t *testing.T) {
			env := newReviewFixture(t)
			env.server.SetPolicyEngine(quorumPolicy(env, policy.ApprovalRequirements{RequiredApprovals: 1}, true))
			handler := quorumAPIHandler(env)
			body := fmt.Sprintf(`{"agent_id":"agent","connector":"files","tool":"write","run_id":%q,"args":{"amount":3}}`, "app-operation-"+uuid.NewString())
			decide := func() governanceDecideResponse {
				t.Helper()
				response := reviewRequest(handler, http.MethodPost, "/governance/decide", "requester", body, nil, "")
				if response.Code != http.StatusOK {
					t.Fatalf("decide: %d %s", response.Code, response.Body.String())
				}
				var decision governanceDecideResponse
				if err := json.Unmarshal(response.Body.Bytes(), &decision); err != nil {
					t.Fatal(err)
				}
				if decision.Effect != policy.EffectPending || decision.ActionID == "" {
					t.Fatalf("not held: %+v", decision)
				}
				return decision
			}
			decision := decide()
			ctx := tenant.ScopedContext(t.Context(), "a")
			row, err := env.store.GetPendingAction(ctx, decision.ActionID)
			if err != nil || row.ExpiresAt == nil || row.RunIncarnationID != "" {
				t.Fatalf("standalone decision acquired managed run meaning: row=%+v err=%v", row, err)
			}
			if initiallyApproved {
				response := reviewRequest(handler, http.MethodPost, "/review-api/pending-actions/"+row.ID+"/approve", "vote-one", "", nil, "")
				if response.Code != http.StatusOK {
					t.Fatalf("approve: %d %s", response.Code, response.Body.String())
				}
			}
			poll := func() governanceActionResponse {
				t.Helper()
				response := reviewRequest(handler, http.MethodGet, "/governance/actions/"+row.ID, "requester", "", nil, "")
				if response.Code != http.StatusOK {
					t.Fatalf("poll: %d %s", response.Code, response.Body.String())
				}
				var status governanceActionResponse
				if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
					t.Fatal(err)
				}
				return status
			}
			want := models.PendingStatusPending
			if initiallyApproved {
				want = models.PendingStatusApproved
			}
			if got := poll(); got.Status != want {
				t.Fatalf("initial poll: %+v", got)
			}
			env.server.SetPolicyEngine(quorumPolicy(env, policy.ApprovalRequirements{RequiredApprovals: 2, ExcludeRequester: true}, true))
			if got := poll(); got.Status != "rejected" || got.ReasonCode != "approval_requirements_changed" {
				t.Fatalf("old approval plan remained usable: %+v", got)
			}
			stored, err := env.store.GetPendingAction(ctx, row.ID)
			if err != nil || stored.Status != want {
				t.Fatalf("poll rewrote historical authority: row=%+v err=%v", stored, err)
			}
			fresh := decide()
			if fresh.ActionID == row.ID {
				t.Fatal("changed approval plan reused prior decision")
			}
			freshRow, err := env.store.GetPendingAction(ctx, fresh.ActionID)
			if err != nil || freshRow.RequiredApprovals != 2 || !freshRow.ExcludeRequester || freshRow.RequesterIdentity != "requester" {
				t.Fatalf("fresh review did not bind stricter plan: row=%+v err=%v", freshRow, err)
			}
		})
	}
}
