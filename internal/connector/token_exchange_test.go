package connector

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func constrainedExchangeFixture() AuthConfig {
	return AuthConfig{Type: "oauth2_token_exchange", TokenURL: "https://issuer.invalid/token", Scopes: []string{"files.read"}, TokenExchange: &TokenExchangeConfig{Audiences: []string{"files-api"}, Resources: []string{"https://files.invalid"}, MaxTTLSeconds: 300}}
}

func TestConstrainedTokenExchangeUsesRegisteredTargetsAndActor(t *testing.T) {
	cfg := constrainedExchangeFixture()
	cfg.TokenExchange.ActorTokenSecretRef = "env:RUNKITE_TEST_ACTOR_ASSERTION"
	cfg.TokenExchange.ActorID = "registered-agent"
	t.Setenv("RUNKITE_TEST_ACTOR_ASSERTION", "synthetic-actor")
	old := httpClient
	t.Cleanup(func() { httpClient = old })
	httpClient = &http.Client{Transport: regressionRoundTripper(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		data, e := url.ParseQuery(string(b))
		if e != nil {
			t.Fatal(e)
		}
		if !reflect.DeepEqual(data["audience"], []string{"files-api"}) || !reflect.DeepEqual(data["resource"], []string{"https://files.invalid"}) || data.Get("scope") != "files.read" || data.Get("subject_token") != "synthetic-subject" || data.Get("actor_token") != "synthetic-actor" || data.Get("actor_token_type") != accessTokenType {
			t.Fatal("registered exchange contract not applied")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"synthetic-result","token_type":"Bearer","issued_token_type":"` + accessTokenType + `","expires_in":120,"scope":"files.read"}`))}, nil
	})}
	token, e := oauth2TokenExchange(context.Background(), cfg, "synthetic-subject")
	if e != nil || token.AccessToken != "synthetic-result" {
		t.Fatalf("exchange failed: %v", e)
	}
}

func TestConstrainedTokenExchangeRejectsWideningAndAmbiguousResponses(t *testing.T) {
	cfg := constrainedExchangeFixture()
	for _, tc := range []struct {
		name, kind, issued, scope string
		ttl                       int64
	}{
		{"no-expiry", "Bearer", accessTokenType, "", 0}, {"ttl-widening", "Bearer", accessTokenType, "", 301}, {"scope-widening", "Bearer", accessTokenType, "files.write", 120}, {"ambiguous-scope", "Bearer", accessTokenType, "files.read  files.read", 120}, {"non-bearer", "DPoP", accessTokenType, "", 120}, {"wrong-issued-type", "Bearer", "jwt", "", 120},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if validateExchangeResponse(cfg, tc.kind, tc.issued, tc.scope, tc.ttl) == nil {
				t.Fatal("unsafe exchange accepted")
			}
		})
	}
	if validateExchangeResponse(cfg, "Bearer", accessTokenType, "", 120) != nil {
		t.Fatal("RFC omitted equal scope rejected")
	}
}

func TestExchangeConstraintsAreSnapshottedAndRevisionBound(t *testing.T) {
	cfg := ConnectorConfig{Auth: constrainedExchangeFixture(), MCP: &MCPConfig{URL: "https://mcp.invalid"}}
	reg := NewRegistry(map[string]ConnectorConfig{"files": cfg})
	ctx, rev, e := reg.BindMCPRequestTarget(context.Background(), "files")
	if e != nil {
		t.Fatal(e)
	}
	if !strings.HasPrefix(rev, "target-v3-") {
		t.Fatal("new contract lacks new target revision version")
	}
	cfg.Auth.TokenExchange.Audiences[0] = "mutated"
	bound, e := reg.BoundDelegationContract(ctx, "files")
	if e != nil || bound.Audiences[0] != "files-api" {
		t.Fatal("caller mutation changed registered target")
	}
	c, _ := reg.Get("files")
	c.Config.Auth.TokenExchange.Audiences[0] = "replacement"
	if _, e = reg.requestTarget(WithExpectedTargetRevision(context.Background(), rev), "files"); e == nil {
		t.Fatal("changed delegation reused old authority")
	}
}

func TestInvalidExchangeFailsBeforeNetworkAndKeepsSecretsOutOfErrors(t *testing.T) {
	for _, change := range []func(*AuthConfig){
		func(c *AuthConfig) { c.TokenExchange.Audiences = nil; c.TokenExchange.Resources = nil }, func(c *AuthConfig) { c.Scopes = nil }, func(c *AuthConfig) { c.Type = "api_key" }, func(c *AuthConfig) { c.TokenExchange.Resources = []string{"https://files.invalid?token=private"} }, func(c *AuthConfig) { c.TokenExchange.ActorTokenSecretRef = "file:/does-not-exist" }, func(c *AuthConfig) { c.TokenExchange.MaxTTLSeconds = 86401 },
	} {
		cfg := constrainedExchangeFixture()
		change(&cfg)
		if ValidateTokenExchangeConfig(cfg) == nil {
			t.Fatal("invalid contract accepted")
		}
	}
	cfg := constrainedExchangeFixture()
	cfg.TokenExchange.ActorTokenSecretRef = "env:RUNKITE_TEST_ACTOR_MISSING"
	cfg.TokenExchange.ActorID = "registered-agent"
	t.Setenv("RUNKITE_TEST_ACTOR_MISSING", "")
	_, e := oauth2TokenExchange(context.Background(), cfg, "synthetic-subject")
	if e == nil || strings.Contains(e.Error(), "RUNKITE_TEST_ACTOR") {
		t.Fatal("missing actor was accepted or leaked reference")
	}
	b, _ := json.Marshal(cfg.TokenExchange)
	if strings.Contains(string(b), "synthetic-subject") {
		t.Fatal("credential entered configuration evidence")
	}
}
