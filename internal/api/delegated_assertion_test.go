package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/transport"
)

func TestDelegatedAssertionRequiresVerifiedFrozenParentProvenance(t *testing.T) {
	for _, tc := range []struct {
		name                              string
		verified, provenance, typed, want bool
	}{
		{"verified-strict-parent", true, true, true, true},
		{"unverified-parent", false, true, true, false},
		{"legacy-verified-parent", true, false, true, false},
		{"extra-only-parent", true, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := &transport.UserContext{Identity: "alice", IsAuthenticated: true, Permissions: []string{"read", "write"}, Extra: map[string]interface{}{"sso_token": "extra-forgery", transport.DelegatedJWTAssertionKey: "reserved-forgery"}}
			if tc.typed {
				user.DelegatedJWTAssertion = "original-strict-jwt"
			}
			var provenance *models.IdentityProvenance
			if tc.provenance {
				provenance = &models.IdentityProvenance{Issuer: "https://issuer.invalid", Subject: "alice", Audience: "runkite", ExpiresAt: time.Now().Add(time.Minute)}
			}
			manifest := buildRunManifest(runManifestInput{user: user, identityVerified: tc.verified, provenance: provenance})
			parent := &models.Run{TenantID: "acme", Metadata: map[string]interface{}{"run_manifest": runManifestToMetadata(manifest)}}
			principal, err := a2aDelegatedPrincipal(user, parent, json.RawMessage(`{"permissions":["read"]}`))
			if err != nil {
				t.Fatal(err)
			}
			want := ""
			if tc.want {
				want = "original-strict-jwt"
			}
			if auth.DelegatedJWTAssertion(principal) != want {
				t.Fatal("A2A changed authenticated assertion provenance")
			}
			childUser := userContextFromAuth(auth.WithContext(context.Background(), principal))
			if childUser == nil || childUser.DelegatedJWTAssertion != want || len(childUser.Permissions) != 1 || childUser.Permissions[0] != "read" {
				t.Fatal("child assignment lost typed assertion or permission attenuation")
			}
			childManifest := buildRunManifest(runManifestInput{user: childUser, identityVerified: auth.HasVerifiedIdentity(principal), provenance: principal.Provenance})
			raw, err := json.Marshal(childManifest)
			if err != nil || strings.Contains(string(raw), "original-strict-jwt") || strings.Contains(string(raw), "forgery") || strings.Contains(string(raw), transport.DelegatedJWTAssertionKey) {
				t.Fatal("raw assertion or arbitrary claim escaped into the durable manifest", err)
			}
			for _, body := range []json.RawMessage{json.RawMessage(`{"_runkite_delegated_jwt":"reserved-forgery"}`), json.RawMessage(`{"extra":{"_runkite_delegated_jwt":"reserved-forgery"}}`)} {
				if _, err := a2aDelegatedPrincipal(user, parent, body); err == nil {
					t.Fatal("on_behalf_of accepted a reserved assertion field")
				}
			}
		})
	}
}

func TestUserContextFromAuthDoesNotPromoteForwardedClaims(t *testing.T) {
	result := &auth.AuthResult{Identity: "alice", Extra: map[string]interface{}{"sso_token": "forged", transport.DelegatedJWTAssertionKey: "forged"}, Provenance: &models.IdentityProvenance{Subject: "alice", ExpiresAt: time.Now().Add(time.Minute)}}
	user := userContextFromAuth(auth.WithContext(context.Background(), result))
	if user == nil || user.DelegatedJWTAssertion != "" {
		t.Fatal("untyped API-key/webhook/claim data became a trusted assertion")
	}
	raw, err := json.Marshal(user)
	if err != nil {
		t.Fatal(err)
	}
	var restored transport.UserContext
	if err := json.Unmarshal(raw, &restored); err != nil || restored.DelegatedJWTAssertion != "" {
		t.Fatal("queue roundtrip promoted a forwarded claim", err)
	}
}
