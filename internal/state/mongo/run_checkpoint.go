package mongo

import (
	"context"
	"errors"

	"github.com/getrunkite/runkite/internal/state"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// lockCheckpointRun forces a write to the run inside the checkpoint transaction.
// A read alone does not fence a later mutation under Mongo snapshot isolation:
// BeginRunAttempt or CompleteRun could commit after that read. Updating this
// internal revision makes those concurrent run writes conflict, so
// WithTransaction retries against their new generation or terminal status.
func (s *Store) lockCheckpointRun(ctx context.Context, binding state.RunCheckpointBinding) error {
	run, err := s.GetRun(ctx, binding.RunID)
	if err != nil {
		var missing *state.ErrNotFound
		if errors.As(err, &missing) {
			return state.CheckRunCheckpointBinding(nil, binding)
		}
		return err
	}
	if err := state.CheckRunCheckpointBinding(run, binding); err != nil {
		return err
	}
	result, err := s.col("runs").UpdateOne(ctx,
		tenantFilter(ctx, bson.M{"run_id": binding.RunID}),
		bson.M{"$inc": bson.M{"_runkite_checkpoint_revision": int64(1)}},
	)
	if err != nil {
		return err
	}
	if result.MatchedCount == 0 {
		return state.CheckRunCheckpointBinding(nil, binding)
	}
	return nil
}

func (s *Store) PutBoundOpaqueCheckpoint(ctx context.Context, binding state.RunCheckpointBinding, checkpointID string, data []byte, framework string, ifMatch *int64) (int64, error) {
	var version int64
	err := s.withTransaction(ctx, func(sessCtx context.Context) error {
		version = 0
		if err := s.lockCheckpointRun(sessCtx, binding); err != nil {
			return err
		}
		var err error
		version, err = s.PutOpaqueCheckpoint(sessCtx, binding.ThreadID, checkpointID, data, framework, ifMatch)
		return err
	})
	return version, err
}

func (s *Store) DeleteBoundOpaqueCheckpoint(ctx context.Context, binding state.RunCheckpointBinding, checkpointID string) error {
	return s.withTransaction(ctx, func(sessCtx context.Context) error {
		if err := s.lockCheckpointRun(sessCtx, binding); err != nil {
			return err
		}
		return s.DeleteOpaqueCheckpoint(sessCtx, binding.ThreadID, checkpointID)
	})
}
