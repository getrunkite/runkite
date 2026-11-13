package auth

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
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
