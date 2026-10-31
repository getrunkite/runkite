package connector

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type vaultReadFailure struct{ sent bool }

func (r *vaultReadFailure) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, `{"data":{"data":{"token":"synthetic-secret"}}}`), nil
	}
	return 0, io.ErrUnexpectedEOF
}

func TestVaultRequiresWholeBoundedResponse(t *testing.T) {
	t.Setenv("VAULT_ADDR", "http://vault.invalid")
	t.Setenv("VAULT_TOKEN", "synthetic-token")
	t.Setenv("VAULT_ALLOWED_PREFIX", "secret/data/runkite/")
	original := vaultHTTPClient.Transport
	t.Cleanup(func() { vaultHTTPClient.Transport = original })
	body := `{"data":{"data":{"token":"synthetic-secret"}}}`
	atLimit := body + strings.Repeat(" ", (1<<20)-len(body))
	for _, tc := range []struct {
		name string
		body func() io.ReadCloser
		err  error
	}{
		{"small", func() io.ReadCloser { return io.NopCloser(strings.NewReader(body)) }, nil},
		{"exact_limit", func() io.ReadCloser { return io.NopCloser(strings.NewReader(atLimit)) }, nil},
		{"over_limit", func() io.ReadCloser { return io.NopCloser(strings.NewReader(atLimit + "{}")) }, ErrBodyTooLarge},
		{"read_error_after_secret", func() io.ReadCloser { return io.NopCloser(&vaultReadFailure{}) }, io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vaultHTTPClient.Transport = regressionRoundTripper(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: tc.body()}, nil
			})
			value, err := ResolveSecretRef(context.Background(), "vault:secret/data/runkite/test#token")
			if !errors.Is(err, tc.err) {
				t.Fatalf("unexpected response outcome: %v", err)
			}
			if tc.err == nil && value != "synthetic-secret" {
				t.Fatal("valid complete secret response rejected")
			}
			if tc.err != nil && value != "" {
				t.Fatal("incomplete or oversized response yielded a secret")
			}
		})
	}
}

func TestVaultDoesNotSendTokenToRedirectTarget(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Add(1)
		_, _ = w.Write([]byte(`{"data":{"data":{"token":"untrusted"}}}`))
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	t.Setenv("VAULT_ADDR", origin.URL)
	t.Setenv("VAULT_TOKEN", "synthetic-vault-redirect-token")
	t.Setenv("VAULT_ALLOWED_PREFIX", "secret/data/runkite/")
	value, err := ResolveSecretRef(context.Background(), "vault:secret/data/runkite/test#token")
	if err == nil || value != "" || leaked.Load() != 0 {
		t.Fatalf("Vault redirect did not fail closed: err=%v redirected=%d", err, leaked.Load())
	}
}

func TestVaultEncodedPathsCannotEscapeAllowedPrefix(t *testing.T) {
	t.Setenv("VAULT_ADDR", "http://vault.invalid/proxy/")
	t.Setenv("VAULT_TOKEN", "synthetic-token")
	t.Setenv("VAULT_ALLOWED_PREFIX", "secret/data/runkite/")
	original := vaultHTTPClient.Transport
	t.Cleanup(func() { vaultHTTPClient.Transport = original })
	var paths []string
	vaultHTTPClient.Transport = regressionRoundTripper(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		if r.URL.RawQuery != "" || r.URL.Fragment != "" || r.URL.RawPath != "" {
			t.Fatal("secret path became URL syntax")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":{"data":{"token":"synthetic-secret"}}}`))}, nil
	})
	for _, path := range []string{
		"secret/data/runkite/%2e%2e/other",
		"secret/data/runkite/%2f..%2fother",
		"secret/data/runkite/%252e%252e/other",
		"secret/data/runkite/%252fother",
		"secret/data/runkite/test%3fother",
		"secret/data/runkite/test%23other",
		"secret/data/runkite/test%5cother",
		"secret/data/runkite/test?other",
		"secret/data/runkite/bad%escape",
	} {
		if value, err := ResolveSecretRef(context.Background(), "vault:"+path+"#token"); err == nil || value != "" {
			t.Fatalf("ambiguous path accepted: %q", path)
		}
	}
	if len(paths) != 0 {
		t.Fatal("rejected path reached Vault")
	}
	for _, path := range []string{
		"secret/data/runkite/valid",
		"secret/data/runkite/space%20name",
		"secret/data/runkite/folder%2Fchild",
	} {
		if _, err := ResolveSecretRef(context.Background(), "vault:"+path+"#token"); err != nil {
			t.Fatalf("valid canonical path rejected: %v", err)
		}
	}
	want := []string{
		"/proxy/v1/secret/data/runkite/valid",
		"/proxy/v1/secret/data/runkite/space name",
		"/proxy/v1/secret/data/runkite/folder/child",
	}
	if len(paths) != len(want) {
		t.Fatalf("fetch count=%d want %d", len(paths), len(want))
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("path=%q want %q", paths[i], want[i])
		}
	}
}
