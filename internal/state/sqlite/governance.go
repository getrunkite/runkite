package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/pagecursor"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

func (s *SQLiteStore) upAuditEvents(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS audit_events (
			id            TEXT PRIMARY KEY,
			ts            TEXT NOT NULL,
			tenant_id     TEXT NOT NULL DEFAULT 'default',
			actor         TEXT DEFAULT '',
			action        TEXT NOT NULL,
			resource_type TEXT DEFAULT '',
			resource_id   TEXT DEFAULT '',
			decision      TEXT NOT NULL,
			reason_code   TEXT DEFAULT '',
			rule_id       TEXT DEFAULT '',
			latency_ms    INTEGER NOT NULL DEFAULT 0,
			run_id        TEXT DEFAULT '',
			generation    INTEGER NOT NULL DEFAULT 0,
			agent_id      TEXT DEFAULT '',
			connector     TEXT DEFAULT '',
			tool          TEXT DEFAULT '',
			attrs         TEXT NOT NULL DEFAULT '{}',
			trace_id      TEXT DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_audit_events_tenant_ts ON audit_events(tenant_id, ts DESC);
		CREATE INDEX IF NOT EXISTS idx_audit_events_run ON audit_events(run_id);
	`)
	return err
}

func (s *SQLiteStore) upPolicyGrants(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS policy_grants (
			id         TEXT PRIMARY KEY,
			tenant_id  TEXT NOT NULL,
			agent_id   TEXT NOT NULL,
			connector  TEXT NOT NULL,
			tools      TEXT NOT NULL DEFAULT '{}',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			UNIQUE (tenant_id, agent_id, connector)
		);
		CREATE INDEX IF NOT EXISTS idx_policy_grants_tenant ON policy_grants(tenant_id);
	`)
	return err
}

func (s *SQLiteStore) upPendingActions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS pending_actions (
			id          TEXT PRIMARY KEY,
			run_id      TEXT NOT NULL,
			generation  INTEGER NOT NULL DEFAULT 0,
			tenant_id   TEXT NOT NULL,
			agent_id    TEXT NOT NULL DEFAULT '',
			connector   TEXT NOT NULL,
			tool        TEXT NOT NULL DEFAULT '',
			rule_id     TEXT NOT NULL DEFAULT '',
			reason      TEXT NOT NULL DEFAULT '',
			reason_code TEXT NOT NULL DEFAULT '',
			status      TEXT NOT NULL DEFAULT 'pending',
			created_at  TEXT NOT NULL,
			updated_at  TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_pending_actions_status_ts ON pending_actions(status, created_at DESC);
		CREATE INDEX IF NOT EXISTS idx_pending_actions_run ON pending_actions(run_id);
	`)
	return err
}

func formatTS(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTS(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

// WriteAuditEvent persists one policy/security decision.
func (s *SQLiteStore) WriteAuditEvent(ctx context.Context, ev *models.AuditEvent) error {
	if ev == nil {
		return nil
	}
	attrs, _ := json.Marshal(ev.Attrs)
	if attrs == nil {
		attrs = []byte("{}")
	}
	tenantID := ev.TenantID
	if tenantID == "" {
		tenantID = tenant.FromContext(ctx)
	}
	ts := ev.TS
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO audit_events (
			id, ts, tenant_id, actor, action, resource_type, resource_id,
			decision, reason_code, rule_id, latency_ms,
			run_id, generation, agent_id, connector, tool, attrs, trace_id
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	`, ev.ID, formatTS(ts), tenantID, ev.Actor, ev.Action, ev.ResourceType, ev.ResourceID,
		ev.Decision, ev.ReasonCode, ev.RuleID, ev.LatencyMs,
		ev.RunID, ev.Generation, ev.AgentID, ev.Connector, ev.Tool, string(attrs), ev.TraceID)
	return err
}

