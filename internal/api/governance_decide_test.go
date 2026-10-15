package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/ratelimit"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestGovernanceDecide_AllowDenyAndPending(t *testing.T) {
	store := newGovernanceSQLite(t)
	eng := policy.New(policy.Config{
		Auditor: policy.NewStoreAuditor(store),
		Grants: []policy.Grant{{
			ID: "g1", TenantID: "default", AgentID: "partner", Connector: "workspace",
			Tools: &policy.ToolFilter{Allow: []string{"deleteFile", "uploadFile"}},
		}},
		MandatoryHITL: []policy.MandatoryHITLRule{{
			ID: "hold-upload", TenantID: "default", AgentID: "partner", Connector: "workspace",
			Tools: []string{"uploadFile"},
		}},
	})
	srv := governanceServer(t, store, eng, nil)

	allow := postGovernance(t, srv, "", `{"agent_id":"partner","connector":"workspace","tool":"deleteFile","args":{"key":"notes/q3.md"},"run_id":"turn-1"}`)
	if allow.Code != http.StatusOK || allow.Effect != "allow" {
		t.Fatalf("allow: status %d effect %q body %s", allow.Code, allow.Effect, allow.Raw)
	}

	deny := postGovernance(t, srv, "", `{"agent_id":"partner","connector":"workspace","tool":"copyFile","run_id":"turn-1"}`)
	if deny.Code != http.StatusOK || deny.Effect != "deny" {
		t.Fatalf("deny: status %d effect %q body %s", deny.Code, deny.Effect, deny.Raw)
	}

	held := postGovernance(t, srv, "", `{"agent_id":"partner","connector":"workspace","tool":"uploadFile","run_id":"turn-1"}`)
	if held.Code != http.StatusOK || held.Effect != "pending" || held.ReasonCode != policy.ReasonMandatoryHITL || held.ActionID == "" {
		t.Fatalf("pending: status %d effect %q code %q id %q body %s", held.Code, held.Effect, held.ReasonCode, held.ActionID, held.Raw)
	}
	again := postGovernance(t, srv, "", `{"agent_id":"partner","connector":"workspace","tool":"uploadFile","run_id":"turn-1"}`)
	if again.ActionID != held.ActionID {
		t.Fatalf("same turn reused action %q, want %q", again.ActionID, held.ActionID)
	}
	missingRun := postGovernance(t, srv, "", `{"agent_id":"partner","connector":"workspace","tool":"uploadFile"}`)
	if missingRun.Code != http.StatusBadRequest {
		t.Fatalf("pending without run_id: status %d body %s", missingRun.Code, missingRun.Raw)
	}
	rows, err := store.SearchPendingActions(tenant.SystemContext(context.Background()), &models.PendingActionSearchRequest{RunID: "turn-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != held.ActionID || rows[0].ExpiresAt == nil {
		t.Fatalf("pending rows %#v", rows)
	}

	events, err := store.SearchAuditEvents(tenant.SystemContext(context.Background()), &models.AuditSearchRequest{
		RunID: "turn-1", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("audit rows %d, want 4", len(events))
	}
	sawPending := false
	for _, ev := range events {
		if ev.ResourceType != "connector" || ev.Connector != "workspace" || ev.RunID != "turn-1" || ev.Generation != 0 {
			t.Fatalf("audit row %#v", ev)
		}
		if ev.Tool == "uploadFile" && ev.Decision == policy.EffectPending {
			sawPending = true
		}
	}
	if !sawPending {
		t.Fatal("audit row for uploadFile should keep the engine effect pending")
	}
}

func TestGovernanceDecide_TenantMismatchAndMissingPolicy(t *testing.T) {
	store := newGovernanceSQLite(t)
	eng := policy.New(policy.Config{
		Grants: []policy.Grant{{
			ID: "g1", TenantID: "default", AgentID: "partner", Connector: "workspace",
		}},
	})
	srv := governanceServer(t, store, eng, nil)

	mismatch := postGovernance(t, srv, "", `{"tenant_id":"acme","agent_id":"partner","connector":"workspace","tool":"deleteFile"}`)
	if mismatch.Code != http.StatusForbidden {
		t.Fatalf("mismatch: status %d body %s", mismatch.Code, mismatch.Raw)
	}

	match := postGovernance(t, srv, "", `{"tenant_id":"default","agent_id":"partner","connector":"workspace","tool":"deleteFile"}`)
	if match.Code != http.StatusOK || match.Effect != "allow" {
		t.Fatalf("matching tenant_id: status %d effect %q body %s", match.Code, match.Effect, match.Raw)
	}
	// agent_id is not looked up in the agents table. A grant for this
	// string allows it; a string with no grant is denied.
	unknown := postGovernance(t, srv, "", `{"agent_id":"not-registered","connector":"workspace","tool":"deleteFile"}`)
	if unknown.Code != http.StatusOK || unknown.Effect != "deny" || unknown.ReasonCode != policy.ReasonPolicyNoGrant {
		t.Fatalf("no grant: status %d effect %q code %q body %s", unknown.Code, unknown.Effect, unknown.ReasonCode, unknown.Raw)
	}

	bad := postGovernance(t, srv, "", `{"agent_id":"partner"}`)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("missing fields: status %d body %s", bad.Code, bad.Raw)
	}

	open := governanceServer(t, store, nil, nil)
	noPolicy := postGovernance(t, open, "", `{"agent_id":"partner","connector":"workspace","tool":"deleteFile"}`)
	if noPolicy.Code != http.StatusConflict {
		t.Fatalf("no policy: status %d body %s", noPolicy.Code, noPolicy.Raw)
	}
}

