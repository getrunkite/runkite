package postgres

import (
	"context"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlcompletion"
	"github.com/jackc/pgx/v5"
)

type completionTx struct{ pgx.Tx }

func (t completionTx) Exec(ctx context.Context, q string, a ...interface{}) error {
	_, err := t.Tx.Exec(ctx, q, a...)
	return err
}
func (t completionTx) Row(ctx context.Context, q string, a ...interface{}) sqlcompletion.Row {
	return t.Tx.QueryRow(ctx, q, a...)
}
func (s *Store) completionDriver() sqlcompletion.Driver {
	return sqlcompletion.Driver{Dialect: "postgres", Begin: func(ctx context.Context) (sqlcompletion.Tx, error) {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return nil, err
		}
		return completionTx{tx}, nil
	}}
}
func (s *Store) BeginRunAttempt(ctx context.Context, id string, generation int64) (bool, error) {
	return s.completionDriver().BeginRunAttempt(ctx, id, generation)
}
func (s *Store) CompleteRun(ctx context.Context, c *state.RunCompletion) (state.CompletionResult, error) {
	return s.completionDriver().CompleteRun(ctx, c)
}
