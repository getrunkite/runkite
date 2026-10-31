package hooks

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type boundedGateTransport func(*http.Request) (*http.Response, error)

func (f boundedGateTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type gateReadFailure struct{ sent bool }

func (r *gateReadFailure) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, `{"allow":true}`), nil
	}
	return 0, io.ErrUnexpectedEOF
}

func TestPreflightRequiresWholeBoundedResponse(t *testing.T) {
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
		{"read_error_after_allow", func() io.ReadCloser { return io.NopCloser(&gateReadFailure{}) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := NewWebhookGate(WebhookGateConfig{URL: "http://preflight.invalid"})
			gate.client.Transport = boundedGateTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: tc.body()}, nil
			})
			if decision := gate.Decide(context.Background(), Event{}); decision.Allow != tc.allowed {
				t.Fatalf("response bound not enforced: %+v", decision)
			}
		})
	}
}
