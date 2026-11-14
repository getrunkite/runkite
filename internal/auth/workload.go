package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/getrunkite/runkite/internal/jsonvalue"
	"github.com/getrunkite/runkite/internal/transport"
)

var ErrWorkloadIdentity = errors.New("verified workload identity required")

type workloadPolicy struct {
	allowed map[string]map[string]bool
	denied  map[string]bool
	maxTTL  time.Duration
}

type workloadAuthorityKey struct{}
type workloadAuthority struct {
	policy *workloadPolicy
	kind   string
}

type workloadIdentityFile struct {
	Identities map[string][]string `json:"identities"`
	DeniedKeys []string            `json:"denied_key_sha256,omitempty"`
}

// ConfigureWorkloadIdentities enables the strict profile. External issuance
// owns certificates and keys; this allowlist only authorizes URI SANs for
// existing runner kinds or execution pools.
func (rt *RunnerTokens) ConfigureWorkloadIdentities(allowed map[string][]string, maxTTL time.Duration) error {
	return rt.ConfigureWorkloadIdentityPolicy(allowed, nil, maxTTL)
}

func (rt *RunnerTokens) ConfigureWorkloadIdentityPolicy(allowed map[string][]string, deniedKeys []string, maxTTL time.Duration) error {
	if !rt.Enabled() || maxTTL < time.Minute || maxTTL > 24*time.Hour || len(allowed) == 0 || len(allowed) > 256 {
		return errors.New("workload identity requires runner tokens, an allowlist and a certificate lifetime between 1m and 24h")
	}
	if len(deniedKeys) > 4096 {
		return errors.New("workload identity key denylist exceeds 4096 entries")
	}
	p := &workloadPolicy{allowed: make(map[string]map[string]bool), denied: make(map[string]bool), maxTTL: maxTTL}
	for _, key := range deniedKeys {
		if len(key) != 64 || p.denied[key] {
			return errors.New("workload identity key denylist must contain unique lowercase SHA-256 fingerprints")
		}
		for _, c := range key {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return errors.New("workload identity key denylist must contain unique lowercase SHA-256 fingerprints")
			}
		}
		p.denied[key] = true
	}
	for kind, uris := range allowed {
		if len(rt.tokens[kind]) == 0 || len(uris) == 0 || len(uris) > 256 {
			return errors.New("workload identity allowlist must name configured runner kinds with bounded URI lists")
		}
		p.allowed[kind] = make(map[string]bool, len(uris))
		for _, uri := range uris {
			if !validWorkloadURI(uri) {
				return errors.New("workload identity requires canonical SPIFFE URI SANs")
			}
			if p.allowed[kind][uri] {
				return errors.New("workload identity URI allowlist must not contain duplicate entries")
			}
			p.allowed[kind][uri] = true
		}
	}
	for kind := range rt.tokens {
		if len(p.allowed[kind]) == 0 {
			return errors.New("workload identity allowlist must cover every configured runner kind")
		}
	}
	rt.workloads = p
	return nil
}

func ConfigureWorkloadIdentitiesFromEnv(rt *RunnerTokens) error {
	mode := os.Getenv("RUNKITE_WORKLOAD_IDENTITY")
	if mode == "" {
		if os.Getenv("RUNKITE_WORKLOAD_IDENTITIES_FILE") != "" || os.Getenv("RUNKITE_WORKLOAD_MAX_CERT_TTL") != "" {
			return errors.New("workload identity options require RUNKITE_WORKLOAD_IDENTITY=mtls")
		}
		return nil
	}
	if mode != "mtls" {
		return errors.New("unsupported workload identity mode")
	}
	maxTTL := time.Hour
	if raw := os.Getenv("RUNKITE_WORKLOAD_MAX_CERT_TTL"); raw != "" {
		var err error
		maxTTL, err = time.ParseDuration(raw)
		if err != nil {
			return errors.New("invalid workload maximum certificate lifetime")
		}
	}
	f, err := os.Open(os.Getenv("RUNKITE_WORKLOAD_IDENTITIES_FILE"))
	if err != nil {
		return errors.New("cannot open workload identity allowlist")
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return errors.New("cannot read bounded workload identity allowlist")
	}
	var configuration workloadIdentityFile
	decoded, err := jsonvalue.DecodeUnique(raw)
	if err != nil {
		return errors.New("invalid or ambiguous workload identity allowlist JSON")
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return errors.New("workload identity allowlist must be an object")
	}
	for key := range object {
		if key != "identities" && key != "denied_key_sha256" {
			return errors.New("unknown workload identity allowlist field")
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&configuration); err != nil {
		return errors.New("invalid workload identity allowlist JSON")
	}
	return rt.ConfigureWorkloadIdentityPolicy(configuration.Identities, configuration.DeniedKeys, maxTTL)
}

func validWorkloadURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || u.Scheme != "spiffe" || u.Host == "" || u.Host != strings.ToLower(u.Host) || u.Hostname() != u.Host || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" || u.ForceQuery || u.Path == "" || strings.Contains(raw, "%") || strings.Contains(u.Path, "//") {
		return false
	}
	for _, c := range u.Host {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '.' && c != '_' {
			return false
		}
	}
	for _, segment := range strings.Split(u.Path, "/")[1:] {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
		for _, c := range segment {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '.' && c != '_' {
				return false
			}
		}
	}
	return u.String() == raw
}

