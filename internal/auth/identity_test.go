package auth

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
)

func TestIdentityProvenanceSurvivesSessionSerialization(t *testing.T) {
	for _, verified := range []bool{true, false} {
		input := &AuthResult{Identity: "jwt-user", TenantID: "a", Permissions: []string{"admin"}}
		if !verified {
			MarkIdentityUnverified(input)
		}
		raw, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		var output AuthResult
		if err := json.Unmarshal(raw, &output); err != nil {
			t.Fatal(err)
		}
		if HasVerifiedIdentity(&output) != verified || output.Identity != "jwt-user" || !authorizedAdmin(&output, true) {
			t.Fatalf("identity provenance changed: input=%v output=%v", verified, HasVerifiedIdentity(&output))
		}
	}
	var legacy AuthResult
	if err := json.Unmarshal([]byte(`{"Identity":"legacy-admin","Permissions":["admin"]}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if HasVerifiedIdentity(&legacy) || !authorizedAdmin(&legacy, true) {
		t.Fatal("legacy session became verified or lost ordinary Admin compatibility")
	}
	if HasVerifiedIdentity(nil) || HasVerifiedIdentity(&AuthResult{Identity: " "}) {
		t.Fatal("missing identity verified")
	}
}

func TestDelegatedJWTAssertionNeverComesFromExtra(t *testing.T) {
	result := &AuthResult{Identity: "alice", Extra: map[string]interface{}{"sso_token": "forged", "_runkite_delegated_jwt": "forged"}, Provenance: &models.IdentityProvenance{Subject: "alice", ExpiresAt: time.Now().Add(time.Minute)}}
	if DelegatedJWTAssertion(result) != "" {
		t.Fatal("Extra established an authenticated assertion")
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var restored AuthResult
	if err := json.Unmarshal(raw, &restored); err != nil || DelegatedJWTAssertion(&restored) != "" {
		t.Fatal("Extra established an assertion through session serialization", err)
	}
	result.delegatedJWT = "original-strict-jwt"
	raw, err = json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &restored); err != nil || DelegatedJWTAssertion(&restored) != "original-strict-jwt" {
		t.Fatal("private session serialization lost the original assertion", err)
	}
	MarkIdentityUnverified(&restored)
	if DelegatedJWTAssertion(&restored) != "" || restored.delegatedJWT != "" {
		t.Fatal("identity attenuation retained a delegated assertion")
	}
	for _, change := range []func(*AuthResult){
		func(r *AuthResult) { r.Provenance = nil },
		func(r *AuthResult) { r.Identity = "bob" },
		func(r *AuthResult) { r.Provenance.ExpiresAt = time.Now().Add(-time.Second) },
	} {
		copy := *result
		copy.Provenance = models.CloneIdentityProvenance(result.Provenance)
		change(&copy)
		if DelegatedJWTAssertion(&copy) != "" {
			t.Fatal("missing, mismatched, or expired identity retained assertion authority")
		}
	}
}

func TestIdentityProvenanceDoesNotVerifyLocalAnonymousSessions(t *testing.T) {
	result, err := (&NoneProvider{}).Authenticate(httptest.NewRequest("GET", "/", nil))
	if err != nil || HasVerifiedIdentity(result) || result.Identity != "anonymous" {
		t.Fatalf("local no-op provider verified a display identity: %+v %v", result, err)
	}
	result, err = authenticateAdminCredential(nil, nil, "", false)
	if err != nil || HasVerifiedIdentity(result) || !authorizedAdmin(result, false) {
		t.Fatalf("local Admin identity provenance or compatibility changed: %+v %v", result, err)
	}
}
