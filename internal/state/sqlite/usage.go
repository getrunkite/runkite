package sqlite

import (
	"context"
	"strings"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
)

func (s *SQLiteStore) upUsageEvents(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS usage_events (
			id           TEXT PRIMARY KEY,
			ts           TEXT NOT NULL,
			tenant_id    TEXT NOT NULL DEFAULT 'default',
			run_id       TEXT NOT NULL DEFAULT '',
			agent_id     TEXT NOT NULL DEFAULT '',
			principal    TEXT NOT NULL DEFAULT '',
			model        TEXT NOT NULL DEFAULT '',
			tokens_in    INTEGER NOT NULL DEFAULT 0,
			tokens_out   INTEGER NOT NULL DEFAULT 0,
			usd_estimate REAL NOT NULL DEFAULT 0,
			source       TEXT NOT NULL DEFAULT '',
			UNIQUE (run_id, source)
		);
		CREATE INDEX IF NOT EXISTS idx_usage_events_tenant_ts ON usage_events(tenant_id, ts);
		CREATE INDEX IF NOT EXISTS idx_usage_events_tenant_agent_ts ON usage_events(tenant_id, agent_id, ts);
		CREATE INDEX IF NOT EXISTS idx_usage_events_run ON usage_events(run_id);
	`)
	return err
}

// WriteUsageEvent upserts one usage row keyed by (run_id, source).
func (s *SQLiteStore) WriteUsageEvent(ctx context.Context, ev *models.UsageEvent) error {
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
		ON CONFLICT(run_id, source) DO UPDATE SET
			ts = excluded.ts,
			tenant_id = excluded.tenant_id,
			agent_id = excluded.agent_id,
			principal = excluded.principal,
			model = excluded.model,
			tokens_in = excluded.tokens_in,
			tokens_out = excluded.tokens_out,
			usd_estimate = excluded.usd_estimate
	`, ev.ID, formatTS(ts), tenantID, ev.RunID, ev.AgentID, ev.Principal, ev.Model,
		ev.TokensIn, ev.TokensOut, ev.USDEstimate, source)
	return err
}

// SumUsage aggregates tokens and USD for a tenant (and optional agent) in [since, until).
func (s *SQLiteStore) SumUsage(ctx context.Context, tenantID, agentID string, since, until time.Time) (tokensIn, tokensOut int64, usd float64, err error) {
	query := `SELECT COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0), COALESCE(SUM(usd_estimate),0)
		FROM usage_events WHERE tenant_id = ? AND ts >= ? AND ts < ?`
	args := []interface{}{tenantID, formatTS(since.UTC()), formatTS(until.UTC())}
	if agentID != "" {
		query += ` AND agent_id = ?`
		args = append(args, agentID)
	}
	err = s.db.QueryRowContext(ctx, query, args...).Scan(&tokensIn, &tokensOut, &usd)
	return tokensIn, tokensOut, usd, err
}

// CountRunsSince counts runs created at/after since for tenant (optional agent).
func (s *SQLiteStore) CountRunsSince(ctx context.Context, tenantID, agentID string, since time.Time) (int64, error) {
	query := `SELECT COUNT(*) FROM runs WHERE tenant_id = ? AND created_at >= ?`
	args := []interface{}{tenantID, since.UTC().Format(time.RFC3339)}
	if agentID != "" {
		query += ` AND agent_id = ?`
		args = append(args, agentID)
	}
	var n int64
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&n)
	return n, err
}

// SearchUsageSummary groups usage_events by tenant/agent/UTC day.
func (s *SQLiteStore) SearchUsageSummary(ctx context.Context, req *models.UsageSummaryRequest) ([]models.UsageSummaryRow, error) {
	if req == nil {
		req = &models.UsageSummaryRequest{}
	}
	// substr(ts,1,10) works for RFC3339Nano UTC strings written by formatTS.
	query := `SELECT substr(ts, 1, 10) AS day, tenant_id, agent_id,
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
		args = append(args, formatTS(req.From.UTC()))
	}
	if req.To != nil {
		where = append(where, "ts < ?")
		args = append(args, formatTS(req.To.UTC()))
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
