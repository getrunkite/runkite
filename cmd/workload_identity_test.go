package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

type workloadCapableStore struct{ state.Store }

func (workloadCapableStore) SupportsWorkloadIdentity() bool { return true }

type workloadLegacyQueue struct{ transport.JobQueue }
type workloadLegacyStore struct{ state.Store }

func TestWorkloadIdentityRequiresBothDirectTLSListeners(t *testing.T) {
	t.Setenv("RUNNER_TOKEN_WORKLOAD_FIXTURE", "synthetic")
	t.Setenv("RUNKITE_WORKLOAD_IDENTITY", "mtls")
	t.Setenv("RUNKITE_WORKLOAD_MAX_CERT_TTL", "1h")
	file := filepath.Join(t.TempDir(), "identities.json")
	if err := os.WriteFile(file, []byte(`{"identities":{"workload-fixture":["spiffe://fixture.test/worker"]},"denied_key_sha256":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUNKITE_WORKLOAD_IDENTITIES_FILE", file)
	keys := []string{"TLS_CERT_FILE", "TLS_KEY_FILE", "TLS_CLIENT_CA_FILE", "GRPC_TLS_CERT_FILE", "GRPC_TLS_KEY_FILE", "GRPC_TLS_CLIENT_CA_FILE"}
	for _, key := range keys {
		t.Setenv(key, "")
	}
	for _, key := range keys {
		if err := configureWorkloadIdentity(auth.LoadRunnerTokensFromEnv()); err == nil {
			t.Fatalf("missing TLS material accepted before %s", key)
		}
		t.Setenv(key, "synthetic-path")
	}
	if err := configureWorkloadIdentity(auth.LoadRunnerTokensFromEnv()); err != nil {
		t.Fatal(err)
	}
}

func TestWorkloadIdentityRejectsUnsupportedBackends(t *testing.T) {
	t.Setenv("RUNNER_TOKEN_WORKLOAD_FIXTURE", "synthetic")
	tokens := auth.LoadRunnerTokensFromEnv()
	if err := tokens.ConfigureWorkloadIdentities(map[string][]string{"workload-fixture": {"spiffe://fixture.test/worker"}}, time.Hour); err != nil {
		t.Fatal(err)
	}
	queue := inprocess.NewQueue()
	if err := validateWorkloadIdentityBackends(tokens, workloadCapableStore{}, queue); err != nil {
		t.Fatal(err)
	}
	if err := validateWorkloadIdentityBackends(tokens, workloadLegacyStore{}, queue); err == nil {
		t.Fatal("store without durable ownership accepted")
	}
	if err := validateWorkloadIdentityBackends(tokens, workloadCapableStore{}, workloadLegacyQueue{queue}); err == nil {
		t.Fatal("queue without atomic ownership accepted")
	}
	if err := validateWorkloadIdentityBackends(nil, nil, nil); err != nil {
		t.Fatalf("legacy disabled mode changed: %v", err)
	}
}
