package connector

import (
	"bytes"
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

func TestMCPTransportBindsDetachedCleanupToOperationDeadline(t *testing.T) {
	scope, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	transport := mcpAuthTransport{scope: scope, next: streamableTestTransport(func(r *http.Request) (*http.Response, error) {
		if _, ok := r.Context().Deadline(); !ok {
			t.Error("cleanup request has no operation deadline")
		}
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	// A background context models the SDK's detached session DELETE context.
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete, "https://upstream.invalid/mcp", nil)
	start := time.Now()
	_, err := transport.RoundTrip(request)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cleanup error=%v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("cleanup exceeded short operation deadline")
	}
}

func TestMCPTransportKeepsRequestAliveUntilBodyClose(t *testing.T) {
	scope, cancel := context.WithCancel(context.Background())
	defer cancel()
	var sent context.Context
	transport := mcpAuthTransport{scope: scope, next: streamableTestTransport(func(r *http.Request) (*http.Response, error) {
		sent = r.Context()
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("result"))}, nil
	})}
	request, _ := http.NewRequest(http.MethodGet, "https://upstream.invalid/mcp", nil)
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	if sent.Err() != nil {
		t.Fatalf("request canceled before response body read: %v", sent.Err())
	}
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "result" {
		t.Fatalf("response body=%q %v", body, err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(sent.Err(), context.Canceled) {
		t.Fatalf("body close did not release request context: %v", sent.Err())
	}
	if scope.Err() != nil {
		t.Fatal("body close canceled the parent operation")
	}
}

func TestMCPTransportScopeCancellationClosesDetachedRequest(t *testing.T) {
	scope, cancel := context.WithCancel(context.Background())
	var sent context.Context
	transport := mcpAuthTransport{scope: scope, next: streamableTestTransport(func(r *http.Request) (*http.Response, error) {
		sent = r.Context()
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("result"))}, nil
	})}
	request, _ := http.NewRequest(http.MethodDelete, "https://upstream.invalid/mcp", nil)
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	cancel()
	select {
	case <-sent.Done():
	case <-time.After(time.Second):
		t.Fatal("operation cancellation did not reach detached request")
	}
	if _, err := transport.RoundTrip(request); !errors.Is(err, context.Canceled) {
		t.Fatalf("request after scope cancel=%v", err)
	}
}

func TestStreamableSDKPreservesOverflowErrorsWithoutToolReplay(t *testing.T) {
	for _, format := range []string{"json", "sse"} {
		t.Run(format, func(t *testing.T) {
			previous := httpClient
			defer func() { httpClient = previous }()
			var calls atomic.Int32
			httpClient = &http.Client{Transport: streamableTestTransport(func(r *http.Request) (*http.Response, error) {
				var raw []byte
				if r.Body != nil {
					raw, _ = io.ReadAll(r.Body)
				}
				var request struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
				}
				_ = json.Unmarshal(raw, &request)
				contentType := "application/json"
				var body []byte
				if request.Method == "tools/call" {
					calls.Add(1)
					if format == "sse" {
						contentType = "text/event-stream"
						// Valid SSE comment lines exceed the byte cap before any tool result.
						body = []byte(strings.Repeat(":"+strings.Repeat("p", 1022)+"\n", int(MaxMCPResponseBytes)/1024+1))
					} else {
						body = []byte(`{"jsonrpc":"2.0","id":` + string(request.ID) + `,"result":{"content":[{"type":"text","text":"` + strings.Repeat("x", int(MaxMCPResponseBytes)) + `"}]}}`)
					}
				} else {
					var result any
					switch request.Method {
					case "server/discover":
						result = map[string]any{"supportedVersions": []string{"2026-07-28"}, "capabilities": map[string]any{"tools": map[string]any{}}}
					case "tools/list":
						result = map[string]any{"tools": []any{map[string]any{"name": "read", "inputSchema": map[string]any{"type": "object"}}}}
					default:
						result = map[string]any{}
					}
					body, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
				}
				return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(bytes.NewReader(body))}, nil
			})}
			registry := NewRegistry(map[string]ConnectorConfig{"test": {Auth: AuthConfig{Type: "api_key", APIKey: "synthetic"}, MCP: &MCPConfig{URL: "https://upstream.invalid/mcp", Transport: "streamable_http"}}})
			_, err := registry.ProxyMCPRequest(context.Background(), "test", nil, toolCallRequest(1, "read"))
			if !errors.Is(err, ErrBodyTooLarge) {
				t.Fatalf("SDK overflow lost typed error: %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("write tool attempts=%d, want one", calls.Load())
			}
		})
	}
}
