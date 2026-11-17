package connector

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestToolsOnlyConfigJSONAndYAML(t *testing.T) {
	for _, format := range []string{"json", "yaml"} {
		for _, tc := range []struct {
			name, value string
			want        bool
			invalid     bool
		}{
			{name: "omitted"},
			{name: "false", value: "false"},
			{name: "true", value: "true", want: true},
			{name: "null", value: "null"},
			{name: "string", value: `"not-a-boolean"`, invalid: true},
			{name: "number", value: "1", invalid: true},
			{name: "object", value: "{}", invalid: true},
			{name: "array", value: "[]", invalid: true},
		} {
			t.Run(format+"/"+tc.name, func(t *testing.T) {
				var cfg ConnectorConfig
				var err error
				if format == "json" {
					field := ""
					if tc.value != "" {
						field = `,"tools_only":` + tc.value
					}
					err = json.Unmarshal([]byte(`{"auth":{"type":"bearer"},"mcp":{"url":"https://mcp.example.test"`+field+`}}`), &cfg)
				} else {
					field := ""
					if tc.value != "" {
						field = "  tools_only: " + tc.value + "\n"
					}
					err = yaml.Unmarshal([]byte("auth:\n  type: bearer\nmcp:\n  url: https://mcp.example.test\n"+field), &cfg)
				}
				if tc.invalid {
					if err == nil {
						t.Fatal("non-boolean profile configuration was accepted")
					}
					return
				}
				if err != nil || cfg.MCP == nil || cfg.MCP.ToolsOnly != tc.want {
					t.Fatalf("tools-only configuration mismatch: error=%v", err)
				}
			})
		}
	}
}

func TestToolsOnlyTargetRevisionPreservesLegacyVectors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pinTools  bool
		correlate bool
		legacy    string
		toolsOnly string
	}{
		{"v1", false, false, "target-v1-4748f63384ea97d2e5d8f52a575f02b91d63cea1d202d7075375113dcf5fe991", "target-v4-e369294e3b2d6af67756816dc9ca1f9093cfc03db4d9273bf0c80a9505b672c1"},
		{"v2", true, false, "target-v2-246603152221b9445228dbc37eed0a7e0800184eca08b66dc42a15c3d3ee221e", "target-v4-59d242ae14edb8415518d8a0c4ed6c9d7fe27ef8c2b93648650e3e79e620e35f"},
		{"v3", true, true, "target-v3-faf6baad933d4fc86f870752d038e603c5dac19d5811a25ffc2010dcb2416109", "target-v4-da26349c18fcbd01e9e8864b4154e3c6b907f5adae79987c3d9e49abae511e44"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := revisionFixture()
			cfg.MCP.PinTools = tc.pinTools
			cfg.MCP.CorrelationHeaders = tc.correlate
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), `"tools_only"`) {
				t.Fatal("default false profile changed existing config serialization")
			}
			for _, input := range []string{string(raw), strings.Replace(string(raw), `"mcp":{`, `"mcp":{"tools_only":false,`, 1)} {
				var parsed ConnectorConfig
				if err := json.Unmarshal([]byte(input), &parsed); err != nil {
					t.Fatal(err)
				}
				if got, err := TargetRevisionForConfig(parsed); err != nil || got != tc.legacy {
					t.Fatalf("legacy target fingerprint changed: got=%q error=%v", got, err)
				}
			}
			cfg.MCP.ToolsOnly = true
			if got, err := TargetRevisionForConfig(cfg); err != nil || got != tc.toolsOnly {
				t.Fatalf("tools-only revision omitted or reinterpreted the completed target descriptor: got=%q error=%v", got, err)
			}
			cfg.MCP.ToolsOnly = false
			if got, err := TargetRevisionForConfig(cfg); err != nil || got != tc.legacy {
				t.Fatal("disabling tools-only did not restore the original contract fingerprint")
			}
		})
	}
}

func TestToolsOnlySnapshotAndExpectedRevision(t *testing.T) {
	cfg := revisionFixture()
	cfg.MCP.ToolsOnly = true
	snapshot := snapshotConnectorConfig(cfg)
	registry := NewRegistry(map[string]ConnectorConfig{"fixture": cfg})
	cfg.MCP.ToolsOnly = false
	if !snapshot.MCP.ToolsOnly {
		t.Fatal("config snapshot shared its profile pointer with caller config")
	}
	registered, err := registry.Get("fixture")
	if err != nil || !registered.Config.MCP.ToolsOnly {
		t.Fatal("registry did not snapshot tools-only configuration")
	}
	bound, revision, err := registry.BindMCPRequestTarget(context.Background(), "fixture")
	if err != nil || !strings.HasPrefix(revision, "target-v4-") {
		t.Fatal("tools-only target was not bound")
	}
	registered.Config.MCP.ToolsOnly = false
	target, err := registry.requestTarget(bound, "fixture")
	if err != nil || !target.config.MCP.ToolsOnly {
		t.Fatal("bound request lost its snapshotted tools-only contract")
	}
	if _, err := registry.requestTarget(WithExpectedTargetRevision(context.Background(), revision), "fixture"); !errors.Is(err, ErrTargetRevisionChanged) {
		t.Fatal("fresh request accepted a revision from a different MCP method profile")
	}
}
