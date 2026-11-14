package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/connector"
	"github.com/getrunkite/runkite/internal/credentials"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
)

type actionCredentialProvider struct {
	issued, revoked, renewed atomic.Int32
	issueHook                func(context.Context)
	issueErr, revokeErr      error
}

func (p *actionCredentialProvider) Issue(ctx context.Context) (credentials.Grant, error) {
	p.issued.Add(1)
	if p.issueHook != nil {
		p.issueHook(ctx)
	}
	if p.issueErr != nil {
		return credentials.Grant{}, p.issueErr
	}
	value, _ := credentials.NewUserPassword("synthetic-vault-user", "synthetic-vault-password")
	return credentials.Grant{Handle: "database/creds/readonly/" + uuid.NewString(), TTL: time.Minute, Renewable: true, Credential: value}, nil
}
func (p *actionCredentialProvider) Renew(_ context.Context, handle string, _ time.Duration) (credentials.Grant, error) {
	p.renewed.Add(1)
	return credentials.Grant{Handle: handle, TTL: 30 * time.Second, Renewable: true}, nil
}
func (p *actionCredentialProvider) Revoke(context.Context, string) error {
	p.revoked.Add(1)
	return p.revokeErr
}
func (p *actionCredentialProvider) Lookup(context.Context, string) (credentials.LeaseStatus, error) {
	return credentials.LeaseStatus{Found: true, TTL: time.Minute, Renewable: true}, nil
}

func newVaultActionFixture(t *testing.T, authority string) (*actionAPIEnv, *actionCredentialProvider) {
	t.Helper()
	env := newActionAPIEnv(t, "json", authority)
	run, err := env.store.GetRun(env.ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	env.binding.RunIncarnationID = run.IncarnationID
	live, err := env.queue.LookupInflight(env.ctx, "run")
	if err != nil || live == nil {
		t.Fatal(err)
	}
	if _, err := env.queue.Ack(env.ctx, "run", live.Generation); err != nil {
		t.Fatal(err)
	}
	live.RunIncarnationID = run.IncarnationID
	if err := env.queue.Enqueue(env.ctx, live); err != nil {
		t.Fatal(err)
	}
	if _, err := env.queue.Dequeue(env.ctx, live.RunnerKind, time.Second); err != nil {
		t.Fatal(err)
	}
	store, err := env.store.CredentialLeaseStorage()
	if err != nil {
		t.Fatal(err)
	}
	keys, err := credentials.NewKeyring("fixture", map[string][]byte{"fixture": []byte(strings.Repeat("f", 32))})
	if err != nil {
		t.Fatal(err)
	}
	p := &actionCredentialProvider{}
	m, err := credentials.NewManager(store, keys, []credentials.Binding{{ID: "reporting", Revision: "fixture-vault-role-1", ConsumerID: "fixture", MaxTTL: time.Minute, RenewIncrement: 30 * time.Second, Provider: p}}, env.s.CheckCredentialOwner)
	if err != nil {
		t.Fatal(err)
	}
	env.s.SetCredentialManager(m)
	entry, err := env.s.connectors.Get("fixture")
	if err != nil {
		t.Fatal(err)
	}
	cfg := entry.Config
	cfg.Auth = connector.AuthConfig{Type: "vault_database", CredentialBinding: "reporting", CredentialBindingRevision: "fixture-vault-role-1", AllowInsecureLoopback: true}
	env.s.SetConnectorRegistry(connector.NewRegistry(map[string]connector.ConnectorConfig{"fixture": cfg}))
	env.setTool(func(w http.ResponseWriter, r *http.Request, id json.RawMessage) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "synthetic-vault-user" || password != "synthetic-vault-password" {
			t.Error("protected executor did not receive private Basic credentials")
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(id) + `,"result":{"content":[{"type":"text","text":"synthetic-sensitive-result"}]}}`))
	})
	return env, p
}

