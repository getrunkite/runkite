package auth

import (
	"context"
	"crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkloadIdentityAllowlistFailsClosed(t *testing.T) {
	for _, uri := range []string{
		"", "https://test.runkite/worker", "spiffe://test.runkite/worker?token=hidden", "spiffe://test.runkite/worker#fragment",
		"spiffe://user@test.runkite/worker", "spiffe://test.runkite:443/worker", "spiffe://TEST.runkite/worker",
		"spiffe://test.runkite/../worker", "spiffe://test.runkite/worker//replica", "spiffe://test.runkite/worker%2Freplica",
	} {
		rt := &RunnerTokens{tokens: map[string][]string{"kind": {"synthetic"}}}
		if err := rt.ConfigureWorkloadIdentities(map[string][]string{"kind": {uri}}, time.Hour); err == nil {
			t.Errorf("invalid URI accepted: %q", uri)
		}
		if rt.WorkloadIdentityEnabled() {
			t.Fatal("failed config enabled identity mode")
		}
	}
	rt := &RunnerTokens{tokens: map[string][]string{"kind": {"synthetic"}, "other": {"other synthetic"}}}
	if err := rt.ConfigureWorkloadIdentities(map[string][]string{"kind": {"spiffe://test.runkite/worker"}}, time.Hour); err == nil {
		t.Fatal("unmapped runner kind accepted")
	}
	if err := rt.ConfigureWorkloadIdentities(map[string][]string{"kind": {"spiffe://test.runkite/worker"}, "other": {"spiffe://test.runkite/other"}}, 25*time.Hour); err == nil {
		t.Fatal("unbounded certificate lifetime accepted")
	}
}

func TestWorkloadIdentityConfigurationRequiresExplicitMode(t *testing.T) {
	t.Setenv("RUNKITE_WORKLOAD_IDENTITY", "")
	t.Setenv("RUNKITE_WORKLOAD_IDENTITIES_FILE", "")
	t.Setenv("RUNKITE_WORKLOAD_MAX_CERT_TTL", "")
	rt := &RunnerTokens{tokens: map[string][]string{"kind": {"synthetic"}}}
	if err := ConfigureWorkloadIdentitiesFromEnv(rt); err != nil || rt.WorkloadIdentityEnabled() {
		t.Fatalf("legacy default changed: %v", err)
	}
	file := filepath.Join(t.TempDir(), "identities.json")
	if err := os.WriteFile(file, []byte(`{"identities":{"kind":["spiffe://test.runkite/worker"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUNKITE_WORKLOAD_IDENTITIES_FILE", file)
	if err := ConfigureWorkloadIdentitiesFromEnv(rt); err == nil {
		t.Fatal("identity config silently ignored without mode")
	}
	t.Setenv("RUNKITE_WORKLOAD_IDENTITY", "mtls")
	if err := ConfigureWorkloadIdentitiesFromEnv(rt); err != nil || !rt.WorkloadIdentityEnabled() {
		t.Fatalf("valid config rejected: %v", err)
	}
	if _, err := rt.AuthenticateWorkloadTLS(context.Background(), &tls.ConnectionState{}, "kind"); err == nil {
		t.Fatal("unverified TLS accepted")
	}
}

func TestWorkloadIdentityKeyRetirementConfiguration(t *testing.T) {
	allowed := map[string][]string{"kind": {"spiffe://test.runkite/worker"}}
	key := strings.Repeat("a", 64)
	for _, denied := range [][]string{{""}, {strings.Repeat("A", 64)}, {strings.Repeat("g", 64)}, {key, key}, make([]string, 4097)} {
		rt := &RunnerTokens{tokens: map[string][]string{"kind": {"synthetic"}}}
		if err := rt.ConfigureWorkloadIdentityPolicy(allowed, denied, time.Hour); err == nil {
			t.Fatal("malformed, duplicate or oversized key denylist accepted")
		}
		if rt.WorkloadIdentityEnabled() {
			t.Fatal("invalid policy partially enabled strict mode")
		}
	}
	rt := &RunnerTokens{tokens: map[string][]string{"kind": {"synthetic"}}}
	if err := rt.ConfigureWorkloadIdentityPolicy(allowed, []string{key}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if !rt.workloads.denied[key] {
		t.Fatal("retired key not stored")
	}
}

func TestWorkloadIdentityFileRejectsAmbiguousAndUnknownFields(t *testing.T) {
	t.Setenv("RUNKITE_WORKLOAD_IDENTITY", "mtls")
	t.Setenv("RUNKITE_WORKLOAD_MAX_CERT_TTL", "1h")
	file := filepath.Join(t.TempDir(), "identities.json")
	t.Setenv("RUNKITE_WORKLOAD_IDENTITIES_FILE", file)
	for _, raw := range []string{
		`{"Identities":{"kind":["spiffe://test.runkite/worker"]}}`,
		`{"identities":{"kind":["spiffe://test.runkite/worker"]},"IDENTITIES":{"kind":["spiffe://test.runkite/other"]}}`,
		`{"identities":{"kind":["spiffe://test.runkite/worker"]},"denied_key_SHA256":[]}`,
		`{"identities":{"kind":["spiffe://test.runkite/worker"]},"identities":{"kind":["spiffe://test.runkite/other"]}}`,
		`{"identities":{"kind":["spiffe://test.runkite/worker"]},"unknown":true}`,
	} {
		if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		rt := &RunnerTokens{tokens: map[string][]string{"kind": {"synthetic"}}}
		if err := ConfigureWorkloadIdentitiesFromEnv(rt); err == nil {
			t.Fatalf("ambiguous configuration accepted: %s", raw)
		}
	}
}
