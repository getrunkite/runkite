package payloadshrink

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testSettings() Settings {
	return Settings{
		Enabled:       true,
		MaxBytes:      1024,
		PreviewBytes:  32,
		CacheTTL:      time.Minute,
		MaxStoreBytes: 1 << 20,
	}
}

func bigRPC(text string) []byte {
	inner, _ := json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
		"isError": false,
	})
	env, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"result":  json.RawMessage(inner),
	})
	return env
}

func TestApply_DisabledIsIdentity(t *testing.T) {
	body := bigRPC(strings.Repeat("x", 2000))
	s := testSettings()
	s.Enabled = false
	got := Apply(context.Background(), NewMemoryStore(), s, "run1", 1, body)
	if string(got) != string(body) {
		t.Fatal("disabled shrink must be byte-identical")
	}
}

func TestApply_UnderCapIsIdentity(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	s := testSettings()
	got := Apply(context.Background(), NewMemoryStore(), s, "run1", 1, body)
	if string(got) != string(body) {
		t.Fatal("under cap must be byte-identical")
	}
}

func TestApply_JSONRPCErrorNotShrunk(t *testing.T) {
	msg := strings.Repeat("nope ", 400)
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"error":   map[string]any{"code": -32000, "message": msg},
	})
	s := testSettings()
	if len(body) <= s.MaxBytes {
		t.Fatalf("need over-cap error body, got %d", len(body))
	}
	got := Apply(context.Background(), NewMemoryStore(), s, "run1", 1, body)
	if string(got) != string(body) {
		t.Fatal("JSON-RPC error must not shrink")
	}
}

func TestApply_ShrinkAndRetrieve(t *testing.T) {
	store := NewMemoryStore()
	s := testSettings()
	text := strings.Repeat("hello-world ", 200)
	body := bigRPC(text)
	if len(body) <= s.MaxBytes {
		t.Fatalf("fixture too small: %d", len(body))
	}
	got := Apply(context.Background(), store, s, "run1", 1, body)
	if len(got) >= len(body) {
		t.Fatalf("expected smaller stub, orig=%d stub=%d", len(body), len(got))
	}
	var env jsonRPCEnvelope
	if err := json.Unmarshal(got, &env); err != nil {
		t.Fatal(err)
	}
	if hasJSONRPCError(env.Error) {
		t.Fatalf("stub error: %s", got)
	}
	preview := previewText(env.Result, 10_000)
	if !strings.Contains(preview, "runkite_retrieve_payload") {
		t.Fatalf("stub missing retrieve hint: %s", preview)
	}
	ref := ParseRef([]byte(`{"ref":"` + extractRef(preview) + `"}`))
	inner, err := Retrieve(context.Background(), store, s, "run1", 1, ref)
	if err != nil {
		t.Fatal(err)
	}
	_, origInner, ok := innerResult(body)
	if !ok {
		t.Fatal("orig")
	}
	if string(inner) != string(origInner) {
		t.Fatalf("retrieve mismatch")
	}
	_, err = Retrieve(context.Background(), store, s, "run1", 2, ref)
	if err != ErrNotFound {
		t.Fatalf("wrong generation: %v", err)
	}
}

func TestApply_StoreErrorPassesThrough(t *testing.T) {
	s := testSettings()
	body := bigRPC(strings.Repeat("z", 2000))
	got := Apply(context.Background(), FailStore{}, s, "run1", 1, body)
	if string(got) != string(body) {
		t.Fatal("store error must pass original through")
	}
}

func TestApply_BudgetSecondPassesThrough(t *testing.T) {
	store := NewMemoryStore()
	s := testSettings()
	s.MaxStoreBytes = 2000
	a := bigRPC(strings.Repeat("a", 1200))
	b := bigRPC(strings.Repeat("b", 1200))
	if len(a) <= s.MaxBytes || len(b) <= s.MaxBytes {
		t.Fatalf("need over-cap fixtures %d %d", len(a), len(b))
	}
	first := Apply(context.Background(), store, s, "run1", 1, a)
	if len(first) >= len(a) {
		t.Fatal("first payload should shrink")
	}
	second := Apply(context.Background(), store, s, "run1", 1, b)
	if string(second) != string(b) {
		t.Fatal("second payload should pass through over budget")
	}
}

func TestRetrieve_Disabled(t *testing.T) {
	s := testSettings()
	s.Enabled = false
	_, err := Retrieve(context.Background(), NewMemoryStore(), s, "run1", 1, "abcd")
	if !IsDisabled(err) {
		t.Fatalf("got %v", err)
	}
}

func TestRetrieve_MissingRef(t *testing.T) {
	_, err := Retrieve(context.Background(), NewMemoryStore(), testSettings(), "run1", 1, "")
	if !IsMissingRef(err) {
		t.Fatalf("got %v", err)
	}
}

func TestInjectRetrieveTool(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"query"}]}}`)
	got := InjectRetrieveTool(body)
	if !strings.Contains(string(got), ToolName) || !strings.Contains(string(got), "query") {
		t.Fatalf("%s", got)
	}
	dup := InjectRetrieveTool(got)
	if strings.Count(string(dup), `"name":"`+ToolName+`"`) != 1 {
		t.Fatalf("expected one retrieve tool: %s", dup)
	}
}

func TestSettingsValidate(t *testing.T) {
	s := testSettings()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	s.MaxBytes = 512
	if err := s.Validate(); err == nil {
		t.Fatal("max_bytes 512")
	}
}

func extractRef(text string) string {
	const needle = `{"ref":"`
	i := strings.Index(text, needle)
	if i < 0 {
		return ""
	}
	rest := text[i+len(needle):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}
