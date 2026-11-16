package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type endpointConfig struct {
	BaseURL         string
	AllowLocalProof bool
}

const endpointBodyLimit int64 = 4 << 20

var (
	errEndpointConfig   = errors.New("invalid Runkite endpoint configuration")
	errEndpointResolve  = errors.New("endpoint resolution failed")
	errEndpointAddress  = errors.New("endpoint address is not allowed")
	errEndpointOrigin   = errors.New("request must use the configured Runkite origin")
	errEndpointConnect  = errors.New("endpoint connection failed")
	errEndpointRead     = errors.New("response could not be read")
	errEndpointBodySize = errors.New("response exceeds the size limit")
)

type endpointLookup func(context.Context, string, string) ([]netip.Addr, error)
type endpointDial func(context.Context, string, string) (net.Conn, error)

func newEndpointClient(ctx context.Context, cfg endpointConfig) (*url.URL, *http.Client, func(), error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return newEndpointClientWithNetwork(ctx, cfg, net.DefaultResolver.LookupNetIP, dialer.DialContext)
}

// The dependencies are explicit so tests can prove that DNS is resolved once
// and only the validated IPs, never a later DNS answer, are dialed.
func newEndpointClientWithNetwork(ctx context.Context, cfg endpointConfig, lookup endpointLookup, dial endpointDial) (*url.URL, *http.Client, func(), error) {
	base, err := parseEndpoint(cfg.BaseURL)
	if err != nil {
		return nil, nil, nil, err
	}
	host := base.Hostname()
	address, addressErr := netip.ParseAddr(host)
	localProof := cfg.AllowLocalProof && base.Scheme == "http" && addressErr == nil && address.IsLoopback() && !address.Is4In6() && address.Zone() == ""
	if base.Scheme != "https" && !localProof {
		return nil, nil, nil, errEndpointAddress
	}

	var addresses []netip.Addr
	if addressErr == nil {
		addresses = []netip.Addr{address}
	} else {
		resolveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		addresses, err = lookup(resolveCtx, "ip", host)
		cancel()
		if err != nil {
			return nil, nil, nil, errEndpointResolve
		}
	}
	if len(addresses) == 0 {
		return nil, nil, nil, errEndpointResolve
	}
	pinned := make([]netip.Addr, 0, len(addresses))
	for _, ip := range addresses {
		if !localProof && !publicEndpointAddress(ip) {
			return nil, nil, nil, errEndpointAddress
		}
		pinned = append(pinned, ip)
	}
	port := endpointPort(base)
	transport := &http.Transport{
		// Do not inherit an ambient proxy, which could resolve or redirect the
		// destination outside the address set validated above.
		Proxy: nil,
		DialContext: func(ctx context.Context, network, target string) (net.Conn, error) {
			targetHost, targetPort, err := net.SplitHostPort(target)
			if err != nil || !strings.EqualFold(targetHost, host) || targetPort != port {
				return nil, errEndpointOrigin
			}
			dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			for _, ip := range pinned {
				conn, err := dial(dialCtx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				if dialCtx.Err() != nil {
					break
				}
			}
			return nil, errEndpointConnect
		},
		// net/http still derives TLS ServerName from the URL's hostname, not
		// the pinned dial IP. Normal certificate verification remains enabled.
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConns:          4,
		MaxIdleConnsPerHost:   4,
		MaxConnsPerHost:       4,
	}
	client := &http.Client{
		Transport: &endpointTransport{scheme: base.Scheme, host: host, port: port, next: transport},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return base, client, transport.CloseIdleConnections, nil
}

func parseEndpoint(raw string) (*url.URL, error) {
	if raw == "" || raw != strings.TrimSpace(raw) || strings.Contains(raw, "#") {
		return nil, errEndpointConfig
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return nil, errEndpointConfig
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Hostname() == "" || strings.HasSuffix(u.Host, ":") {
		return nil, errEndpointConfig
	}
	for _, c := range u.Hostname() {
		if c <= 0x20 || c >= 0x7f {
			return nil, errEndpointConfig
		}
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil {
		if ip.Zone() != "" || (ip.Is6() && !strings.HasPrefix(u.Host, "[")) {
			return nil, errEndpointConfig
		}
	} else {
		host := strings.TrimSuffix(u.Hostname(), ".")
		if len(host) > 253 {
			return nil, errEndpointConfig
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return nil, errEndpointConfig
			}
			for _, c := range label {
				allowed := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-'
				if !allowed {
					return nil, errEndpointConfig
				}
			}
		}
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, errEndpointConfig
		}
	}
	u.Host = strings.ToLower(u.Host)
	u.Path = ""
	return u, nil
}

func endpointPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

var blockedEndpointPrefixes = [...]netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.31.196.0/24"),
	netip.MustParsePrefix("192.52.193.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.175.48.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("2620:4f:8000::/48"),
	netip.MustParsePrefix("3fff::/20"),
}

func publicEndpointAddress(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" || ip.Is4In6() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, prefix := range blockedEndpointPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

type endpointTransport struct {
	scheme string
	host   string
	port   string
	next   http.RoundTripper
}

func (t *endpointTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil || req.URL.User != nil || req.URL.Opaque != "" || req.URL.Scheme != t.scheme || !strings.EqualFold(req.URL.Hostname(), t.host) || endpointPort(req.URL) != t.port || (req.Host != "" && !strings.EqualFold(req.Host, req.URL.Host)) {
		return nil, errEndpointOrigin
	}
	response, err := t.next.RoundTrip(req)
	if err != nil {
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		// A malformed upstream status line can appear in a transport error.
		// Do not reflect any upstream body or provider detail to the caller.
		return nil, errEndpointConnect
	}
	if response.ContentLength > endpointBodyLimit {
		if response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, errEndpointBodySize
	}
	if response.Body != nil {
		response.Body = &endpointBody{source: response.Body, remaining: endpointBodyLimit}
	}
	return response, nil
}

type endpointBody struct {
	source    io.ReadCloser
	remaining int64
	overflow  bool
}

func (b *endpointBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.overflow {
		return 0, errEndpointBodySize
	}
	if b.remaining == 0 {
		var probe [1]byte
		n, err := b.source.Read(probe[:])
		if n > 0 {
			b.overflow = true
			_ = b.source.Close()
			return 0, errEndpointBodySize
		}
		return 0, endpointReadError(err)
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.source.Read(p)
	b.remaining -= int64(n)
	return n, endpointReadError(err)
}

func (b *endpointBody) Close() error { return b.source.Close() }

func endpointReadError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, io.EOF):
		return io.EOF
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	default:
		return errEndpointRead
	}
}
