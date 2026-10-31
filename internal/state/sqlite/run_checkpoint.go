package sqlite

import (
	"context"

	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlcompletion"
)

func (s *SQLiteStore) PutOpaqueCheckpoint(ctx context.Context, threadID, checkpointID string, data []byte, framework string, ifMatch *int64) (int64, error) {
	var version int64
	err := s.completionDriver().WithThread(ctx, threadID, func(tx sqlcompletion.Tx) error {
		var err error
		version, err = s.putOpaqueCheckpoint(ctx, tx.Row, threadID, checkpointID, data, framework, ifMatch)
		return err
	})
	return version, err
}

func (s *SQLiteStore) PutBoundOpaqueCheckpoint(ctx context.Context, binding state.RunCheckpointBinding, checkpointID string, data []byte, framework string, ifMatch *int64) (int64, error) {
	var version int64
	err := sqlcompletion.DatabaseSQL(s.db, "sqlite").WithRunCheckpoint(ctx, binding, func(tx sqlcompletion.Tx) error {
		var err error
		version, err = s.putOpaqueCheckpoint(ctx, tx.Row, binding.ThreadID, checkpointID, data, framework, ifMatch)
		return err
	})
	return version, err
}

func (s *SQLiteStore) DeleteBoundOpaqueCheckpoint(ctx context.Context, binding state.RunCheckpointBinding, checkpointID string) error {
	return sqlcompletion.DatabaseSQL(s.db, "sqlite").DeleteBoundOpaqueCheckpoint(ctx, binding, checkpointID)
}
