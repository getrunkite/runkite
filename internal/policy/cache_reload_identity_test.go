package policy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type exactIdentityRoundTrip func(*http.Request) (*http.Response, error)

func (f exactIdentityRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPolicyReloadDoesNotAdoptInflightOldCacheDecision(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	engine := New(Config{ForceEnable: true, DefaultEffect: EffectAllow, CacheTTL: time.Hour, Webhook: &WebhookConfig{URL: "https://policy.invalid/decide"}})
	engine.webhook.client.Transport = exactIdentityRoundTrip(func(*http.Request) (*http.Response, error) {
		close(entered)
		<-release
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"effect":"allow"}`)), Header: make(http.Header)}, nil
	})
	input := PolicyInput{Stage: StageToolCall, TenantID: "tenant", AgentID: "agent", Connector: "bank", Tool: "write"}
	first := make(chan PolicyDecision, 1)
	go func() { first <- engine.Decide(context.Background(), input) }()
	<-entered
	engine.ReplaceOverlays([]Grant{{TenantID: "tenant", AgentID: "agent", Connector: "bank", Tools: &ToolFilter{Deny: []string{"write"}}}})
	close(release)
	if got := <-first; got.Effect != EffectDeny {
		t.Fatalf("old evaluation survived reload: %+v", got)
	}
	if got := engine.Decide(context.Background(), input); got.Effect != EffectDeny {
		t.Fatalf("old evaluation repopulated current cache with allow: %+v", got)
	}
}
