package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/getrunkite/runkite/internal/models"
)

func (s *SQLiteStore) upFinOpsOverlays(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS finops_overlays (
			id         TEXT PRIMARY KEY,
			payload    TEXT NOT NULL DEFAULT '{}',
			updated_at TEXT NOT NULL,
			updated_by TEXT NOT NULL DEFAULT ''
		);
	`)
	return err
}

func (s *SQLiteStore) GetFinOpsOverlay(ctx context.Context) (*models.FinOpsOverlay, error) {
	var (
		id, payload, updatedBy, updatedAtStr string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, payload, updated_at, updated_by FROM finops_overlays WHERE id = ?
	`, models.FinOpsOverlayID).Scan(&id, &payload, &updatedAtStr, &updatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ts, err := time.Parse(time.RFC3339Nano, updatedAtStr)
	if err != nil {
		ts, err = time.Parse(time.RFC3339, updatedAtStr)
		if err != nil {
			ts = time.Now().UTC()
		}
	}
	return &models.FinOpsOverlay{
		ID:        id,
		Payload:   json.RawMessage(payload),
		UpdatedAt: ts.UTC(),
		UpdatedBy: updatedBy,
	}, nil
}

func (s *SQLiteStore) UpsertFinOpsOverlay(ctx context.Context, o *models.FinOpsOverlay) error {
	if o == nil {
		return nil
	}
	id := o.ID
	if id == "" {
		id = models.FinOpsOverlayID
	}
	payload := string(o.Payload)
	if payload == "" {
		payload = "{}"
	}
	ts := o.UpdatedAt
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO finops_overlays (id, payload, updated_at, updated_by)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			payload = excluded.payload,
			updated_at = excluded.updated_at,
			updated_by = excluded.updated_by
	`, id, payload, ts.UTC().Format(time.RFC3339Nano), o.UpdatedBy)
	return err
}

func (s *SQLiteStore) DeleteFinOpsOverlay(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM finops_overlays WHERE id = ?`, models.FinOpsOverlayID)
	return err
}
