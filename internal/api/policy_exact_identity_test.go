package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/config"
	"github.com/getrunkite/runkite/internal/connector"
	"github.com/getrunkite/runkite/internal/hooks"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestExactAgentConnectorGrantBoundary(t *testing.T) {
	for _, connectorAlias := range []bool{false, true} {
		for _, sqlGrant := range []bool{false, true} {
			t.Run(map[bool]string{false: "agent/", true: "connector/"}[connectorAlias]+map[bool]string{false: "config_grant", true: "sql_admin_grant"}[sqlGrant], func(t *testing.T) {
				const privilegedAgent = "echo"
				assignedAgent := " echo "
				const connectorName = "secret-api"
				requestedConnector := connectorName
				if connectorAlias {
					assignedAgent = privilegedAgent
					requestedConnector = " secret-api "
				}
				const secret = "fixture-connector-credential"
				ctx := context.Background()
				db, err := sqlite.New("")
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if err := db.Init(ctx); err != nil {
					t.Fatal(err)
				}
				graphConfig := config.LangGraphConfig{Graphs: map[string]string{privilegedAgent: "./agent.py:graph", assignedAgent: "./agent.py:graph"}}
				entries, err := graphConfig.ParseGraphEntries()
				if err != nil || len(entries) != len(graphConfig.Graphs) {
					t.Fatal("registration rejected exact keys", err)
				}
				for _, entry := range entries {
					if err := db.UpsertAgent(ctx, &models.Agent{AgentID: entry.GraphID, Name: entry.GraphID}); err != nil {
						t.Fatal(err)
					}
				}
				a, err := db.GetAgent(ctx, assignedAgent)
				if err != nil || a.AgentID != assignedAgent {
					t.Fatal("stored identity changed", err)
				}
				queue := inprocess.NewQueue()
				plane := api.NewServer(db, queue, inprocess.NewBroker(), inprocess.NewCancelBus())
				plane.SetConnectorRegistry(connector.NewRegistry(map[string]connector.ConnectorConfig{connectorName: {Auth: connector.AuthConfig{Type: "bearer", BearerToken: secret}}, " secret-api ": {Auth: connector.AuthConfig{Type: "bearer", BearerToken: secret}}}))
				pc := policy.Config{ForceEnable: true}
				if !sqlGrant {
					pc.Grants = []policy.Grant{{ID: "privileged-only", TenantID: "default", AgentID: privilegedAgent, Connector: connectorName}}
				}
				plane.SetPolicyEngine(policy.New(pc))
				dispatcher := hooks.NewDispatcher()
				defer dispatcher.Close(ctx)
				plane.RegisterAdmissionGate(dispatcher)
				plane.SetHookDispatcher(dispatcher)
				t.Setenv("RUNNER_TOKEN_PYTHON_LANGGRAPH", "fixture-runner-token")
				t.Setenv("RUNNER_TENANTS_PYTHON_LANGGRAPH", "default")
				runnerTokens := auth.LoadRunnerTokensFromEnv()
				callers := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{"caller": {Name: "caller", Permissions: []string{auth.AgentRunPermission(assignedAgent)}}})
				admins := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{"admin": {Name: "admin", Permissions: []string{"admin"}}})
				handler := auth.MiddlewareWithOpts(callers, admins, runnerTokens, auth.MiddlewareOpts{Inflight: queue}, plane.Handler())
				post := func(path, key string, body any) *httptest.ResponseRecorder {
					raw, _ := json.Marshal(body)
					request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
					request.Header.Set("Authorization", "Bearer "+key)
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, request)
					return response
				}
				if sqlGrant {
					response := post("/admin-api/policy-grants", "admin", map[string]any{"id": "privileged-only", "tenant_id": "default", "agent_id": privilegedAgent, "connector": connectorName})
					if response.Code != 201 {
						t.Fatalf("grant creation: %d %s", response.Code, response.Body.String())
					}
				}
				response := post("/threads/owned-thread/runs", "caller", map[string]any{"run_id": "owned-run", "agent_id": assignedAgent, "input": map[string]any{"messages": []any{map[string]any{"role": "user", "content": "fixture"}}}, "if_not_exists": "create"})
				if response.Code != 200 {
					t.Fatalf("run admission: %d %s", response.Code, response.Body.String())
				}
				assignment, err := queue.Dequeue(ctx, "python-langgraph", time.Second)
				if err != nil || assignment == nil || assignment.GraphID != assignedAgent {
					t.Fatal("wrong active assignment", assignment, err)
				}
				request := httptest.NewRequest(http.MethodPost, "/internal/connectors/"+url.PathEscape(requestedConnector)+"/session", nil)
				request.Header.Set(auth.HeaderRunnerKind, "python-langgraph")
				request.Header.Set(auth.HeaderRunnerToken, "fixture-runner-token")
				request.Header.Set(auth.HeaderRunID, assignment.RunID)
				request.Header.Set(auth.HeaderGeneration, strconv.FormatInt(assignment.Generation, 10))
				output := httptest.NewRecorder()
				handler.ServeHTTP(output, request)
				if output.Code != http.StatusForbidden {
					t.Fatalf("agent %q borrowed %q connector grant: status=%d credential_returned=%t", assignedAgent, privilegedAgent, output.Code, bytes.Contains(output.Body.Bytes(), []byte(secret)))
				}
			})
		}
	}
}

