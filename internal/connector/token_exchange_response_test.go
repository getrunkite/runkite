package connector

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestConstrainedExchangeValidatesActualProviderResponse(t *testing.T) {
	valid := `"access_token":"synthetic-exchanged-token","token_type":"Bearer","issued_token_type":"` + accessTokenType + `","expires_in":120`
	for _, tc := range []struct {
		name, body string
		allowed    bool
	}{
		{"omitted-equal-scope", `{` + valid + `}`, true},
		{"explicit-scope", `{` + valid + `,"scope":"files.read"}`, true},
		{"null-scope", `{` + valid + `,"scope": null}`, false},
		{"wrong-scope-type", `{` + valid + `,"scope":["files.read"]}`, false},
		{"duplicate-scope", `{` + valid + `,"scope":"files.write","scope":"files.read"}`, false},
		{"case-variant-scope", `{` + valid + `,"scope":"files.write","Scope":"files.read"}`, false},
		{"case-variant-expiry", `{` + valid + `,"Expires_In":120}`, false},
		{"duplicate-expiry", `{` + valid + `,"expires_in":120}`, false},
		{"duplicate-access-token", `{` + valid + `,"access_token":"replacement-token"}`, false},
		{"access-token-control", `{"access_token":"synthetic\ntoken","token_type":"Bearer","issued_token_type":"` + accessTokenType + `","expires_in":120}`, false},
		{"null-refresh-token", `{` + valid + `,"refresh_token":null}`, false},
		{"null-instance-url", `{` + valid + `,"instance_url":null}`, false},
		{"scope-widening", `{` + valid + `,"scope":"files.write"}`, false},
		{"string-expiry", `{"access_token":"synthetic","token_type":"Bearer","issued_token_type":"` + accessTokenType + `","expires_in":"120"}`, false},
		{"expired", `{"access_token":"synthetic","token_type":"Bearer","issued_token_type":"` + accessTokenType + `","expires_in":0}`, false},
		{"ttl-widening", `{"access_token":"synthetic","token_type":"Bearer","issued_token_type":"` + accessTokenType + `","expires_in":301}`, false},
		{"wrong-token-type", `{"access_token":"synthetic","token_type":"DPoP","issued_token_type":"` + accessTokenType + `","expires_in":120}`, false},
		{"trailing-object", `{` + valid + `}{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := httpClient
			t.Cleanup(func() { httpClient = old })
			var requests atomic.Int32
			httpClient = &http.Client{Transport: regressionRoundTripper(func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			token, err := oauth2TokenExchange(context.Background(), constrainedExchangeFixture(), "synthetic-subject")
			if requests.Load() != 1 || (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v calls=%d error=%v", tc.allowed, requests.Load(), err)
			}
			if tc.allowed && (token == nil || token.AccessToken != "synthetic-exchanged-token") {
				t.Fatal("validated token was lost")
			}
			if err != nil && (token != nil || strings.Contains(err.Error(), "synthetic") || strings.Contains(err.Error(), "replacement-token")) {
				t.Fatal("rejected provider response returned credential material")
			}
		})
	}
}

func TestConstrainedExchangeSubjectCannotBeSubstitutedBeforeNetwork(t *testing.T) {
	for _, tc := range []struct {
		name     string
		trusted  bool
		supplied map[string]interface{}
		allowed  bool
	}{
		{"no-private-binding", false, map[string]interface{}{"sso_token": "alice-assertion"}, false},
		{"other-principal", true, map[string]interface{}{"sso_token": "bob-assertion"}, false},
		{"other-tenant", true, map[string]interface{}{"sso_token": "alice-other-tenant"}, false},
		{"wrong-type", true, map[string]interface{}{"sso_token": 12}, false},
		{"same-assertion", true, map[string]interface{}{"sso_token": "alice-assertion"}, true},
		{"derive-assignment-assertion", true, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := httpClient
			t.Cleanup(func() { httpClient = old })
			var calls atomic.Int32
			httpClient = &http.Client{Transport: regressionRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				if err := r.ParseForm(); err != nil || r.Form.Get("subject_token") != "alice-assertion" {
					t.Error("exchange did not use trusted subject")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"synthetic","token_type":"Bearer","issued_token_type":"` + accessTokenType + `","expires_in":120}`))}, nil
			})}
			ctx := context.Background()
			if tc.trusted {
				ctx = WithDelegatedSubject(ctx, "alice-assertion")
			}
			registry := NewRegistry(map[string]ConnectorConfig{"files": {Auth: constrainedExchangeFixture()}})
			_, err := registry.GetSession(ctx, "files", tc.supplied)
			if (err == nil) != tc.allowed || (!tc.allowed && calls.Load() != 0) || (tc.allowed && calls.Load() != 1) {
				t.Fatalf("allowed=%v issuer_calls=%d error=%v", tc.allowed, calls.Load(), err)
			}
		})
	}
}

func TestConstrainedExchangeRequiresTLSOrExplicitLoopbackFixture(t *testing.T) {
	for _, tc := range []struct {
		endpoint          string
		loopback, allowed bool
	}{
		{"https://issuer.invalid/token", false, true},
		{"http://issuer.invalid/token", false, false},
		{"http://issuer.invalid/token", true, false},
		{"http://127.0.0.1:8200/token", false, false},
		{"http://127.0.0.1:8200/token", true, true},
		{"http://[::1]:8200/token", true, true},
		{"https://user:password@issuer.invalid/token", false, false},
		{"https://issuer.invalid/token?credential=private", false, false},
	} {
		cfg := constrainedExchangeFixture()
		cfg.TokenURL, cfg.AllowInsecureLoopback = tc.endpoint, tc.loopback
		if (ValidateTokenExchangeConfig(cfg) == nil) != tc.allowed {
			t.Fatalf("unexpected transport acceptance: loopback=%v allowed=%v", tc.loopback, tc.allowed)
		}
	}
}
