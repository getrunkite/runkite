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

func (s *SQLiteStore) upPendingActionsArgs(ctx context.Context) error {
	stmts := []string{
		`ALTER TABLE pending_actions ADD COLUMN args_digest TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE pending_actions ADD COLUMN args TEXT NOT NULL DEFAULT '{}'`,
		`ALTER TABLE pending_actions ADD COLUMN decided_by TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_pending_actions_consume ON pending_actions(run_id, generation, connector, tool, status, args_digest)`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

func (s *SQLiteStore) upPendingActionsExpiresAt(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `ALTER TABLE pending_actions ADD COLUMN expires_at TEXT`)
	return err
}

const pendingActionSelectCols = `id, run_id, generation, tenant_id, agent_id, connector, tool,
		rule_id, reason, reason_code, status, created_at, updated_at, args_digest, args, decided_by, expires_at, run_incarnation_id, target_revision`

func formatTS(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// formatExpiry uses second precision so SQLite text comparison matches
// chronological order. Nil stays NULL (connector HITL, no sweep).
func formatExpiry(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
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

// CreatePendingAction inserts or reuses a scoped open approval. The canonical
// action ID is written back to a.ID. A nullable unique key arbitrates concurrent
// creates; legacy rows are reused without changing their stored history.
func (s *SQLiteStore) CreatePendingAction(ctx context.Context, a *models.PendingAction) error {
	if a == nil {
		return nil
	}
	if err := s.completionDriver().BindPendingAction(ctx, a); err != nil {
		return err
	}
	now := time.Now().UTC()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	if a.Status == "" {
		a.Status = models.PendingStatusPending
	}
	var openKey any
	if a.Status == models.PendingStatusPending {
		existing, err := s.findOpenPendingAction(ctx, a.TenantID, a.AgentID, a.RunID, a.Generation, a.Connector, a.Tool, a.ArgsDigest, a.ExpiresAt != nil, a.RunIncarnationID, a.TargetRevision)
		if err != nil {
			return err
		}
		if existing != nil {
			a.ID = existing.ID
			return nil
		}
		openKey = models.PendingActionKey(a)
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO pending_actions (
			id, run_id, generation, tenant_id, agent_id, connector, tool,
			rule_id, reason, reason_code, status, created_at, updated_at,
			args_digest, args, decided_by, expires_at, pending_key, run_incarnation_id, target_revision
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT (pending_key) DO NOTHING`,
		a.ID, a.RunID, a.Generation, a.TenantID, a.AgentID, a.Connector, a.Tool,
		a.RuleID, a.Reason, a.ReasonCode, a.Status, formatTS(a.CreatedAt), formatTS(a.UpdatedAt),
		a.ArgsDigest, string(models.MarshalPendingArgs(a.Args)), a.DecidedBy, formatExpiry(a.ExpiresAt), openKey, a.RunIncarnationID, a.TargetRevision)
	if err != nil {
		return err
	}
	if openKey != nil {
		existing, err := s.findOpenPendingAction(ctx, a.TenantID, a.AgentID, a.RunID, a.Generation, a.Connector, a.Tool, a.ArgsDigest, a.ExpiresAt != nil, a.RunIncarnationID, a.TargetRevision)
		if err != nil {
			return err
		}
		if existing != nil {
			a.ID = existing.ID
			return nil
		}
		// A concurrent approval can remove the open key after the insert.
		// Return only an action that actually exists, otherwise ask the caller to retry.
		if _, err := s.GetPendingAction(ctx, a.ID); err != nil {
			return err
		}
	}
	return nil
}

// ExpireGovernancePending denies embedded rows past expires_at.
// ponytail: no index on expires_at. Upgrade path is an index on
// (status, expires_at) if this sweep shows up in traces.
func (s *SQLiteStore) ExpireGovernancePending(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE pending_actions
		SET status = ?, reason_code = ?, updated_at = ?, pending_key = NULL
		WHERE status = ? AND expires_at IS NOT NULL AND expires_at <= ?
	`, models.PendingStatusDenied, models.PendingReasonExpired, formatTS(now),
		models.PendingStatusPending, now.UTC().Format(time.RFC3339))
	return err
}

// GetPendingAction returns one row by id.
func (s *SQLiteStore) GetPendingAction(ctx context.Context, id string) (*models.PendingAction, error) {
	query := `SELECT ` + pendingActionSelectCols + ` FROM pending_actions WHERE id = ?`
	args := []any{id}
	if !tenant.IsSystem(ctx) {
		query += " AND tenant_id = ?"
		args = append(args, tenant.FromContext(ctx))
	}
	row := s.db.QueryRowContext(ctx, query, args...)
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
	query := `SELECT ` + pendingActionSelectCols + ` FROM pending_actions`
	var args []interface{}
	var where []string
	if !tenant.IsSystem(ctx) {
		where = append(where, "tenant_id = ?")
		args = append(args, tenant.FromContext(ctx))
	} else if req.TenantID != "" {
		where = append(where, "tenant_id = ?")
		args = append(args, req.TenantID)
	}
	if req.TargetRevision != "" {
		where = append(where, "target_revision = ?")
		args = append(args, req.TargetRevision)
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
	query := `UPDATE pending_actions SET status = ?, updated_at = ?, pending_key = NULL
		WHERE id = ? AND status = ?`
	args := []any{toStatus, formatTS(time.Now().UTC()), id, fromStatus}
	if !tenant.IsSystem(ctx) {
		query += " AND tenant_id = ?"
		args = append(args, tenant.FromContext(ctx))
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return &state.ErrConflict{Resource: "pending_action", ID: id, Reason: "status changed"}
	}
	return nil
}

// FindOpenPendingAction returns the oldest still-pending row for this
// call tuple and argument digest.
func (s *SQLiteStore) FindOpenPendingAction(ctx context.Context, tenantID, agentID, runID string, generation int64, connector, tool, argsDigest, targetRevision string, embedded bool) (*models.PendingAction, error) {
	if embedded {
		targetRevision = ""
	} else if !models.ValidConnectorTargetRevision(targetRevision) {
		return nil, nil
	}
	incarnation := ""
	if !embedded {
		var err error
		incarnation, err = s.completionDriver().PendingRunIncarnation(ctx, tenantID, agentID, runID)
		if err != nil {
			return nil, err
		}
	}
	return s.findOpenPendingAction(ctx, tenantID, agentID, runID, generation, connector, tool, argsDigest, embedded, incarnation, targetRevision)
}

func (s *SQLiteStore) findOpenPendingAction(ctx context.Context, tenantID, agentID, runID string, generation int64, connector, tool, argsDigest string, embedded bool, incarnation, targetRevision string) (*models.PendingAction, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+pendingActionSelectCols+`
		FROM pending_actions
		WHERE run_id = ? AND generation = ? AND connector = ? AND tool = ?
		  AND status = ? AND args_digest = ?
		  AND tenant_id = ? AND agent_id = ? AND (expires_at IS NOT NULL) = ? AND run_incarnation_id = ? AND target_revision = ?
		ORDER BY created_at ASC LIMIT 1`,
		runID, generation, connector, tool, models.PendingStatusPending, argsDigest, tenantID, agentID, embedded, incarnation, targetRevision)
	a, err := scanPendingAction(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return a, nil
}

func (s *SQLiteStore) SetPendingActionApproved(ctx context.Context, id, decidedBy string) error {
	now := time.Now().UTC()
	query := `
		UPDATE pending_actions SET status = ?, decided_by = ?, updated_at = ?, pending_key = NULL
		WHERE id = ? AND status = ? AND (expires_at IS NULL OR expires_at > ?) AND (
            expires_at IS NOT NULL
            OR EXISTS (SELECT 1 FROM runs AS r WHERE r.run_id=pending_actions.run_id AND r.tenant_id=pending_actions.tenant_id AND r.agent_id=pending_actions.agent_id AND r.incarnation_id=pending_actions.run_incarnation_id AND r.incarnation_id<>'')
            OR (run_incarnation_id='' AND NOT EXISTS (SELECT 1 FROM runs AS r WHERE r.run_id=pending_actions.run_id AND r.tenant_id=pending_actions.tenant_id))
        ) AND (expires_at IS NOT NULL OR (length(target_revision)=74 AND substr(target_revision,1,10)='target-v1-' AND substr(target_revision,11) NOT GLOB '*[^0-9a-f]*'))`
	args := []any{models.PendingStatusApproved, decidedBy, formatTS(now), id, models.PendingStatusPending, formatExpiry(&now)}
	if !tenant.IsSystem(ctx) {
		query += " AND tenant_id = ?"
		args = append(args, tenant.FromContext(ctx))
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return &state.ErrConflict{Resource: "pending_action", ID: id, Reason: "status changed or approval expired"}
	}
	return nil
}

// ConsumeApprovedAction atomically marks an approved action consumed.
func (s *SQLiteStore) ConsumeApprovedAction(ctx context.Context, tenantID, agentID, runID string, generation int64, connector, tool, argsDigest, targetRevision string) (string, error) {
	return s.completionDriver().ConsumeApprovedPendingAction(ctx, tenantID, agentID, runID, generation, connector, tool, argsDigest, targetRevision)
}

func scanPendingAction(row govScanner) (*models.PendingAction, error) {
	var a models.PendingAction
	var createdRaw, updatedRaw, argsRaw string
	var expiresRaw sql.NullString
	if err := row.Scan(
		&a.ID, &a.RunID, &a.Generation, &a.TenantID, &a.AgentID, &a.Connector, &a.Tool,
		&a.RuleID, &a.Reason, &a.ReasonCode, &a.Status, &createdRaw, &updatedRaw,
		&a.ArgsDigest, &argsRaw, &a.DecidedBy, &expiresRaw, &a.RunIncarnationID, &a.TargetRevision,
	); err != nil {
		return nil, err
	}
	if t, err := parseTS(createdRaw); err == nil {
		a.CreatedAt = t
	}
	if t, err := parseTS(updatedRaw); err == nil {
		a.UpdatedAt = t
	}
	if expiresRaw.Valid && expiresRaw.String != "" {
		if t, err := time.Parse(time.RFC3339, expiresRaw.String); err == nil {
			a.ExpiresAt = &t
		}
	}
	a.Args = models.UnmarshalPendingArgs([]byte(argsRaw))
	return &a, nil
}
