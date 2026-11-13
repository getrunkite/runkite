package connector

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func expireProxyBreakerCooldown(c *Connector) {
	c.breaker.mu.Lock()
	defer c.breaker.mu.Unlock()
	c.breaker.openedAt = time.Now().Add(-time.Hour)
}

func oauthProxyBreakerRegistry(authType, mode string) *Registry {
	return NewRegistry(map[string]ConnectorConfig{"fixture": {
		Auth:           AuthConfig{Type: authType, TokenURL: "https://example.invalid/token", ClientID: "fixture", ClientSecret: "synthetic"},
		MCP:            &MCPConfig{URL: "https://example.invalid/mcp", Transport: mode},
		CircuitBreaker: &CircuitBreakerConfig{FailureThreshold: 1, CooldownSeconds: 30},
	}})
}

func TestOAuthProxyHalfOpenRecoveryUsesOneAdmissionUntilMCPCompletes(t *testing.T) {
	previous := httpClient
	t.Cleanup(func() { httpClient = previous })
	for _, authType := range []string{"oauth2_client_credentials", "oauth2_token_exchange"} {
		for _, mode := range []string{"json", "streamable_http"} {
			t.Run(authType+"/"+mode, func(t *testing.T) {
				var failToken atomic.Bool
				failToken.Store(true)
				var tokenCalls, effects atomic.Int32
				probeStarted, releaseProbe := make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				release := func() { releaseOnce.Do(func() { close(releaseProbe) }) }
				defer release()
				mcpTransport := actionObservationTransport(t, mode, func(_ *http.Request, id json.RawMessage) (*http.Response, error) {
					if effects.Add(1) == 1 {
						close(probeStarted)
						<-releaseProbe
					}
					return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":` + string(id) + `,"result":{"content":[]}}`))}, nil
				})
				httpClient = &http.Client{Transport: regressionRoundTripper(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path != "/token" {
						return mcpTransport.RoundTrip(r)
					}
					tokenCalls.Add(1)
					status, body := http.StatusOK, `{"access_token":"synthetic-access","expires_in":7200}`
					if failToken.Load() {
						status, body = http.StatusServiceUnavailable, `{"error":"temporarily_unavailable"}`
					}
					return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
				})}
				registry := oauthProxyBreakerRegistry(authType, mode)
				c, _ := registry.Get("fixture")
				user := map[string]interface{}{"sso_token": "synthetic-user"}
				call := func() error {
					_, err := registry.ProxyMCPRequest(context.Background(), "fixture", user, toolCallRequest(1, "write"))
					return err
				}
				if err := call(); err == nil || c.breaker.State() != "open" || tokenCalls.Load() != 1 || effects.Load() != 0 {
					t.Fatalf("initial OAuth failure did not open breaker: error=%v state=%s tokens=%d effects=%d", err, c.breaker.State(), tokenCalls.Load(), effects.Load())
				}
				var circuitOpen *ErrCircuitOpen
				if err := call(); !errors.As(err, &circuitOpen) || tokenCalls.Load() != 1 {
					t.Fatalf("open breaker attempted OAuth: error=%v tokens=%d", err, tokenCalls.Load())
				}
				failToken.Store(false)
				expireProxyBreakerCooldown(c)
				result := make(chan error, 1)
				go func() { result <- call() }()
				select {
				case <-probeStarted:
				case err := <-result:
					t.Fatalf("half-open OAuth probe rejected its own admission: %v", err)
				case <-time.After(3 * time.Second):
					t.Fatal("half-open probe did not reach protected MCP request")
				}
				if c.breaker.State() != "half_open" {
					t.Fatal("token success closed breaker before protected MCP probe completed")
				}
				if err := call(); !errors.As(err, &circuitOpen) || tokenCalls.Load() != 2 || effects.Load() != 1 {
					t.Fatalf("a second probe escaped: error=%v tokens=%d effects=%d", err, tokenCalls.Load(), effects.Load())
				}
				release()
				if err := <-result; err != nil || c.breaker.State() != "closed" {
					t.Fatalf("successful probe did not recover: error=%v state=%s", err, c.breaker.State())
				}
				if err := call(); err != nil || effects.Load() != 2 {
					t.Fatalf("normal request after recovery failed: error=%v effects=%d", err, effects.Load())
				}
				wantTokens := int32(2)
				if authType == "oauth2_token_exchange" {
					wantTokens = 3
				}
				if tokenCalls.Load() != wantTokens {
					t.Fatalf("OAuth cache or per-user exchange changed: requests=%d want=%d", tokenCalls.Load(), wantTokens)
				}
			})
		}
	}
}

