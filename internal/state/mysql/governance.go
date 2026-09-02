package mysql

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
	"github.com/getrunkite/runkite/internal/state/migrate"
	"github.com/getrunkite/runkite/internal/tenant"
)

func (s *Store) upAuditEvents(ctx context.Context, db migrate.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS audit_events (
			id            VARCHAR(255) PRIMARY KEY,
			ts            DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			tenant_id     VARCHAR(255) NOT NULL DEFAULT 'default',
			actor         TEXT,
			action        VARCHAR(255) NOT NULL,
			resource_type VARCHAR(255) DEFAULT '',
			resource_id   VARCHAR(255) DEFAULT '',
			decision      VARCHAR(64) NOT NULL,
			reason_code   VARCHAR(255) DEFAULT '',
			rule_id       VARCHAR(255) DEFAULT '',
			latency_ms    INT NOT NULL DEFAULT 0,
			run_id        VARCHAR(255) DEFAULT '',
			generation    BIGINT NOT NULL DEFAULT 0,
			agent_id      VARCHAR(255) DEFAULT '',
			connector     VARCHAR(255) DEFAULT '',
			tool          VARCHAR(255) DEFAULT '',
			attrs         JSON,
			trace_id      VARCHAR(255) DEFAULT '',
			KEY idx_audit_events_tenant_ts (tenant_id, ts),
			KEY idx_audit_events_run (run_id)
		) ENGINE=InnoDB CHARSET=utf8mb4`)
	return err
}

func (s *Store) upPolicyGrants(ctx context.Context, db migrate.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS policy_grants (
			id         VARCHAR(255) PRIMARY KEY,
			tenant_id  VARCHAR(255) NOT NULL,
			agent_id   VARCHAR(255) NOT NULL,
			connector  VARCHAR(255) NOT NULL,
			tools      JSON,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			UNIQUE KEY uq_policy_grants_tac (tenant_id, agent_id, connector),
			KEY idx_policy_grants_tenant (tenant_id)
		) ENGINE=InnoDB CHARSET=utf8mb4`)
	return err
}

func (s *Store) upPendingActions(ctx context.Context, db migrate.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS pending_actions (
			id          VARCHAR(255) PRIMARY KEY,
			run_id      VARCHAR(255) NOT NULL,
			generation  BIGINT NOT NULL DEFAULT 0,
			tenant_id   VARCHAR(255) NOT NULL,
			agent_id    VARCHAR(255) NOT NULL DEFAULT '',
			connector   VARCHAR(255) NOT NULL,
			tool        VARCHAR(255) NOT NULL DEFAULT '',
			rule_id     VARCHAR(255) NOT NULL DEFAULT '',
			reason      TEXT,
			reason_code VARCHAR(255) NOT NULL DEFAULT '',
			status      VARCHAR(64) NOT NULL DEFAULT 'pending',
			created_at  DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			updated_at  DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			KEY idx_pending_actions_status_ts (status, created_at),
			KEY idx_pending_actions_run (run_id)
		) ENGINE=InnoDB CHARSET=utf8mb4`)
	return err
}

// WriteAuditEvent persists one policy/security decision.
func (s *Store) WriteAuditEvent(ctx context.Context, ev *models.AuditEvent) error {
	if ev == nil {
		return nil
	}
	attrs, _ := json.Marshal(ev.Attrs)
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
	`, ev.ID, ts, tenantID, ev.Actor, ev.Action, ev.ResourceType, ev.ResourceID,
		ev.Decision, ev.ReasonCode, ev.RuleID, ev.LatencyMs,
		ev.RunID, ev.Generation, ev.AgentID, ev.Connector, ev.Tool, nullableJSON(attrs), ev.TraceID)
	return err
}

// SearchAuditEvents lists policy decisions newest-first.
func (s *Store) SearchAuditEvents(ctx context.Context, req *models.AuditSearchRequest) ([]*models.AuditEvent, error) {
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
	if req.ReasonCode != "" {
		where = append(where, "reason_code = ?")
		args = append(args, req.ReasonCode)
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
		args = append(args, req.Since.UTC())
	}
	if req.Until != nil {
		where = append(where, "ts < ?")
		args = append(args, req.Until.UTC())
	}
	tc, err := pagecursor.DecodeTime(req.Cursor)
	if err != nil {
		return nil, err
	}
	if tc.ID != "" {
		where = append(where, "(ts < ? OR (ts = ? AND id < ?))")
		args = append(args, tc.Time, tc.Time, tc.ID)
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
		var attrsRaw []byte
		var actor, resType, resID, reason, ruleID, runID, agent, connector, tool, trace sql.NullString
		if err := rows.Scan(
			&ev.ID, &ev.TS, &ev.TenantID, &actor, &ev.Action, &resType, &resID,
			&ev.Decision, &reason, &ruleID, &ev.LatencyMs,
			&runID, &ev.Generation, &agent, &connector, &tool, &attrsRaw, &trace,
		); err != nil {
			return nil, err
		}
		ev.Actor, ev.ResourceType, ev.ResourceID = actor.String, resType.String, resID.String
		ev.ReasonCode, ev.RuleID = reason.String, ruleID.String
		ev.RunID, ev.AgentID, ev.Connector, ev.Tool, ev.TraceID = runID.String, agent.String, connector.String, tool.String, trace.String
		if len(attrsRaw) > 0 {
			_ = json.Unmarshal(attrsRaw, &ev.Attrs)
		}
		events = append(events, &ev)
	}
	return events, rows.Err()
}

