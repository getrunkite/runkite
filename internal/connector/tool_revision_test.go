package connector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAdvertisedToolChangeBlocksBeforeApprovalConsumption(t *testing.T) {
	var definition atomic.Value
	definition.Store(`{"name":"write","inputSchema":{"type":"object","properties":{"amount":{"const":9007199254740992}}}}`)
	var calls, prepared atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "tools/list" {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"runkite-schema","result":{"tools":[` + definition.Load().(string) + `]}}`))
			return
		}
		calls.Add(1)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer server.Close()
	cfg := revisionFixture()
	cfg.MCP.URL = server.URL
	cfg.MCP.PinTools = true
	registry := NewRegistry(map[string]ConnectorConfig{"files": cfg})
	ctx, target, err := registry.BindMCPRequestTarget(context.Background(), "files")
	if err != nil || !strings.HasPrefix(target, "target-v2-") {
		t.Fatal("new target version missing")
	}
	ctx, first, err := registry.BindMCPToolRevision(ctx, "files", "write", nil)
	if err != nil || !strings.HasPrefix(first, "tool-v1-") {
		t.Fatal(err)
	}
	definition.Store(`{"name":"write","inputSchema":{"type":"object","properties":{"amount":{"const":9007199254740993}}}}`)
	ctx = WithMCPActionTrace(ctx, NewMCPActionTrace(func(context.Context) error { prepared.Add(1); return nil }))
	_, err = registry.ProxyMCPRequest(ctx, "files", nil, toolCallRequest(1, "write"))
	if !errors.Is(err, ErrToolRevisionChanged) || prepared.Load() != 0 || calls.Load() != 0 {
		t.Fatal("changed advertised definition consumed approval or reached tool")
	}
	ctx2, _, _ := registry.BindMCPRequestTarget(context.Background(), "files")
	ctx2, second, err := registry.BindMCPToolRevision(ctx2, "files", "write", nil)
	if err != nil || first == second {
		t.Fatal("distinct exact numeric schemas shared a revision")
	}
	if _, err := registry.ProxyMCPRequest(ctx2, "files", nil, toolCallRequest(1, "write")); err != nil || calls.Load() != 1 {
		t.Fatal("current schema did not dispatch once")
	}
}

func TestToolDiscoveryRejectsAmbiguousDefinitions(t *testing.T) {
	for _, tools := range []string{`[{"name":"write","name":"read"}]`, `[{"name":"write"},{"name":"write"}]`, `[]`} {
		t.Run(tools, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"runkite-schema","result":{"tools":` + tools + `}}`))
			}))
			defer server.Close()
			cfg := revisionFixture()
			cfg.MCP.URL = server.URL
			cfg.MCP.PinTools = true
			registry := NewRegistry(map[string]ConnectorConfig{"files": cfg})
			ctx, _, _ := registry.BindMCPRequestTarget(context.Background(), "files")
			if _, _, err := registry.BindMCPToolRevision(ctx, "files", "write", nil); !errors.Is(err, ErrToolRevisionUnavailable) {
				t.Fatal("ambiguous tool set accepted")
			}
		})
	}
}