func assertCredentialSafe(t *testing.T, raw string) {
	t.Helper()
	for _, secret := range []string{"synthetic-vault-user", "synthetic-vault-password", "database/creds/readonly/", "protected_handle", "lease-v1."} {
		if strings.Contains(raw, secret) {
			t.Fatal("credential material escaped metadata or response")
		}
	}
}

func TestVaultConnectorRequiresAuthorizationBeforeIssue(t *testing.T) {
	for _, authority := range []string{"policy", "approved", "deny"} {
		t.Run(authority, func(t *testing.T) {
			env, p := newVaultActionFixture(t, authority)
			if authority == "deny" {
				env.s.SetPolicyEngine(policy.New(policy.Config{ForceEnable: true, DefaultEffect: policy.EffectDeny, AuditRequired: true, ActionEvidence: true, Auditor: policy.NewStoreAuditor(env.store)}))
			}
			session, err := env.s.connectors.GetSession(env.ctx, "fixture", nil)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(session)
			assertCredentialSafe(t, string(raw))
			if p.issued.Load() != 0 {
				t.Fatal("session/prewarm issued credentials")
			}
			response := env.call(t)
			assertCredentialSafe(t, response.Body.String())
			if authority != "policy" {
				if p.issued.Load() != 0 || env.effects.Load() != 0 {
					t.Fatal("held or denied tool acquired a credential")
				}
				return
			}
			if response.Code != 200 || env.effects.Load() != 1 || p.issued.Load() != 1 || p.revoked.Load() != 1 {
				t.Fatalf("authorized credential consumer failed: code=%d issue=%d effect=%d revoke=%d", response.Code, p.issued.Load(), env.effects.Load(), p.revoked.Load())
			}
			action, err := env.store.GetConnectorAction(env.ctx, response.Header().Get(headerConnectorActionID))
			if err != nil || action.CredentialRef == nil || action.State != models.ConnectorActionResponseObserved || action.RunIncarnationID != env.binding.RunIncarnationID {
				t.Fatal("missing credential action provenance", err)
			}
			lease, err := env.s.credentialManager.Get(env.ctx, "acme", *action.CredentialRef)
			if err != nil || lease.State != credentials.StateRevoked || lease.Owner.ActionID != action.ID {
				t.Fatal("lease not correlated and cleaned", err)
			}
			raw, _ = json.Marshal([]any{action, lease})
			assertCredentialSafe(t, string(raw))
		})
	}
}

func TestVaultApprovalConsumptionAndIntentPrecedeIssuance(t *testing.T) {
	env, p := newVaultActionFixture(t, "approved")
	approvalID := env.approve(t)
	p.issueHook = func(ctx context.Context) {
		rows, err := env.s.credentialManager.Search(ctx, "acme", 10)
		if err != nil || len(rows) != 1 {
			t.Fatal("no durable issuance intent", err)
		}
		action, err := env.store.GetConnectorAction(ctx, rows[0].Owner.ActionID)
		if err != nil || action.State != models.ConnectorActionDispatchIntent || action.PendingActionID != approvalID || action.AuthorizationSource != "approved_capability" || action.CredentialRef == nil || *action.CredentialRef != rows[0].ID {
			t.Fatal("Vault contacted before exact approval and intent committed", err)
		}
		approval, err := env.store.GetPendingAction(ctx, approvalID)
		if err != nil || approval.Status != models.PendingStatusConsumed {
			t.Fatal("Vault contacted before approval consumption", err)
		}
	}
	response := env.call(t)
	if response.Code != 200 || env.effects.Load() != 1 || p.issued.Load() != 1 {
		t.Fatalf("approved Vault call failed: %d %s", response.Code, response.Body.String())
	}
}

func TestVaultIntentFailureCannotIssue(t *testing.T) {
	env, p := newVaultActionFixture(t, "approved")
	approvalID := env.approve(t)
	env.store.failPrepare.Store(true)
	response := env.call(t)
	if response.Code != http.StatusServiceUnavailable || p.issued.Load() != 0 || env.effects.Load() != 0 {
		t.Fatal("failed intent issued a credential")
	}
	approval, err := env.store.GetPendingAction(env.ctx, approvalID)
	if err != nil || approval.Status != models.PendingStatusApproved {
		t.Fatal("failed intent consumed authority", err)
	}
}

