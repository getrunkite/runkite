package connector

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMCPActionEvidenceRefusalDoesNotStrandHalfOpenBreaker(t *testing.T) {
	registry := NewRegistry(map[string]ConnectorConfig{"fixture": {Auth: AuthConfig{Type: "bearer", BearerToken: "synthetic"}, MCP: &MCPConfig{URL: "https://example.invalid/mcp"}}})
	c, _ := registry.Get("fixture")
	c.breaker.mu.Lock()
	c.breaker.state = stateOpen
	c.breaker.openedAt = time.Now().Add(-time.Hour)
	c.breaker.mu.Unlock()
	observed := NewMCPActionTrace(func(context.Context) error { return errors.New("fixture audit unavailable") })
	_, err := registry.ProxyMCPRequest(WithMCPActionTrace(context.Background(), observed), "fixture", nil, toolCallRequest(1, "write"))
	if !errors.Is(err, ErrMCPDispatchBlocked) {
		t.Fatalf("expected local refusal: %v", err)
	}
	if !c.breaker.Allow() {
		t.Fatal("local evidence failure stranded or delayed the next upstream health probe")
	}
}

func actionObservationTransport(t *testing.T, mode string, tool func(*http.Request, json.RawMessage) (*http.Response, error)) http.RoundTripper {
	t.Helper()
	return regressionRoundTripper(func(r *http.Request) (*http.Response, error) {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if r.Body != nil {
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode request: %v", err)
				return nil, errors.New("fixture decode failure")
			}
		}
		if request.Method == "tools/call" {
			return tool(r, request.ID)
		}
		var result any = map[string]any{}
		switch request.Method {
		case "server/discover":
			result = map[string]any{"supportedVersions": []string{"2026-07-28"}, "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "write", "inputSchema": map[string]any{"type": "object"}}}}
		}
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
		return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})
}

func TestMCPActionEvidenceCallbackPrecedesEffectsOnBothTransports(t *testing.T) {
	previous := httpClient
	t.Cleanup(func() { httpClient = previous })
	for _, mode := range []string{"json", "streamable_http"} {
		for _, blocked := range []bool{false, true} {
			name := mode + "/allowed"
			if blocked {
				name = mode + "/blocked"
			}
			t.Run(name, func(t *testing.T) {
				var effects, authorizations atomic.Int32
				httpClient = &http.Client{Transport: actionObservationTransport(t, mode, func(r *http.Request, id json.RawMessage) (*http.Response, error) {
					if authorizations.Load() != 1 {
						t.Error("effect preceded durable authorization callback")
					}
					effects.Add(1)
					return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":` + string(id) + `,"result":{"content":[]}}`))}, nil
				})}
				observed := NewMCPActionTrace(func(context.Context) error {
					authorizations.Add(1)
					if blocked {
						return errors.New("synthetic SQL evidence failure")
					}
					return nil
				})
				registry := NewRegistry(map[string]ConnectorConfig{"fixture": {Auth: AuthConfig{Type: "bearer", BearerToken: "synthetic-secret"}, MCP: &MCPConfig{URL: "https://example.invalid/mcp", Transport: mode}}})
				_, err := registry.ProxyMCPRequest(WithMCPActionTrace(context.Background(), observed), "fixture", nil, toolCallRequest(1, "write"))
				ob := observed.Snapshot()
				if blocked {
					if !errors.Is(err, ErrMCPDispatchBlocked) || effects.Load() != 0 || ob.DispatchStarted {
						t.Fatalf("blocked dispatch escaped: error=%v effects=%d observation=%+v", err, effects.Load(), ob)
					}
				} else if err != nil || effects.Load() != 1 || !ob.DispatchStarted || !ob.ResponseObserved || ob.HTTPStatus == nil || *ob.HTTPStatus != http.StatusOK {
					t.Fatalf("allowed dispatch evidence missing: error=%v effects=%d observation=%+v", err, effects.Load(), ob)
				}
				if authorizations.Load() != 1 {
					t.Fatalf("authorization callback calls=%d, want one", authorizations.Load())
				}
			})
		}
	}
}

