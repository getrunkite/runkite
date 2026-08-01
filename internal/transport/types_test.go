package transport_test

import (
	"encoding/json"
	"testing"

	"github.com/getrunkite/runkite/internal/transport"
)

// TestUserContext_WireFlattenExtra is the regression for the auth→tools
// gap: AuthResult.Extra (sso_token, email, …) used to serialize under
// a nested "extra" key, so runner code doing user.to_dict().get("sso_token")
// silently got None even with a valid SSO session.
func TestUserContext_WireFlattenExtra(t *testing.T) {
	u := transport.UserContext{
		Identity:        "alice-123",
		DisplayName:     "Alice",
		IsAuthenticated: true,
		Permissions:     []string{"read"},
		Extra: map[string]interface{}{
			"email":     "alice@example.com",
			"sso_token": "raw-jwt",
		},
	}
	raw, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	var flat map[string]interface{}
	if err := json.Unmarshal(raw, &flat); err != nil {
		t.Fatal(err)
	}
	if _, nested := flat["extra"]; nested {
		t.Fatalf("extra must be flattened on the wire, got %s", raw)
	}
	if flat["sso_token"] != "raw-jwt" {
		t.Errorf("expected top-level sso_token, got %s", raw)
	}
	if flat["email"] != "alice@example.com" {
		t.Errorf("expected top-level email, got %s", raw)
	}
	if flat["identity"] != "alice-123" {
		t.Errorf("expected identity preserved, got %s", raw)
	}
}

func TestUserContext_RoundTripFlatAndLegacyNested(t *testing.T) {
	flat := []byte(`{"identity":"u1","is_authenticated":true,"email":"a@b.c","sso_token":"tok"}`)
	var u transport.UserContext
	if err := json.Unmarshal(flat, &u); err != nil {
		t.Fatal(err)
	}
	if u.Extra["sso_token"] != "tok" || u.Extra["email"] != "a@b.c" {
		t.Fatalf("flat unmarshal Extra=%v", u.Extra)
	}

	legacy := []byte(`{"identity":"u1","is_authenticated":true,"extra":{"sso_token":"legacy-tok"}}`)
	var u2 transport.UserContext
	if err := json.Unmarshal(legacy, &u2); err != nil {
		t.Fatal(err)
	}
	if u2.Extra["sso_token"] != "legacy-tok" {
		t.Fatalf("legacy nested unmarshal Extra=%v", u2.Extra)
	}
}
