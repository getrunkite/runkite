package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
)

type mapInflight map[string]*transport.RunAssignment

func (m mapInflight) LookupInflight(_ context.Context, runID string) (*transport.RunAssignment, error) {
	return m[runID], nil
}

func TestMiddleware_RunBindingRequiredOnStore(t *testing.T) {
	inflight := mapInflight{
		"run-1": {RunID: "run-1", Generation: 2, TenantID: "acme", GraphID: "echo"},
	}
	var gotTenant, gotAgent string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTenant = tenant.FromContext(r.Context())
		if b := auth.RunBindingFromContext(r.Context()); b != nil {
			gotAgent = b.AgentID
		}
		w.WriteHeader(http.StatusOK)
	})
	handler := auth.MiddlewareWithOpts(nil, nil, nil, auth.MiddlewareOpts{Inflight: inflight}, next)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/internal/store/items", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unbound store call: status %d, want 401", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["reason_code"] != auth.ReasonRunBindingRequired {
		t.Fatalf("reason_code=%q", body["reason_code"])
	}

	req := httptest.NewRequest("GET", "/internal/store/items", nil)
	req.Header.Set(auth.HeaderRunID, "run-1")
	req.Header.Set(auth.HeaderGeneration, "2")
	req.Header.Set(auth.HeaderTenantID, "evil") // must be ignored
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bound call: status %d body %s", rec.Code, rec.Body.String())
	}
	if gotTenant != "acme" {
		t.Fatalf("tenant=%q, want acme (from assignment, not header)", gotTenant)
	}
	if gotAgent != "echo" {
		t.Fatalf("agent=%q, want echo", gotAgent)
	}
}

func TestMiddleware_RunBindingGenerationMismatch(t *testing.T) {
	inflight := mapInflight{
		"run-1": {RunID: "run-1", Generation: 2, TenantID: "acme"},
	}
	handler := auth.MiddlewareWithOpts(nil, nil, nil, auth.MiddlewareOpts{Inflight: inflight},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))

	req := httptest.NewRequest("POST", "/internal/connectors/sf/session", nil)
	req.Header.Set(auth.HeaderRunID, "run-1")
	req.Header.Set(auth.HeaderGeneration, "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["reason_code"] != auth.ReasonRunGenerationMismatch {
		t.Fatalf("reason_code=%q", body["reason_code"])
	}
}

func TestMiddleware_RunBindingGenerationZeroIsNotWildcard(t *testing.T) {
	for _, tc := range []struct {
		name       string
		assignment int64
		presented  int64
		want       int
	}{
		{"legacy_cannot_use_current", 2, 0, http.StatusForbidden},
		{"current_cannot_use_legacy", 0, 2, http.StatusForbidden},
		{"actual_legacy_matches", 0, 0, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inflight := mapInflight{"run": {RunID: "run", Generation: tc.assignment, TenantID: "acme"}}
			handler := auth.MiddlewareWithOpts(nil, nil, nil, auth.MiddlewareOpts{Inflight: inflight},
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
			req := httptest.NewRequest("POST", "/internal/connectors/example/session", nil)
			req.Header.Set(auth.HeaderRunID, "run")
			req.Header.Set(auth.HeaderGeneration, strconv.FormatInt(tc.presented, 10))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestMiddleware_RunBindingRequiredOnCheckpoints(t *testing.T) {
	handler := auth.MiddlewareWithOpts(nil, nil, nil, auth.MiddlewareOpts{Inflight: mapInflight{}},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("PUT", "/internal/checkpoints/thr-1/cp-1", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unbound checkpoint call: status %d, want 401", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["reason_code"] != auth.ReasonRunBindingRequired {
		t.Fatalf("reason_code=%q", body["reason_code"])
	}
}

func TestMiddleware_RunBindingNotInflight(t *testing.T) {
	handler := auth.MiddlewareWithOpts(nil, nil, nil, auth.MiddlewareOpts{Inflight: mapInflight{}},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))

	req := httptest.NewRequest("POST", "/internal/vectors/search", nil)
	req.Header.Set(auth.HeaderRunID, "missing")
	req.Header.Set(auth.HeaderGeneration, "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["reason_code"] != auth.ReasonRunNotInflight {
		t.Fatalf("reason_code=%q", body["reason_code"])
	}
}

func TestMiddleware_RunBindingAttachesPrincipal(t *testing.T) {
	inflight := mapInflight{
		"run-1": {
			RunID: "run-1", Generation: 1, TenantID: "acme", GraphID: "echo",
			User: &transport.UserContext{Identity: "alice", DisplayName: "Alice"},
		},
	}
	var gotIdentity string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ar := auth.FromContext(r.Context()); ar != nil {
			gotIdentity = ar.Identity
		}
		w.WriteHeader(200)
	})
	handler := auth.MiddlewareWithOpts(nil, nil, nil, auth.MiddlewareOpts{Inflight: inflight}, next)

	req := httptest.NewRequest("POST", "/internal/connectors/sf/mcp", nil)
	req.Header.Set(auth.HeaderRunID, "run-1")
	req.Header.Set(auth.HeaderGeneration, strconv.FormatInt(1, 10))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if gotIdentity != "alice" {
		t.Fatalf("identity=%q, want alice from assignment.User", gotIdentity)
	}
}

func TestMiddleware_UnboundInternalStillUsesTenantHeader(t *testing.T) {
	// Schema / status paths are not run-bound; tenant header still applies.
	inflight := mapInflight{}
	var got string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = tenant.FromContext(r.Context())
		w.WriteHeader(200)
	})
	handler := auth.MiddlewareWithOpts(nil, nil, nil, auth.MiddlewareOpts{Inflight: inflight}, next)

	req := httptest.NewRequest("GET", "/internal/runs/x/status", nil)
	req.Header.Set(auth.HeaderTenantID, "acme")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if got != "acme" {
		t.Fatalf("tenant=%q", got)
	}
}

func TestMiddleware_RunBindingRequiresRunnerKindOwner(t *testing.T) {
	t.Setenv("RUNNER_TOKEN_PYTHON_LANGGRAPH", "tok-py")
	t.Setenv("RUNNER_TENANTS_PYTHON_LANGGRAPH", "acme")
	tokens := auth.LoadRunnerTokensFromEnv()
	inflight := mapInflight{"run-foreign": {RunID: "run-foreign", RunnerKind: "typescript-langgraphjs", Generation: 1, TenantID: "acme"}}
	handler := auth.MiddlewareWithOpts(nil, nil, tokens, auth.MiddlewareOpts{Inflight: inflight}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("foreign run reached handler") }))
	req := httptest.NewRequest("GET", "/internal/store/items", nil)
	req.Header.Set("X-Runner-Kind", "python-langgraph")
	req.Header.Set("X-Runner-Token", "tok-py")
	req.Header.Set(auth.HeaderRunID, "run-foreign")
	req.Header.Set(auth.HeaderGeneration, "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusForbidden || body["reason_code"] != auth.ReasonRunRunnerKindMismatch {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
