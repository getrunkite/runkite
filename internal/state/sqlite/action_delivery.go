package sqlite

import (
	"context"
	"time"

	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlcompletion"
)

var _ state.ActionDeliveryStore = (*SQLiteStore)(nil)

func (s *SQLiteStore) ClaimActionDeliveries(ctx context.Context, sinkID string, now, leaseUntil time.Time, limit int) ([]*state.ActionDelivery, error) {
	return s.completionDriver().ClaimActionDeliveries(ctx, sinkID, now, leaseUntil, limit)
}
func (s *SQLiteStore) AckActionDelivery(ctx context.Context, id, token string) error {
	return s.completionDriver().AckActionDelivery(ctx, id, token)
}
func (s *SQLiteStore) FailActionDelivery(ctx context.Context, id, token string, nextAt time.Time, reason string, dead bool) error {
	return s.completionDriver().FailActionDelivery(ctx, id, token, nextAt, reason, dead)
}
func (s *SQLiteStore) SearchActionDeliveries(ctx context.Context, req *state.ActionDeliverySearchRequest) ([]*state.ActionDelivery, error) {
	return s.completionDriver().SearchActionDeliveries(ctx, req)
}
func (s *SQLiteStore) RetryActionDelivery(ctx context.Context, tenantID, id string) error {
	return s.completionDriver().RetryActionDelivery(ctx, tenantID, id)
}

func (s *SQLiteStore) upActionDeliveries(ctx context.Context) error {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('connector_actions') WHERE name='export_sink_id'`).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE connector_actions ADD COLUMN export_sink_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	for _, q := range sqlcompletion.ActionDeliverySchema("sqlite") {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_action_deliveries_claim ON action_deliveries(sink_id,status,available_at,claim_until,id)`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_action_deliveries_tenant ON action_deliveries(tenant_id,created_at,id)`); err != nil {
		return err
	}
	return nil
}
