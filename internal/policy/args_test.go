package policy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBindArgs_EmptyIsObjectDigest(t *testing.T) {
	_, d1, meta := BindArgs(nil)
	_, d2, _ := BindArgs([]byte(`{}`))
	if d1 == "" || d1 != d2 {
		t.Fatalf("empty and {} must share digest; d1=%s d2=%s", d1, d2)
	}
	if meta != nil {
		t.Fatalf("empty object meta=%v want nil", meta)
	}
}

func TestBindArgs_StableKeyOrder(t *testing.T) {
	_, a, _ := BindArgs([]byte(`{"b":1,"a":2}`))
	_, b, _ := BindArgs([]byte(`{"a":2,"b":1}`))
	if a != b {
		t.Fatalf("digest must ignore object key order: %s vs %s", a, b)
	}
}

func TestBindArgs_RedactsSecretsKeepsDigest(t *testing.T) {
	parsed, digest, meta := BindArgs([]byte(`{"amount":50,"token":"sekrit","nested":{"password":"x","ok":true}}`))
	m, ok := parsed.(map[string]any)
	if !ok || m["token"] != "sekrit" {
		t.Fatalf("full parse dropped secret: %#v", parsed)
	}
	if digest == "" {
		t.Fatal("missing digest")
	}
	if _, ok := meta["token"]; ok {
		t.Fatalf("display meta leaked token: %#v", meta)
	}
	nested, _ := meta["nested"].(map[string]any)
	if _, ok := nested["password"]; ok {
		t.Fatalf("display meta leaked nested password: %#v", meta)
	}
	if nested["ok"] != true {
		t.Fatalf("kept nested ok: %#v", meta)
	}
	if meta["amount"] != json.Number("50") {
		t.Fatalf("amount=%v", meta["amount"])
	}
}

func TestBindArgs_JSONStringRetainsItsType(t *testing.T) {
	raw, err := json.Marshal(`{"amount":9}`)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _, _ := BindArgs(raw)
	if parsed != `{"amount":9}` {
		t.Fatalf("string arguments changed type: %#v", parsed)
	}
	_, objectDigest, _ := BindArgs([]byte(`{"amount":9}`))
	_, stringDigest, _ := BindArgs(raw)
	if objectDigest == stringDigest {
		t.Fatal("string and object share authorization")
	}
}

func TestBindArgs_CapsStringRunes(t *testing.T) {
	long := strings.Repeat("x", 400)
	raw, _ := json.Marshal(map[string]any{"note": long})
	_, _, meta := BindArgs(raw)
	got, _ := meta["note"].(string)
	if got != strings.Repeat("x", 256) {
		t.Fatalf("len=%d want 256", len(got))
	}
}

func TestBindArgs_RedactsCredentialAliasesAtEveryDisplayDepth(t *testing.T) {
	aliases := []string{
		"access_token", "accessToken", "ACCESS-TOKEN", "refresh_token", "refreshToken",
		"client_secret", "clientSecret", "bearer_token", "bearerToken", "apiKey", "api-key",
		"id_token", "idToken", "sso_token", "subject_token", "actor_token", "session_token",
		"private_key", "privateKey", "secret_key", "secretKey", "passphrase", "client_assertion",
		"X-API-Key", "X-Auth-Token", " Authorization ",
	}
	for _, alias := range aliases {
		t.Run(alias, func(t *testing.T) {
			credentials := map[string]any{alias: "synthetic-secret", "ok": true}
			raw, err := json.Marshal(map[string]any{alias: "synthetic-secret", "nested": credentials, "items": []any{credentials}})
			if err != nil {
				t.Fatal(err)
			}
			parsed, _, meta, err := BindArgsStrict(raw)
			if err != nil {
				t.Fatal(err)
			}
			if parsed.(map[string]any)[alias] != "synthetic-secret" {
				t.Fatal("redaction changed policy arguments")
			}
			display, err := json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(display), "synthetic-secret") {
				t.Fatal("credential alias survived display redaction")
			}
			if meta["nested"].(map[string]any)["ok"] != true || meta["items"].([]any)[0].(map[string]any)["ok"] != true {
				t.Fatal("redaction removed unrelated nested fields")
			}
		})
	}
}

func TestBindArgs_CredentialRedactionPreservesExactDigestAndPolicyArguments(t *testing.T) {
	raw := []byte(`{"access_token":"sentinel","amount":50,"nested":{"clientSecret":"nested-secret","ok":true}}`)
	parsed, digest, meta, err := BindArgsStrict(raw)
	if err != nil {
		t.Fatal(err)
	}
	// Existing v2 canonical form represents 50 as 5e1.
	const existingDigest = "fc1eb180e353dafa98dd75419c68ad935454eacac7cf1187541b31e353bb8bf1"
	if digest != existingDigest {
		t.Fatalf("authorization digest changed: %s", digest)
	}
	full := parsed.(map[string]any)
	if full["access_token"] != "sentinel" || full["nested"].(map[string]any)["clientSecret"] != "nested-secret" {
		t.Fatal("redaction removed policy inputs")
	}
	if _, exists := meta["access_token"]; exists {
		t.Fatal("display retained access token")
	}
	_, changedDigest, _, err := BindArgsStrict([]byte(`{"access_token":"different","amount":50,"nested":{"clientSecret":"nested-secret","ok":true}}`))
	if err != nil || changedDigest == digest {
		t.Fatal("secret value no longer contributes to authorization digest")
	}
}

func TestBindArgs_CredentialRedactionKeepsUnrelatedArgumentNames(t *testing.T) {
	raw := []byte(`{"token_count":5,"access_token_count":3,"password_policy":"strong","secret_name":"reference","authorization_status":"pending","api_key_id":"reference","name":"read"}`)
	parsed, _, meta, err := BindArgsStrict(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(meta) != len(parsed.(map[string]any)) || meta["name"] != "read" || meta["token_count"] != json.Number("5") {
		t.Fatal("redaction matched unrelated argument names")
	}
}
