package postgres

import (
	"context"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlcompletion"
	"github.com/jackc/pgx/v5/pgxpool"
)

var _ state.ProtectedActionStore = (*Store)(nil)

func (s *Store) CreateConnectorAction(ctx context.Context, a *models.ConnectorAction, mutation string) (*models.ConnectorAction, error) {
	return s.completionDriver().CreateConnectorAction(ctx, a, mutation)
}

func (s *Store) RecordConnectorActionDecision(ctx context.Context, id string, version int64, dec models.ConnectorActionDecision, mutation string) (*models.ConnectorAction, error) {
	return s.completionDriver().RecordConnectorActionDecision(ctx, id, version, dec, mutation)
}

func (s *Store) PeekApprovedConnectorAction(ctx context.Context, id string) (*models.PendingAction, error) {
	return s.completionDriver().PeekApprovedConnectorAction(ctx, id)
}

func (s *Store) PrepareConnectorActionDispatch(ctx context.Context, id string, version int64, approval, source string, deadline time.Time, mutation string) (*state.ConnectorActionDispatch, error) {
	return s.completionDriver().PrepareConnectorActionDispatch(ctx, id, version, approval, source, deadline, mutation)
}

func (s *Store) FinishConnectorAction(ctx context.Context, id string, version int64, dispatch string, obs models.ConnectorActionObservation, mutation string) (*models.ConnectorAction, error) {
	return s.completionDriver().FinishConnectorAction(ctx, id, version, dispatch, obs, mutation)
}

func (s *Store) GetConnectorAction(ctx context.Context, id string) (*models.ConnectorAction, error) {
	return s.completionDriver().GetConnectorAction(ctx, id)
}

func (s *Store) SearchConnectorActions(ctx context.Context, req *models.ConnectorActionSearchRequest) ([]*models.ConnectorAction, error) {
	return s.completionDriver().SearchConnectorActions(ctx, req)
}

func (s *Store) ListConnectorActionEvents(ctx context.Context, id string, after int64, limit int) ([]*models.ConnectorActionEvent, error) {
	return s.completionDriver().ListConnectorActionEvents(ctx, id, after, limit)
}

func (s *Store) MarkExpiredConnectorActionsUnknown(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	return s.completionDriver().MarkExpiredConnectorActionsUnknown(ctx, cutoff, limit)
}

func (s *Store) PruneConnectorActions(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	return s.completionDriver().PruneConnectorActions(ctx, cutoff, limit)
}

func (t completionTx) ActionQuery(ctx context.Context, q string, args ...interface{}) (sqlcompletion.ActionRows, error) {
	return t.Query(ctx, q, args...)
}

func (s *Store) upConnectorActions(ctx context.Context, conn *pgxpool.Conn) error {
	if _, err := conn.Exec(ctx, `ALTER TABLE runs ADD COLUMN IF NOT EXISTS incarnation_id TEXT NOT NULL DEFAULT (gen_random_uuid()::text)`); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `ALTER TABLE pending_actions ADD COLUMN IF NOT EXISTS run_incarnation_id TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	for _, q := range sqlcompletion.ConnectorActionSchema("postgres") {
		if _, err := conn.Exec(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
