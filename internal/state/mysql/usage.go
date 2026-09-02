package mysql

import (
	"context"
	"strings"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state/migrate"
	"github.com/getrunkite/runkite/internal/tenant"
)

func (s *Store) upUsageEvents(ctx context.Context, db migrate.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS usage_events (
			id           VARCHAR(255) PRIMARY KEY,
			ts           DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			tenant_id    VARCHAR(255) NOT NULL DEFAULT 'default',
			run_id       VARCHAR(255) NOT NULL DEFAULT '',
			agent_id     VARCHAR(255) NOT NULL DEFAULT '',
			principal    VARCHAR(255) NOT NULL DEFAULT '',
			model        VARCHAR(255) NOT NULL DEFAULT '',
			tokens_in    BIGINT NOT NULL DEFAULT 0,
			tokens_out   BIGINT NOT NULL DEFAULT 0,
			usd_estimate DOUBLE NOT NULL DEFAULT 0,
			source       VARCHAR(64) NOT NULL DEFAULT '',
			UNIQUE KEY uq_usage_events_run_source (run_id, source),
			KEY idx_usage_events_tenant_ts (tenant_id, ts),
			KEY idx_usage_events_tenant_agent_ts (tenant_id, agent_id, ts),
			KEY idx_usage_events_run (run_id)
		) ENGINE=InnoDB CHARSET=utf8mb4`)
	return err
}

// WriteUsageEvent upserts one usage row keyed by (run_id, source).
func (s *Store) WriteUsageEvent(ctx context.Context, ev *models.UsageEvent) error {
	if ev == nil {
		return nil
	}
	tenantID := ev.TenantID
	if tenantID == "" {
		tenantID = tenant.FromContext(ctx)
	}
	ts := ev.TS
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	source := ev.Source
	if source == "" {
		source = models.UsageSourceTerminalOutput
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO usage_events (
			id, ts, tenant_id, run_id, agent_id, principal, model,
			tokens_in, tokens_out, usd_estimate, source
		) VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE
			ts = VALUES(ts),
			tenant_id = VALUES(tenant_id),
			agent_id = VALUES(agent_id),
			principal = VALUES(principal),
			model = VALUES(model),
			tokens_in = VALUES(tokens_in),
			tokens_out = VALUES(tokens_out),
			usd_estimate = VALUES(usd_estimate)
	`, ev.ID, ts, tenantID, ev.RunID, ev.AgentID, ev.Principal, ev.Model,
		ev.TokensIn, ev.TokensOut, ev.USDEstimate, source)
	return err
}

// SumUsage aggregates tokens and USD for a tenant (and optional agent) in [since, until).
func (s *Store) SumUsage(ctx context.Context, tenantID, agentID string, since, until time.Time) (tokensIn, tokensOut int64, usd float64, err error) {
	query := `SELECT COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0), COALESCE(SUM(usd_estimate),0)
		FROM usage_events WHERE tenant_id = ? AND ts >= ? AND ts < ?`
	args := []interface{}{tenantID, since.UTC(), until.UTC()}
	if agentID != "" {
		query += ` AND agent_id = ?`
		args = append(args, agentID)
	}
	err = s.db.QueryRowContext(ctx, query, args...).Scan(&tokensIn, &tokensOut, &usd)
	return tokensIn, tokensOut, usd, err
}

// CountRunsSince counts runs created at/after since for tenant (optional agent).
func (s *Store) CountRunsSince(ctx context.Context, tenantID, agentID string, since time.Time) (int64, error) {
	query := `SELECT COUNT(*) FROM runs WHERE tenant_id = ? AND created_at >= ?`
	args := []interface{}{tenantID, since.UTC()}
	if agentID != "" {
		query += ` AND agent_id = ?`
		args = append(args, agentID)
	}
	var n int64
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&n)
	return n, err
}

// SearchUsageSummary groups usage_events by tenant/agent/UTC day.
func (s *Store) SearchUsageSummary(ctx context.Context, req *models.UsageSummaryRequest) ([]models.UsageSummaryRow, error) {
	if req == nil {
		req = &models.UsageSummaryRequest{}
	}
	query := `SELECT DATE_FORMAT(ts, '%Y-%m-%d') AS day, tenant_id, agent_id,
		COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0),
		COALESCE(SUM(usd_estimate),0), COUNT(DISTINCT run_id)
		FROM usage_events`
	var args []interface{}
	var where []string
	if !tenant.IsSystem(ctx) {
		where = append(where, "tenant_id = ?")
		args = append(args, tenant.FromContext(ctx))
	} else if req.TenantID != "" {
		where = append(where, "tenant_id = ?")
		args = append(args, req.TenantID)
	}
	if req.AgentID != "" {
		where = append(where, "agent_id = ?")
		args = append(args, req.AgentID)
	}
	if req.From != nil {
		where = append(where, "ts >= ?")
		args = append(args, req.From.UTC())
	}
	if req.To != nil {
		where = append(where, "ts < ?")
		args = append(args, req.To.UTC())
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += ` GROUP BY day, tenant_id, agent_id ORDER BY day DESC, tenant_id ASC, agent_id ASC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.UsageSummaryRow{}
	for rows.Next() {
		var row models.UsageSummaryRow
		if err := rows.Scan(&row.Day, &row.TenantID, &row.AgentID,
			&row.TokensIn, &row.TokensOut, &row.USDEstimate, &row.RunCount); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
