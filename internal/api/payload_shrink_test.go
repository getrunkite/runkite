package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/connector"
	"github.com/getrunkite/runkite/internal/payloadshrink"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func shrinkCallBody(tool, args string) string {
	if args == "" {
		args = "{}"
	}
	return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}`
}

func largeMCPResult() []byte {
	text := strings.Repeat("payload-chunk-", 120)
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

func shrinkSettings() payloadshrink.Settings {
	return payloadshrink.Settings{
		Enabled:       true,
		MaxBytes:      1024,
		PreviewBytes:  40,
		CacheTTL:      time.Minute,
		MaxStoreBytes: 1 << 20,
	}
}

func newMCPDown(t *testing.T, body []byte, status int) *httptest.Server {
	t.Helper()
	if status == 0 {
		status = http.StatusOK
	}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(down.Close)
	return down
}

func newShrinkAPI(t *testing.T, downURL string, cfg payloadshrink.Settings, store payloadshrink.Store, grants []policy.Grant) *Server {
	t.Helper()
	reg := connector.NewRegistry(map[string]connector.ConnectorConfig{
		"bank": {
			Auth: connector.AuthConfig{Type: "bearer", BearerToken: "tok"},
			MCP:  &connector.MCPConfig{URL: downURL},
		},
	})
	s := NewServer(nil, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	s.SetConnectorRegistry(reg)
	if grants != nil {
		s.SetPolicyEngine(policy.New(policy.Config{Grants: grants}))
	}
	s.SetPayloadShrink(cfg, store)
	return s
}

func doMCP(t *testing.T, s *Server, binding *auth.RunBinding, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/connectors/bank/mcp", strings.NewReader(body))
	req.SetPathValue("name", "bank")
	req = req.WithContext(auth.WithRunBinding(req.Context(), binding))
	attachConnectorSession(t, s, binding, "bank", req)
	rec := httptest.NewRecorder()
	s.handleProxyMCPRequest(rec, req)
	return rec
}

func payrollBinding() *auth.RunBinding {
	return &auth.RunBinding{
		RunID: "run-shrink-1", Generation: 1, TenantID: "acme", AgentID: "payroll",
		User: &transport.UserContext{Identity: "alice"},
	}
}

func TestPayloadShrink_DisabledByteIdentical(t *testing.T) {
	fixture := largeMCPResult()
	down := newMCPDown(t, fixture, 200)
	s := newShrinkAPI(t, down.URL, payloadshrink.Settings{Enabled: false, MaxBytes: 1024, PreviewBytes: 10, CacheTTL: time.Minute, MaxStoreBytes: 4096}, payloadshrink.NewMemoryStore(), nil)
	rec := doMCP(t, s, payrollBinding(), shrinkCallBody("query", `{"q":"x"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if rec.Body.String() != string(fixture)+"\n" && rec.Body.String() != string(fixture) {
		// httptest may not add newline; compare bytes
		if string(rec.Body.Bytes()) != string(fixture) {
			t.Fatalf("want fixture bytes\n got %s", rec.Body.String())
		}
	}
}

func TestPayloadShrink_DefaultOmitByteIdentical(t *testing.T) {
	fixture := largeMCPResult()
	down := newMCPDown(t, fixture, 200)
	s := newShrinkAPI(t, down.URL, payloadshrink.Settings{}, nil, nil)
	rec := doMCP(t, s, payrollBinding(), shrinkCallBody("query", `{}`))
	if string(rec.Body.Bytes()) != string(fixture) {
		t.Fatalf("omit shrink must match v0.4.1 bytes")
	}
}

