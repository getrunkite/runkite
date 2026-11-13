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
)

type exactToolFixture struct {
	listing atomic.Int32
	calls   atomic.Int32
	format  string
	result  func(int32, string) string
}

func (f *exactToolFixture) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodDelete {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Cursor string `json:"cursor"`
		} `json:"params"`
	}
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &request)
	}
	result := `{}`
	switch request.Method {
	case "server/discover":
		result = `{"supportedVersions":["2026-07-28"],"capabilities":{"tools":{}}}`
	case "tools/list":
		result = f.result(f.listing.Add(1), request.Params.Cursor)
	case "tools/call":
		f.calls.Add(1)
		result = `{"content":[{"type":"text","text":"ok"}]}`
	}
	if len(request.ID) == 0 {
		return &http.Response{StatusCode: 202, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	wire := `{"jsonrpc":"2.0","id":` + string(request.ID) + `,"result":` + result + `}`
	media := "application/json"
	if f.format == "sse" && request.Method == "tools/list" {
		media = "text/event-stream"
		wire = ": heartbeat\r\n\r\nevent: message\r\ndata: " + wire + "\r\n\r\n"
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {media}}, Body: io.NopCloser(strings.NewReader(wire))}, nil
}

func exactRegistryFixture(t *testing.T, f *exactToolFixture) *Registry {
	t.Helper()
	previous := httpClient
	t.Cleanup(func() { httpClient = previous })
	httpClient = &http.Client{Transport: f}
	return NewRegistry(map[string]ConnectorConfig{"files": {Auth: AuthConfig{Type: "bearer", BearerToken: "synthetic"}, MCP: &MCPConfig{URL: "https://mcp.invalid/fixture", Transport: "streamable_http", PinTools: true}}})
}

func TestStreamableExactToolRevisionRejectsSameSessionSchemaChange(t *testing.T) {
	for _, format := range []string{"json", "sse"} {
		for _, change := range []string{"large-number", "unknown-annotation"} {
			t.Run(format+"/"+change, func(t *testing.T) {
				first := `{"name":"write","inputSchema":{"type":"object","properties":{"amount":{"const":9007199254740992}}},"annotations":{"x-operator":{"owner":"first"}}}`
				second := strings.Replace(first, "9007199254740992", "9007199254740993", 1)
				if change == "unknown-annotation" {
					second = strings.Replace(first, `"owner":"first"`, `"owner":"second"`, 1)
				}
				f := &exactToolFixture{format: format, result: func(call int32, _ string) string {
					definition := first
					if call >= 3 {
						definition = second
					}
					return `{"tools":[` + definition + `]}`
				}}
				registry := exactRegistryFixture(t, f)
				ctx, _, err := registry.BindMCPRequestTarget(t.Context(), "files")
				if err != nil {
					t.Fatal(err)
				}
				ctx, original, err := registry.BindMCPToolRevision(ctx, "files", "write", nil)
				if err != nil {
					t.Fatal("initial exact definition unavailable", err)
				}
				var prepared atomic.Int32
				ctx = WithMCPActionTrace(ctx, NewMCPActionTrace(func(context.Context) error { prepared.Add(1); return nil }))
				if _, err := registry.ProxyMCPRequest(ctx, "files", nil, toolCallRequest(1, "write")); !errors.Is(err, ErrToolRevisionChanged) {
					t.Fatal("same-session changed raw definition accepted", err)
				}
				if f.calls.Load() != 0 || prepared.Load() != 0 {
					t.Fatal("changed schema consumed authority or dispatched tool")
				}
				fresh, _, _ := registry.BindMCPRequestTarget(t.Context(), "files")
				fresh, current, err := registry.BindMCPToolRevision(fresh, "files", "write", nil)
				if err != nil || current == original {
					t.Fatal("distinct raw definitions shared a revision", err)
				}
				fresh = WithMCPActionTrace(fresh, NewMCPActionTrace(func(context.Context) error { prepared.Add(1); return nil }))
				response, err := registry.ProxyMCPRequest(fresh, "files", nil, toolCallRequest(1, "write"))
				if err != nil || response == nil || f.calls.Load() != 1 || prepared.Load() != 1 {
					t.Fatal("stable exact schema did not forward once", err)
				}
			})
		}
	}
}

