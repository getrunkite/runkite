package auth_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/golang-jwt/jwt/v5"
)

func TestStrictJWTIdentityAndIssuerAssertedActors(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	server := testJWKS(t, &key.PublicKey)
	p, err := auth.NewJWTProvider(auth.JWTConfig{JWKSURL: server.URL, Issuer: "https://issuer.invalid", Audience: "runkite", StrictIdentity: true, AllowInsecureLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		change  func(jwt.MapClaims)
		allowed bool
	}{
		{"valid", func(jwt.MapClaims) {}, true},
		{"no-expiry", func(c jwt.MapClaims) { delete(c, "exp") }, false},
		{"expired", func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Hour).Unix() }, false},
		{"no-subject", func(c jwt.MapClaims) { delete(c, "sub") }, false},
		{"wrong-subject-type", func(c jwt.MapClaims) { c["sub"] = 12 }, false},
		{"control-subject", func(c jwt.MapClaims) { c["sub"] = "alice\nadmin" }, false},
		{"wrong-issuer", func(c jwt.MapClaims) { c["iss"] = "https://other.invalid" }, false},
		{"wrong-audience", func(c jwt.MapClaims) { c["aud"] = "other" }, false},
		{"no-audience", func(c jwt.MapClaims) { delete(c, "aud") }, false},
		{"actor-string", func(c jwt.MapClaims) { c["act"] = "actor" }, false},
		{"actor-no-subject", func(c jwt.MapClaims) { c["act"] = map[string]any{"iss": "https://issuer.invalid"} }, false},
		{"actor-depth", func(c jwt.MapClaims) {
			var a any
			for range 5 {
				a = map[string]any{"sub": "actor", "act": a}
			}
			c["act"] = a
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := jwt.MapClaims{"iss": "https://issuer.invalid", "aud": "runkite", "sub": "alice", "exp": time.Now().Add(time.Minute).Unix(), "act": map[string]any{"sub": "agent-service", "iss": "https://issuer.invalid", "do_not_capture": "private-arbitrary-claim"}}
			tc.change(claims)
			token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
			token.Header["kid"] = "test-key-1"
			signed, e := token.SignedString(key)
			if e != nil {
				t.Fatal(e)
			}
			req := httptest.NewRequest("POST", "/runs", nil)
			req.Header.Set("Authorization", "Bearer "+signed)
			result, e := p.Authenticate(req)
			if (e == nil) != tc.allowed {
				t.Fatalf("allowed=%v error=%v", tc.allowed, e)
			}
			if e != nil {
				return
			}
			if result.Provenance == nil || result.Provenance.Subject != "alice" || len(result.Provenance.Actors) != 1 || result.Provenance.Actors[0].Subject != "agent-service" {
				t.Fatalf("bad provenance: %#v", result.Provenance)
			}
			raw, e := json.Marshal(result)
			if e != nil {
				t.Fatal(e)
			}
			if strings.Contains(string(raw), signed) || strings.Contains(string(raw), "private-arbitrary-claim") {
				t.Fatal("credential or arbitrary claim leaked into evidence")
			}
			var roundtrip auth.AuthResult
			if e := json.Unmarshal(raw, &roundtrip); e != nil {
				t.Fatal(e)
			}
			if roundtrip.Provenance == nil || !auth.HasVerifiedIdentity(&roundtrip) {
				t.Fatal("session roundtrip lost provenance")
			}
			auth.MarkIdentityUnverified(&roundtrip)
			if roundtrip.Provenance != nil {
				t.Fatal("unverified identity retained provenance")
			}
		})
	}
}

func TestStrictJWTConfigurationRequiresIssuerAndAudience(t *testing.T) {
	for _, cfg := range []auth.JWTConfig{{StrictIdentity: true}, {StrictIdentity: true, Issuer: "https://issuer.invalid"}, {StrictIdentity: true, Audience: "runkite"}} {
		if _, err := auth.NewJWTProvider(cfg); err == nil {
			t.Fatal("incomplete strict JWT profile accepted")
		}
	}
}

func TestStrictJWTDelegationCapturesOnlyTheAuthenticatedBearer(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	server := testJWKS(t, &key.PublicKey)
	for _, tc := range []struct {
		name            string
		strict, forward bool
	}{
		{"strict-forwarded", true, true},
		{"strict-not-forwarded", true, false},
		{"legacy-forwarded", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider, err := auth.NewJWTProvider(auth.JWTConfig{JWKSURL: server.URL, Issuer: "https://issuer.invalid", Audience: "runkite", StrictIdentity: tc.strict, AllowInsecureLoopback: true, ForwardToken: tc.forward, RawTokenField: "sso_token", ExtraClaims: []string{"other_token", "_runkite_delegated_jwt"}, ClaimAliases: map[string]string{"other_token": "sso_token"}, ForwardHeaders: map[string]string{"X-Other-Token": "sso_token", "X-Reserved-Token": "_runkite_delegated_jwt"}})
			if err != nil {
				t.Fatal(err)
			}
			token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": "https://issuer.invalid", "aud": "runkite", "sub": "alice", "exp": time.Now().Add(time.Minute).Unix(), "other_token": "claim-forgery", "_runkite_delegated_jwt": "reserved-claim-forgery"})
			token.Header["kid"] = "test-key-1"
			signed, err := token.SignedString(key)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest("POST", "/runs", nil)
			request.Header.Set("Authorization", "Bearer "+signed)
			request.Header.Set("X-Other-Token", "header-forgery")
			request.Header.Set("X-Reserved-Token", "reserved-header-forgery")
			result, err := provider.Authenticate(request)
			if err != nil {
				t.Fatal(err)
			}
			if result.Extra["sso_token"] != "header-forgery" || result.Extra["_runkite_delegated_jwt"] != "reserved-header-forgery" {
				t.Fatal("legacy forwarding no longer preserves the configured Extra behavior")
			}
			want := ""
			if tc.strict && tc.forward {
				want = signed
			}
			if auth.DelegatedJWTAssertion(result) != want {
				t.Fatal("typed assertion came from a forwarded claim/header or bypassed strict opt-in")
			}
			raw, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			var restored auth.AuthResult
			if err := json.Unmarshal(raw, &restored); err != nil || auth.DelegatedJWTAssertion(&restored) != want {
				t.Fatal("session roundtrip changed the original authenticated assertion", err)
			}
		})
	}
}
