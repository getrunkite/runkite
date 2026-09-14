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
	if meta["amount"] != float64(50) {
		t.Fatalf("amount=%v", meta["amount"])
	}
}

func TestBindArgs_JSONStringObject(t *testing.T) {
	raw, err := json.Marshal(`{"amount":9}`)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _, _ := BindArgs(raw)
	m, ok := parsed.(map[string]any)
	if !ok || m["amount"] != float64(9) {
		t.Fatalf("string-encoded arguments: %#v", parsed)
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
