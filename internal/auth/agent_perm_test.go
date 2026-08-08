package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getrunkite/runkite/internal/auth"
)

func TestCanRunAgent(t *testing.T) {
	cases := []struct {
		name   string
		perms  []string
		agent  string
		want   bool
		nilRes bool
	}{
		{name: "nil unrestricted", nilRes: true, agent: "echo", want: true},
		{name: "empty unrestricted", perms: nil, agent: "echo", want: true},
		{name: "write any", perms: []string{"write"}, agent: "echo", want: true},
		{name: "admin any", perms: []string{"admin"}, agent: "echo", want: true},
		{name: "exact grant", perms: []string{auth.AgentRunPermission("echo")}, agent: "echo", want: true},
		{name: "wrong agent", perms: []string{auth.AgentRunPermission("other")}, agent: "echo", want: false},
		{name: "read only", perms: []string{"read"}, agent: "echo", want: false},
		{name: "empty agent", perms: []string{"write"}, agent: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var res *auth.AuthResult
			if !tc.nilRes {
				res = &auth.AuthResult{Identity: "u", Permissions: tc.perms}
			}
			if got := auth.CanRunAgent(res, tc.agent); got != tc.want {
				t.Fatalf("CanRunAgent=%v want %v", got, tc.want)
			}
		})
	}
}

func TestAgentRunPermission_OnlyRunCreateRoutes(t *testing.T) {
	p := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"fine": {Permissions: []string{auth.AgentRunPermission("echo")}},
	})
	h := auth.Middleware(p, nil, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	allow := []struct{ method, path string }{
		{http.MethodPost, "/threads/t1/runs"},
		{http.MethodPost, "/threads/t1/runs/stream"},
		{http.MethodPost, "/threads/t1/runs/wait"},
		{http.MethodPost, "/runs"},
		{http.MethodPost, "/runs/stream"},
		{http.MethodPost, "/runs/wait"},
	}
	deny := []struct{ method, path string }{
		{http.MethodDelete, "/threads/t1"},
		{http.MethodPost, "/threads"},
		{http.MethodPost, "/threads/t1/runs/run-1/cancel"},
		{http.MethodPost, "/runs/search"},
		{http.MethodPut, "/store/items"},
		{http.MethodGet, "/threads/t1"},
		{http.MethodGet, "/runs/run-1"},
		{http.MethodDelete, "/runs/run-1"},
	}

	for _, tc := range allow {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer fine")
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("want allow %s %s, got %d", tc.method, tc.path, rec.Code)
		}
	}
	for _, tc := range deny {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer fine")
		h.ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Fatalf("want deny %s %s, got %d", tc.method, tc.path, rec.Code)
		}
	}
}

func TestAgentRunPermission_WithReadCanStreamButNotDelete(t *testing.T) {
	p := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"fine": {Permissions: []string{"read", auth.AgentRunPermission("echo")}},
	})
	h := auth.Middleware(p, nil, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	getRec := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/threads/t1/runs/r1/stream", nil)
	getReq.Header.Set("Authorization", "Bearer fine")
	h.ServeHTTP(getRec, getReq)
	if getRec.Code != 200 {
		t.Fatalf("read+agents:run should GET stream, got %d", getRec.Code)
	}

	delRec := httptest.NewRecorder()
	delReq := httptest.NewRequest(http.MethodDelete, "/threads/t1", nil)
	delReq.Header.Set("Authorization", "Bearer fine")
	h.ServeHTTP(delRec, delReq)
	if delRec.Code != 403 {
		t.Fatalf("read+agents:run must not DELETE threads, got %d", delRec.Code)
	}
}
