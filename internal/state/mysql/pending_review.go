package mysql

import (
	"context"

	"github.com/getrunkite/runkite/internal/state/migrate"
	"github.com/getrunkite/runkite/internal/state/sqlcompletion"
)

func (s *Store) upPendingReviewAuthority(ctx context.Context, db migrate.DB) error {
	if err := s.addColumnIfMissing(ctx, db, "pending_actions", "policy_revision", `VARCHAR(74) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := s.addColumnIfMissing(ctx, db, "pending_actions", "tool_revision", `VARCHAR(72) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := s.addColumnIfMissing(ctx, db, "pending_actions", "requester_identity", `VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := s.addColumnIfMissing(ctx, db, "pending_actions", "required_approvals", `INTEGER NOT NULL DEFAULT 1 CHECK (required_approvals BETWEEN 1 AND 5)`); err != nil {
		return err
	}
	if err := s.addColumnIfMissing(ctx, db, "pending_actions", "exclude_requester", `BOOLEAN NOT NULL DEFAULT FALSE`); err != nil {
		return err
	}
	for _, q := range sqlcompletion.PendingApprovalSchema("mysql") {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