func TestVaultCleanupFailurePreservesObservedToolResponse(t *testing.T) {
	env, p := newVaultActionFixture(t, "policy")
	p.revokeErr = errors.New("synthetic-vault-password must not appear in errors")
	response := env.call(t)
	assertCredentialSafe(t, response.Body.String())
	if response.Code != 200 || !strings.Contains(response.Body.String(), "synthetic-sensitive-result") || env.effects.Load() != 1 || p.issued.Load() != 1 {
		t.Fatal("cleanup rewrote or retried an observed tool response")
	}
	action, err := env.store.GetConnectorAction(env.ctx, response.Header().Get(headerConnectorActionID))
	if err != nil || action.State != models.ConnectorActionResponseObserved {
		t.Fatal("cleanup erased observed evidence", err)
	}
	lease, err := env.s.credentialManager.Get(env.ctx, "acme", *action.CredentialRef)
	if err != nil || lease.State != credentials.StateRevocationUnknown {
		t.Fatal("cleanup falsely confirmed", err)
	}
}

func TestVaultUnknownIssuanceDoesNotDispatchOrExposeProviderError(t *testing.T) {
	env, p := newVaultActionFixture(t, "policy")
	p.issueErr = errors.New("synthetic-vault-password provider raw message")
	response := env.call(t)
	assertCredentialSafe(t, response.Body.String())
	if response.Code != http.StatusBadGateway || env.effects.Load() != 0 || p.issued.Load() != 1 {
		t.Fatal("unknown issuance sent the protected tool")
	}
	action, err := env.store.GetConnectorAction(env.ctx, response.Header().Get(headerConnectorActionID))
	if err != nil || action.State != models.ConnectorActionOutcomeUnknown || action.ReasonCode != "credential_phase_failed" {
		t.Fatal("credential intent outcome was misrepresented", err)
	}
	lease, err := env.s.credentialManager.Get(env.ctx, "acme", *action.CredentialRef)
	if err != nil || lease.State != credentials.StateIssuanceUnknown {
		t.Fatal("unknown grant was hidden", err)
	}
}

func TestVaultMissingOrStaleIncarnationCannotIssue(t *testing.T) {
	for _, nonce := range []string{"", "stale-incarnation"} {
		t.Run(nonce, func(t *testing.T) {
			env, p := newVaultActionFixture(t, "policy")
			env.binding.RunIncarnationID = nonce
			response := env.call(t)
			if response.Code != 503 || p.issued.Load() != 0 || env.effects.Load() != 0 {
				t.Fatal("unbound or stale incarnation issued credentials")
			}
		})
	}
}