// SearchAuditEvents lists policy decisions newest-first.
func (s *SQLiteStore) SearchAuditEvents(ctx context.Context, req *models.AuditSearchRequest) ([]*models.AuditEvent, error) {
	if req == nil {
		req = &models.AuditSearchRequest{}
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	query := `SELECT id, ts, tenant_id, actor, action, resource_type, resource_id,
		decision, reason_code, rule_id, latency_ms,
		run_id, generation, agent_id, connector, tool, attrs, trace_id
		FROM audit_events`
	var args []interface{}
	var where []string
	if !tenant.IsSystem(ctx) {
		where = append(where, "tenant_id = ?")
		args = append(args, tenant.FromContext(ctx))
	} else if req.TenantID != "" {
		where = append(where, "tenant_id = ?")
		args = append(args, req.TenantID)
	}
	if req.Decision != "" {
		where = append(where, "decision = ?")
		args = append(args, req.Decision)
	}
	if req.Action != "" {
		where = append(where, "action = ?")
		args = append(args, req.Action)
	}
	if req.RunID != "" {
		where = append(where, "run_id = ?")
		args = append(args, req.RunID)
	}
	if req.AgentID != "" {
		where = append(where, "agent_id = ?")
		args = append(args, req.AgentID)
	}
	if req.Connector != "" {
		where = append(where, "connector = ?")
		args = append(args, req.Connector)
	}
	if req.Tool != "" {
		where = append(where, "tool = ?")
		args = append(args, req.Tool)
	}
	if req.Since != nil {
		where = append(where, "ts >= ?")
		args = append(args, formatTS(*req.Since))
	}
	if req.Until != nil {
		where = append(where, "ts < ?")
		args = append(args, formatTS(*req.Until))
	}
	tc, err := pagecursor.DecodeTime(req.Cursor)
	if err != nil {
		return nil, err
	}
	if tc.ID != "" {
		where = append(where, "(ts < ? OR (ts = ? AND id < ?))")
		args = append(args, formatTS(tc.Time), formatTS(tc.Time), tc.ID)
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	if tc.ID != "" {
		query += " ORDER BY ts DESC, id DESC LIMIT ?"
		args = append(args, limit)
	} else {
		query += " ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?"
		args = append(args, limit, req.Offset)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []*models.AuditEvent{}
	for rows.Next() {
		var ev models.AuditEvent
		var tsRaw, attrsRaw string
		if err := rows.Scan(
			&ev.ID, &tsRaw, &ev.TenantID, &ev.Actor, &ev.Action, &ev.ResourceType, &ev.ResourceID,
			&ev.Decision, &ev.ReasonCode, &ev.RuleID, &ev.LatencyMs,
			&ev.RunID, &ev.Generation, &ev.AgentID, &ev.Connector, &ev.Tool, &attrsRaw, &ev.TraceID,
		); err != nil {
			return nil, err
		}
		if t, err := parseTS(tsRaw); err == nil {
			ev.TS = t
		}
		if attrsRaw != "" {
			_ = json.Unmarshal([]byte(attrsRaw), &ev.Attrs)
		}
		events = append(events, &ev)
	}
	return events, rows.Err()
}

// ListPolicyGrants returns every durable grant.
func (s *SQLiteStore) ListPolicyGrants(ctx context.Context) ([]*models.PolicyGrant, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, tenant_id, agent_id, connector, tools, created_at, updated_at
		FROM policy_grants ORDER BY tenant_id ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPolicyGrants(rows)
}

// SearchPolicyGrants lists grants with optional filters and keyset paging.
func (s *SQLiteStore) SearchPolicyGrants(ctx context.Context, req *models.PolicyGrantSearchRequest) ([]*models.PolicyGrant, error) {
	if req == nil {
		req = &models.PolicyGrantSearchRequest{}
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	query := `SELECT id, tenant_id, agent_id, connector, tools, created_at, updated_at FROM policy_grants`
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
	if req.Connector != "" {
		where = append(where, "connector = ?")
		args = append(args, req.Connector)
	}
	kc, err := pagecursor.DecodeKey(req.Cursor)
	if err != nil {
		return nil, err
	}
	if kc.ID != "" {
		where = append(where, "(tenant_id > ? OR (tenant_id = ? AND id > ?))")
		args = append(args, kc.Key, kc.Key, kc.ID)
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	if kc.ID != "" {
		query += " ORDER BY tenant_id ASC, id ASC LIMIT ?"
		args = append(args, limit)
	} else {
		query += " ORDER BY tenant_id ASC, id ASC LIMIT ? OFFSET ?"
		args = append(args, limit, req.Offset)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPolicyGrants(rows)
}

// UpsertPolicyGrant inserts or replaces a grant by id.
func (s *SQLiteStore) UpsertPolicyGrant(ctx context.Context, g *models.PolicyGrant) error {
	if g == nil {
		return nil
	}
	tools, _ := json.Marshal(g.Tools)
	if tools == nil || string(tools) == "null" {
		tools = []byte("{}")
	}
	now := time.Now().UTC()
	if g.CreatedAt.IsZero() {
		g.CreatedAt = now
	}
	g.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO policy_grants (id, tenant_id, agent_id, connector, tools, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			tenant_id = excluded.tenant_id,
			agent_id = excluded.agent_id,
			connector = excluded.connector,
			tools = excluded.tools,
			updated_at = excluded.updated_at`,
		g.ID, g.TenantID, g.AgentID, g.Connector, string(tools), formatTS(g.CreatedAt), formatTS(g.UpdatedAt))
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint") {
		return &state.ErrConflict{
			Resource: "policy_grant",
			ID:       g.TenantID + "/" + g.AgentID + "/" + g.Connector,
			Reason:   "already exists for tenant/agent/connector (use PUT on the existing id)",
		}
	}
	return err
}

// GetPolicyGrant returns one grant by id.
func (s *SQLiteStore) GetPolicyGrant(ctx context.Context, id string) (*models.PolicyGrant, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, agent_id, connector, tools, created_at, updated_at
		FROM policy_grants WHERE id = ?`, id)
	g, err := scanPolicyGrant(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &state.ErrNotFound{Resource: "policy_grant", ID: id}
		}
		return nil, err
	}
	return g, nil
}

// DeletePolicyGrant removes a grant by id.
func (s *SQLiteStore) DeletePolicyGrant(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM policy_grants WHERE id = ?`, id)
	return err
}

type govScanner interface {
	Scan(dest ...any) error
}

func scanPolicyGrant(row govScanner) (*models.PolicyGrant, error) {
	var g models.PolicyGrant
	var toolsRaw, createdRaw, updatedRaw string
	if err := row.Scan(&g.ID, &g.TenantID, &g.AgentID, &g.Connector, &toolsRaw, &createdRaw, &updatedRaw); err != nil {
		return nil, err
	}
	if t, err := parseTS(createdRaw); err == nil {
		g.CreatedAt = t
	}
	if t, err := parseTS(updatedRaw); err == nil {
		g.UpdatedAt = t
	}
	if toolsRaw != "" && toolsRaw != "{}" && toolsRaw != "null" {
		var tf models.PolicyToolFilter
		if err := json.Unmarshal([]byte(toolsRaw), &tf); err == nil {
			if len(tf.Allow) > 0 || len(tf.Deny) > 0 {
				g.Tools = &tf
			}
		}
	}
	return &g, nil
}

func scanPolicyGrants(rows *sql.Rows) ([]*models.PolicyGrant, error) {
	out := []*models.PolicyGrant{}
	for rows.Next() {
		g, err := scanPolicyGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// CreatePendingAction inserts a new pending HITL row.
func (s *SQLiteStore) CreatePendingAction(ctx context.Context, a *models.PendingAction) error {
	if a == nil {
		return nil
	}
	now := time.Now().UTC()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	if a.Status == "" {
		a.Status = models.PendingStatusPending
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO pending_actions (
			id, run_id, generation, tenant_id, agent_id, connector, tool,
			rule_id, reason, reason_code, status, created_at, updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.RunID, a.Generation, a.TenantID, a.AgentID, a.Connector, a.Tool,
		a.RuleID, a.Reason, a.ReasonCode, a.Status, formatTS(a.CreatedAt), formatTS(a.UpdatedAt))
	return err
}

// GetPendingAction returns one row by id.
func (s *SQLiteStore) GetPendingAction(ctx context.Context, id string) (*models.PendingAction, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, run_id, generation, tenant_id, agent_id, connector, tool,
		       rule_id, reason, reason_code, status, created_at, updated_at
		FROM pending_actions WHERE id = ?`, id)
	a, err := scanPendingAction(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &state.ErrNotFound{Resource: "pending_action", ID: id}
		}
		return nil, err
	}
	return a, nil
}

// SearchPendingActions lists pending actions with optional filters.
func (s *SQLiteStore) SearchPendingActions(ctx context.Context, req *models.PendingActionSearchRequest) ([]*models.PendingAction, error) {
	if req == nil {
		req = &models.PendingActionSearchRequest{}
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	query := `SELECT id, run_id, generation, tenant_id, agent_id, connector, tool,
		rule_id, reason, reason_code, status, created_at, updated_at FROM pending_actions`
	var args []interface{}
	var where []string
	if !tenant.IsSystem(ctx) {
		where = append(where, "tenant_id = ?")
		args = append(args, tenant.FromContext(ctx))
	} else if req.TenantID != "" {
		where = append(where, "tenant_id = ?")
		args = append(args, req.TenantID)
	}
	if req.Status != "" {
		where = append(where, "status = ?")
		args = append(args, req.Status)
	}
	if req.RunID != "" {
		where = append(where, "run_id = ?")
		args = append(args, req.RunID)
	}
	if req.Connector != "" {
		where = append(where, "connector = ?")
		args = append(args, req.Connector)
	}
	tc, err := pagecursor.DecodeTime(req.Cursor)
	if err != nil {
		return nil, err
	}
	if tc.ID != "" {
		where = append(where, "(created_at < ? OR (created_at = ? AND id < ?))")
		args = append(args, formatTS(tc.Time), formatTS(tc.Time), tc.ID)
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	if tc.ID != "" {
		query += " ORDER BY created_at DESC, id DESC LIMIT ?"
		args = append(args, limit)
	} else {
		query += " ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?"
		args = append(args, limit, req.Offset)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*models.PendingAction{}
	for rows.Next() {
		a, err := scanPendingAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetPendingActionStatus updates status if still in fromStatus.
func (s *SQLiteStore) SetPendingActionStatus(ctx context.Context, id, fromStatus, toStatus string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE pending_actions SET status = ?, updated_at = ?
		WHERE id = ? AND status = ?`,
		toStatus, formatTS(time.Now().UTC()), id, fromStatus)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return &state.ErrConflict{Resource: "pending_action", ID: id, Reason: "status changed"}
	}
	return nil
}

// FindOpenPendingAction returns the oldest still-pending row for a call tuple.
func (s *SQLiteStore) FindOpenPendingAction(ctx context.Context, runID string, generation int64, connector, tool string) (*models.PendingAction, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, run_id, generation, tenant_id, agent_id, connector, tool,
		       rule_id, reason, reason_code, status, created_at, updated_at
		FROM pending_actions
		WHERE run_id = ? AND generation = ? AND connector = ? AND tool = ?
		  AND status = ?
		ORDER BY created_at ASC LIMIT 1`,
		runID, generation, connector, tool, models.PendingStatusPending)
	a, err := scanPendingAction(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return a, nil
}

// ConsumeApprovedAction atomically marks an approved action consumed.
func (s *SQLiteStore) ConsumeApprovedAction(ctx context.Context, runID string, generation int64, connector, tool string) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()

	var id string
	err = tx.QueryRowContext(ctx, `
		SELECT id FROM pending_actions
		WHERE run_id = ? AND generation = ? AND connector = ? AND tool = ?
		  AND status = ?
		ORDER BY created_at ASC LIMIT 1`,
		runID, generation, connector, tool, models.PendingStatusApproved).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE pending_actions SET status = ?, updated_at = ?
		WHERE id = ? AND status = ?`,
		models.PendingStatusConsumed, formatTS(time.Now().UTC()), id, models.PendingStatusApproved)
	if err != nil {
		return "", err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return "", nil
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

func scanPendingAction(row govScanner) (*models.PendingAction, error) {
	var a models.PendingAction
	var createdRaw, updatedRaw string
	if err := row.Scan(
		&a.ID, &a.RunID, &a.Generation, &a.TenantID, &a.AgentID, &a.Connector, &a.Tool,
		&a.RuleID, &a.Reason, &a.ReasonCode, &a.Status, &createdRaw, &updatedRaw,
	); err != nil {
		return nil, err
	}
	if t, err := parseTS(createdRaw); err == nil {
		a.CreatedAt = t
	}
	if t, err := parseTS(updatedRaw); err == nil {
		a.UpdatedAt = t
	}
	return &a, nil
}