func TestMCPActionObservationDoesNotRetainIDsOrPayloads(t *testing.T) {
	id := json.RawMessage(`"synthetic-sensitive-request-id"`)
	for _, tc := range []struct {
		name string
		body string
		kind string
	}{
		{"result", `{"jsonrpc":"2.0","id":` + string(id) + `,"result":{"content":[{"type":"text","text":"synthetic-sensitive-result"}]}}`, "jsonrpc_result"},
		{"error", `{"jsonrpc":"2.0","id":` + string(id) + `,"error":{"code":-32001,"message":"synthetic-sensitive-error","data":{"token":"synthetic-sensitive-token"}}}`, "jsonrpc_error"},
		{"tool_error", `{"jsonrpc":"2.0","id":` + string(id) + `,"result":{"isError":true,"content":[]}}`, "tool_error_reported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed := NewMCPActionTrace(func(context.Context) error { return nil })
			if err := observed.beforeDispatch(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := observed.observe([]byte(tc.body), id, false); err != nil {
				t.Fatal(err)
			}
			ob := observed.Snapshot()
			if !ob.ResponseObserved || ob.ResponseKind != tc.kind {
				t.Fatalf("wrong observation: %+v", ob)
			}
			raw, _ := json.Marshal(ob)
			if strings.Contains(string(raw), "synthetic-sensitive") {
				t.Fatal("observation retained request ID, provider text or tool payload")
			}
		})
	}
}

func TestMCPActionRejectsUnverifiedResponseOnlyWhenEnabled(t *testing.T) {
	previous := httpClient
	t.Cleanup(func() { httpClient = previous })
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":2,"result":{"content":[]}}`,
		`{"jsonrpc":"2.0","id":1,"id":1,"result":{}}`,
		`{"jsonrpc":"2.0","id":1,"RESULT":{}}`,
		`{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":1,"message":"error"}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":null,"message":"error"}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"isError":null}}`,
		`{"jsonrpc":"2.0","id":1,"result":`,
	} {
		httpClient = &http.Client{Transport: regressionRoundTripper(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		registry := NewRegistry(map[string]ConnectorConfig{"fixture": {Auth: AuthConfig{Type: "bearer", BearerToken: "synthetic"}, MCP: &MCPConfig{URL: "https://example.invalid/mcp"}}})
		if _, err := registry.ProxyMCPRequest(context.Background(), "fixture", nil, toolCallRequest(1, "write")); err != nil {
			t.Fatalf("disabled evidence changed legacy forwarding: %v", err)
		}
		observed := NewMCPActionTrace(func(context.Context) error { return nil })
		_, err := registry.ProxyMCPRequest(WithMCPActionTrace(context.Background(), observed), "fixture", nil, toolCallRequest(1, "write"))
		ob := observed.Snapshot()
		if !errors.Is(err, ErrMCPResponseUnverified) || !ob.DispatchStarted || ob.ResponseObserved {
			t.Fatalf("unverified response treated as an observation: error=%v observation=%+v", err, ob)
		}
	}
}

func TestMCPActionLostResponseNeverReplaysOnBothTransports(t *testing.T) {
	previous := httpClient
	t.Cleanup(func() { httpClient = previous })
	for _, mode := range []string{"json", "streamable_http"} {
		t.Run(mode, func(t *testing.T) {
			var effects atomic.Int32
			httpClient = &http.Client{Transport: actionObservationTransport(t, mode, func(*http.Request, json.RawMessage) (*http.Response, error) {
				effects.Add(1)
				return nil, errors.New("response lost after external effect")
			})}
			registry := NewRegistry(map[string]ConnectorConfig{"fixture": {Auth: AuthConfig{Type: "bearer", BearerToken: "synthetic"}, MCP: &MCPConfig{URL: "https://example.invalid/mcp", Transport: mode}}})
			observed := NewMCPActionTrace(func(context.Context) error { return nil })
			_, err := registry.ProxyMCPRequest(WithMCPActionTrace(context.Background(), observed), "fixture", nil, toolCallRequest(1, "write"))
			ob := observed.Snapshot()
			if err == nil || effects.Load() != 1 || !ob.DispatchStarted || ob.ResponseObserved {
				t.Fatalf("lost response replayed or falsely resolved: error=%v effects=%d observation=%+v", err, effects.Load(), ob)
			}
		})
	}
}
