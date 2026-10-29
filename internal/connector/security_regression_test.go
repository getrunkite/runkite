package connector

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type regressionRoundTripper func(*http.Request) (*http.Response, error)

func (f regressionRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMCPRejectsAmbiguousReservedKeys(t *testing.T) {
	bodies := []string{
		`{"jsonrpc":"2.0","method":"tools/call","METHOD":"ping","params":{"name":"delete","arguments":{}}}`,
		`{"jsonrpc":"2.0","method":"tools/call","method":"ping","params":{"name":"delete","arguments":{}}}`,
		`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"delete","NAME":"read","arguments":{}}}`,
		`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"delete","name":"read","arguments":{}}}`,
		`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"read","arguments":{},"ARGUMENTS":{"amount":5000}}}`,
		`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"read","arguments":{},"arguments":{"amount":5000}}}`,
		`{"jsonrpc":"2.0","method":"ping","params":{},"PARAMS":{"name":"delete"}}`,
	}
	original := httpClient
	defer func() { httpClient = original }()
	hits := 0
	httpClient = &http.Client{Transport: regressionRoundTripper(func(r *http.Request) (*http.Response, error) { hits++; return nil, errors.New("must not forward") })}
	reg := NewRegistry(map[string]ConnectorConfig{"test": {Auth: AuthConfig{Type: "api_key", APIKey: "synthetic"}, MCP: &MCPConfig{URL: "https://example.invalid/mcp"}, Tools: &ToolFilter{Allow: []string{"read"}}}})
	for _, body := range bodies {
		if _, err := reg.ProxyMCPRequest(context.Background(), "test", nil, []byte(body)); err == nil {
			t.Errorf("accepted ambiguous request: %s", body)
		}
	}
	if hits != 0 {
		t.Fatalf("forwarded %d ambiguous requests", hits)
	}
}

func TestMCPCanonicalRequestPreservesArgumentCase(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"name":"a","NAME":"b","arguments":1,"ARGUMENTS":2}}}`)
	req, err := ParseMCPRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(req.Body, &envelope); err != nil {
		t.Fatal(err)
	}
	var params map[string]json.RawMessage
	_ = json.Unmarshal(envelope["params"], &params)
	if string(params["arguments"]) != string(req.Arguments) {
		t.Fatal("forwarded arguments differ from inspected arguments")
	}
	var args map[string]any
	_ = json.Unmarshal(req.Arguments, &args)
	if args["name"] != "a" || args["NAME"] != "b" || len(args) != 4 {
		t.Fatalf("argument keys altered: %#v", args)
	}
}

func TestConnectorBodyLimitsDoNotTruncateOrRetry(t *testing.T) {
	original := httpClient
	defer func() { httpClient = original }()
	for _, tc := range []struct {
		name  string
		limit int64
		call  func() error
	}{
		{"mcp", MaxMCPResponseBytes, func() error {
			_, _, err := forwardMCPRequest(context.Background(), "https://example.invalid/mcp", "", []byte(`{}`))
			return err
		}},
		{"oauth", MaxOAuthResponseBytes, func() error {
			_, err := oauth2ClientCredentials(context.Background(), AuthConfig{TokenURL: "https://example.invalid/token"})
			return err
		}},
		{"exchange", MaxOAuthResponseBytes, func() error {
			_, err := oauth2TokenExchange(context.Background(), AuthConfig{TokenURL: "https://example.invalid/token"}, "synthetic")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits := 0
			httpClient = &http.Client{Transport: regressionRoundTripper(func(r *http.Request) (*http.Response, error) {
				hits++
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(strings.Repeat("x", int(tc.limit)+1)))}, nil
			})}
			if err := tc.call(); !errors.Is(err, ErrBodyTooLarge) {
				t.Fatalf("oversize error = %v", err)
			}
			if hits != 1 {
				t.Fatalf("attempts = %d, want exactly one", hits)
			}
		})
	}
	if _, err := ParseMCPRequest([]byte(strings.Repeat("x", int(MaxMCPRequestBytes)+1))); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("request oversize error = %v", err)
	}
	for _, n := range []int64{0, 16, MaxOAuthResponseBytes} {
		body, err := readLimitedBody(strings.NewReader(strings.Repeat("x", int(n))), n)
		if err != nil || int64(len(body)) != n {
			t.Fatalf("exact bound %d: len=%d err=%v", n, len(body), err)
		}
	}
}
