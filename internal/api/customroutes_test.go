package api_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
)

// buildTestCustomProxy mirrors cmd/serve.go's initCustomRoutesProxy exactly
// (strip /custom, reverse-proxy the rest) so this test exercises the same
// mounting logic the real binary uses, without needing to spin up the
// actual binary + config file parsing for a unit test.
func buildTestCustomProxy(target *url.URL) http.Handler {
	return http.StripPrefix("/custom", httputil.NewSingleHostReverseProxy(target))
}

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
	env := newTestEnv(t)
	env.apiServer.SetCustomRoutesProxy(buildTestCustomProxy(target))

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
// backend surfaces as 502 (via the ErrorHandler configured in
// initCustomRoutesProxy), not a hang or a raw connection-refused panic.
func TestCustomRoutes_BackendDownReturns502(t *testing.T) {
	unreachable, _ := url.Parse("http://127.0.0.1:1") // port 1: nothing listens there
	proxy := httputil.NewSingleHostReverseProxy(unreachable)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.WriteHeader(http.StatusBadGateway)
	}

	env := newTestEnv(t)
	env.apiServer.SetCustomRoutesProxy(http.StripPrefix("/custom", proxy))

	resp, err := http.Get(env.srv.URL + "/custom/anything")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("expected 502 for an unreachable backend, got %d", resp.StatusCode)
	}
}
