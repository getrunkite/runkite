package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"testing"
)

func TestConnectorFixedTransportClassificationsRemainSafeForErrorMappings(t *testing.T) {
	previousHTTP, previousVault := httpClient, vaultHTTPClient
	t.Cleanup(func() { httpClient, vaultHTTPClient = previousHTTP, previousVault })
	t.Setenv("VAULT_TOKEN", oauthSecretSentinel)
	t.Setenv("VAULT_ALLOWED_PREFIX", "secret/data/runkite/")
	endpoint := "https://user:" + oauthSecretSentinel + "@example.invalid/" + oauthSecretSentinel + "?token=" + oauthSecretSentinel
	t.Setenv("VAULT_ADDR", endpoint)
	for _, source := range []string{"oauth", "mcp", "vault"} {
		for _, cause := range []error{syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.ETIMEDOUT, syscall.EPIPE} {
			t.Run(source+"/"+cause.Error(), func(t *testing.T) {
				calls := 0
				transport := regressionRoundTripper(func(*http.Request) (*http.Response, error) {
					calls++
					return nil, &url.Error{Op: "Post", URL: endpoint, Err: fmt.Errorf("%s: %w", oauthSecretSentinel, cause)}
				})
				httpClient, vaultHTTPClient = &http.Client{Transport: transport}, &http.Client{Transport: transport}
				var err error
				switch source {
				case "oauth":
					_, err = oauthSecurityRegistry("oauth2_client_credentials", endpoint).GetSession(context.Background(), "example", nil)
				case "vault":
					_, err = ResolveSecretRef(context.Background(), "vault:secret/data/runkite/test#token")
				case "mcp":
					registry := NewRegistry(map[string]ConnectorConfig{"example": {Auth: AuthConfig{Type: "api_key", APIKey: oauthSecretSentinel}, MCP: &MCPConfig{URL: endpoint}}})
					_, err = registry.ProxyMCPRequest(context.Background(), "example", nil, toolCallRequest(1, "read"))
				}
				assertConnectorErrorSafeToLog(t, err)
				if !errors.Is(err, cause) || !strings.Contains(err.Error(), cause.Error()) {
					t.Fatalf("lost fixed transport classification for error mapping: %v", err)
				}
				if calls != 1 {
					t.Fatalf("endpoint attempts = %d, want one", calls)
				}
			})
		}
	}
}

func TestConnectorTransportErrorsOmitCredentialsAndKeepSafeCauses(t *testing.T) {
	previousHTTP, previousVault := httpClient, vaultHTTPClient
	t.Cleanup(func() { httpClient, vaultHTTPClient = previousHTTP, previousVault })
	t.Setenv("VAULT_TOKEN", oauthSecretSentinel)
	t.Setenv("VAULT_ALLOWED_PREFIX", "secret/data/runkite/")
	for _, source := range []string{"mcp", "vault"} {
		for _, tc := range []struct {
			name     string
			endpoint string
			body     func() io.ReadCloser
			err      error
			cause    error
			calls    int
		}{
			{"malformed_url", "https://example.invalid/" + oauthSecretSentinel + "/%zz", nil, nil, nil, 0},
			{"transport", "https://user:" + oauthSecretSentinel + "@example.invalid/" + oauthSecretSentinel + "?token=" + oauthSecretSentinel, nil, errors.New(oauthSecretSentinel), nil, 1},
			{"cancelled", "https://example.invalid/", nil, fmt.Errorf("%s: %w", oauthSecretSentinel, context.Canceled), context.Canceled, 1},
			{"deadline", "https://example.invalid/", nil, fmt.Errorf("%s: %w", oauthSecretSentinel, context.DeadlineExceeded), context.DeadlineExceeded, 1},
			{"read_failure", "https://example.invalid/", func() io.ReadCloser { return oauthFailingReader{errors.New(oauthSecretSentinel)} }, nil, nil, 1},
			{"incomplete_body", "https://example.invalid/", func() io.ReadCloser {
				return oauthFailingReader{fmt.Errorf("%s: %w", oauthSecretSentinel, io.ErrUnexpectedEOF)}
			}, nil, io.ErrUnexpectedEOF, 1},
		} {
			t.Run(source+"/"+tc.name, func(t *testing.T) {
				calls := 0
				transport := regressionRoundTripper(func(r *http.Request) (*http.Response, error) {
					calls++
					if tc.body != nil {
						return &http.Response{StatusCode: http.StatusOK, Body: tc.body()}, nil
					}
					return nil, tc.err
				})
				httpClient, vaultHTTPClient = &http.Client{Transport: transport}, &http.Client{Transport: transport}
				var err error
				if source == "vault" {
					t.Setenv("VAULT_ADDR", tc.endpoint)
					_, err = ResolveSecretRef(context.Background(), "vault:secret/data/runkite/test#token")
				} else {
					registry := NewRegistry(map[string]ConnectorConfig{"example": {Auth: AuthConfig{Type: "api_key", APIKey: oauthSecretSentinel}, MCP: &MCPConfig{URL: tc.endpoint}}})
					_, err = registry.ProxyMCPRequest(context.Background(), "example", nil, toolCallRequest(1, "read"))
				}
				assertConnectorErrorSafeToLog(t, err)
				if tc.cause != nil && !errors.Is(err, tc.cause) {
					t.Fatalf("lost safe failure cause: %v", err)
				}
				if calls != tc.calls {
					t.Fatalf("endpoint attempts = %d, want %d", calls, tc.calls)
				}
			})
		}
	}
}

