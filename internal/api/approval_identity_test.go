package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/google/uuid"
)

type unverifiedApprovalProvider struct{}

func (unverifiedApprovalProvider) Authenticate(*http.Request) (*auth.AuthResult, error) {
	result := &auth.AuthResult{Identity: "jwt-user", TenantID: "a", Permissions: []string{"admin"}}
	auth.MarkIdentityUnverified(result)
	return result, nil
}

func TestStrictApprovalDoesNotAcceptCompatibilitySubjectOrLegacySession(t *testing.T) {
	env := newReviewFixture(t)
	env.server.SetPolicyEngine(quorumPolicy(env, policy.ApprovalRequirements{RequiredApprovals: 2}, false))
	ctx := tenant.ScopedContext(t.Context(), "a")
	binding := &auth.RunBinding{RunID: uuid.NewString(), ThreadID: uuid.NewString(), TenantID: "a", AgentID: "agent", Generation: 1, User: &transport.UserContext{Identity: "requester"}}
	seedManagedPendingFixture(t, env.store, binding, true)
	_, digest, args := policy.BindArgs(json.RawMessage(`{"amount":123}`))
	row := &models.PendingAction{ID: pendingActionIDPrefix + uuid.NewString(), RunID: binding.RunID, TenantID: "a", AgentID: "agent", Generation: 1, Connector: "files", Tool: "write", ArgsDigest: digest, Args: args, TargetRevision: currentFixtureTargetRevision(t, env.server, "files"), PolicyRevision: env.server.policy.Revision(), RequiredApprovals: 2}
	if err := env.store.CreatePendingAction(ctx, row); err != nil {
		t.Fatal(err)
	}
	handler := auth.MiddlewareWithOpts(unverifiedApprovalProvider{}, nil, nil, auth.MiddlewareOpts{StrictPermissions: true}, env.server.Handler())
	for _, surface := range []string{"admin-api", "review-api"} {
		response := reviewRequest(handler, http.MethodPost, "/"+surface+"/pending-actions/"+row.ID+"/approve", "synthetic", "", nil, "")
		if response.Code != http.StatusConflict && response.Code != http.StatusForbidden {
			t.Fatalf("compatibility subject voted on %s: %d %s", surface, response.Code, response.Body.String())
		}
	}
	// A pre-upgrade Redis blob carries display identity/roles but no proof.
	var legacy auth.AuthResult
	if err := json.Unmarshal([]byte(`{"Identity":"legacy-admin","TenantID":"a","Permissions":["admin"]}`), &legacy); err != nil {
		t.Fatal(err)
	}
	sessions := auth.NewAdminSessionStore(0)
	session, err := sessions.Create(&legacy)
	if err != nil {
		t.Fatal(err)
	}
	handler = auth.MiddlewareWithOpts(nil, nil, nil, auth.MiddlewareOpts{StrictPermissions: true, AdminSessions: sessions}, env.server.Handler())
	response := reviewRequest(handler, http.MethodPost, "/admin-api/pending-actions/"+row.ID+"/approve", "", "", &http.Cookie{Name: auth.CookieAdminSession, Value: session.ID}, session.CSRF)
	if response.Code != http.StatusConflict {
		t.Fatalf("legacy cookie counted as verified vote: %d %s", response.Code, response.Body.String())
	}
	stored, err := env.store.GetPendingAction(ctx, row.ID)
	if err != nil || stored.ApprovalCount != 0 || stored.Status != models.PendingStatusPending {
		t.Fatalf("unverified subjects changed authority: row=%+v err=%v", stored, err)
	}
}

