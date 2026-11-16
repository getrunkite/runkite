package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getrunkite/runkite/internal/auth"
)

func TestScopedSpecialistRoutePermissions(t *testing.T) {
	for _, profile := range []struct {
		name           string
		perms          []string
		mcp, get, wait bool
	}{
		{"run", []string{"agents:echo:run"}, true, false, false},
		{"observe", []string{"agents:echo:observe"}, false, true, true},
		{"specialist", []string{"agents:echo:run", "agents:echo:observe"}, true, true, true},
	} {
		t.Run(profile.name, func(t *testing.T) {
			p := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{"k": {Name: "caller", Permissions: profile.perms}})
			h := auth.MiddlewareWithOpts(p, nil, nil, auth.MiddlewareOpts{StrictPermissions: true}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
			for _, route := range []struct {
				method, path string
				allow        bool
			}{
				{"POST", "/mcp", profile.mcp}, {"GET", "/mcp", profile.mcp}, {"DELETE", "/mcp", profile.mcp},
				{"GET", "/runs/r", profile.get}, {"GET", "/runs/r/wait", profile.wait},
				{"GET", "/runs/r/stream", false}, {"GET", "/runs/r/cost", false}, {"HEAD", "/runs/r", false},
				{"GET", "/threads/t/runs/r", false}, {"GET", "/threads/t/runs/r/wait", false},
				{"POST", "/runs/search", false}, {"GET", "/threads/t", false}, {"GET", "/store/items", false},
				{"POST", "/runs/r/cancel", false}, {"DELETE", "/runs/r", false}, {"GET", "/admin-api/runs", false},
			} {
				req := httptest.NewRequest(route.method, route.path, nil)
				req.Header.Set("Authorization", "Bearer k")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				want := 403
				if route.allow {
					want = 204
				}
				if w.Code != want {
					t.Errorf("%s %s: got %d want %d", route.method, route.path, w.Code, want)
				}
			}
		})
	}
}

func TestCanObserveAgentSeparatesRunAuthority(t *testing.T) {
	for _, tc := range []struct {
		perms []string
		agent string
		want  bool
	}{
		{[]string{"agents:echo:run"}, "echo", false}, {[]string{"agents:echo:observe"}, "echo", true},
		{[]string{"agents:echo:observe"}, "other", false}, {[]string{"read"}, "other", true},
		{[]string{"agents:echo:observe"}, " echo ", false},
		{[]string{auth.AgentObservePermission(" echo ")}, " echo ", true},
		{[]string{auth.AgentObservePermission(" echo ")}, "echo", false},
		{[]string{"write"}, "other", true}, {[]string{"admin"}, "other", true},
	} {
		if got := auth.CanObserveAgent(&auth.AuthResult{Permissions: tc.perms}, tc.agent); got != tc.want {
			t.Fatalf("%v/%s: %v", tc.perms, tc.agent, got)
		}
	}
}
