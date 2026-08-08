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

func (s *Store) upBreakGlassWindows(ctx context.Context, conn *pgxpool.Conn) error {
	_, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS break_glass_windows (
			id         TEXT PRIMARY KEY,
			tenant_id  TEXT NOT NULL,
			agent_id   TEXT NOT NULL DEFAULT '',
			reason     TEXT NOT NULL,
			created_by TEXT DEFAULT '',
			starts_at  TIMESTAMPTZ NOT NULL,
			expires_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE INDEX IF NOT EXISTS idx_break_glass_active
			ON break_glass_windows (tenant_id, agent_id, expires_at);
	`)
	return err
}

func (s *Store) CreateBreakGlassWindow(ctx context.Context, w *models.BreakGlassWindow) error {
	if w == nil {
		return nil
	}
	now := time.Now().UTC()
	if w.CreatedAt.IsZero() {
		w.CreatedAt = now
	}
	w.UpdatedAt = now
	_, err := s.pool.Exec(ctx, `
		INSERT INTO break_glass_windows
			(id, tenant_id, agent_id, reason, created_by, starts_at, expires_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, w.ID, w.TenantID, w.AgentID, w.Reason, w.CreatedBy, w.StartsAt, w.ExpiresAt, w.CreatedAt, w.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return &state.ErrConflict{Resource: "break_glass_window", ID: w.ID}
		}
	}
	return err
}

func (s *Store) GetBreakGlassWindow(ctx context.Context, id string) (*models.BreakGlassWindow, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, tenant_id, agent_id, reason, created_by, starts_at, expires_at, created_at, updated_at
		FROM break_glass_windows WHERE id = $1`, id)
	w, err := scanBreakGlassWindow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, &state.ErrNotFound{Resource: "break_glass_window", ID: id}
		}
		return nil, err
	}
	return w, nil
}

func (s *Store) DeleteBreakGlassWindow(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM break_glass_windows WHERE id = $1`, id)
	return err
}

func (s *Store) ListBreakGlassWindows(ctx context.Context) ([]*models.BreakGlassWindow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, tenant_id, agent_id, reason, created_by, starts_at, expires_at, created_at, updated_at
		FROM break_glass_windows ORDER BY tenant_id ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBreakGlassWindows(rows)
}

func (s *Store) SearchBreakGlassWindows(ctx context.Context, req *models.BreakGlassSearchRequest) ([]*models.BreakGlassWindow, error) {
	if req == nil {
		req = &models.BreakGlassSearchRequest{}
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	query := `SELECT id, tenant_id, agent_id, reason, created_by, starts_at, expires_at, created_at, updated_at FROM break_glass_windows`
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
	return scanBreakGlassWindows(rows)
}

// FindActiveBreakGlass returns the most specific active window: agent-scoped
// wins over tenant-wide (agent_id empty), then earliest expiry.
func (s *Store) FindActiveBreakGlass(ctx context.Context, tenantID, agentID string) (*models.BreakGlassWindow, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, tenant_id, agent_id, reason, created_by, starts_at, expires_at, created_at, updated_at
		FROM break_glass_windows
		WHERE tenant_id = $1 AND (agent_id = $2 OR agent_id = '')
			AND starts_at <= NOW() AND expires_at > NOW()
		ORDER BY CASE WHEN agent_id = $2 THEN 0 ELSE 1 END, expires_at ASC
		LIMIT 1`, tenantID, agentID)
	w, err := scanBreakGlassWindow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return w, nil
}

func scanBreakGlassWindows(rows pgx.Rows) ([]*models.BreakGlassWindow, error) {
	out := []*models.BreakGlassWindow{}
	for rows.Next() {
		w, err := scanBreakGlassWindow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func scanBreakGlassWindow(row interface{ Scan(dest ...any) error }) (*models.BreakGlassWindow, error) {
	var w models.BreakGlassWindow
	if err := row.Scan(&w.ID, &w.TenantID, &w.AgentID, &w.Reason, &w.CreatedBy,
		&w.StartsAt, &w.ExpiresAt, &w.CreatedAt, &w.UpdatedAt); err != nil {
		return nil, err
	}
	return &w, nil
}
