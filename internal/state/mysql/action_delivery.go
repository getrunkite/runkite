package mysql

import (
	"context"
	"time"

	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/migrate"
	"github.com/getrunkite/runkite/internal/state/sqlcompletion"
)

var _ state.ActionDeliveryStore = (*Store)(nil)

func (s *Store) ClaimActionDeliveries(ctx context.Context, sinkID string, now, leaseUntil time.Time, limit int) ([]*state.ActionDelivery, error) {
	return s.connectorActionDriver().ClaimActionDeliveries(ctx, sinkID, now, leaseUntil, limit)
}
func (s *Store) AckActionDelivery(ctx context.Context, id, token string) error {
	return s.connectorActionDriver().AckActionDelivery(ctx, id, token)
}
func (s *Store) FailActionDelivery(ctx context.Context, id, token string, nextAt time.Time, reason string, dead bool) error {
	return s.connectorActionDriver().FailActionDelivery(ctx, id, token, nextAt, reason, dead)
}
func (s *Store) SearchActionDeliveries(ctx context.Context, req *state.ActionDeliverySearchRequest) ([]*state.ActionDelivery, error) {
	return s.connectorActionDriver().SearchActionDeliveries(ctx, req)
}
func (s *Store) RetryActionDelivery(ctx context.Context, tenantID, id string) error {
	return s.connectorActionDriver().RetryActionDelivery(ctx, tenantID, id)
}

func (s *Store) upActionDeliveries(ctx context.Context, db migrate.DB) error {
	if err := s.addColumnIfMissing(ctx, db, "connector_actions", "export_sink_id", `VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	for _, q := range sqlcompletion.ActionDeliverySchema("mysql") {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	if err := s.addIndexIfMissing(ctx, db, "action_deliveries", "idx_action_deliveries_claim", "sink_id,status,available_at,claim_until,id"); err != nil {
		return err
	}
	if err := s.addIndexIfMissing(ctx, db, "action_deliveries", "idx_action_deliveries_tenant", "tenant_id,created_at,id"); err != nil {
		return err
	}
	return nil
}
