package mysql

import (
	"context"
	"time"

	"github.com/getrunkite/runkite/internal/state/migrate"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
)

func (s *Store) upUsageHolds(ctx context.Context, db migrate.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS usage_holds (
			run_id      VARCHAR(255) PRIMARY KEY,
			tenant_id   VARCHAR(255) NOT NULL DEFAULT 'default',
			agent_id    VARCHAR(255) NOT NULL DEFAULT '',
			usd_hold    DOUBLE NOT NULL DEFAULT 0,
			tokens_hold BIGINT NOT NULL DEFAULT 0,
			created_at  DATETIME(6) NOT NULL,
			KEY idx_usage_holds_tenant_created (tenant_id, created_at),
			KEY idx_usage_holds_tenant_agent_created (tenant_id, agent_id, created_at)
		) ENGINE=InnoDB CHARSET=utf8mb4`)
	return err
}

func (s *Store) UpsertUsageHold(ctx context.Context, h *models.UsageHold) error {
	if h == nil || h.RunID == "" {
		return nil
	}
	tenantID := h.TenantID
	if tenantID == "" {
		tenantID = tenant.FromContext(ctx)
	}
	ts := h.CreatedAt
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO usage_holds (run_id, tenant_id, agent_id, usd_hold, tokens_hold, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			tenant_id=VALUES(tenant_id),
			agent_id=VALUES(agent_id),
			usd_hold=VALUES(usd_hold),
			tokens_hold=VALUES(tokens_hold),
			created_at=VALUES(created_at)
	`, h.RunID, tenantID, h.AgentID, h.USDHold, h.TokensHold, ts)
	return err
}

func (s *Store) ReleaseUsageHold(ctx context.Context, runID string) error {
	if runID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM usage_holds WHERE run_id = ?`, runID)
	return err
}

func (s *Store) SumUsageHolds(ctx context.Context, tenantID, agentID string, since, until time.Time) (usd float64, tokens int64, count int64, err error) {
	if tenantID == "" {
		tenantID = tenant.FromContext(ctx)
	}
	q := `SELECT COALESCE(SUM(usd_hold),0), COALESCE(SUM(tokens_hold),0), COUNT(*)
		FROM usage_holds WHERE tenant_id = ? AND created_at >= ? AND created_at < ?`
	args := []interface{}{tenantID, since, until}
	if agentID != "" {
		q += ` AND agent_id = ?`
		args = append(args, agentID)
	}
	err = s.db.QueryRowContext(ctx, q, args...).Scan(&usd, &tokens, &count)
	return
}