func TestGovernancePositiveIdentityBytesAndCacheAreExact(t *testing.T) {
	engine := policy.New(policy.Config{CacheTTL: time.Hour, Grants: []policy.Grant{{TenantID: "default", AgentID: "agent", Connector: "bank", Tools: &policy.ToolFilter{Allow: []string{"write", "send\x00other"}}}}})
	plane := api.NewServer(nil, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	plane.SetPolicyEngine(engine)
	provider := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"caller": {Name: "caller", Permissions: []string{auth.GovernanceDecidePermission}},
		"padded": {Name: "padded", TenantID: " default ", Permissions: []string{auth.GovernanceDecidePermission}},
	})
	handler := auth.Middleware(provider, nil, nil, plane.Handler())
	call := func(key, agent, connector, tool string, tenantClaim *string) (int, string) {
		body := map[string]any{"agent_id": agent, "connector": connector, "tool": tool, "args": map[string]any{}}
		if tenantClaim != nil {
			body["tenant_id"] = *tenantClaim
		}
		raw, _ := json.Marshal(body)
		request := httptest.NewRequest(http.MethodPost, "/governance/decide", bytes.NewReader(raw))
		request.Header.Set("Authorization", "Bearer "+key)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		var result struct {
			Effect string `json:"effect"`
		}
		_ = json.Unmarshal(response.Body.Bytes(), &result)
		return response.Code, result.Effect
	}
	for _, tc := range []struct{ key, agent, connector, tool, effect string }{
		{"caller", "agent", "bank", "write", "allow"},
		{"caller", " agent ", "bank", "write", "deny"},
		{"caller", "agent", " bank ", "write", "deny"},
		{"caller", "agent", "bank", " write ", "deny"},
		{"padded", "agent", "bank", "write", "deny"},
		{"caller", "agent", "bank", "send\x00other", "allow"},
		{"caller", "agent", "bank\x00send", "other", "deny"},
	} {
		if code, effect := call(tc.key, tc.agent, tc.connector, tc.tool, nil); code != 200 || effect != tc.effect {
			t.Fatalf("exact governance identity case %+v: status=%d effect=%s", tc, code, effect)
		}
	}
	claim := " default "
	if code, _ := call("caller", "agent", "bank", "write", &claim); code != http.StatusForbidden {
		t.Fatal("normalized a different tenant claim")
	}
	if code, effect := call("padded", "agent", "bank", "write", &claim); code != 200 || effect != "deny" {
		t.Fatalf("exact padded tenant claim rejected or borrowed authority: %d %s", code, effect)
	}
}

func TestAdminGrantHTTPWritesPreserveExactIdentity(t *testing.T) {
	db, err := sqlite.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	plane := api.NewServer(db, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	engine := policy.New(policy.Config{ForceEnable: true})
	plane.SetPolicyEngine(engine)
	handler := auth.Middleware(auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{"admin": {Name: "admin", Permissions: []string{"admin"}}}), nil, nil, plane.Handler())
	grant := models.PolicyGrant{ID: " grant/id ", TenantID: " tenant ", AgentID: " agent ", Connector: " connector ", Tools: &models.PolicyToolFilter{Allow: []string{"read", " read ", "comma,name"}, Deny: []string{" delete "}}}
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		path := "/admin-api/policy-grants"
		status := http.StatusCreated
		if method == http.MethodPut {
			path += "/" + url.PathEscape(grant.ID)
			status = http.StatusOK
			grant.Tools.Allow = append(grant.Tools.Allow, "next")
		}
		raw, _ := json.Marshal(grant)
		request := httptest.NewRequest(method, path, bytes.NewReader(raw))
		request.Header.Set("Authorization", "Bearer admin")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != status {
			t.Fatalf("exact grant write: %d %s", response.Code, response.Body.String())
		}
		saved, err := db.GetPolicyGrant(context.Background(), grant.ID)
		if err != nil || saved.ID != grant.ID || saved.TenantID != grant.TenantID || saved.AgentID != grant.AgentID || saved.Connector != grant.Connector {
			t.Fatal("HTTP write retargeted exact grant identity", saved, err)
		}
		gotTools, _ := json.Marshal(saved.Tools)
		wantTools, _ := json.Marshal(grant.Tools)
		if !bytes.Equal(gotTools, wantTools) {
			t.Fatal("HTTP write retargeted wire tool names", string(gotTools))
		}
	}
	rows, err := db.ListPolicyGrants(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatal("write introduced a neighboring grant", len(rows), err)
	}
	if _, err := db.GetPolicyGrant(context.Background(), "grant/id"); err == nil {
		t.Fatal("write targeted a neighboring row ID")
	}
}
