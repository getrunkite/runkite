package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirectModeTenantGapWarning(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	defaultOnly := filepath.Join(dir, "default.json")
	if err := os.WriteFile(defaultOnly, []byte(`{
	  "graphs": {"echo": "./g.py:graph"},
	  "auth": {
	    "type": "api_key",
	    "keys": {
	      "k1": {"name": "op", "permissions": ["admin"], "tenant_id": "default"}
	    }
	  }
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := directModeTenantGapWarning(defaultOnly); got != "" {
		t.Fatalf("default-only tenants should not warn, got %q", got)
	}

	multi := filepath.Join(dir, "multi.json")
	if err := os.WriteFile(multi, []byte(`{
	  "graphs": {"echo": "./g.py:graph"},
	  "auth": {
	    "type": "api_key",
	    "keys": {
	      "k1": {"name": "acme", "permissions": ["admin"], "tenant_id": "acme-corp"}
	    }
	  }
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := directModeTenantGapWarning(multi)
	if !strings.Contains(got, "non-default tenant_id") || !strings.Contains(got, "POSTGRES_DSN") {
		t.Fatalf("multi-tenant warning missing expected fragments, got %q", got)
	}
}
