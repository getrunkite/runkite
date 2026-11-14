package sqlite

import (
	"context"

	"github.com/getrunkite/runkite/internal/state/sqlcompletion"
)

func (s *SQLiteStore) upPendingReviewAuthority(ctx context.Context) error {
	var n_policy_revision int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('pending_actions') WHERE name='policy_revision'`).Scan(&n_policy_revision); err != nil {
		return err
	}
	if n_policy_revision == 0 {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE pending_actions ADD COLUMN policy_revision TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	var n_tool_revision int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('pending_actions') WHERE name='tool_revision'`).Scan(&n_tool_revision); err != nil {
		return err
	}
	if n_tool_revision == 0 {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE pending_actions ADD COLUMN tool_revision TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	var n_requester_identity int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('pending_actions') WHERE name='requester_identity'`).Scan(&n_requester_identity); err != nil {
		return err
	}
	if n_requester_identity == 0 {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE pending_actions ADD COLUMN requester_identity TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	var n_required_approvals int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('pending_actions') WHERE name='required_approvals'`).Scan(&n_required_approvals); err != nil {
		return err
	}
	if n_required_approvals == 0 {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE pending_actions ADD COLUMN required_approvals INTEGER NOT NULL DEFAULT 1 CHECK (required_approvals BETWEEN 1 AND 5)`); err != nil {
			return err
		}
	}
	var n_exclude_requester int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('pending_actions') WHERE name='exclude_requester'`).Scan(&n_exclude_requester); err != nil {
		return err
	}
	if n_exclude_requester == 0 {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE pending_actions ADD COLUMN exclude_requester BOOLEAN NOT NULL DEFAULT FALSE`); err != nil {
			return err
		}
	}
	for _, q := range sqlcompletion.PendingApprovalSchema("sqlite") {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
