package main

import (
	"errors"
	"os"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/transport"
)

func configureWorkloadIdentity(tokens *auth.RunnerTokens) error {
	if err := auth.ConfigureWorkloadIdentitiesFromEnv(tokens); err != nil {
		return err
	}
	if !tokens.WorkloadIdentityEnabled() {
		return nil
	}
	for _, prefix := range []string{"TLS_", "GRPC_TLS_"} {
		if os.Getenv(prefix+"CERT_FILE") == "" || os.Getenv(prefix+"KEY_FILE") == "" || os.Getenv(prefix+"CLIENT_CA_FILE") == "" {
			return errors.New("strict workload identity requires direct verified client TLS on both HTTP and gRPC listeners")
		}
	}
	return nil
}

func validateWorkloadIdentityBackends(tokens *auth.RunnerTokens, store state.Store, queue transport.JobQueue) error {
	if !tokens.WorkloadIdentityEnabled() {
		return nil
	}
	q, ok := queue.(transport.WorkloadIdentityQueue)
	if !ok || !q.SupportsWorkloadIdentity() {
		return errors.New("strict workload identity requires the inprocess or Redis queue")
	}
	s, ok := store.(interface{ SupportsWorkloadIdentity() bool })
	if !ok || !s.SupportsWorkloadIdentity() {
		return errors.New("strict workload identity requires a supported SQL store")
	}
	return nil
}
