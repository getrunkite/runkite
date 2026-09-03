package api

import (
	"reflect"
	"testing"
)

func TestExtractAllowedToolsUnset(t *testing.T) {
	if got := extractAllowedTools(nil); got != nil {
		t.Fatalf("nil metadata: got %#v, want nil", got)
	}
	if got := extractAllowedTools(map[string]interface{}{}); got != nil {
		t.Fatalf("empty metadata: got %#v, want nil", got)
	}
	if got := extractAllowedTools(map[string]interface{}{"connector_needs": []string{"sf"}}); got != nil {
		t.Fatalf("unrelated keys: got %#v, want nil", got)
	}
}

func TestExtractAllowedToolsExplicitEmpty(t *testing.T) {
	meta := map[string]interface{}{
		"allowed_tools":     []string{},
		"allowed_tools_set": true,
	}
	got := extractAllowedTools(meta)
	if got == nil {
		t.Fatal("expected non-nil pointer for explicit empty allowlist")
	}
	if len(*got) != 0 {
		t.Fatalf("got %v, want empty slice", *got)
	}
}

func TestExtractAllowedToolsJSONRoundTrip(t *testing.T) {
	meta := map[string]interface{}{
		"allowed_tools":     []interface{}{"search", "get_record"},
		"allowed_tools_set": true,
	}
	got := extractAllowedTools(meta)
	if got == nil {
		t.Fatal("expected allowlist")
	}
	want := []string{"search", "get_record"}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("got %v, want %v", *got, want)
	}
}

func TestExtractAllowedToolsKeyPresenceWithoutFlag(t *testing.T) {
	// After a store round-trip the set flag may be missing but the key remains.
	meta := map[string]interface{}{
		"allowed_tools": []interface{}{"only_this"},
	}
	got := extractAllowedTools(meta)
	if got == nil || len(*got) != 1 || (*got)[0] != "only_this" {
		t.Fatalf("got %#v", got)
	}
}
