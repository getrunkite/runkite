package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestGovernancePendingIdentityScope(t *testing.T) {
	store := newGovernanceSQLite(t)
	var grants []policy.Grant
	var rules []policy.MandatoryHITLRule
	for _, scope := range [][2]string{{"tenant-a", "agent-a"}, {"tenant-b", "agent-a"}, {"tenant-a", "agent-b"}} {
		grants = append(grants, policy.Grant{TenantID: scope[0], AgentID: scope[1], Connector: "files"})
		rules = append(rules, policy.MandatoryHITLRule{TenantID: scope[0], AgentID: scope[1], Connector: "files"})
	}
	srv := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	srv.SetPolicyEngine(policy.New(policy.Config{Grants: grants, MandatoryHITL: rules}))
	provider := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"a": {Name: "a", TenantID: "tenant-a", Permissions: []string{auth.GovernanceDecidePermission}},
		"b": {Name: "b", TenantID: "tenant-b", Permissions: []string{auth.GovernanceDecidePermission}},
	})
	handler := auth.Middleware(provider, nil, nil, srv.Handler())
	ids := map[string]bool{}
	for _, tc := range []struct{ key, agent string }{{"a", "agent-a"}, {"b", "agent-a"}, {"a", "agent-b"}} {
		req := httptest.NewRequest(http.MethodPost, "/governance/decide", strings.NewReader(`{"agent_id":"`+tc.agent+`","connector":"files","tool":"upload","run_id":"turn-1","args":{"key":"a"}}`))
		req.Header.Set("Authorization", "Bearer "+tc.key)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var response struct {
			ActionID string `json:"action_id"`
			Effect   string `json:"effect"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusOK || response.Effect != "pending" || response.ActionID == "" {
			t.Fatalf("decide=%d %s", rec.Code, rec.Body.String())
		}
		if ids[response.ActionID] {
			t.Fatal("distinct tenant or agent reused action")
		}
		ids[response.ActionID] = true
		poll := httptest.NewRequest(http.MethodGet, "/governance/actions/"+response.ActionID, nil)
		poll.Header.Set("Authorization", "Bearer "+tc.key)
		polled := httptest.NewRecorder()
		handler.ServeHTTP(polled, poll)
		if polled.Code != http.StatusOK {
			t.Fatalf("own pending poll=%d %s", polled.Code, polled.Body.String())
		}
	}
}

func TestAdminCannotApproveOverdueEmbeddedPending(t *testing.T) {
	store := newGovernanceSQLite(t)
	ctx := tenant.SystemContext(context.Background())
	past := time.Now().UTC().Add(-time.Hour)
	for _, row := range []*models.PendingAction{
		{ID: "overdue", TenantID: "default", AgentID: "agent", RunID: "embedded", Connector: "files", Tool: "upload", ExpiresAt: &past},
		{ID: "connector", TenantID: "default", AgentID: "agent", RunID: "connector", Connector: "files", Tool: "upload"},
	} {
		if err := store.CreatePendingAction(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	srv := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	srv.SetPolicyEngine(policy.New(policy.Config{Grants: []policy.Grant{{TenantID: "default", AgentID: "agent", Connector: "files"}}}))
	for _, tc := range []struct {
		id   string
		want int
	}{{"overdue", http.StatusConflict}, {"connector", http.StatusOK}} {
		req := httptest.NewRequest(http.MethodPost, "/admin-api/pending-actions/"+tc.id+"/approve", nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("approve %s=%d %s", tc.id, rec.Code, rec.Body.String())
		}
	}
}
