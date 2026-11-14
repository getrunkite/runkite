package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/getrunkite/runkite/internal/connector"
	"github.com/getrunkite/runkite/internal/transport"
)

func TestConstrainedConnectorAPIUsesOnlyAssignedSubject(t *testing.T) {
	for _, surface := range []string{"session", "mcp"} {
		for _, tc := range []struct {
			name, supplied string
			missingTrusted bool
			forgedExtra    bool
			allowed        bool
		}{
			{name: "derive-from-assignment", allowed: true},
			{name: "same-assertion", supplied: "alice-acme-assertion", allowed: true},
			{name: "other-principal", supplied: "bob-acme-assertion"},
			{name: "other-tenant", supplied: "alice-foreign-tenant-assertion"},
			{name: "header-cannot-create-trusted-subject", supplied: "alice-acme-assertion", missingTrusted: true},
			{name: "extra-cannot-create-trusted-subject", missingTrusted: true, forgedExtra: true},
			{name: "typed-subject-wins-over-extra", forgedExtra: true, allowed: true},
		} {
			t.Run(surface+"/"+tc.name, func(t *testing.T) {
				env := newActionAPIEnv(t, "json", "policy")
				if !tc.missingTrusted {
					env.binding.User.DelegatedJWTAssertion = "alice-acme-assertion"
				}
				if tc.forgedExtra {
					env.binding.User.Extra = map[string]interface{}{"sso_token": "bob-assertion", transport.DelegatedJWTAssertionKey: "bob-assertion", "extra": map[string]interface{}{transport.DelegatedJWTAssertionKey: "bob-assertion"}}
				}
				var issuerCalls atomic.Int32
				issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					issuerCalls.Add(1)
					if err := r.ParseForm(); err != nil || r.Form.Get("subject_token") != "alice-acme-assertion" || r.Form.Get("audience") != "files-api" || r.Form.Get("scope") != "files.read" {
						t.Error("token endpoint received a substituted subject or target")
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"access_token":"synthetic-result","token_type":"Bearer","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","expires_in":120,"scope":"files.read"}`))
				}))
				t.Cleanup(issuer.Close)
				entry, err := env.s.connectors.Get("fixture")
				if err != nil {
					t.Fatal(err)
				}
				cfg := entry.Config
				cfg.Auth = connector.AuthConfig{Type: "oauth2_token_exchange", TokenURL: issuer.URL, AllowInsecureLoopback: true, Scopes: []string{"files.read"}, TokenExchange: &connector.TokenExchangeConfig{Audiences: []string{"files-api"}, MaxTTLSeconds: 300}}
				env.s.SetConnectorRegistry(connector.NewRegistry(map[string]connector.ConnectorConfig{"fixture": cfg}))
				env.setTool(func(w http.ResponseWriter, r *http.Request, id json.RawMessage) {
					if r.Header.Get("Authorization") != "Bearer synthetic-result" {
						t.Error("MCP executor did not receive the exchanged credential")
					}
					_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(id) + `,"result":{"content":[]}}`))
				})
				body := recordedToolCall
				if surface == "session" {
					values := map[string]any{}
					if tc.supplied != "" {
						values["sso_token"] = tc.supplied
						values["identity"] = "bob"
						values["tenant_id"] = "foreign"
					}
					raw, marshalErr := json.Marshal(map[string]any{"user_context": values})
					if marshalErr != nil {
						t.Fatal(marshalErr)
					}
					body = string(raw)
				}
				r := httptest.NewRequest(http.MethodPost, "/internal/connectors/fixture/"+surface, strings.NewReader(body)).WithContext(env.ctx)
				r.SetPathValue("name", "fixture")
				if surface == "mcp" {
					attachConnectorSession(t, env.s, env.binding, "fixture", r)
					if tc.supplied != "" {
						raw, marshalErr := json.Marshal(map[string]any{"sso_token": tc.supplied, "identity": "bob", "tenant_id": "foreign"})
						if marshalErr != nil {
							t.Fatal(marshalErr)
						}
						r.Header.Set("X-Runkite-User-Context", string(raw))
					}
				}
				w := httptest.NewRecorder()
				if surface == "session" {
					env.s.handleGetConnectorSession(w, r)
				} else {
					env.s.handleProxyMCPRequest(w, r)
				}
				if (w.Code == http.StatusOK) != tc.allowed || (!tc.allowed && (issuerCalls.Load() != 0 || env.effects.Load() != 0)) || (tc.allowed && issuerCalls.Load() != 1) {
					t.Fatalf("allowed=%v code=%d issuer=%d effects=%d", tc.allowed, w.Code, issuerCalls.Load(), env.effects.Load())
				}
				if surface == "mcp" && tc.allowed && env.effects.Load() != 1 {
					t.Fatal("valid delegated tool did not run exactly once")
				}
				if strings.Contains(w.Body.String(), "assertion") || strings.Contains(w.Body.String(), "synthetic-result") {
					t.Fatal("a subject or access token escaped through the plane response")
				}
			})
		}
	}
}
