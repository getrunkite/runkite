package sqlite

import (
	"context"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
)

func (s *SQLiteStore) upUsageHolds(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS usage_holds (
			run_id      TEXT PRIMARY KEY,
			tenant_id   TEXT NOT NULL DEFAULT 'default',
			agent_id    TEXT NOT NULL DEFAULT '',
			usd_hold    REAL NOT NULL DEFAULT 0,
			tokens_hold INTEGER NOT NULL DEFAULT 0,
			created_at  TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_usage_holds_tenant_created ON usage_holds(tenant_id, created_at);
		CREATE INDEX IF NOT EXISTS idx_usage_holds_tenant_agent_created ON usage_holds(tenant_id, agent_id, created_at);
	`)
	return err
}

// UpsertUsageHold inserts or replaces the open hold for a run.
func (s *SQLiteStore) UpsertUsageHold(ctx context.Context, h *models.UsageHold) error {
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
		ON CONFLICT(run_id) DO UPDATE SET
			tenant_id=excluded.tenant_id,
			agent_id=excluded.agent_id,
			usd_hold=excluded.usd_hold,
			tokens_hold=excluded.tokens_hold,
			created_at=excluded.created_at
	`, h.RunID, tenantID, h.AgentID, h.USDHold, h.TokensHold, formatTS(ts))
	return err
}

// ReleaseUsageHold deletes the open hold for a run (no-op if missing).
func (s *SQLiteStore) ReleaseUsageHold(ctx context.Context, runID string) error {
	if runID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM usage_holds WHERE run_id = ?`, runID)
	return err
}

// SumUsageHolds totals open holds for tenant (and optional agent) in [since, until).
func (s *SQLiteStore) SumUsageHolds(ctx context.Context, tenantID, agentID string, since, until time.Time) (usd float64, tokens int64, count int64, err error) {
	if tenantID == "" {
		tenantID = tenant.FromContext(ctx)
	}
	q := `SELECT COALESCE(SUM(usd_hold),0), COALESCE(SUM(tokens_hold),0), COUNT(*)
		FROM usage_holds WHERE tenant_id = ? AND created_at >= ? AND created_at < ?`
	args := []interface{}{tenantID, formatTS(since), formatTS(until)}
	if agentID != "" {
		q += ` AND agent_id = ?`
		args = append(args, agentID)
	}
	err = s.db.QueryRowContext(ctx, q, args...).Scan(&usd, &tokens, &count)
	return
}