func TestStreamableRawToolDiscoveryRejectsAmbiguityBeforeSDKConversion(t *testing.T) {
	for _, format := range []string{"json", "sse"} {
		for _, tools := range []string{
			`[{"name":"write","name":"read","inputSchema":{"type":"object"}}]`,
			`[{"name":"write","Name":"read","inputSchema":{"type":"object"}}]`,
			`[{"name":"write","inputSchema":{"type":"object","const":1,"const":2}}]`,
			`[{"name":"write","inputSchema":{"type":"object"}},{"name":"write","inputSchema":{"type":"object"}}]`,
		} {
			t.Run(format+tools, func(t *testing.T) {
				f := &exactToolFixture{format: format, result: func(int32, string) string { return `{"tools":` + tools + `}` }}
				r := exactRegistryFixture(t, f)
				ctx, _, _ := r.BindMCPRequestTarget(t.Context(), "files")
				if _, _, err := r.BindMCPToolRevision(ctx, "files", "write", nil); !errors.Is(err, ErrToolRevisionUnavailable) {
					t.Fatal("ambiguous raw tool definition accepted", err)
				}
				if f.calls.Load() != 0 {
					t.Fatal("schema discovery dispatched tool")
				}
			})
		}
	}
}

func TestStreamableFinalDiscoveryChecksEveryPage(t *testing.T) {
	for _, format := range []string{"json", "sse"} {
		t.Run(format, func(t *testing.T) {
			f := &exactToolFixture{format: format, result: func(call int32, cursor string) string {
				if cursor == "" {
					return `{"tools":[{"name":"write","inputSchema":{"type":"object"}}],"nextCursor":"second"}`
				}
				if call >= 6 {
					return `{"tools":[{"name":"write","inputSchema":{"type":"object"}}]}`
				}
				return `{"tools":[{"name":"read","inputSchema":{"type":"object"}}]}`
			}}
			r := exactRegistryFixture(t, f)
			ctx, _, _ := r.BindMCPRequestTarget(t.Context(), "files")
			ctx, _, err := r.BindMCPToolRevision(ctx, "files", "write", nil)
			if err != nil {
				t.Fatal(err)
			}
			var prepared atomic.Int32
			ctx = WithMCPActionTrace(ctx, NewMCPActionTrace(func(context.Context) error { prepared.Add(1); return nil }))
			if _, err := r.ProxyMCPRequest(ctx, "files", nil, toolCallRequest(1, "write")); !errors.Is(err, ErrToolRevisionUnavailable) {
				t.Fatal("duplicate definition on final later page accepted", err)
			}
			if prepared.Load() != 0 || f.calls.Load() != 0 {
				t.Fatal("later-page ambiguity consumed authority")
			}
		})
	}
}

func TestStreamableRawToolCursorCyclesFailClosed(t *testing.T) {
	f := &exactToolFixture{format: "sse", result: func(_ int32, cursor string) string {
		return `{"tools":[{"name":"` + map[bool]string{true: "write", false: "read"}[cursor == ""] + `","inputSchema":{"type":"object"}}],"nextCursor":"again"}`
	}}
	r := exactRegistryFixture(t, f)
	ctx, _, _ := r.BindMCPRequestTarget(t.Context(), "files")
	if _, _, err := r.BindMCPToolRevision(ctx, "files", "write", nil); !errors.Is(err, ErrToolRevisionUnavailable) || f.listing.Load() != 2 {
		t.Fatal("cursor cycle accepted or unbounded", err, f.listing.Load())
	}
}

