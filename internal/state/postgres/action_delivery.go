package postgres

import (
	"context"
	"time"

	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlcompletion"
	"github.com/jackc/pgx/v5/pgxpool"
)

var _ state.ActionDeliveryStore = (*Store)(nil)

func (s *Store) ClaimActionDeliveries(ctx context.Context, sinkID string, now, leaseUntil time.Time, limit int) ([]*state.ActionDelivery, error) {
	return s.completionDriver().ClaimActionDeliveries(ctx, sinkID, now, leaseUntil, limit)
}
func (s *Store) AckActionDelivery(ctx context.Context, id, token string) error {
	return s.completionDriver().AckActionDelivery(ctx, id, token)
}
func (s *Store) FailActionDelivery(ctx context.Context, id, token string, nextAt time.Time, reason string, dead bool) error {
	return s.completionDriver().FailActionDelivery(ctx, id, token, nextAt, reason, dead)
}
func (s *Store) SearchActionDeliveries(ctx context.Context, req *state.ActionDeliverySearchRequest) ([]*state.ActionDelivery, error) {
	return s.completionDriver().SearchActionDeliveries(ctx, req)
}
func (s *Store) RetryActionDelivery(ctx context.Context, tenantID, id string) error {
	return s.completionDriver().RetryActionDelivery(ctx, tenantID, id)
}

func (s *Store) upActionDeliveries(ctx context.Context, conn *pgxpool.Conn) error {
	if _, err := conn.Exec(ctx, `ALTER TABLE connector_actions ADD COLUMN IF NOT EXISTS export_sink_id TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	for _, q := range sqlcompletion.ActionDeliverySchema("postgres") {
		if _, err := conn.Exec(ctx, q); err != nil {
			return err
		}
	}
	if _, err := conn.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_action_deliveries_claim ON action_deliveries(sink_id,status,available_at,claim_until,id)`); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_action_deliveries_tenant ON action_deliveries(tenant_id,created_at,id)`); err != nil {
		return err
	}
	return nil
}
