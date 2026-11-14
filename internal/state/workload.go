package state

import (
	"context"
	"encoding/json"
	"time"

	"github.com/getrunkite/runkite/internal/transport"
)

const WorkloadOwnerKey = "_runkite_workload_owner"

type trustedAttemptRecoveryKey struct{}

// WithTrustedAttemptRecovery is only for a control-plane reaper which already
// verified the exact retained queue generation. It never grants runner access.
func WithTrustedAttemptRecovery(ctx context.Context) context.Context {
	return context.WithValue(ctx, trustedAttemptRecoveryKey{}, true)
}

type WorkloadIdentityStore interface {
	SupportsWorkloadIdentity() bool
}

func StoredWorkloadOwner(metadata map[string]interface{}) (*transport.WorkloadIdentity, error) {
	value, present := metadata[WorkloadOwnerKey]
	if !present {
		return nil, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, &ErrRunCheckpointFence{ReasonCode: "workload_owner_invalid"}
	}
	var owner transport.WorkloadIdentity
	if json.Unmarshal(raw, &owner) != nil || owner.URI == "" || len(owner.KeySHA256) != 64 || owner.NotAfter.IsZero() {
		return nil, &ErrRunCheckpointFence{ReasonCode: "workload_owner_invalid"}
	}
	return &owner, nil
}

func CheckWorkloadOwner(ctx context.Context, metadata map[string]interface{}) error {
	owner, err := StoredWorkloadOwner(metadata)
	if err != nil || owner == nil {
		return err
	}
	if !transport.MatchesWorkload(owner, transport.WorkloadIdentityFromContext(ctx), time.Now()) {
		return &ErrRunCheckpointFence{ReasonCode: "workload_owner_mismatch"}
	}
	return nil
}

// BindAttemptWorkload may transfer ownership only when a new generation is
// installed. Missing identity never turns an owned run back into a fleet run.
func BindAttemptWorkload(ctx context.Context, metadata map[string]interface{}, generation int64) error {
	owner, err := StoredWorkloadOwner(metadata)
	if err != nil {
		return err
	}
	actor := transport.WorkloadIdentityFromContext(ctx)
	current := MetadataGeneration(metadata, AttemptGenerationKey)
	_, attempted := metadata[AttemptGenerationKey]
	if trusted, _ := ctx.Value(trustedAttemptRecoveryKey{}).(bool); trusted && (!attempted || generation > current) {
		if actor != nil {
			metadata[WorkloadOwnerKey] = *actor
		}
		return nil
	}
	if attempted && current == generation {
		if owner == nil && actor == nil {
			return nil
		}
		if !transport.MatchesWorkload(owner, actor, time.Now()) {
			return &ErrRunCheckpointFence{ReasonCode: "workload_owner_mismatch"}
		}
		return nil
	}
	if actor == nil {
		if owner != nil {
			return &ErrRunCheckpointFence{ReasonCode: "workload_owner_required"}
		}
		return nil
	}
	if !actor.ValidAt(time.Now()) {
		return &ErrRunCheckpointFence{ReasonCode: "workload_identity_expired"}
	}
	metadata[WorkloadOwnerKey] = *actor
	return nil
}

func CheckRunCompletionOwner(ctx context.Context, metadata map[string]interface{}, completion *RunCompletion) error {
	if completion.TrustedSystem {
		return nil
	}
	return CheckWorkloadOwner(ctx, metadata)
}
