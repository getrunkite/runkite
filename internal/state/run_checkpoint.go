package state

import (
	"context"

	"github.com/getrunkite/runkite/internal/models"
)

// RunCheckpointBinding identifies the durable attempt allowed to mutate a
// thread's opaque state. Stores check it under the same transaction as the
// mutation, serialized with BeginRunAttempt and terminal completion.
type RunCheckpointBinding struct {
	RunID      string
	Generation int64
	ThreadID   string
}

type RunCheckpointStore interface {
	PutBoundOpaqueCheckpoint(context.Context, RunCheckpointBinding, string, []byte, string, *int64) (int64, error)
	DeleteBoundOpaqueCheckpoint(context.Context, RunCheckpointBinding, string) error
}

// ErrRunCheckpointFence is a rejected attempt, not a blob-version conflict.
// The HTTP proxy preserves the existing run-binding denial reason codes.
type ErrRunCheckpointFence struct{ ReasonCode string }

func (e *ErrRunCheckpointFence) Error() string { return "checkpoint attempt rejected: " + e.ReasonCode }

func CheckRunCheckpointBinding(run *models.Run, binding RunCheckpointBinding) error {
	if run == nil || TerminalRunStatus(run.Status) {
		return &ErrRunCheckpointFence{ReasonCode: "run_not_inflight"}
	}
	if run.ThreadID != binding.ThreadID {
		return &ErrRunCheckpointFence{ReasonCode: "run_thread_mismatch"}
	}
	if MetadataGeneration(run.Metadata, AttemptGenerationKey) != binding.Generation {
		return &ErrRunCheckpointFence{ReasonCode: "run_generation_mismatch"}
	}
	return nil
}
