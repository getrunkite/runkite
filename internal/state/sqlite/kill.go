package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/pagecursor"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

func (s *SQLiteStore) upKillSwitches(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS kill_switches (
			id         TEXT PRIMARY KEY,
			tenant_id  TEXT NOT NULL,
			agent_id   TEXT NOT NULL DEFAULT '',
			pause_only INTEGER NOT NULL DEFAULT 0,
			reason     TEXT DEFAULT '',
			created_by TEXT DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			UNIQUE (tenant_id, agent_id)
		);
		CREATE INDEX IF NOT EXISTS idx_kill_switches_tenant ON kill_switches(tenant_id);
	`)
	return err
}

func (s *SQLiteStore) ListKillSwitches(ctx context.Context) ([]*models.KillSwitch, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, tenant_id, agent_id, pause_only, reason, created_by, created_at, updated_at
		FROM kill_switches ORDER BY tenant_id ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanKillSwitches(rows)
}

func (s *SQLiteStore) SearchKillSwitches(ctx context.Context, req *models.KillSwitchSearchRequest) ([]*models.KillSwitch, error) {
	if req == nil {
		req = &models.KillSwitchSearchRequest{}
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	query := `SELECT id, tenant_id, agent_id, pause_only, reason, created_by, created_at, updated_at FROM kill_switches`
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
	return scanKillSwitches(rows)
}

func (s *SQLiteStore) UpsertKillSwitch(ctx context.Context, k *models.KillSwitch) error {
	if k == nil {
		return nil
	}
	now := time.Now().UTC()
	if k.CreatedAt.IsZero() {
		k.CreatedAt = now
	}
	k.UpdatedAt = now
	pause := 0
	if k.PauseOnly {
		pause = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO kill_switches (id, tenant_id, agent_id, pause_only, reason, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			tenant_id = excluded.tenant_id,
			agent_id = excluded.agent_id,
			pause_only = excluded.pause_only,
			reason = excluded.reason,
			created_by = excluded.created_by,
			updated_at = excluded.updated_at
	`, k.ID, k.TenantID, k.AgentID, pause, k.Reason, k.CreatedBy, formatTS(k.CreatedAt), formatTS(k.UpdatedAt))
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint") {
		return &state.ErrConflict{
			Resource: "kill_switch",
			ID:       k.TenantID + "/" + k.AgentID,
			Reason:   "already exists for tenant/agent (DELETE then recreate, or reuse the same id)",
		}
	}
	return err
}

func (s *SQLiteStore) GetKillSwitch(ctx context.Context, id string) (*models.KillSwitch, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, agent_id, pause_only, reason, created_by, created_at, updated_at
		FROM kill_switches WHERE id = ?`, id)
	k, err := scanKillSwitch(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &state.ErrNotFound{Resource: "kill_switch", ID: id}
		}
		return nil, err
	}
	return k, nil
}

func (s *SQLiteStore) DeleteKillSwitch(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM kill_switches WHERE id = ?`, id)
	return err
}

// FindActiveKill returns the most specific active switch: agent-scoped
// wins over tenant-wide (agent_id empty).
func (s *SQLiteStore) FindActiveKill(ctx context.Context, tenantID, agentID string) (*models.KillSwitch, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, agent_id, pause_only, reason, created_by, created_at, updated_at
		FROM kill_switches
		WHERE tenant_id = ? AND (agent_id = ? OR agent_id = '')
		ORDER BY CASE WHEN agent_id = ? THEN 0 ELSE 1 END
		LIMIT 1`, tenantID, agentID, agentID)
	k, err := scanKillSwitch(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return k, nil
}

func scanKillSwitches(rows *sql.Rows) ([]*models.KillSwitch, error) {
	out := []*models.KillSwitch{}
	for rows.Next() {
		k, err := scanKillSwitch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func scanKillSwitch(row govScanner) (*models.KillSwitch, error) {
	var k models.KillSwitch
	var pause int
	var createdRaw, updatedRaw string
	if err := row.Scan(&k.ID, &k.TenantID, &k.AgentID, &pause, &k.Reason, &k.CreatedBy, &createdRaw, &updatedRaw); err != nil {
		return nil, err
	}
	k.PauseOnly = pause != 0
	if t, err := parseTS(createdRaw); err == nil {
		k.CreatedAt = t
	}
	if t, err := parseTS(updatedRaw); err == nil {
		k.UpdatedAt = t
	}
	return &k, nil
}