func TestPayloadShrink_OverCapRetrieveOriginal(t *testing.T) {
	fixture := largeMCPResult()
	if len(fixture) <= 1024 {
		t.Fatalf("fixture %d", len(fixture))
	}
	down := newMCPDown(t, fixture, 200)
	store := payloadshrink.NewMemoryStore()
	s := newShrinkAPI(t, down.URL, shrinkSettings(), store, nil)
	rec := doMCP(t, s, payrollBinding(), shrinkCallBody("query", `{}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	if len(rec.Body.Bytes()) >= len(fixture) {
		t.Fatalf("expected stub smaller than %d, got %d", len(fixture), rec.Body.Len())
	}
	var env struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	text := ""
	if len(env.Result.Content) > 0 {
		text = env.Result.Content[0].Text
	}
	ref := extractShrinkRef(text)
	if ref == "" {
		t.Fatalf("no ref in %s", rec.Body.String())
	}
	rec2 := doMCP(t, s, payrollBinding(), shrinkCallBody(payloadshrink.ToolName, `{"ref":"`+ref+`"}`))
	if rec2.Code != http.StatusOK {
		t.Fatalf("retrieve status %d %s", rec2.Code, rec2.Body.String())
	}
	var got struct {
		Error  json.RawMessage `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Error) > 0 && string(got.Error) != "null" {
		t.Fatalf("retrieve error %s", rec2.Body.String())
	}
	var orig struct {
		Result json.RawMessage `json:"result"`
	}
	_ = json.Unmarshal(fixture, &orig)
	if string(got.Result) != string(orig.Result) {
		t.Fatalf("retrieve result mismatch")
	}
}

func TestPayloadShrink_RetrieveNoSession(t *testing.T) {
	s := newShrinkAPI(t, "http://127.0.0.1:1", shrinkSettings(), payloadshrink.NewMemoryStore(), nil)
	ensureConnectorSessions(s)
	req := httptest.NewRequest(http.MethodPost, "/internal/connectors/bank/mcp", strings.NewReader(shrinkCallBody(payloadshrink.ToolName, `{"ref":"abcd"}`)))
	req.SetPathValue("name", "bank")
	req = req.WithContext(auth.WithRunBinding(req.Context(), payrollBinding()))
	rec := httptest.NewRecorder()
	s.handleProxyMCPRequest(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d want 401, body %s", rec.Code, rec.Body.String())
	}
}

func TestPayloadShrink_RetrieveWrongGeneration(t *testing.T) {
	s := newShrinkAPI(t, "http://127.0.0.1:1", shrinkSettings(), payloadshrink.NewMemoryStore(), nil)
	bind := payrollBinding()
	req := httptest.NewRequest(http.MethodPost, "/internal/connectors/bank/mcp", strings.NewReader(shrinkCallBody(payloadshrink.ToolName, `{"ref":"abcd"}`)))
	req.SetPathValue("name", "bank")
	attachConnectorSession(t, s, bind, "bank", req)
	other := *bind
	other.Generation = 99
	req = req.WithContext(auth.WithRunBinding(req.Context(), &other))
	rec := httptest.NewRecorder()
	s.handleProxyMCPRequest(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d want 403, body %s", rec.Code, rec.Body.String())
	}
}

func TestPayloadShrink_UnknownRefJSONRPC(t *testing.T) {
	down := newMCPDown(t, []byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`), 200)
	s := newShrinkAPI(t, down.URL, shrinkSettings(), payloadshrink.NewMemoryStore(), nil)
	rec := doMCP(t, s, payrollBinding(), shrinkCallBody(payloadshrink.ToolName, `{"ref":"deadbeefdeadbeef"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "payload_not_found") || !strings.Contains(rec.Body.String(), "-32000") {
		t.Fatalf("want JSON-RPC payload_not_found, got %s", rec.Body.String())
	}
}

func TestPayloadShrink_MissingRefJSONRPC(t *testing.T) {
	down := newMCPDown(t, []byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`), 200)
	s := newShrinkAPI(t, down.URL, shrinkSettings(), payloadshrink.NewMemoryStore(), nil)
	rec := doMCP(t, s, payrollBinding(), shrinkCallBody(payloadshrink.ToolName, `{}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "missing ref") {
		t.Fatalf("want missing ref, got %s", rec.Body.String())
	}
}

func TestPayloadShrink_RedisDownPassesOriginal(t *testing.T) {
	fixture := largeMCPResult()
	down := newMCPDown(t, fixture, 200)
	s := newShrinkAPI(t, down.URL, shrinkSettings(), payloadshrink.FailStore{}, nil)
	rec := doMCP(t, s, payrollBinding(), shrinkCallBody("query", `{}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if string(rec.Body.Bytes()) != string(fixture) {
		t.Fatal("redis error must forward original body")
	}
}

func TestPayloadShrink_ToolsListInjectIffLive(t *testing.T) {
	listBody := []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"query"}]}}`)
	down := newMCPDown(t, listBody, 200)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

	off := newShrinkAPI(t, down.URL, payloadshrink.Settings{}, nil, nil)
	recOff := doMCP(t, off, payrollBinding(), body)
	if string(recOff.Body.Bytes()) != string(listBody) {
		t.Fatalf("off list must be identical, got %s", recOff.Body.String())
	}

	on := newShrinkAPI(t, down.URL, shrinkSettings(), payloadshrink.NewMemoryStore(), nil)
	recOn := doMCP(t, on, payrollBinding(), body)
	if !strings.Contains(recOn.Body.String(), payloadshrink.ToolName) {
		t.Fatalf("live list missing retrieve: %s", recOn.Body.String())
	}
}

func TestPayloadShrink_RetrieveBypassesDecide(t *testing.T) {
	store := payloadshrink.NewMemoryStore()
	cfg := shrinkSettings()
	inner := []byte(`{"content":[{"type":"text","text":"secret-original"}],"isError":false}`)
	ref := "abcabcabcabcabca"
	if err := store.Set(context.Background(), "rk:payload:v:run-shrink-1:1:"+ref, inner, time.Minute); err != nil {
		t.Fatal(err)
	}
	down := newMCPDown(t, []byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`), 200)
	s := newShrinkAPI(t, down.URL, cfg, store, []policy.Grant{{
		ID: "g", TenantID: "acme", AgentID: "payroll", Connector: "bank",
		Tools: &policy.ToolFilter{Allow: []string{"query"}},
	}})
	rec := doMCP(t, s, payrollBinding(), shrinkCallBody(payloadshrink.ToolName, `{"ref":"`+ref+`"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "denied by policy") {
		t.Fatalf("retrieve must bypass Decide: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "secret-original") {
		t.Fatalf("got %s", rec.Body.String())
	}
}

func TestPayloadShrink_BudgetSecondPassesThrough(t *testing.T) {
	cfg := shrinkSettings()
	cfg.MaxStoreBytes = 2000
	store := payloadshrink.NewMemoryStore()
	var n int
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		text := strings.Repeat(string(rune('a'+n)), 1200)
		inner, _ := json.Marshal(map[string]any{
			"content": []map[string]string{{"type": "text", "text": text}},
			"isError": false,
		})
		env, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "result": json.RawMessage(inner),
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(env)
	}))
	t.Cleanup(down.Close)
	s := newShrinkAPI(t, down.URL, cfg, store, nil)
	first := doMCP(t, s, payrollBinding(), shrinkCallBody("query", `{"n":1}`))
	if first.Code != http.StatusOK {
		t.Fatalf("first %d %s", first.Code, first.Body.String())
	}
	if !strings.Contains(first.Body.String(), payloadshrink.ToolName) {
		t.Fatal("first should shrink")
	}
	second := doMCP(t, s, payrollBinding(), shrinkCallBody("query", `{"n":2}`))
	if second.Code != http.StatusOK {
		t.Fatalf("second %d %s", second.Code, second.Body.String())
	}
	if strings.Contains(second.Body.String(), payloadshrink.ToolName) {
		t.Fatal("second over budget should pass through")
	}
}

func extractShrinkRef(text string) string {
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