func TestGovernanceDecide_BadBodyAndLongID(t *testing.T) {
	store := newGovernanceSQLite(t)
	eng := policy.New(policy.Config{
		Grants: []policy.Grant{{
			ID: "g1", TenantID: "default", AgentID: "partner", Connector: "workspace",
		}},
	})
	srv := governanceServer(t, store, eng, nil)

	junk := postGovernance(t, srv, "", `not-json`)
	if junk.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: status %d body %s", junk.Code, junk.Raw)
	}

	atCap := postGovernance(t, srv, "", fmt.Sprintf(
		`{"agent_id":%q,"connector":"workspace","tool":"deleteFile"}`,
		strings.Repeat("a", 256),
	))
	if atCap.Code != http.StatusOK || atCap.Effect != "deny" {
		t.Fatalf("256-char agent id: status %d effect %q body %s", atCap.Code, atCap.Effect, atCap.Raw)
	}

	over := postGovernance(t, srv, "", fmt.Sprintf(
		`{"agent_id":%q,"connector":"workspace","tool":"deleteFile"}`,
		strings.Repeat("a", 257),
	))
	if over.Code != http.StatusBadRequest {
		t.Fatalf("257-char agent id: status %d body %s", over.Code, over.Raw)
	}
}

func TestGovernancePending_PollApproveDenyExpire(t *testing.T) {
	store := newGovernanceSQLite(t)
	eng := policy.New(policy.Config{
		Grants: []policy.Grant{{
			ID: "g1", TenantID: "default", AgentID: "partner", Connector: "workspace",
		}},
		MandatoryHITL: []policy.MandatoryHITLRule{{
			ID: "hold", TenantID: "default", AgentID: "partner", Connector: "workspace",
		}},
	})
	provider := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"gov":  {Name: "gov", Permissions: []string{auth.GovernanceDecidePermission}},
		"acme": {Name: "acme", Permissions: []string{auth.GovernanceDecidePermission}, TenantID: "acme"},
		"read": {Name: "read", Permissions: []string{"read"}},
	})
	apiServer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	apiServer.SetPolicyEngine(eng)
	srv := httptest.NewServer(auth.Middleware(provider, nil, nil, apiServer.Handler()))
	t.Cleanup(srv.Close)
	ctx := tenant.SystemContext(context.Background())

	held := postGovernance(t, srv, "gov", `{"agent_id":"partner","connector":"workspace","tool":"deleteFile","run_id":"turn-a"}`)
	if held.Code != http.StatusOK || held.Effect != "pending" || held.ActionID == "" {
		t.Fatalf("decide: status %d effect %q body %s", held.Code, held.Effect, held.Raw)
	}
	polled := getGovernance(t, srv, "gov", held.ActionID)
	if polled.Code != http.StatusOK || polled.Status != "pending" {
		t.Fatalf("poll pending: status %d body %s", polled.Code, polled.Raw)
	}
	if got := getGovernance(t, srv, "read", held.ActionID); got.Code != http.StatusForbidden {
		t.Fatalf("read key poll: status %d body %s", got.Code, got.Raw)
	}
	if got := getGovernance(t, srv, "acme", held.ActionID); got.Code != http.StatusNotFound {
		t.Fatalf("other tenant: status %d body %s", got.Code, got.Raw)
	}

	if err := store.SetPendingActionApproved(ctx, held.ActionID, "ops"); err != nil {
		t.Fatal(err)
	}
	approved := getGovernance(t, srv, "gov", held.ActionID)
	if approved.Code != http.StatusOK || approved.Status != "approved" {
		t.Fatalf("approved: status %d body %s", approved.Code, approved.Raw)
	}

	denied := postGovernance(t, srv, "gov", `{"agent_id":"partner","connector":"workspace","tool":"deleteFile","run_id":"turn-b"}`)
	if denied.Effect != "pending" || denied.ActionID == "" || denied.ActionID == held.ActionID {
		t.Fatalf("second action: %+v", denied)
	}
	if err := store.SetPendingActionStatus(ctx, denied.ActionID, models.PendingStatusPending, models.PendingStatusDenied); err != nil {
		t.Fatal(err)
	}
	rejected := getGovernance(t, srv, "gov", denied.ActionID)
	if rejected.Status != "rejected" {
		t.Fatalf("rejected: status %d body %s", rejected.Code, rejected.Raw)
	}

	restore := api.SetGovernancePendingTTLForTest(-time.Hour)
	t.Cleanup(restore)
	stale := postGovernance(t, srv, "gov", `{"agent_id":"partner","connector":"workspace","tool":"deleteFile","run_id":"turn-c"}`)
	if stale.Effect != "pending" || stale.ActionID == "" {
		t.Fatalf("stale decide: %+v", stale)
	}
	expired := getGovernance(t, srv, "gov", stale.ActionID)
	if expired.Status != "rejected" || expired.ReasonCode != models.PendingReasonExpired {
		t.Fatalf("expired: status %d body %s", expired.Code, expired.Raw)
	}
}

