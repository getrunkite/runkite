package sqlite

import (
	"context"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlcompletion"
)

var _ state.ProtectedActionStore = (*SQLiteStore)(nil)

func (s *SQLiteStore) CreateConnectorAction(ctx context.Context, a *models.ConnectorAction, mutation string) (*models.ConnectorAction, error) {
	return s.completionDriver().CreateConnectorAction(ctx, a, mutation)
}

func (s *SQLiteStore) RecordConnectorActionDecision(ctx context.Context, id string, version int64, dec models.ConnectorActionDecision, mutation string) (*models.ConnectorAction, error) {
	return s.completionDriver().RecordConnectorActionDecision(ctx, id, version, dec, mutation)
}

func (s *SQLiteStore) PeekApprovedConnectorAction(ctx context.Context, id string) (*models.PendingAction, error) {
	return s.completionDriver().PeekApprovedConnectorAction(ctx, id)
}

func (s *SQLiteStore) PrepareConnectorActionDispatch(ctx context.Context, id string, version int64, approval, source string, deadline time.Time, mutation string) (*state.ConnectorActionDispatch, error) {
	return s.completionDriver().PrepareConnectorActionDispatch(ctx, id, version, approval, source, deadline, mutation)
}

func (s *SQLiteStore) FinishConnectorAction(ctx context.Context, id string, version int64, dispatch string, obs models.ConnectorActionObservation, mutation string) (*models.ConnectorAction, error) {
	return s.completionDriver().FinishConnectorAction(ctx, id, version, dispatch, obs, mutation)
}

func (s *SQLiteStore) GetConnectorAction(ctx context.Context, id string) (*models.ConnectorAction, error) {
	return s.completionDriver().GetConnectorAction(ctx, id)
}

func (s *SQLiteStore) SearchConnectorActions(ctx context.Context, req *models.ConnectorActionSearchRequest) ([]*models.ConnectorAction, error) {
	return s.completionDriver().SearchConnectorActions(ctx, req)
}

func (s *SQLiteStore) ListConnectorActionEvents(ctx context.Context, id string, after int64, limit int) ([]*models.ConnectorActionEvent, error) {
	return s.completionDriver().ListConnectorActionEvents(ctx, id, after, limit)
}

func (s *SQLiteStore) MarkExpiredConnectorActionsUnknown(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	return s.completionDriver().MarkExpiredConnectorActionsUnknown(ctx, cutoff, limit)
}

func (s *SQLiteStore) PruneConnectorActions(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	return s.completionDriver().PruneConnectorActions(ctx, cutoff, limit)
}

func (s *SQLiteStore) upConnectorActions(ctx context.Context) error {
	// ALTER TABLE does not allow a nonconstant expression default. The
	// trigger generates the same insertion-time identity atomically, including
	// legacy INSERT statements and synchronous cached-run creation.
	var columns int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('runs') WHERE name='incarnation_id'`).Scan(&columns); err != nil {
		return err
	}
	if columns == 0 {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE runs ADD COLUMN incarnation_id TEXT`); err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER IF NOT EXISTS runs_incarnation_after_insert AFTER INSERT ON runs WHEN NEW.incarnation_id IS NULL OR NEW.incarnation_id='' BEGIN UPDATE runs SET incarnation_id=lower(hex(randomblob(16))) WHERE run_id=NEW.run_id; END`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE runs SET incarnation_id=lower(hex(randomblob(16))) WHERE incarnation_id IS NULL OR incarnation_id=''`); err != nil {
		return err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('pending_actions') WHERE name='run_incarnation_id'`).Scan(&columns); err != nil {
		return err
	}
	if columns == 0 {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE pending_actions ADD COLUMN run_incarnation_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	for _, q := range sqlcompletion.ConnectorActionSchema("sqlite") {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return s.normalizeCheckpointTimes(ctx)
}