func TestOAuthProxyEvidenceRefusalDoesNotMarkTargetFailure(t *testing.T) {
	previous := httpClient
	t.Cleanup(func() { httpClient = previous })
	for _, authType := range []string{"oauth2_client_credentials", "oauth2_token_exchange"} {
		t.Run(authType, func(t *testing.T) {
			var tokenCalls, effects atomic.Int32
			httpClient = &http.Client{Transport: regressionRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/token" {
					status, body := http.StatusOK, `{"access_token":"synthetic-access","expires_in":7200}`
					if tokenCalls.Add(1) == 1 {
						status, body = http.StatusServiceUnavailable, `{"error":"temporarily_unavailable"}`
					}
					return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
				}
				effects.Add(1)
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`))}, nil
			})}
			registry := oauthProxyBreakerRegistry(authType, "json")
			c, _ := registry.Get("fixture")
			user := map[string]interface{}{"sso_token": "synthetic-user"}
			if _, err := registry.ProxyMCPRequest(context.Background(), "fixture", user, toolCallRequest(1, "write")); err == nil {
				t.Fatal("fixture initial OAuth request unexpectedly succeeded")
			}
			expireProxyBreakerCooldown(c)
			c.breaker.mu.Lock()
			failures, openedAt := c.breaker.consecutiveFailures, c.breaker.openedAt
			c.breaker.mu.Unlock()
			observed := NewMCPActionTrace(func(context.Context) error { return errors.New("fixture evidence unavailable") })
			_, err := registry.ProxyMCPRequest(WithMCPActionTrace(context.Background(), observed), "fixture", user, toolCallRequest(1, "write"))
			if !errors.Is(err, ErrMCPDispatchBlocked) || effects.Load() != 0 || tokenCalls.Load() != 2 {
				t.Fatalf("evidence refusal tested target or failed before OAuth: error=%v tokens=%d effects=%d", err, tokenCalls.Load(), effects.Load())
			}
			c.breaker.mu.Lock()
			unchanged := c.breaker.consecutiveFailures == failures && c.breaker.openedAt.Equal(openedAt) && c.breaker.state == stateOpen
			c.breaker.mu.Unlock()
			if !unchanged {
				t.Fatal("local evidence refusal changed target failure accounting or cooldown")
			}
			if _, err := registry.ProxyMCPRequest(context.Background(), "fixture", user, toolCallRequest(1, "write")); err != nil || effects.Load() != 1 {
				t.Fatalf("authorized probe could not recover after local refusal: error=%v effects=%d", err, effects.Load())
			}
		})
	}
}

func TestOAuthProxyCountsEachFailedOAuthAttemptOnce(t *testing.T) {
	previous := httpClient
	t.Cleanup(func() { httpClient = previous })
	for _, authType := range []string{"oauth2_client_credentials", "oauth2_token_exchange"} {
		t.Run(authType, func(t *testing.T) {
			var calls atomic.Int32
			httpClient = &http.Client{Transport: regressionRoundTripper(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader(`{"error":"temporarily_unavailable"}`))}, nil
			})}
			registry := oauthProxyBreakerRegistry(authType, "json")
			c, _ := registry.Get("fixture")
			c.breaker.cfg.FailureThreshold = 2
			for attempt, wantState := range []string{"closed", "open"} {
				_, err := registry.ProxyMCPRequest(context.Background(), "fixture", map[string]interface{}{"sso_token": "synthetic"}, toolCallRequest(1, "write"))
				if err == nil || calls.Load() != int32(attempt+1) || c.breaker.State() != wantState {
					t.Fatalf("failure counted more than once: attempt=%d calls=%d state=%s error=%v", attempt+1, calls.Load(), c.breaker.State(), err)
				}
			}
			_, err := registry.ProxyMCPRequest(context.Background(), "fixture", map[string]interface{}{"sso_token": "synthetic"}, toolCallRequest(1, "write"))
			var open *ErrCircuitOpen
			if !errors.As(err, &open) || calls.Load() != 2 {
				t.Fatalf("breaker did not enforce configured threshold: calls=%d error=%v", calls.Load(), err)
			}
		})
	}
}
