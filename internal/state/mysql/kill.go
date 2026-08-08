package mysql

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/pagecursor"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/migrate"
	"github.com/getrunkite/runkite/internal/tenant"
)

func (s *Store) upKillSwitches(ctx context.Context, db migrate.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS kill_switches (
			id         VARCHAR(255) PRIMARY KEY,
			tenant_id  VARCHAR(255) NOT NULL,
			agent_id   VARCHAR(255) NOT NULL DEFAULT '',
			pause_only TINYINT(1) NOT NULL DEFAULT 0,
			reason     TEXT,
			created_by VARCHAR(255) DEFAULT '',
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			UNIQUE KEY uq_kill_switches_ta (tenant_id, agent_id),
			KEY idx_kill_switches_tenant (tenant_id)
		) ENGINE=InnoDB CHARSET=utf8mb4`)
	return err
}

func (s *Store) ListKillSwitches(ctx context.Context) ([]*models.KillSwitch, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, tenant_id, agent_id, pause_only, reason, created_by, created_at, updated_at
		FROM kill_switches ORDER BY tenant_id ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanKillSwitches(rows)
}

func (s *Store) SearchKillSwitches(ctx context.Context, req *models.KillSwitchSearchRequest) ([]*models.KillSwitch, error) {
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

func (s *Store) UpsertKillSwitch(ctx context.Context, k *models.KillSwitch) error {
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
	res, err := s.db.ExecContext(ctx, `
		UPDATE kill_switches SET tenant_id = ?, agent_id = ?, pause_only = ?, reason = ?, created_by = ?, updated_at = ?
		WHERE id = ?`,
		k.TenantID, k.AgentID, pause, k.Reason, k.CreatedBy, k.UpdatedAt, k.ID)
	if err != nil {
		if isDuplicateKeyError(err) {
			return killSwitchConflict(k)
		}
		return err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		return nil
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO kill_switches (id, tenant_id, agent_id, pause_only, reason, created_by, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		k.ID, k.TenantID, k.AgentID, pause, k.Reason, k.CreatedBy, k.CreatedAt, k.UpdatedAt)
	if isDuplicateKeyError(err) {
		return killSwitchConflict(k)
	}
	return err
}

func killSwitchConflict(k *models.KillSwitch) error {
	return &state.ErrConflict{
		Resource: "kill_switch",
		ID:       k.TenantID + "/" + k.AgentID,
		Reason:   "already exists for tenant/agent (DELETE then recreate, or reuse the same id)",
	}
}

func (s *Store) GetKillSwitch(ctx context.Context, id string) (*models.KillSwitch, error) {
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

func (s *Store) DeleteKillSwitch(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM kill_switches WHERE id = ?`, id)
	return err
}

func (s *Store) FindActiveKill(ctx context.Context, tenantID, agentID string) (*models.KillSwitch, error) {
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
	if err := row.Scan(&k.ID, &k.TenantID, &k.AgentID, &pause, &k.Reason, &k.CreatedBy, &k.CreatedAt, &k.UpdatedAt); err != nil {
		return nil, err
	}
	k.PauseOnly = pause != 0
	return &k, nil
}
