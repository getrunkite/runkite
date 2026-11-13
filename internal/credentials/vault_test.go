package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func privateFixtureFile(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private")
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func fixtureVault(t *testing.T, handler http.Handler) (*VaultProvider, string) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	file := privateFixtureFile(t, "synthetic-token")
	p, err := NewVaultProvider(VaultConfig{Address: srv.URL, Namespace: "team", TokenFile: file, Mount: "database", Role: "readonly", AllowInsecureLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	return p, file
}

func TestVaultUsesRestrictedRotatingTokenAndSynchronousRevoke(t *testing.T) {
	handle := "database/creds/readonly/synthetic-lease"
	var issued atomic.Int32
	p, file := fixtureVault(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Namespace") != "team" {
			t.Error("namespace missing")
		}
		switch r.URL.Path {
		case "/v1/database/creds/readonly":
			if r.Method != "GET" || r.Header.Get("X-Vault-Token") != "synthetic-token" {
				t.Error("issue authentication wrong")
			}
			issued.Add(1)
			_, _ = w.Write([]byte(`{"lease_id":"` + handle + `","lease_duration":43,"renewable":true,"data":{"username":"synthetic-user","password":"synthetic-password"}}`))
		case "/v1/sys/leases/renew":
			if r.Header.Get("X-Vault-Token") != "rotated-token" {
				t.Error("workload token was cached")
			}
			var body struct {
				Lease     string `json:"lease_id"`
				Increment int    `json:"increment"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Lease != handle || body.Increment != 30 {
				t.Error("renewal body wrong")
			}
			_, _ = w.Write([]byte(`{"lease_id":"` + handle + `","lease_duration":12,"renewable":true}`))
		case "/v1/sys/leases/revoke":
			var body struct {
				Lease string `json:"lease_id"`
				Sync  bool   `json:"sync"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Lease != handle || !body.Sync {
				t.Error("revocation was not synchronous")
			}
			w.WriteHeader(204)
		default:
			t.Error("unexpected path")
			w.WriteHeader(404)
		}
	}))
	g, err := p.Issue(t.Context())
	if err != nil || g.TTL != 43*time.Second {
		t.Fatal(g, err)
	}
	if err := os.WriteFile(file, []byte("rotated-token"), 0600); err != nil {
		t.Fatal(err)
	}
	renewed, err := p.Renew(t.Context(), handle, 30*time.Second)
	if err != nil || renewed.TTL != 12*time.Second {
		t.Fatal(renewed, err)
	}
	if err := p.Revoke(t.Context(), handle); err != nil {
		t.Fatal(err)
	}
	if issued.Load() != 1 || p.client.Transport.(*http.Transport).DisableKeepAlives != true {
		t.Fatal("issuance replay protection unavailable")
	}
}

func TestVaultLookupNeverTreatsArbitrary404OrErrorsAsRevoked(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		absent bool
	}{{400, `{"errors":["invalid lease"]}`, true}, {404, `{"errors":["invalid lease"]}`, false}, {403, `{"errors":["permission denied"]}`, false}, {500, `{"errors":["synthetic-provider-secret"]}`, false}, {400, `{"errors":["invalid lease","other"]}`, false}} {
		t.Run(tc.body+http.StatusText(tc.status), func(t *testing.T) {
			p, _ := fixtureVault(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			status, err := p.Lookup(t.Context(), "database/creds/readonly/synthetic")
			if tc.absent {
				if err != nil || status.Found {
					t.Fatal("documented lease absence not recognized", status, err)
				}
			} else if err == nil || strings.Contains(err.Error(), "synthetic-provider-secret") {
				t.Fatal("unverified absence or secret-bearing error", err)
			}
		})
	}
}

func TestVaultRejectsForeignLeaseBeforeSendingAndRedirects(t *testing.T) {
	var effects atomic.Int32
	p, _ := fixtureVault(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effects.Add(1)
		w.Header().Set("Location", "https://other.example/secret")
		w.WriteHeader(307)
	}))
	if _, err := p.Renew(t.Context(), "database/creds/admin/foreign", time.Second); err == nil {
		t.Fatal("foreign lease accepted")
	}
	if err := p.Revoke(t.Context(), "database/creds/admin/foreign"); err == nil {
		t.Fatal("foreign lease accepted")
	}
	if effects.Load() != 0 {
		t.Fatal("foreign lease contacted provider")
	}
	if _, err := p.Issue(t.Context()); err == nil {
		t.Fatal("redirect accepted")
	}
	if effects.Load() != 1 {
		t.Fatal("redirect issued duplicate request")
	}
}

func TestVaultIssueCancellationDoesNotRetryAndErrorsStayOpaque(t *testing.T) {
	var calls atomic.Int32
	p, _ := fixtureVault(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); <-r.Context().Done() }))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	_, err := p.Issue(ctx)
	if err == nil || strings.Contains(err.Error(), p.cfg.Address) || strings.Contains(err.Error(), "synthetic-token") || calls.Load() != 1 {
		t.Fatal("issuance retried or error exposed provider details", err, calls.Load())
	}
}

func TestVaultBindingRejectsArbitraryPathsAndPublicHTTP(t *testing.T) {
	base := VaultConfig{Address: "https://vault.example", TokenFile: "/operator/token", Mount: "database", Role: "readonly"}
	for _, mutate := range []func(*VaultConfig){func(c *VaultConfig) { c.Address = "http://vault.example" }, func(c *VaultConfig) { c.Address = "https://user:password@vault.example" }, func(c *VaultConfig) { c.Address += "?token=secret" }, func(c *VaultConfig) { c.Mount = "database/../sys" }, func(c *VaultConfig) { c.Mount = "database%2fsys" }, func(c *VaultConfig) { c.Role = "../admin" }, func(c *VaultConfig) { c.Namespace = "team\n" }} {
		cfg := base
		mutate(&cfg)
		if _, err := NewVaultProvider(cfg); err == nil {
			t.Fatal("unsafe provider config accepted")
		}
	}
	p, _ := NewVaultProvider(base)
	a := p.Revision("consumer", time.Minute, time.Second)
	base.Role = "other"
	next, _ := NewVaultProvider(base)
	if a == next.Revision("consumer", time.Minute, time.Second) {
		t.Fatal("role change did not change binding revision")
	}
}

func TestPrivateFileRejectsWidePermissionsAndSymlinks(t *testing.T) {
	file := privateFixtureFile(t, "synthetic-token")
	if _, err := ReadPrivateFile(file, 16); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivateFile(file, 16); err == nil {
		t.Fatal("world-readable token accepted")
	}
	if err := os.Chmod(file, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivateFile(link, 16); err == nil {
		t.Fatal("symlink token accepted")
	}
	if _, err := ReadPrivateFile(file, 3); err == nil {
		t.Fatal("oversized token accepted")
	}
}

func TestPreflightTokenFailureCannotPretendToIssue(t *testing.T) {
	store, _ := fixtureStore(t)
	p, file := fixtureVault(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("preflight failure contacted Vault") }))
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	m := fixtureManager(t, store, p, nil)
	r, err := m.Execute(t.Context(), fixtureOwner(), "reporting", func(context.Context, UserPassword) error { t.Fatal("missing token reached consumer"); return nil })
	if !errors.Is(err, ErrIssuanceUnknown) || r.State != StateNotIssued {
		t.Fatal("preflight outcome wrong", r, err)
	}
}
