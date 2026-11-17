package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

func (s *Store) upRunFinalization(ctx context.Context, conn *pgxpool.Conn) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = tx.Exec(ctx, `ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS run_incarnation_id TEXT COLLATE "C" NOT NULL DEFAULT '';
 ALTER TABLE usage_holds ADD COLUMN IF NOT EXISTS run_incarnation_id TEXT COLLATE "C" NOT NULL DEFAULT '';
 ALTER TABLE usage_events DROP CONSTRAINT IF EXISTS usage_events_run_id_source_key;
 CREATE UNIQUE INDEX IF NOT EXISTS uq_usage_events_record_source ON usage_events(run_id,run_incarnation_id,source);`)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
