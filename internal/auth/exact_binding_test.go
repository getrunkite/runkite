package auth_test

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
)

func TestRunBindingPreservesExactTenant(t *testing.T) {
	want := " acme "
	lookup := mapInflight{"run": {RunID: "run", Generation: 1, TenantID: want}}
	h := auth.MiddlewareWithOpts(nil, nil, nil, auth.MiddlewareOpts{Inflight: lookup}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := tenant.FromContext(r.Context()); got != want {
			t.Errorf("tenant changed from %q to %q", want, got)
		}
	}))
	req := httptest.NewRequest("GET", "/internal/store/items", nil)
	req.Header.Set(auth.HeaderRunID, "run")
	req.Header.Set(auth.HeaderGeneration, "1")
	h.ServeHTTP(httptest.NewRecorder(), req)
}

func TestRunnerTenantAllowlistDoesNotNormalizeIdentities(t *testing.T) {
	withEnv(t, map[string]string{"RUNNER_TOKEN_PYTHON_LANGGRAPH": "token", "RUNNER_TENANTS_PYTHON_LANGGRAPH": "default,acme"})
	rt := auth.LoadRunnerTokensFromEnv()
	for _, tid := range []string{" acme", "acme ", "\tacme", " "} {
		if rt.AllowsTenant("python-langgraph", tid) {
			t.Errorf("distinct tenant %q borrowed an allowlist identity", tid)
		}
	}
}

func TestEncodedTenantHeaderPreservesExactIdentity(t *testing.T) {
	want := "雪"
	h := auth.MiddlewareWithOpts(nil, nil, nil, auth.MiddlewareOpts{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := tenant.FromContext(r.Context()); got != want {
			t.Errorf("got tenant %q, want %q", got, want)
		}
	}))
	r := httptest.NewRequest("GET", "/internal/runs/run/status", nil)
	r.Header.Set("X-Runkite-Tenant-Id-B64", base64.RawURLEncoding.EncodeToString([]byte(want)))
	h.ServeHTTP(httptest.NewRecorder(), r)
}

func TestRunBindingEncodedIdentityIsExact(t *testing.T) {
	const encodedHeader = "X-Runkite-Run-Id-B64"
	for _, id := range []string{"run/雪?#%", " legacy ", "line\nbreak"} {
		t.Run(id, func(t *testing.T) {
			lookup := mapInflight{id: {RunID: id, Generation: 1, TenantID: "acme", GraphID: "agent"}}
			called := false
			h := auth.MiddlewareWithOpts(nil, nil, nil, auth.MiddlewareOpts{Inflight: lookup}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				if b := auth.RunBindingFromContext(r.Context()); b == nil || b.RunID != id {
					t.Fatalf("wrong binding: %+v", b)
				}
			}))
			req := httptest.NewRequest("GET", "/internal/store/items", nil)
			req.Header.Set(encodedHeader, base64.RawURLEncoding.EncodeToString([]byte(id)))
			req.Header.Set(auth.HeaderGeneration, "1")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if !called || w.Code != 200 {
				t.Fatalf("encoded exact ID rejected: %d %s", w.Code, w.Body.String())
			}
		})
	}
	for _, tc := range []struct{ raw, encoded string }{{"run", "cnVu"}, {" run ", ""}, {"", "not+url"}, {"", "_w"}, {"", "Zh"}} {
		t.Run(tc.raw+tc.encoded, func(t *testing.T) {
			lookup := mapInflight{"run": &transport.RunAssignment{RunID: "run", Generation: 1}}
			h := auth.MiddlewareWithOpts(nil, nil, nil, auth.MiddlewareOpts{Inflight: lookup}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("ambiguous binding reached handler") }))
			req := httptest.NewRequest("GET", "/internal/store/items", nil)
			if tc.raw != "" {
				req.Header.Set(auth.HeaderRunID, tc.raw)
			}
			if tc.encoded != "" {
				req.Header.Set(encodedHeader, tc.encoded)
			}
			req.Header.Set(auth.HeaderGeneration, "1")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != 401 {
				t.Fatalf("status=%d, want 401", w.Code)
			}
		})
	}
}
