package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
)

func (s *Store) upUsageHolds(ctx context.Context, conn *pgxpool.Conn) error {
	_, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS usage_holds (
			run_id      TEXT PRIMARY KEY,
			tenant_id   TEXT NOT NULL DEFAULT 'default',
			agent_id    TEXT NOT NULL DEFAULT '',
			usd_hold    DOUBLE PRECISION NOT NULL DEFAULT 0,
			tokens_hold BIGINT NOT NULL DEFAULT 0,
			created_at  TIMESTAMPTZ NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_usage_holds_tenant_created ON usage_holds(tenant_id, created_at);
		CREATE INDEX IF NOT EXISTS idx_usage_holds_tenant_agent_created ON usage_holds(tenant_id, agent_id, created_at);
	`)
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
	_, err := s.pool.Exec(ctx, `
		INSERT INTO usage_holds (run_id, tenant_id, agent_id, usd_hold, tokens_hold, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (run_id) DO UPDATE SET
			tenant_id=EXCLUDED.tenant_id,
			agent_id=EXCLUDED.agent_id,
			usd_hold=EXCLUDED.usd_hold,
			tokens_hold=EXCLUDED.tokens_hold,
			created_at=EXCLUDED.created_at
	`, h.RunID, tenantID, h.AgentID, h.USDHold, h.TokensHold, ts)
	return err
}

func (s *Store) ReleaseUsageHold(ctx context.Context, runID string) error {
	if runID == "" {
		return nil
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM usage_holds WHERE run_id = $1`, runID)
	return err
}

func (s *Store) SumUsageHolds(ctx context.Context, tenantID, agentID string, since, until time.Time) (usd float64, tokens int64, count int64, err error) {
	if tenantID == "" {
		tenantID = tenant.FromContext(ctx)
	}
	q := `SELECT COALESCE(SUM(usd_hold),0), COALESCE(SUM(tokens_hold),0), COUNT(*)
		FROM usage_holds WHERE tenant_id = $1 AND created_at >= $2 AND created_at < $3`
	args := []interface{}{tenantID, since, until}
	if agentID != "" {
		q += ` AND agent_id = $4`
		args = append(args, agentID)
	}
	err = s.pool.QueryRow(ctx, q, args...).Scan(&usd, &tokens, &count)
	return
}

// ExpireUsageHolds deletes open holds with created_at strictly before olderThan.
func (s *Store) ExpireUsageHolds(ctx context.Context, olderThan time.Time) (int64, error) {
	if olderThan.IsZero() {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM usage_holds WHERE created_at < $1`, olderThan.UTC())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

