package api_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/customroutes"
)

// TestCustomRoutes_ProxiesAndStripsPrefix proves /custom/* is forwarded to
// the configured URL with the /custom prefix stripped -- the user's
// app/sidecar sees paths as if mounted at its own root.
func TestCustomRoutes_ProxiesAndStripsPrefix(t *testing.T) {
	var gotPath, gotMethod string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("custom app response"))
	}))
	defer backend.Close()

	target, _ := url.Parse(backend.URL)
	proxy, err := customroutes.NewProxy(target, "/custom")
	if err != nil {
		t.Fatal(err)
	}

	env := newTestEnv(t)
	env.apiServer.SetCustomRoutesProxy(proxy, "/custom")

	resp, err := http.Post(env.srv.URL+"/custom/webhook/incoming", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if gotPath != "/webhook/incoming" {
		t.Errorf("expected backend to see stripped path /webhook/incoming, got %q", gotPath)
	}
	if gotMethod != "POST" {
		t.Errorf("expected POST forwarded, got %q", gotMethod)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 from proxy, got %d", resp.StatusCode)
	}
	if string(body) != "custom app response" {
		t.Errorf("expected backend response body forwarded, got %q", body)
	}
}

// TestCustomRoutes_ConfigurableMount proves a product-specific mount
// (not /custom) is honored end-to-end through the API server wrapper.
func TestCustomRoutes_ConfigurableMount(t *testing.T) {
	var gotPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	target, _ := url.Parse(backend.URL)
	proxy, err := customroutes.NewProxy(target, "/sales-assistant")
	if err != nil {
		t.Fatal(err)
	}
	env := newTestEnv(t)
	env.apiServer.SetCustomRoutesProxy(proxy, "/sales-assistant")

	resp, err := http.Get(env.srv.URL + "/sales-assistant/v1/favourites")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if gotPath != "/v1/favourites" {
		t.Errorf("want /v1/favourites, got %q", gotPath)
	}
}

// TestCustomRoutes_UnconfiguredIs404 proves the default (no
// SetCustomRoutesProxy call, matching production with no custom_routes
// config) leaves /custom/* as a plain 404, not a panic or hang.
func TestCustomRoutes_UnconfiguredIs404(t *testing.T) {
	env := newTestEnv(t)
	resp, err := http.Get(env.srv.URL + "/custom/anything")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for unconfigured custom routes, got %d", resp.StatusCode)
	}
}

// TestCustomRoutes_BackendDownReturns502 proves a genuinely unreachable
// backend surfaces as 502, not a hang or a raw connection-refused panic.
func TestCustomRoutes_BackendDownReturns502(t *testing.T) {
	unreachable, _ := url.Parse("http://127.0.0.1:1") // port 1: nothing listens there
	proxy, err := customroutes.NewProxy(unreachable, "/custom")
	if err != nil {
		t.Fatal(err)
	}

	env := newTestEnv(t)
	env.apiServer.SetCustomRoutesProxy(proxy, "/custom")

	resp, err := http.Get(env.srv.URL + "/custom/anything")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("expected 502 for an unreachable backend, got %d", resp.StatusCode)
	}
}

// TestCustomRoutes_InjectsAuthIdentityThroughAPIServer proves the
// identity headers reach the backend when the request is authenticated
// through the real auth middleware + API server path.
func TestCustomRoutes_InjectsAuthIdentityThroughAPIServer(t *testing.T) {
	var gotID, gotTenant string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID = r.Header.Get(customroutes.HeaderIdentity)
		gotTenant = r.Header.Get(customroutes.HeaderTenantID)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	target, _ := url.Parse(backend.URL)
	proxy, err := customroutes.NewProxy(target, "/custom")
	if err != nil {
		t.Fatal(err)
	}

	env := newTestEnv(t)
	env.apiServer.SetCustomRoutesProxy(proxy, "/custom")
	provider := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"test-key": {Name: "tester", TenantID: "t1", Permissions: []string{"admin"}},
	})
	env.srv.Close()
	env.srv = httptest.NewServer(auth.Middleware(provider, nil, nil, env.apiServer.Handler()))
	t.Cleanup(env.srv.Close)

	req, _ := http.NewRequest(http.MethodGet, env.srv.URL+"/custom/whoami", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set(customroutes.HeaderIdentity, "spoofed")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if gotID != "tester" {
		t.Errorf("identity: want tester, got %q", gotID)
	}
	if gotTenant != "t1" {
		t.Errorf("tenant: want t1, got %q", gotTenant)
	}
}
