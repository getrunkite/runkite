package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

func endpointTestLookup(_ context.Context, _, _ string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
}

func endpointTestNoDial(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("unexpected dial")
}

func TestEndpointConfiguration(t *testing.T) {
	cases := []struct {
		name  string
		url   string
		local bool
		allow bool
	}{
		{"public_https", "https://specialist.example", false, true},
		{"origin_slash", "https://specialist.example/", false, true},
		{"public_https_port", "https://specialist.example:2026", false, true},
		{"public_literal", "https://1.1.1.1", false, true},
		{"public_ipv6", "https://[2606:4700:4700::1111]", false, true},
		{"plain_http", "http://specialist.example", false, false},
		{"flag_does_not_allow_public_http", "http://specialist.example", true, false},
		{"loopback_default", "http://127.0.0.1:2026", false, false},
		{"loopback_explicit", "http://127.0.0.1:2026", true, true},
		{"loopback_v6_explicit", "http://[::1]:2026", true, true},
		{"loopback_dns_not_exception", "http://localhost:2026", true, false},
		{"private_not_exception", "http://10.0.0.1:2026", true, false},
		{"private_https_not_exception", "https://127.0.0.1:2026", true, false},
		{"mapped_loopback_not_exception", "http://[::ffff:127.0.0.1]:2026", true, false},
		{"userinfo", "https://user:credential@specialist.example", false, false},
		{"query", "https://specialist.example?token=value", false, false},
		{"empty_query", "https://specialist.example?", false, false},
		{"fragment", "https://specialist.example/#part", false, false},
		{"empty_fragment", "https://specialist.example#", false, false},
		{"mcp_path", "https://specialist.example/mcp", false, false},
		{"encoded_path", "https://specialist.example/%2f", false, false},
		{"scheme_relative", "//specialist.example", false, false},
		{"opaque", "https:specialist.example", false, false},
		{"other_scheme", "file://specialist.example", false, false},
		{"blank_host", "https:///", false, false},
		{"blank_port", "https://specialist.example:", false, false},
		{"zero_port", "https://specialist.example:0", false, false},
		{"large_port", "https://specialist.example:65536", false, false},
		{"negative_port", "https://specialist.example:-1", false, false},
		{"named_port", "https://specialist.example:https", false, false},
		{"surrounding_space", " https://specialist.example", false, false},
		{"zone", "https://[fe80::1%25eth0]", false, false},
		{"bare_ipv6", "https://2606:4700:4700::1111", false, false},
		{"invalid_dns_label", "https://specialist_example", false, false},
		{"empty_dns_label", "https://specialist..example", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, client, closeIdle, err := newEndpointClientWithNetwork(context.Background(), endpointConfig{BaseURL: tc.url, AllowLocalProof: tc.local}, endpointTestLookup, endpointTestNoDial)
			if (err == nil) != tc.allow {
				t.Fatalf("allowed=%v, error=%v", tc.allow, err)
			}
			if err != nil {
				if base != nil || client != nil || closeIdle != nil {
					t.Fatal("invalid endpoint returned a usable client")
				}
				return
			}
			defer closeIdle()
			if base.Path != "" {
				t.Fatalf("origin was not normalized: %q", base.Path)
			}
		})
	}
}

func TestEndpointPublicAddresses(t *testing.T) {
	for _, raw := range []string{
		"0.0.0.0", "0.1.2.3", "10.1.2.3", "100.64.0.1", "100.127.255.254",
		"127.0.0.1", "169.254.169.254", "172.16.1.1", "172.31.255.254",
		"192.168.1.1", "192.0.0.9", "192.0.2.1", "192.31.196.1",
		"192.52.193.1", "192.88.99.1", "192.175.48.1", "198.18.1.1",
		"198.19.255.254", "198.51.100.1", "203.0.113.1", "224.0.0.1",
		"239.255.255.254", "240.0.0.1", "255.255.255.255", "::", "::1",
		"::ffff:127.0.0.1", "::ffff:1.1.1.1", "fc00::1", "fe80::1",
		"fe80::1%eth0", "ff02::1", "64:ff9b::a00:1", "100::1",
		"2001::1", "2001:20::1", "2001:db8::1", "2002:7f00:1::1",
		"2620:4f:8000::1", "3fff::1", "5f00::1",
	} {
		t.Run(raw, func(t *testing.T) {
			if publicEndpointAddress(netip.MustParseAddr(raw)) {
				t.Fatal("unsafe address accepted")
			}
		})
	}
	for _, raw := range []string{"1.1.1.1", "8.8.8.8", "93.184.216.34", "2606:4700:4700::1111", "2001:4860:4860::8888"} {
		if !publicEndpointAddress(netip.MustParseAddr(raw)) {
			t.Fatalf("public address %s rejected", raw)
		}
	}
	if publicEndpointAddress(netip.Addr{}) {
		t.Fatal("invalid address accepted")
	}
}

