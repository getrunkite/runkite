package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRunnerKindContextRequiresAuthenticatedToken(t *testing.T) {
	production := &RunnerTokens{tokens: map[string][]string{"schema-owner": {"synthetic-owner-token"}}}
	for _, tc := range []struct {
		name, kind, token, wantKind string
		tokens                      *RunnerTokens
		status                      int
	}{
		{name: "verified", tokens: production, kind: "schema-owner", token: "synthetic-owner-token", wantKind: "schema-owner", status: 204},
		{name: "wrong_token", tokens: production, kind: "schema-owner", token: "synthetic-wrong-token", status: 401},
		{name: "cross_kind", tokens: production, kind: "schema-other", token: "synthetic-owner-token", status: 401},
		{name: "missing_credentials", tokens: production, status: 401},
		{name: "local_no_headers", status: 204},
		{name: "local_spoofed_headers", kind: "schema-owner", token: "synthetic-owner-token", status: 204},
		{name: "empty_token_catalog", tokens: &RunnerTokens{}, kind: "schema-owner", token: "synthetic-owner-token", status: 204},
	} {
		for _, route := range []struct{ method, path string }{{"PUT", "/internal/agents/agent/schema"}, {"POST", "/internal/agents/agent/schema/recover"}} {
			t.Run(tc.name+"/"+route.method, func(t *testing.T) {
				calls := 0
				handler := Middleware(nil, nil, tc.tokens, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if got := RunnerKindFromContext(r.Context()); got != tc.wantKind {
						t.Errorf("authenticated kind=%q, want %q", got, tc.wantKind)
					}
					w.WriteHeader(http.StatusNoContent)
				}))
				r := httptest.NewRequest(route.method, route.path, nil)
				r.Header.Set(HeaderRunnerKind, tc.kind)
				r.Header.Set(HeaderRunnerToken, tc.token)
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if w.Code != tc.status || (tc.status == 204 && calls != 1) || (tc.status != 204 && calls != 0) {
					t.Fatalf("status=%d calls=%d, want status=%d", w.Code, calls, tc.status)
				}
			})
		}
	}
}
