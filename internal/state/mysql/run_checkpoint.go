package mysql

import (
	"context"

	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlcompletion"
)

func (s *Store) PutOpaqueCheckpoint(ctx context.Context, threadID, checkpointID string, data []byte, framework string, ifMatch *int64) (int64, error) {
	return s.putOpaqueCheckpoint(ctx, threadID, checkpointID, data, framework, ifMatch, nil)
}

func (s *Store) PutBoundOpaqueCheckpoint(ctx context.Context, binding state.RunCheckpointBinding, checkpointID string, data []byte, framework string, ifMatch *int64) (int64, error) {
	return s.putOpaqueCheckpoint(ctx, binding.ThreadID, checkpointID, data, framework, ifMatch, &binding)
}

func (s *Store) DeleteBoundOpaqueCheckpoint(ctx context.Context, binding state.RunCheckpointBinding, checkpointID string) error {
	return sqlcompletion.DatabaseSQL(s.db, "mysql").DeleteBoundOpaqueCheckpoint(ctx, binding, checkpointID)
}