func TestStandaloneSeparationRequiresVerifiedRequester(t *testing.T) {
	env := newReviewFixture(t)
	env.server.SetPolicyEngine(quorumPolicy(env, policy.ApprovalRequirements{RequiredApprovals: 1, ExcludeRequester: true}, true))
	handler := auth.MiddlewareWithOpts(unverifiedApprovalProvider{}, nil, nil, auth.MiddlewareOpts{StrictPermissions: true}, env.server.Handler())
	operation := "app-operation-" + uuid.NewString()
	body, _ := json.Marshal(map[string]any{"agent_id": "agent", "connector": "files", "tool": "write", "run_id": operation, "args": map[string]any{"amount": 3}})
	response := reviewRequest(handler, http.MethodPost, "/governance/decide", "synthetic", string(body), nil, "")
	if response.Code != http.StatusForbidden || !json.Valid(response.Body.Bytes()) {
		t.Fatalf("unverified requester issued held authority: %d %s", response.Code, response.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result["reason_code"] != "verified_requester_required" {
		t.Fatalf("unsafe requester error: %s %v", response.Body.String(), err)
	}
	rows, err := env.store.SearchPendingActions(tenant.ScopedContext(t.Context(), "a"), &models.PendingActionSearchRequest{RunID: operation})
	if err != nil || len(rows) != 0 {
		t.Fatalf("unverified requester persisted pending authority: %+v %v", rows, err)
	}
}

func TestManagedRequesterProofCannotComeFromRunMetadataOrA2AClaims(t *testing.T) {
	env := newReviewFixture(t)
	env.server.SetPolicyEngine(quorumPolicy(env, policy.ApprovalRequirements{RequiredApprovals: 1, ExcludeRequester: true}, true))
	ctx := tenant.ScopedContext(t.Context(), "a")
	if err := env.store.UpsertAgent(ctx, &models.Agent{AgentID: "agent", Name: "agent"}); err != nil {
		t.Fatal(err)
	}
	handler := auth.MiddlewareWithOpts(unverifiedApprovalProvider{}, nil, nil, auth.MiddlewareOpts{StrictPermissions: true}, env.server.Handler())
	response := reviewRequest(handler, http.MethodPost, "/runs", "synthetic", `{"agent_id":"agent","input":{},"metadata":{"run_manifest":{"principal":{"identity":"forged","verified_identity":true}}}}`, nil, "")
	if response.Code != http.StatusOK {
		t.Fatalf("create: %d %s", response.Code, response.Body.String())
	}
	var run models.Run
	if err := json.Unmarshal(response.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	frozen, _ := json.Marshal(run.Metadata["run_manifest"])
	var manifest models.RunManifest
	if err := json.Unmarshal(frozen, &manifest); err != nil || manifest.Principal == nil || manifest.Principal.Identity != "jwt-user" || manifest.Principal.VerifiedIdentity {
		t.Fatalf("caller metadata promoted requester provenance: %s %v", frozen, err)
	}
	if _, err := env.store.BeginRunAttempt(ctx, run.RunID, 1); err != nil {
		t.Fatal(err)
	}
	_, digest, args := policy.BindArgs(json.RawMessage(`{"amount":123}`))
	row := &models.PendingAction{ID: pendingActionIDPrefix + uuid.NewString(), RunID: run.RunID, TenantID: "a", AgentID: "agent", Generation: 1, Connector: "files", Tool: "write", ArgsDigest: digest, Args: args, TargetRevision: currentFixtureTargetRevision(t, env.server, "files"), PolicyRevision: env.server.policy.Revision(), ExcludeRequester: true}
	if err := env.store.CreatePendingAction(ctx, row); err == nil {
		t.Fatal("unverified durable requester minted strict pending authority")
	}
	for _, verified := range []bool{false, true} {
		parent := &models.Run{TenantID: "a", Metadata: map[string]interface{}{"run_manifest": models.RunManifest{Principal: &models.RunManifestPrincipal{Identity: "requester", Permissions: []string{"write"}, VerifiedIdentity: verified}}}}
		user := &transport.UserContext{Identity: "requester", Permissions: []string{"write"}, Extra: map[string]interface{}{"verified_identity": true}}
		delegated, err := a2aDelegatedPrincipal(user, parent, json.RawMessage(`{"extra":{"verified_identity":true}}`))
		if err != nil || auth.HasVerifiedIdentity(delegated) != verified {
			t.Fatalf("delegation promoted claims instead of inheriting frozen proof: verified=%v result=%+v err=%v", verified, delegated, err)
		}
	}
}
