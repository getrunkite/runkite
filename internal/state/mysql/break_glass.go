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

func (s *Store) upBreakGlassWindows(ctx context.Context, db migrate.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS break_glass_windows (
			id         VARCHAR(255) PRIMARY KEY,
			tenant_id  VARCHAR(255) NOT NULL,
			agent_id   VARCHAR(255) NOT NULL DEFAULT '',
			reason     TEXT NOT NULL,
			created_by VARCHAR(255) DEFAULT '',
			starts_at  DATETIME(6) NOT NULL,
			expires_at DATETIME(6) NOT NULL,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			KEY idx_break_glass_active (tenant_id, agent_id, expires_at)
		) ENGINE=InnoDB CHARSET=utf8mb4`)
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
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO break_glass_windows
			(id, tenant_id, agent_id, reason, created_by, starts_at, expires_at, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		w.ID, w.TenantID, w.AgentID, w.Reason, w.CreatedBy, w.StartsAt, w.ExpiresAt, w.CreatedAt, w.UpdatedAt)
	if isDuplicateKeyError(err) {
		return &state.ErrConflict{Resource: "break_glass_window", ID: w.ID}
	}
	return err
}

func (s *Store) GetBreakGlassWindow(ctx context.Context, id string) (*models.BreakGlassWindow, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, agent_id, reason, created_by, starts_at, expires_at, created_at, updated_at
		FROM break_glass_windows WHERE id = ?`, id)
	w, err := scanBreakGlassWindow(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &state.ErrNotFound{Resource: "break_glass_window", ID: id}
		}
		return nil, err
	}
	return w, nil
}

func (s *Store) DeleteBreakGlassWindow(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM break_glass_windows WHERE id = ?`, id)
	return err
}

func (s *Store) ListBreakGlassWindows(ctx context.Context) ([]*models.BreakGlassWindow, error) {
	rows, err := s.db.QueryContext(ctx, `
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
	return scanBreakGlassWindows(rows)
}

// FindActiveBreakGlass returns the most specific active window: agent-scoped
// wins over tenant-wide (agent_id empty), then earliest expiry.
func (s *Store) FindActiveBreakGlass(ctx context.Context, tenantID, agentID string) (*models.BreakGlassWindow, error) {
	now := time.Now().UTC()
	row := s.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, agent_id, reason, created_by, starts_at, expires_at, created_at, updated_at
		FROM break_glass_windows
		WHERE tenant_id = ? AND (agent_id = ? OR agent_id = '')
			AND starts_at <= ? AND expires_at > ?
		ORDER BY CASE WHEN agent_id = ? THEN 0 ELSE 1 END, expires_at ASC
		LIMIT 1`, tenantID, agentID, now, now, agentID)
	w, err := scanBreakGlassWindow(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return w, nil
}

func scanBreakGlassWindows(rows *sql.Rows) ([]*models.BreakGlassWindow, error) {
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

func scanBreakGlassWindow(row govScanner) (*models.BreakGlassWindow, error) {
	var w models.BreakGlassWindow
	if err := row.Scan(&w.ID, &w.TenantID, &w.AgentID, &w.Reason, &w.CreatedBy,
		&w.StartsAt, &w.ExpiresAt, &w.CreatedAt, &w.UpdatedAt); err != nil {
		return nil, err
	}
	return &w, nil
}
