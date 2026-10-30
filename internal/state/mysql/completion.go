package mysql

import (
	"context"

	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlcompletion"
)

func (s *Store) BeginRunAttempt(ctx context.Context, id string, generation int64) (bool, error) {
	return sqlcompletion.DatabaseSQL(s.db, "mysql").BeginRunAttempt(ctx, id, generation)
}
func (s *Store) CompleteRun(ctx context.Context, c *state.RunCompletion) (state.CompletionResult, error) {
	driver := sqlcompletion.DatabaseSQL(s.db, "mysql")
	driver.ValidateValues = validateCompletionApplicationJSON
	return driver.CompleteRun(ctx, c)
}
