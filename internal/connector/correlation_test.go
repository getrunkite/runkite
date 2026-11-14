package connector

import (
	"context"
	"net/http"
	"testing"
)

func TestMCPPublicCorrelationRequiresOperatorOptInAndTrustedContext(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		reg := NewRegistry(map[string]ConnectorConfig{"files": {Auth: AuthConfig{Type: "bearer", BearerToken: "synthetic"}, MCP: &MCPConfig{URL: "https://mcp.invalid", CorrelationHeaders: enabled}}})
		ctx, _, err := reg.BindMCPRequestTarget(context.Background(), "files")
		if err != nil {
			t.Fatal(err)
		}
		blank := http.Header{}
		applyActionCorrelation(ctx, blank)
		if len(blank) != 0 {
			t.Fatal("correlation synthesized without action")
		}
		h := http.Header{}
		applyActionCorrelation(WithMCPActionCorrelation(ctx, "action", "cred-reference"), h)
		if (h.Get("X-Runkite-Action-Id") == "action") != enabled {
			t.Fatal("operator correlation setting ignored")
		}
		if enabled && h.Get("X-Runkite-Lease-Ref") != "cred-reference" {
			t.Fatal("lease reference absent")
		}
		bad := http.Header{}
		applyActionCorrelation(WithMCPActionCorrelation(ctx, "action\r\nInjected: value", ""), bad)
		if len(bad) != 0 {
			t.Fatal("unsafe correlation accepted")
		}
	}
}

func TestMCPHeaderOptInChangesTargetRevision(t *testing.T) {
	cfg := ConnectorConfig{Auth: AuthConfig{Type: "bearer"}, MCP: &MCPConfig{URL: "https://mcp.invalid"}}
	a, e := TargetRevisionForConfig(cfg)
	if e != nil {
		t.Fatal(e)
	}
	cfg.MCP.CorrelationHeaders = true
	b, e := TargetRevisionForConfig(cfg)
	if e != nil || a == b {
		t.Fatal("header contract not included in target authority")
	}
}