func TestEndpointAllDNSAnswersMustBePublic(t *testing.T) {
	for _, unsafe := range []string{"10.0.0.1", "169.254.169.254", "100.64.0.1", "::ffff:1.1.1.1", "fc00::1", "2001:db8::1"} {
		t.Run(unsafe, func(t *testing.T) {
			lookup := func(context.Context, string, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr(unsafe)}, nil
			}
			_, _, _, err := newEndpointClientWithNetwork(context.Background(), endpointConfig{BaseURL: "https://specialist.example"}, lookup, endpointTestNoDial)
			if !errors.Is(err, errEndpointAddress) {
				t.Fatalf("mixed DNS answer did not fail closed: %v", err)
			}
		})
	}
}

func TestEndpointDNSFailureDoesNotExposeDetails(t *testing.T) {
	for _, lookup := range []endpointLookup{
		func(context.Context, string, string) ([]netip.Addr, error) { return nil, nil },
		func(context.Context, string, string) ([]netip.Addr, error) {
			return nil, errors.New("private upstream credential detail")
		},
	} {
		_, _, _, err := newEndpointClientWithNetwork(context.Background(), endpointConfig{BaseURL: "https://specialist.example"}, lookup, endpointTestNoDial)
		if !errors.Is(err, errEndpointResolve) || strings.Contains(err.Error(), "credential") {
			t.Fatalf("unexpected resolution error: %v", err)
		}
	}
}

func TestEndpointPinsDNSAndKeepsTLSVerification(t *testing.T) {
	lookups := 0
	lookup := func(context.Context, string, string) ([]netip.Addr, error) {
		lookups++
		if lookups > 1 {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("8.8.8.8")}, nil
	}
	var dialed []string
	dial := func(ctx context.Context, network, target string) (net.Conn, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("dial has no deadline")
		}
		dialed = append(dialed, target)
		if len(dialed)%2 == 1 {
			return nil, errors.New("first address unavailable")
		}
		client, server := net.Pipe()
		_ = server.Close()
		return client, nil
	}
	_, client, closeIdle, err := newEndpointClientWithNetwork(context.Background(), endpointConfig{BaseURL: "https://specialist.example"}, lookup, dial)
	if err != nil {
		t.Fatal(err)
	}
	defer closeIdle()
	transport := client.Transport.(*endpointTransport).next.(*http.Transport)
	for range 2 {
		conn, err := transport.DialContext(context.Background(), "tcp", "specialist.example:443")
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
	}
	if lookups != 1 || strings.Join(dialed, ",") != "1.1.1.1:443,8.8.8.8:443,1.1.1.1:443,8.8.8.8:443" {
		t.Fatalf("destination was not pinned: lookups=%d targets=%v", lookups, dialed)
	}
	if transport.Proxy != nil || transport.TLSClientConfig.InsecureSkipVerify || transport.TLSClientConfig.ServerName != "" {
		t.Fatal("proxy or TLS verification was weakened")
	}
	if transport.TLSHandshakeTimeout <= 0 || transport.ResponseHeaderTimeout <= 0 || transport.IdleConnTimeout <= 0 {
		t.Fatal("finite connection timeouts missing")
	}
	if _, err := transport.DialContext(context.Background(), "tcp", "different.example:443"); !errors.Is(err, errEndpointOrigin) {
		t.Fatalf("dial allowed a replacement origin: %v", err)
	}
}

type endpointTestRoundTripper func(*http.Request) (*http.Response, error)

