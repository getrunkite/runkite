package postgres

import (
	"context"

	"github.com/getrunkite/runkite/internal/state/sqlcompletion"
	"github.com/jackc/pgx/v5/pgxpool"
)

func (s *Store) upPendingReviewAuthority(ctx context.Context, conn *pgxpool.Conn) error {
	if _, err := conn.Exec(ctx, `ALTER TABLE pending_actions ADD COLUMN IF NOT EXISTS policy_revision TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `ALTER TABLE pending_actions ADD COLUMN IF NOT EXISTS tool_revision TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `ALTER TABLE pending_actions ADD COLUMN IF NOT EXISTS requester_identity TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `ALTER TABLE pending_actions ADD COLUMN IF NOT EXISTS required_approvals INTEGER NOT NULL DEFAULT 1 CHECK (required_approvals BETWEEN 1 AND 5)`); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `ALTER TABLE pending_actions ADD COLUMN IF NOT EXISTS exclude_requester BOOLEAN NOT NULL DEFAULT FALSE`); err != nil {
		return err
	}
	for _, q := range sqlcompletion.PendingApprovalSchema("postgres") {
		if _, err := conn.Exec(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