func TestRawToolSSECorrelatesResponseIDAndPreservesUnknownFields(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n", "\r"} {
		t.Run(newline, func(t *testing.T) {
			wrong := `{"jsonrpc":"2.0","id":"other","result":{"tools":[{"name":"wrong","inputSchema":{}}]}}`
			right := `{"jsonrpc":"2.0","id":7,"result":{"tools":[{"name":"write","inputSchema":{"const":9007199254740993},"annotations":{"unknown":9007199254740993}}]}}`
			wire := "event: message" + newline + "data: " + wrong + newline + newline + "data: " + right + newline + newline
			captured, page, err := captureToolSSE(strings.NewReader(wire), json.RawMessage(`7`), MaxMCPResponseBytes)
			if err != nil || len(captured) == 0 || len(page.tools) != 1 || page.tools[0]["name"] != "write" || !bytes.Contains(page.result, []byte(`9007199254740993`)) {
				t.Fatal("SSE authority used an unrelated or lossy response", err)
			}
			if _, _, err := captureToolSSE(strings.NewReader("data: "+wrong+newline+newline), json.RawMessage(`7`), MaxMCPResponseBytes); !errors.Is(err, ErrToolRevisionUnavailable) {
				t.Fatal("unmatched SSE response accepted")
			}
		})
	}
}

func TestRawToolCaptureIsBoundedAndPublicDefaultUnchanged(t *testing.T) {
	if _, _, err := captureToolSSE(strings.NewReader(":"+strings.Repeat("p", 1024)), json.RawMessage(`1`), 128); !errors.Is(err, ErrToolRevisionUnavailable) {
		t.Fatal("SSE capture was unbounded")
	}
	f := &exactToolFixture{format: "json", result: func(int32, string) string { return `{"tools":[{"name":"write","inputSchema":{"type":"object"}}]}` }}
	r := exactRegistryFixture(t, f)
	entry, _ := r.Get("files")
	entry.Config.MCP.PinTools = false
	if _, err := r.ProxyMCPRequest(t.Context(), "files", nil, toolCallRequest(1, "write")); err != nil || f.calls.Load() != 1 {
		t.Fatal("default unpinned transport changed", err)
	}
}

type readAfterResponse struct{ called atomic.Int32 }

func (r *readAfterResponse) Read([]byte) (int, error) {
	r.called.Add(1)
	return 0, errors.New("read past correlated response")
}

func TestRawToolSSEDoesNotWaitForEOFOrIgnoreAnUnmatchedResponse(t *testing.T) {
	wire := "data: " + `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"write","inputSchema":{}}]}}` + "\n\n"
	tail := &readAfterResponse{}
	if _, _, err := captureToolSSE(io.MultiReader(strings.NewReader(wire), tail), json.RawMessage(`1`), MaxMCPResponseBytes); err != nil || tail.called.Load() != 0 {
		t.Fatal("capture waited past a complete correlated response", err)
	}
}

func TestStreamableRawToolCaptureCapsAggregateListingBytes(t *testing.T) {
	padding := strings.Repeat("p", 4<<20)
	f := &exactToolFixture{format: "json", result: func(_ int32, cursor string) string {
		if cursor == "" {
			return `{"tools":[{"name":"write","inputSchema":{"type":"object"},"description":"` + padding + `"}],"nextCursor":"second"}`
		}
		return `{"tools":[{"name":"read","inputSchema":{"type":"object"},"description":"` + padding + `"}]}`
	}}
	r := exactRegistryFixture(t, f)
	ctx, _, _ := r.BindMCPRequestTarget(t.Context(), "files")
	if _, _, err := r.BindMCPToolRevision(ctx, "files", "write", nil); !errors.Is(err, ErrToolRevisionUnavailable) || f.listing.Load() != 2 || f.calls.Load() != 0 {
		t.Fatal("aggregate listing exceeded8MiB without failing closed", err)
	}
}
