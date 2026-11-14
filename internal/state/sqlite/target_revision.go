package sqlite

import "context"

func (s *SQLiteStore) upPendingTargetRevision(ctx context.Context) error {
	var columns int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('pending_actions') WHERE name='target_revision'`).Scan(&columns); err != nil {
		return err
	}
	if columns != 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `ALTER TABLE pending_actions ADD COLUMN target_revision TEXT NOT NULL DEFAULT ''`)
	return err
}
