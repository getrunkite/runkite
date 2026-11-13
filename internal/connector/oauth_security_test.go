package connector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

const oauthSecretSentinel = "synthetic-oauth-secret-must-not-be-logged"

func oauthSecurityRegistry(authType, tokenURL string) *Registry {
	return NewRegistry(map[string]ConnectorConfig{"example": {
		Auth: AuthConfig{Type: authType, TokenURL: tokenURL, ClientID: "client", ClientSecret: oauthSecretSentinel},
	}})
}

// Exercise the error through the registry, which wraps it before API handlers
// write it to stdout. Both supported log formats must retain only safe context.
func assertConnectorErrorSafeToLog(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected connector request failure")
	}
	for _, text := range []string{err.Error(), fmt.Sprintf("%+v", err)} {
		if strings.Contains(text, oauthSecretSentinel) {
			t.Fatal("connector error exposed secret content")
		}
	}
	for _, jsonFormat := range []bool{false, true} {
		var output bytes.Buffer
		var handler slog.Handler = slog.NewTextHandler(&output, nil)
		if jsonFormat {
			handler = slog.NewJSONHandler(&output, nil)
		}
		slog.New(handler).Error("connector session failed", "connector", "example", "error", err)
		if strings.Contains(output.String(), oauthSecretSentinel) {
			t.Fatal("connector log exposed secret content")
		}
	}
}

func TestOAuthEndpointErrorsKeepOnlySafeClassification(t *testing.T) {
	previous := httpClient
	t.Cleanup(func() { httpClient = previous })
	for _, authType := range []string{"oauth2_client_credentials", "oauth2_token_exchange"} {
		for _, tc := range []struct {
			name string
			body string
			code string
		}{
			{"standard_code", `{"error":"invalid_client","error_description":"` + oauthSecretSentinel + `","access_token":"` + oauthSecretSentinel + `","request_id":"` + oauthSecretSentinel + `"}`, "invalid_client"},
			{"token_exchange_code", `{"error":"invalid_target","error_description":"` + oauthSecretSentinel + `"}`, "invalid_target"},
			{"arbitrary_code", `{"error":"` + oauthSecretSentinel + `"}`, ""},
			{"code_with_suffix", `{"error":"invalid_client_` + oauthSecretSentinel + `"}`, ""},
			{"code_with_newline", `{"error":"invalid_client\n` + oauthSecretSentinel + `"}`, ""},
			{"text_response", oauthSecretSentinel, ""},
			{"malformed_response", `{"error":"` + oauthSecretSentinel, ""},
		} {
			t.Run(authType+"/"+tc.name, func(t *testing.T) {
				calls := 0
				httpClient = &http.Client{Transport: regressionRoundTripper(func(r *http.Request) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{"X-Request-Id": {oauthSecretSentinel}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
				})}
				registry := oauthSecurityRegistry(authType, "https://example.invalid/token")
				_, err := registry.GetSession(context.Background(), "example", map[string]interface{}{"sso_token": oauthSecretSentinel})
				assertConnectorErrorSafeToLog(t, err)
				var endpointError *OAuthEndpointError
				if !errors.As(err, &endpointError) || endpointError.StatusCode != http.StatusUnauthorized || endpointError.Code != tc.code {
					t.Fatalf("missing safe endpoint classification: %v", err)
				}
				if tc.code != "" && !strings.Contains(err.Error(), tc.code) {
					t.Fatal("standard error code unavailable to connector error taxonomy")
				}
				if calls != 1 {
					t.Fatalf("token endpoint attempts = %d, want one", calls)
				}
			})
		}
	}
}

type oauthFailingReader struct{ err error }

func (r oauthFailingReader) Read([]byte) (int, error) { return 0, r.err }
func (r oauthFailingReader) Close() error             { return nil }

func TestOAuthRequestFailuresOmitURLAndUntrustedErrors(t *testing.T) {
	previous := httpClient
	t.Cleanup(func() { httpClient = previous })
	for _, authType := range []string{"oauth2_client_credentials", "oauth2_token_exchange"} {
		for _, tc := range []struct {
			name     string
			endpoint string
			response *http.Response
			err      error
			cause    error
			calls    int
		}{
			{"malformed_url", "https://example.invalid/" + oauthSecretSentinel + "/%zz", nil, nil, nil, 0},
			{"transport", "https://user:" + oauthSecretSentinel + "@example.invalid/" + oauthSecretSentinel + "?token=" + oauthSecretSentinel, nil, errors.New(oauthSecretSentinel), nil, 1},
			{"cancelled", "https://example.invalid/token", nil, fmt.Errorf("%s: %w", oauthSecretSentinel, context.Canceled), context.Canceled, 1},
			{"deadline", "https://example.invalid/token", nil, fmt.Errorf("%s: %w", oauthSecretSentinel, context.DeadlineExceeded), context.DeadlineExceeded, 1},
			{"read_failure", "https://example.invalid/token", &http.Response{StatusCode: http.StatusOK, Body: oauthFailingReader{errors.New(oauthSecretSentinel)}}, nil, nil, 1},
			{"parse_failure", "https://example.invalid/token", &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"expires_in":"` + oauthSecretSentinel + `"}`))}, nil, nil, 1},
		} {
			t.Run(authType+"/"+tc.name, func(t *testing.T) {
				calls := 0
				httpClient = &http.Client{Transport: regressionRoundTripper(func(r *http.Request) (*http.Response, error) {
					calls++
					return tc.response, tc.err
				})}
				_, err := oauthSecurityRegistry(authType, tc.endpoint).GetSession(context.Background(), "example", map[string]interface{}{"sso_token": oauthSecretSentinel})
				assertConnectorErrorSafeToLog(t, err)
				if tc.cause != nil && !errors.Is(err, tc.cause) {
					t.Fatalf("lost safe failure cause: %v", err)
				}
				if calls != tc.calls {
					t.Fatalf("token endpoint attempts = %d, want %d", calls, tc.calls)
				}
			})
		}
	}
}
