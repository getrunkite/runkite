package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDiscoveryNeedsNoDatabaseCredentialAndExecutionDoes(t *testing.T) {
	t.Setenv("PGPASSWORD", "")
	handler, err := newExecutor("host=127.0.0.1 port=1 dbname=synthetic sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, params, want string }{{"initialize", `{}`, `"serverInfo"`}, {"ping", `{}`, `"result":{}`}, {"tools/list", `{}`, `"report_count"`}, {"tools/call", `{"name":"report_count","arguments":{}}`, `database authorization unavailable`}, {"tools/call", `{"name":"arbitrary_sql","arguments":{}}`, `only report_count`}, {"resources/read", `{}`, `method not supported`}, {"prompts/get", `{}`, `method not supported`}} {
		t.Run(tc.method+tc.want, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"`+tc.method+`","params":`+tc.params+`}`))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != 200 || !strings.Contains(w.Body.String(), tc.want) {
				t.Fatal("unexpected MCP behavior", w.Code, w.Body.String())
			}
		})
	}
}

func TestExecutorRejectsInlineCredentialsAndUnverifiedRemoteDatabase(t *testing.T) {
	t.Setenv("PGPASSWORD", "")
	for _, dsn := range []string{"host=127.0.0.1 password=synthetic-secret sslmode=disable", "host=remote.example sslmode=disable", "host=remote.example sslmode=require"} {
		if _, err := newExecutor(dsn); err == nil || strings.Contains(err.Error(), "synthetic-secret") {
			t.Fatal("unsafe database target accepted or secret exposed", err)
		}
	}
}
