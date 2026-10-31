package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/connector"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestHardeningLegacyApprovalCannotContinueThroughPoll(t *testing.T) {
	server, database, _ := hardeningTestEnv(t)
	server.SetPolicyEngine(policy.New(policy.Config{ForceEnable: true}))
	store := any(database).(pendingActionStore)
	a := &models.PendingAction{ID: "pending-old-binding", RunID: "old-operation", TenantID: "default", AgentID: "agent", Connector: "bank", Tool: "transfer", ArgsDigest: "legacy", Status: models.PendingStatusPending}
	if err := store.CreatePendingAction(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	approve := httptest.NewRequest("POST", "/admin-api/pending-actions/"+a.ID+"/approve", nil)
	approve.SetPathValue("id", a.ID)
	refused := httptest.NewRecorder()
	server.handleAdminApprovePendingAction(refused, approve)
	if refused.Code != http.StatusConflict || !strings.Contains(refused.Body.String(), "legacy argument binding") {
		t.Fatal("legacy pending action could be newly approved")
	}
	if err := store.SetPendingActionApproved(context.Background(), a.ID, "old-operator"); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/governance/actions/"+a.ID, nil)
	r.SetPathValue("actionID", a.ID)
	w := httptest.NewRecorder()
	server.handleGovernanceGetAction(w, r)
	var result governanceActionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "rejected" || result.ReasonCode != "approval_binding_legacy" {
		t.Fatalf("old authorization reusable: %s", w.Body.String())
	}
}

func TestHardeningManualWebhookFindsOldIDAndRefusesRedirect(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		t.Run(fmt.Sprint(redirect), func(t *testing.T) {
			_, database, live := hardeningTestEnv(t)
			var hits atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(200) }))
			defer target.Close()
			endpoint := target.URL
			if redirect {
				origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Location", target.URL)
					w.WriteHeader(307)
				}))
				defer origin.Close()
				endpoint = origin.URL
			}
			old := &models.WebhookDeadLetter{ID: "old-event", URL: endpoint, Payload: []byte(`{"event":"old"}`), FailedAt: time.Now().Add(-time.Hour)}
			if err := database.SaveWebhookDeadLetter(context.Background(), old); err != nil {
				t.Fatal(err)
			}
			if !redirect {
				for i := 0; i < 1001; i++ {
					if err := database.SaveWebhookDeadLetter(context.Background(), &models.WebhookDeadLetter{ID: fmt.Sprintf("new-event-%d", i), URL: endpoint, Payload: []byte(`{}`), FailedAt: time.Now()}); err != nil {
						t.Fatal(err)
					}
				}
			}
			response, err := http.Post(live.URL+"/admin-api/webhooks/dead-letters/old-event/redeliver", "application/json", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var result redeliverWebhookResponse
			if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != 200 || result.Delivered == redirect {
				t.Fatalf("incorrect delivery result: %d %+v", response.StatusCode, result)
			}
			if redirect && hits.Load() != 0 || !redirect && hits.Load() != 1 {
				t.Fatalf("unexpected upstream replay: %d", hits.Load())
			}
		})
	}
}

func TestHardeningDotOnlyIDsRejectedBeforePersistence(t *testing.T) {
	s := NewServer(nil, nil, nil, nil)
	for _, id := range []string{".", ".."} {
		request := httptest.NewRequest("POST", "/threads", strings.NewReader(`{"thread_id":"`+id+`"}`))
		w := httptest.NewRecorder()
		s.handleCreateThread(w, request)
		if w.Code != http.StatusBadRequest {
			t.Fatal("unroutable thread ID accepted")
		}
		for _, ids := range [][2]string{{id, "run"}, {"thread", id}} {
			if _, _, err := s.createRunCtx(context.Background(), ids[0], &models.RunCreate{RunID: ids[1]}); !errors.Is(err, errInvalidResourceID) {
				t.Fatalf("unroutable run/thread accepted: %v", err)
			}
		}
	}
}

func TestHardeningApprovalExactArgumentsAndCurrentPolicy(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(map[bool]string{false: "neighbor", true: "revoked"}[revoke], func(t *testing.T) {
			store, err := sqlite.New(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			if err := store.Init(context.Background()); err != nil {
				t.Fatal(err)
			}
			var hits atomic.Int32
			s, admin, binding, call, parse := newBankTransferHITL(t, store, "hardening-tenant", "hardening-run", &hits)
			_, body := call("9007199254740992")
			_, data := parse(body)
			id, _ := data["action_id"].(string)
			if id == "" {
				t.Fatalf("missing pending: %s", body)
			}
			response, err := http.Post(admin.URL+"/admin-api/pending-actions/"+id+"/approve", "application/json", nil)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatal(response.StatusCode)
			}
			if revoke {
				s.SetPolicyEngine(policy.New(policy.Config{Grants: []policy.Grant{{ID: "revoked", TenantID: binding.TenantID, AgentID: binding.AgentID, Connector: "bank", Tools: &policy.ToolFilter{Deny: []string{"transfer"}}}}}))
				_, body = call("9007199254740992")
			} else {
				_, body = call("9007199254740993")
			}
			code, _ := parse(body)
			if code != -32000 || hits.Load() != 0 {
				t.Fatalf("approval bypass reached upstream: %s hits=%d", body, hits.Load())
			}
			row, err := store.GetPendingAction(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if row.Status != models.PendingStatusApproved {
				t.Fatal("unrelated/denied request consumed approval")
			}
			if !revoke {
				_, _ = call("9007199254740992")
				if hits.Load() != 1 {
					t.Fatal("matching exact approved retry did not execute")
				}
			}
		})
	}
}

func TestHardeningUpstream401NeverRequestsSessionRenewal(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"reason_code":"connector_session_invalid"}`))
	}))
	t.Cleanup(up.Close)
	s := NewServer(nil, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	s.SetConnectorRegistry(connector.NewRegistry(map[string]connector.ConnectorConfig{"test": {Auth: connector.AuthConfig{Type: "bearer", BearerToken: "synthetic"}, MCP: &connector.MCPConfig{URL: up.URL}}}))
	binding := &auth.RunBinding{RunID: "bound", Generation: 3, TenantID: "test", AgentID: "agent"}
	r := httptest.NewRequest("POST", "/internal/connectors/test/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write","arguments":{}}}`))
	r.SetPathValue("name", "test")
	r = r.WithContext(auth.WithRunBinding(r.Context(), binding))
	attachConnectorSession(t, s, binding, "test", r)
	w := httptest.NewRecorder()
	s.handleProxyMCPRequest(w, r)
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != 502 || body["reason_code"] != "connector_upstream_unauthorized" || hits.Load() != 1 {
		t.Fatalf("ambiguous retry status: %d %s hits=%d", w.Code, w.Body.String(), hits.Load())
	}
}

func hardeningTestEnv(t *testing.T) (*Server, *sqlite.SQLiteStore, *httptest.Server) {
	t.Helper()
	database, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	server := NewServer(database, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	live := httptest.NewServer(server.Handler())
	t.Cleanup(func() { live.Close(); _ = database.Close() })
	return server, database, live
}
