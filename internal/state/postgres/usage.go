package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
)

func (s *Store) upUsageEvents(ctx context.Context, conn *pgxpool.Conn) error {
	_, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS usage_events (
			id           TEXT PRIMARY KEY,
			ts           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			tenant_id    TEXT NOT NULL DEFAULT 'default',
			run_id       TEXT NOT NULL DEFAULT '',
			agent_id     TEXT NOT NULL DEFAULT '',
			principal    TEXT NOT NULL DEFAULT '',
			model        TEXT NOT NULL DEFAULT '',
			tokens_in    BIGINT NOT NULL DEFAULT 0,
			tokens_out   BIGINT NOT NULL DEFAULT 0,
			usd_estimate DOUBLE PRECISION NOT NULL DEFAULT 0,
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
	_, err := s.pool.Exec(ctx, `
		INSERT INTO usage_events (
			id, ts, tenant_id, run_id, agent_id, principal, model,
			tokens_in, tokens_out, usd_estimate, source
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (run_id, source) DO UPDATE SET
			ts = EXCLUDED.ts,
			tenant_id = EXCLUDED.tenant_id,
			agent_id = EXCLUDED.agent_id,
			principal = EXCLUDED.principal,
			model = EXCLUDED.model,
			tokens_in = EXCLUDED.tokens_in,
			tokens_out = EXCLUDED.tokens_out,
			usd_estimate = EXCLUDED.usd_estimate
	`, ev.ID, ts, tenantID, ev.RunID, ev.AgentID, ev.Principal, ev.Model,
		ev.TokensIn, ev.TokensOut, ev.USDEstimate, source)
	return err
}

// SumUsage aggregates tokens and USD for a tenant (and optional agent) in [since, until).
func (s *Store) SumUsage(ctx context.Context, tenantID, agentID string, since, until time.Time) (tokensIn, tokensOut int64, usd float64, err error) {
	query := `SELECT COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0), COALESCE(SUM(usd_estimate),0)
		FROM usage_events WHERE tenant_id = $1 AND ts >= $2 AND ts < $3`
	args := []interface{}{tenantID, since.UTC(), until.UTC()}
	if agentID != "" {
		query += ` AND agent_id = $4`
		args = append(args, agentID)
	}
	err = s.pool.QueryRow(ctx, query, args...).Scan(&tokensIn, &tokensOut, &usd)
	return tokensIn, tokensOut, usd, err
}

// CountRunsSince counts runs created at/after since for tenant (optional agent).
func (s *Store) CountRunsSince(ctx context.Context, tenantID, agentID string, since time.Time) (int64, error) {
	query := `SELECT COUNT(*) FROM runs WHERE tenant_id = $1 AND created_at >= $2`
	args := []interface{}{tenantID, since.UTC()}
	if agentID != "" {
		query += ` AND agent_id = $3`
		args = append(args, agentID)
	}
	var n int64
	err := s.pool.QueryRow(ctx, query, args...).Scan(&n)
	return n, err
}

// SearchUsageSummary groups usage_events by tenant/agent/UTC day.
func (s *Store) SearchUsageSummary(ctx context.Context, req *models.UsageSummaryRequest) ([]models.UsageSummaryRow, error) {
	if req == nil {
		req = &models.UsageSummaryRequest{}
	}
	query := `SELECT to_char(ts AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS day, tenant_id, agent_id,
		COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0),
		COALESCE(SUM(usd_estimate),0), COUNT(DISTINCT run_id)
		FROM usage_events`
	var args []interface{}
	var where []string
	argN := 1
	if !tenant.IsSystem(ctx) {
		where = append(where, fmt.Sprintf("tenant_id = $%d", argN))
		args = append(args, tenant.FromContext(ctx))
		argN++
	} else if req.TenantID != "" {
		where = append(where, fmt.Sprintf("tenant_id = $%d", argN))
		args = append(args, req.TenantID)
		argN++
	}
	if req.AgentID != "" {
		where = append(where, fmt.Sprintf("agent_id = $%d", argN))
		args = append(args, req.AgentID)
		argN++
	}
	if req.From != nil {
		where = append(where, fmt.Sprintf("ts >= $%d", argN))
		args = append(args, req.From.UTC())
		argN++
	}
	if req.To != nil {
		where = append(where, fmt.Sprintf("ts < $%d", argN))
		args = append(args, req.To.UTC())
		argN++
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += ` GROUP BY day, tenant_id, agent_id ORDER BY day DESC, tenant_id ASC, agent_id ASC`
	rows, err := s.pool.Query(ctx, query, args...)
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
