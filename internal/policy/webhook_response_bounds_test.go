package policy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type boundedWebhookTransport func(*http.Request) (*http.Response, error)

func (f boundedWebhookTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type webhookReadFailure struct{ sent bool }

func (r *webhookReadFailure) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, `{"allow":true}`), nil
	}
	return 0, io.ErrUnexpectedEOF
}

func TestPolicyWebhookRequiresWholeBoundedResponse(t *testing.T) {
	allow := `{"allow":true}`
	atLimit := allow + strings.Repeat(" ", (1<<20)-len(allow))
	for _, tc := range []struct {
		name    string
		body    func() io.ReadCloser
		allowed bool
	}{
		{"small", func() io.ReadCloser { return io.NopCloser(strings.NewReader(allow)) }, true},
		{"exact_limit", func() io.ReadCloser { return io.NopCloser(strings.NewReader(atLimit)) }, true},
		{"over_limit", func() io.ReadCloser { return io.NopCloser(strings.NewReader(atLimit + "{}")) }, false},
		{"read_error_after_allow", func() io.ReadCloser { return io.NopCloser(&webhookReadFailure{}) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			webhook := NewWebhook(WebhookConfig{URL: "http://policy.invalid"})
			webhook.client.Transport = boundedWebhookTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: tc.body()}, nil
			})
			decision := webhook.Decide(context.Background(), PolicyInput{}, true)
			if (decision.Effect == EffectAllow) != tc.allowed {
				t.Fatalf("response bound not enforced: %+v", decision)
			}
			if !tc.allowed && decision.ReasonCode != ReasonPolicyWebhookFailed {
				t.Fatalf("unexpected denial: %+v", decision)
			}
			if !tc.allowed {
				optOut := webhook.Decide(context.Background(), PolicyInput{}, false)
				if optOut.Effect != EffectAllow || !strings.Contains(optOut.Reason, "fail_open") {
					t.Fatalf("explicit fail-open configuration changed: %+v", optOut)
				}
			}
		})
	}
}
