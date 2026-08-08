package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/pagecursor"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

func (s *Store) upKillSwitches(ctx context.Context, conn *pgxpool.Conn) error {
	_, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS kill_switches (
			id         TEXT PRIMARY KEY,
			tenant_id  TEXT NOT NULL,
			agent_id   TEXT NOT NULL DEFAULT '',
			pause_only BOOLEAN NOT NULL DEFAULT FALSE,
			reason     TEXT DEFAULT '',
			created_by TEXT DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (tenant_id, agent_id)
		);
		CREATE INDEX IF NOT EXISTS idx_kill_switches_tenant ON kill_switches(tenant_id);
	`)
	return err
}

func (s *Store) ListKillSwitches(ctx context.Context) ([]*models.KillSwitch, error) {
	rows, err := s.pool.Query(ctx, `
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
	kc, err := pagecursor.DecodeKey(req.Cursor)
	if err != nil {
		return nil, err
	}
	if kc.ID != "" {
		where = append(where, fmt.Sprintf("(tenant_id > $%d OR (tenant_id = $%d AND id > $%d))", argN, argN, argN+1))
		args = append(args, kc.Key, kc.ID)
		argN += 2
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	if kc.ID != "" {
		query += fmt.Sprintf(" ORDER BY tenant_id ASC, id ASC LIMIT $%d", argN)
		args = append(args, limit)
	} else {
		query += fmt.Sprintf(" ORDER BY tenant_id ASC, id ASC LIMIT $%d OFFSET $%d", argN, argN+1)
		args = append(args, limit, req.Offset)
	}
	rows, err := s.pool.Query(ctx, query, args...)
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
	_, err := s.pool.Exec(ctx, `
		INSERT INTO kill_switches (id, tenant_id, agent_id, pause_only, reason, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (id) DO UPDATE SET
			tenant_id = EXCLUDED.tenant_id,
			agent_id = EXCLUDED.agent_id,
			pause_only = EXCLUDED.pause_only,
			reason = EXCLUDED.reason,
			created_by = EXCLUDED.created_by,
			updated_at = EXCLUDED.updated_at
	`, k.ID, k.TenantID, k.AgentID, k.PauseOnly, k.Reason, k.CreatedBy, k.CreatedAt, k.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return &state.ErrConflict{
				Resource: "kill_switch",
				ID:       k.TenantID + "/" + k.AgentID,
				Reason:   "already exists for tenant/agent (DELETE then recreate, or reuse the same id)",
			}
		}
	}
	return err
}

func (s *Store) GetKillSwitch(ctx context.Context, id string) (*models.KillSwitch, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, tenant_id, agent_id, pause_only, reason, created_by, created_at, updated_at
		FROM kill_switches WHERE id = $1`, id)
	k, err := scanKillSwitch(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, &state.ErrNotFound{Resource: "kill_switch", ID: id}
		}
		return nil, err
	}
	return k, nil
}

func (s *Store) DeleteKillSwitch(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM kill_switches WHERE id = $1`, id)
	return err
}

func (s *Store) FindActiveKill(ctx context.Context, tenantID, agentID string) (*models.KillSwitch, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, tenant_id, agent_id, pause_only, reason, created_by, created_at, updated_at
		FROM kill_switches
		WHERE tenant_id = $1 AND (agent_id = $2 OR agent_id = '')
		ORDER BY CASE WHEN agent_id = $2 THEN 0 ELSE 1 END
		LIMIT 1`, tenantID, agentID)
	k, err := scanKillSwitch(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return k, nil
}

func scanKillSwitches(rows pgx.Rows) ([]*models.KillSwitch, error) {
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

func scanKillSwitch(row interface{ Scan(dest ...any) error }) (*models.KillSwitch, error) {
	var k models.KillSwitch
	if err := row.Scan(&k.ID, &k.TenantID, &k.AgentID, &k.PauseOnly, &k.Reason, &k.CreatedBy, &k.CreatedAt, &k.UpdatedAt); err != nil {
		return nil, err
	}
	return &k, nil
}
