package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

func (s *Store) upPendingTargetRevision(ctx context.Context, conn *pgxpool.Conn) error {
	_, err := conn.Exec(ctx, `ALTER TABLE pending_actions ADD COLUMN IF NOT EXISTS target_revision TEXT NOT NULL DEFAULT ''`)
	return err
}