func (f endpointTestRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEndpointSameOriginAndNoRedirect(t *testing.T) {
	_, client, closeIdle, err := newEndpointClientWithNetwork(context.Background(), endpointConfig{BaseURL: "https://specialist.example"}, endpointTestLookup, endpointTestNoDial)
	if err != nil {
		t.Fatal(err)
	}
	defer closeIdle()
	calls := 0
	guard := client.Transport.(*endpointTransport)
	guard.next = endpointTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"http://169.254.169.254/latest/meta-data/"}}, Body: io.NopCloser(strings.NewReader("redirect"))}, nil
	})
	for _, target := range []string{"https://specialist.example/mcp", "https://specialist.example/runs/op-1", "https://specialist.example:443/runs/op-1"} {
		response, err := client.Get(target)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusFound {
			t.Fatal("redirect was followed")
		}
	}
	if calls != 3 {
		t.Fatalf("redirect generated another request: %d", calls)
	}
	for _, target := range []string{"http://specialist.example/mcp", "https://different.example/mcp", "https://specialist.example:8443/mcp", "https://user@specialist.example/mcp"} {
		request, err := http.NewRequest(http.MethodGet, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := guard.RoundTrip(request); !errors.Is(err, errEndpointOrigin) {
			t.Fatalf("different origin allowed: %v", err)
		}
	}
	request, _ := http.NewRequest(http.MethodGet, "https://specialist.example/mcp", nil)
	request.Host = "different.example"
	if _, err := guard.RoundTrip(request); !errors.Is(err, errEndpointOrigin) {
		t.Fatal("replacement Host header allowed")
	}
	if calls != 3 {
		t.Fatal("rejected origin reached transport")
	}
}

type endpointTestBody struct {
	io.Reader
	closed bool
}

func (b *endpointTestBody) Close() error { b.closed = true; return nil }

func TestEndpointResponseBodyBound(t *testing.T) {
	for _, tc := range []struct {
		name   string
		size   int64
		length int64
		allow  bool
	}{
		{"small", 32, 32, true},
		{"exact_limit", endpointBodyLimit, endpointBodyLimit, true},
		{"chunked_exact_limit", endpointBodyLimit, -1, true},
		{"oversize_header", endpointBodyLimit + 1, endpointBodyLimit + 1, false},
		{"chunked_oversize", endpointBodyLimit + 1, -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &endpointTestBody{Reader: bytes.NewReader(bytes.Repeat([]byte{'x'}, int(tc.size)))}
			guard := &endpointTransport{scheme: "https", host: "specialist.example", port: "443", next: endpointTestRoundTripper(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: body, ContentLength: tc.length}, nil
			})}
			request, _ := http.NewRequest(http.MethodGet, "https://specialist.example/mcp", nil)
			response, err := guard.RoundTrip(request)
			var got []byte
			if err == nil {
				got, err = io.ReadAll(response.Body)
				_ = response.Body.Close()
			}
			if tc.allow {
				if err != nil || int64(len(got)) != tc.size {
					t.Fatalf("bounded valid body failed: bytes=%d error=%v", len(got), err)
				}
			} else if !errors.Is(err, errEndpointBodySize) || int64(len(got)) > endpointBodyLimit {
				t.Fatalf("oversize response did not fail: bytes=%d error=%v", len(got), err)
			}
			if !body.closed {
				t.Fatal("response body was not closed")
			}
		})
	}
}

func TestEndpointTransportErrorsAreSanitized(t *testing.T) {
	guard := &endpointTransport{scheme: "https", host: "specialist.example", port: "443", next: endpointTestRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("malformed HTTP response containing private detail")
	})}
	request, _ := http.NewRequest(http.MethodGet, "https://specialist.example/mcp", nil)
	if _, err := guard.RoundTrip(request); !errors.Is(err, errEndpointConnect) || strings.Contains(err.Error(), "private") {
		t.Fatalf("raw transport error exposed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := guard.RoundTrip(request.WithContext(ctx)); !errors.Is(err, context.Canceled) {
		t.Fatalf("request cancellation lost: %v", err)
	}
}

func TestEndpointBodyErrorsAreSanitized(t *testing.T) {
	if err := endpointReadError(errors.New("upstream read includes private detail")); !errors.Is(err, errEndpointRead) || strings.Contains(err.Error(), "private") {
		t.Fatalf("raw read error exposed: %v", err)
	}
	for _, expected := range []error{io.EOF, context.Canceled, context.DeadlineExceeded} {
		if got := endpointReadError(expected); got != expected {
			t.Fatalf("expected %v, got %v", expected, got)
		}
	}
}