func TestStreamableConnectorErrorsDoNotExposeCredentialsOrReplayTools(t *testing.T) {
	previous := httpClient
	t.Cleanup(func() { httpClient = previous })
	for _, phase := range []string{"initialize", "tool_transport", "tool_body"} {
		t.Run(phase, func(t *testing.T) {
			toolCalls := 0
			httpClient = &http.Client{Transport: regressionRoundTripper(func(r *http.Request) (*http.Response, error) {
				var request struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
				}
				if r.Body != nil {
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Fatalf("decode SDK request: %v", err)
					}
				}
				if request.Method == "tools/call" {
					toolCalls++
				}
				if phase == "initialize" || request.Method == "tools/call" && phase == "tool_transport" {
					return nil, errors.New(oauthSecretSentinel)
				}
				if request.Method == "tools/call" && phase == "tool_body" {
					return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Header: http.Header{"Content-Type": {"application/json"}}, Body: oauthFailingReader{errors.New(oauthSecretSentinel)}}, nil
				}
				var result any = map[string]any{}
				switch request.Method {
				case "server/discover":
					result = map[string]any{"supportedVersions": []string{"2026-07-28"}, "capabilities": map[string]any{"tools": map[string]any{}}}
				case "tools/list":
					result = map[string]any{"tools": []any{map[string]any{"name": "read", "inputSchema": map[string]any{"type": "object"}}}}
				}
				body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			})}
			endpoint := "https://user:" + oauthSecretSentinel + "@example.invalid/" + oauthSecretSentinel + "?token=" + oauthSecretSentinel
			registry := NewRegistry(map[string]ConnectorConfig{"example": {Auth: AuthConfig{Type: "api_key", APIKey: oauthSecretSentinel}, MCP: &MCPConfig{URL: endpoint, Transport: "streamable_http"}}})
			response, err := registry.ProxyMCPRequest(context.Background(), "example", nil, toolCallRequest(1, "read"))
			if err != nil {
				assertConnectorErrorSafeToLog(t, err)
			} else if response == nil || !strings.Contains(string(response.Body), `"error"`) {
				t.Fatal("failed transport reported success")
			}
			if response != nil && strings.Contains(string(response.Body), oauthSecretSentinel) {
				t.Fatal("SDK transport error exposed credentials in JSON-RPC response")
			}
			wantCalls := 1
			if phase == "initialize" {
				wantCalls = 0
			}
			if toolCalls != wantCalls {
				t.Fatalf("tool attempts = %d, want %d", toolCalls, wantCalls)
			}
		})
	}
}

func TestConnectorErrorSanitizationPreservesBodyLimitDuringCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := safeConnectorRequestError(ctx, "read response failed", fmt.Errorf("%s: %w", oauthSecretSentinel, ErrBodyTooLarge))
	assertConnectorErrorSafeToLog(t, err)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrBodyTooLarge) {
		t.Fatal("sanitization lost a concurrent safe failure cause")
	}
}