func replaceVaultAssignment(t *testing.T, env *actionAPIEnv, change func(*transport.RunAssignment)) {
	t.Helper()
	live, err := env.queue.LookupInflight(env.ctx, "run")
	if err != nil || live == nil {
		t.Fatal(err)
	}
	change(live)
	if err := env.queue.Enqueue(env.ctx, live); err != nil {
		t.Fatal(err)
	}
	if _, err := env.queue.Dequeue(env.ctx, live.RunnerKind, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestVaultCancelReclaimAndIdentityChangeDuringIssueNeverReleaseToMCP(t *testing.T) {
	for _, scenario := range []string{"cancel", "reclaim", "principal", "thread", "nonce", "policy"} {
		t.Run(scenario, func(t *testing.T) {
			env, p := newVaultActionFixture(t, "policy")
			p.issueHook = func(context.Context) {
				switch scenario {
				case "cancel":
					if err := env.queue.Cancel(env.ctx, "run"); err != nil {
						t.Fatal(err)
					}
				case "reclaim":
					if ok, err := env.store.BeginRunAttempt(env.ctx, "run", 3); err != nil || !ok {
						t.Fatal(err)
					}
					replaceVaultAssignment(t, env, func(a *transport.RunAssignment) { a.Generation = 3 })
				case "principal":
					replaceVaultAssignment(t, env, func(a *transport.RunAssignment) { a.User = &transport.UserContext{Identity: "mallory"} })
				case "thread":
					replaceVaultAssignment(t, env, func(a *transport.RunAssignment) { a.ThreadID = "foreign-thread" })
				case "nonce":
					replaceVaultAssignment(t, env, func(a *transport.RunAssignment) { a.RunIncarnationID = "replacement-incarnation" })
				case "policy":
					env.s.SetPolicyEngine(policy.New(policy.Config{ForceEnable: true, DefaultEffect: policy.EffectDeny, AuditRequired: true, ActionEvidence: true, Auditor: policy.NewStoreAuditor(env.store)}))
				}
			}
			response := env.call(t)
			assertCredentialSafe(t, response.Body.String())
			if response.Code != 502 || p.issued.Load() != 1 || p.revoked.Load() != 1 || env.effects.Load() != 0 {
				t.Fatalf("stale issuer escaped to MCP: code=%d issue=%d revoke=%d effect=%d", response.Code, p.issued.Load(), p.revoked.Load(), env.effects.Load())
			}
		})
	}
}

func TestCredentialMetadataAndManualMutationScopes(t *testing.T) {
	env, p := newVaultActionFixture(t, "policy")
	p.revokeErr = errors.New("synthetic outage")
	response := env.call(t)
	action, err := env.store.GetConnectorAction(env.ctx, response.Header().Get(headerConnectorActionID))
	if err != nil {
		t.Fatal(err)
	}
	id := *action.CredentialRef
	for _, selected := range []string{"", "other", "acme"} {
		r := httptest.NewRequest("GET", "/admin-api/credential-leases/"+id+"?tenant_id="+selected, nil)
		r.SetPathValue("id", id)
		w := httptest.NewRecorder()
		env.s.handleAdminCredentialLease(w, r)
		assertCredentialSafe(t, w.Body.String())
		want := 200
		switch selected {
		case "":
			want = 400
		case "other":
			want = 404
		}
		if w.Code != want {
			t.Fatal("metadata scope mismatch", w.Code, want)
		}
	}
	for _, selected := range []string{"acme", "other"} {
		r := httptest.NewRequest("GET", "/review-api/credential-leases/"+id, nil)
		r.SetPathValue("id", id)
		r = r.WithContext(auth.WithContext(tenant.SystemContext(r.Context()), &auth.AuthResult{Identity: "reader", TenantID: selected, Permissions: []string{"governance:read"}}))
		w := httptest.NewRecorder()
		env.s.handleReviewCredentialLease(w, r)
		want := 200
		if selected != "acme" {
			want = 404
		}
		if w.Code != want {
			t.Fatal("Review metadata scope widened", w.Code, want)
		}
		assertCredentialSafe(t, w.Body.String())
	}
	r := httptest.NewRequest("POST", "/admin-api/credential-leases/"+id+"/revoke?tenant_id=acme", strings.NewReader(`{"owner":{"tenant_id":"other"}}`))
	r.SetPathValue("id", id)
	r.SetPathValue("operation", "revoke")
	w := httptest.NewRecorder()
	env.s.handleAdminCredentialMutation(w, r)
	if w.Code != 400 {
		t.Fatal("caller supplied credential authority")
	}
	p.revokeErr = nil
	r = httptest.NewRequest("POST", "/admin-api/credential-leases/"+id+"/revoke?tenant_id=acme", strings.NewReader(`{}`))
	r.SetPathValue("id", id)
	r.SetPathValue("operation", "revoke")
	w = httptest.NewRecorder()
	env.s.handleAdminCredentialMutation(w, r)
	if w.Code != 200 {
		t.Fatal("exact old lease cleanup unavailable", w.Code)
	}
	assertCredentialSafe(t, w.Body.String())
}