func (rt *RunnerTokens) WorkloadIdentityEnabled() bool { return rt != nil && rt.workloads != nil }

// AuthenticateWorkloadTLS accepts only a certificate verified by this server's
// TLS handshake. PeerCertificates alone, forwarded headers and certificate CN
// are deliberately insufficient. The public key is proof of possession from TLS.
func (rt *RunnerTokens) AuthenticateWorkloadTLS(ctx context.Context, connection *tls.ConnectionState, kind string) (context.Context, error) {
	if !rt.WorkloadIdentityEnabled() {
		return ctx, nil
	}
	if connection == nil || len(connection.VerifiedChains) == 0 || len(connection.VerifiedChains[0]) == 0 || len(connection.PeerCertificates) == 0 {
		return nil, ErrWorkloadIdentity
	}
	leaf := connection.VerifiedChains[0][0]
	now := time.Now()
	if leaf != connection.PeerCertificates[0] || len(leaf.URIs) != 1 || leaf.IsCA || now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) || leaf.NotAfter.Sub(leaf.NotBefore) > rt.workloads.maxTTL {
		return nil, ErrWorkloadIdentity
	}
	uri := leaf.URIs[0].String()
	if !validWorkloadURI(uri) || !rt.workloads.allowed[kind][uri] {
		return nil, ErrWorkloadIdentity
	}
	expires := leaf.NotAfter
	for _, cert := range connection.VerifiedChains[0] {
		if now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
			return nil, ErrWorkloadIdentity
		}
		if cert.NotAfter.Before(expires) {
			expires = cert.NotAfter
		}
	}
	key := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	ctx = transport.WithWorkloadIdentity(ctx, &transport.WorkloadIdentity{URI: uri, KeySHA256: hex.EncodeToString(key[:]), NotAfter: expires.UTC()})
	ctx = context.WithValue(ctx, workloadAuthorityKey{}, workloadAuthority{policy: rt.workloads, kind: kind})
	if err := RequireWorkloadAuthority(ctx); err != nil {
		return nil, err
	}
	return ctx, nil
}

// RequireWorkloadAuthority rechecks the immutable deployment policy for every
// operation. Replacing that policy requires draining/restarting every replica.
func RequireWorkloadAuthority(ctx context.Context) error {
	authority, bound := ctx.Value(workloadAuthorityKey{}).(workloadAuthority)
	if !bound {
		return nil
	}
	identity := WorkloadIdentityFromContext(ctx)
	if !identity.ValidAt(time.Now()) || !authority.policy.allowed[authority.kind][identity.URI] || authority.policy.denied[identity.KeySHA256] {
		return ErrWorkloadIdentity
	}
	return nil
}

func WorkloadIdentityFromContext(ctx context.Context) *transport.WorkloadIdentity {
	return transport.WorkloadIdentityFromContext(ctx)
}

// RequireWorkloadOwner checks both the original dispatch expiry and current
// connection proof. A kind or pool token does not authorize another worker.
func RequireWorkloadOwner(ctx context.Context, owner *transport.WorkloadIdentity) error {
	if err := RequireWorkloadAuthority(ctx); err != nil {
		return err
	}
	if !transport.MatchesWorkload(owner, WorkloadIdentityFromContext(ctx), time.Now()) {
		return fmt.Errorf("%w: assignment owner mismatch or expired", ErrWorkloadIdentity)
	}
	return nil
}