// ListPolicyGrants returns every durable grant.
func (s *Store) ListPolicyGrants(ctx context.Context) ([]*models.PolicyGrant, error) {
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
func (s *Store) SearchPolicyGrants(ctx context.Context, req *models.PolicyGrantSearchRequest) ([]*models.PolicyGrant, error) {
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

// UpsertPolicyGrant inserts or replaces by id. MySQL cannot scope
// ON DUPLICATE KEY UPDATE to only the primary key, so update-by-id then
// insert maps a (tenant,agent,connector) collision to ErrConflict.
func (s *Store) UpsertPolicyGrant(ctx context.Context, g *models.PolicyGrant) error {
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
	res, err := s.db.ExecContext(ctx, `
		UPDATE policy_grants SET tenant_id = ?, agent_id = ?, connector = ?, tools = ?, updated_at = ?
		WHERE id = ?`,
		g.TenantID, g.AgentID, g.Connector, nullableJSON(tools), g.UpdatedAt, g.ID)
	if err != nil {
		if isDuplicateKeyError(err) {
			return policyGrantConflict(g)
		}
		return err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		return nil
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO policy_grants (id, tenant_id, agent_id, connector, tools, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?)`,
		g.ID, g.TenantID, g.AgentID, g.Connector, nullableJSON(tools), g.CreatedAt, g.UpdatedAt)
	if isDuplicateKeyError(err) {
		return policyGrantConflict(g)
	}
	return err
}

func policyGrantConflict(g *models.PolicyGrant) error {
	return &state.ErrConflict{
		Resource: "policy_grant",
		ID:       g.TenantID + "/" + g.AgentID + "/" + g.Connector,
		Reason:   "already exists for tenant/agent/connector (use PUT on the existing id)",
	}
}

// GetPolicyGrant returns one grant by id.
func (s *Store) GetPolicyGrant(ctx context.Context, id string) (*models.PolicyGrant, error) {
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
func (s *Store) DeletePolicyGrant(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM policy_grants WHERE id = ?`, id)
	return err
}

type govScanner interface {
	Scan(dest ...any) error
}

func scanPolicyGrant(row govScanner) (*models.PolicyGrant, error) {
	var g models.PolicyGrant
	var toolsRaw []byte
	if err := row.Scan(&g.ID, &g.TenantID, &g.AgentID, &g.Connector, &toolsRaw, &g.CreatedAt, &g.UpdatedAt); err != nil {
		return nil, err
	}
	if len(toolsRaw) > 0 && string(toolsRaw) != "{}" && string(toolsRaw) != "null" {
		var tf models.PolicyToolFilter
		if err := json.Unmarshal(toolsRaw, &tf); err == nil {
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
func (s *Store) CreatePendingAction(ctx context.Context, a *models.PendingAction) error {
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
		a.RuleID, a.Reason, a.ReasonCode, a.Status, a.CreatedAt, a.UpdatedAt)
	return err
}

// GetPendingAction returns one row by id.
func (s *Store) GetPendingAction(ctx context.Context, id string) (*models.PendingAction, error) {
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
func (s *Store) SearchPendingActions(ctx context.Context, req *models.PendingActionSearchRequest) ([]*models.PendingAction, error) {
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
		args = append(args, tc.Time, tc.Time, tc.ID)
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
func (s *Store) SetPendingActionStatus(ctx context.Context, id, fromStatus, toStatus string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE pending_actions SET status = ?, updated_at = ?
		WHERE id = ? AND status = ?`,
		toStatus, time.Now().UTC(), id, fromStatus)
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
func (s *Store) FindOpenPendingAction(ctx context.Context, runID string, generation int64, connector, tool string) (*models.PendingAction, error) {
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
func (s *Store) ConsumeApprovedAction(ctx context.Context, runID string, generation int64, connector, tool string) (string, error) {
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
		ORDER BY created_at ASC LIMIT 1 FOR UPDATE`,
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
		models.PendingStatusConsumed, time.Now().UTC(), id, models.PendingStatusApproved)
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
	var reason sql.NullString
	if err := row.Scan(
		&a.ID, &a.RunID, &a.Generation, &a.TenantID, &a.AgentID, &a.Connector, &a.Tool,
		&a.RuleID, &reason, &a.ReasonCode, &a.Status, &a.CreatedAt, &a.UpdatedAt,
	); err != nil {
		return nil, err
	}
	a.Reason = reason.String
	return &a, nil
}
