package sqlite

import (
	"context"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlcompletion"
)

func (s *SQLiteStore) BeginRunAttempt(ctx context.Context, id string, generation int64) (bool, error) {
	return sqlcompletion.DatabaseSQL(s.db, "sqlite").BeginRunAttempt(ctx, id, generation)
}
func (s *SQLiteStore) CompleteRun(ctx context.Context, c *state.RunCompletion) (state.CompletionResult, error) {
	return sqlcompletion.DatabaseSQL(s.db, "sqlite").CompleteRun(ctx, c)
}