func TestGovernanceDecide_PermissionAndAgentRateLimit(t *testing.T) {
	store := newGovernanceSQLite(t)
	eng := policy.New(policy.Config{
		Grants: []policy.Grant{{
			ID: "g1", TenantID: "default", AgentID: "partner", Connector: "workspace",
		}},
	})
	provider := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"writer": {Name: "writer", Permissions: []string{"write"}},
		"runner": {Name: "runner", Permissions: []string{auth.AgentRunPermission("partner")}},
		"gov":    {Name: "gov", Permissions: []string{auth.GovernanceDecidePermission}},
		"admin":  {Name: "admin", Permissions: []string{"admin"}},
	})
	apiServer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	apiServer.SetPolicyEngine(eng)
	srv := httptest.NewServer(auth.Middleware(provider, nil, nil, apiServer.Handler()))
	t.Cleanup(srv.Close)

	for _, key := range []string{"writer", "runner"} {
		got := postGovernance(t, srv, key, `{"agent_id":"partner","connector":"workspace","tool":"deleteFile"}`)
		if got.Code != http.StatusForbidden {
			t.Fatalf("%s: status %d body %s", key, got.Code, got.Raw)
		}
	}

	ok := postGovernance(t, srv, "gov", `{"agent_id":"partner","connector":"workspace","tool":"deleteFile"}`)
	if ok.Code != http.StatusOK || ok.Effect != "allow" {
		t.Fatalf("governance key: status %d effect %q body %s", ok.Code, ok.Effect, ok.Raw)
	}
	adminOK := postGovernance(t, srv, "admin", `{"agent_id":"partner","connector":"workspace","tool":"deleteFile"}`)
	if adminOK.Code != http.StatusOK || adminOK.Effect != "allow" {
		t.Fatalf("admin: status %d effect %q body %s", adminOK.Code, adminOK.Effect, adminOK.Raw)
	}

	limitedAPI := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	limitedAPI.SetPolicyEngine(eng)
	limitedAPI.SetRateLimiter(ratelimit.New(&ratelimit.Config{PerAgent: &ratelimit.Rule{RPS: 0.0001, Burst: 1}}))
	limited := httptest.NewServer(auth.Middleware(provider, nil, nil, limitedAPI.Handler()))
	t.Cleanup(limited.Close)
	first := postGovernance(t, limited, "gov", `{"agent_id":"partner","connector":"workspace","tool":"deleteFile"}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first under burst: status %d body %s", first.Code, first.Raw)
	}
	second := postGovernance(t, limited, "gov", `{"agent_id":"partner","connector":"workspace","tool":"deleteFile"}`)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("rate limit: status %d body %s", second.Code, second.Raw)
	}
}

type governanceResult struct {
	Code       int
	Effect     string
	Status     string
	ReasonCode string
	ActionID   string
	Raw        string
}

func newGovernanceSQLite(t *testing.T) *sqlite.SQLiteStore {
	t.Helper()
	store, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	return store
}

func governanceServer(t *testing.T, store *sqlite.SQLiteStore, eng *policy.Engine, lim *ratelimit.Limiter) *httptest.Server {
	t.Helper()
	apiServer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	apiServer.SetPolicyEngine(eng)
	if lim != nil {
		apiServer.SetRateLimiter(lim)
	}
	srv := httptest.NewServer(apiServer.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func postGovernance(t *testing.T, srv *httptest.Server, apiKey, body string) governanceResult {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/governance/decide", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var parsed struct {
		Effect     string `json:"effect"`
		Status     string `json:"status"`
		ReasonCode string `json:"reason_code"`
		ActionID   string `json:"action_id"`
	}
	_ = json.Unmarshal(raw, &parsed)
	return governanceResult{
		Code: resp.StatusCode, Effect: parsed.Effect, Status: parsed.Status,
		ReasonCode: parsed.ReasonCode, ActionID: parsed.ActionID, Raw: string(bytes.TrimSpace(raw)),
	}
}

func getGovernance(t *testing.T, srv *httptest.Server, apiKey, actionID string) governanceResult {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/governance/actions/"+actionID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var parsed struct {
		Effect     string `json:"effect"`
		Status     string `json:"status"`
		ReasonCode string `json:"reason_code"`
		ActionID   string `json:"action_id"`
	}
	_ = json.Unmarshal(raw, &parsed)
	return governanceResult{
		Code: resp.StatusCode, Effect: parsed.Effect, Status: parsed.Status,
		ReasonCode: parsed.ReasonCode, ActionID: parsed.ActionID, Raw: string(bytes.TrimSpace(raw)),
	}
}
