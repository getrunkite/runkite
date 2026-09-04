package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state/migrate"
)

func (s *Store) upFinOpsOverlays(ctx context.Context, db migrate.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS finops_overlays (
			id         VARCHAR(255) PRIMARY KEY,
			payload    JSON NOT NULL,
			updated_at DATETIME(6) NOT NULL,
			updated_by VARCHAR(255) NOT NULL DEFAULT ''
		) ENGINE=InnoDB CHARSET=utf8mb4`)
	return err
}

func (s *Store) GetFinOpsOverlay(ctx context.Context) (*models.FinOpsOverlay, error) {
	var (
		id, updatedBy string
		payload       []byte
		updatedAt     time.Time
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, payload, updated_at, updated_by FROM finops_overlays WHERE id = ?
	`, models.FinOpsOverlayID).Scan(&id, &payload, &updatedAt, &updatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &models.FinOpsOverlay{
		ID:        id,
		Payload:   json.RawMessage(payload),
		UpdatedAt: updatedAt.UTC(),
		UpdatedBy: updatedBy,
	}, nil
}

func (s *Store) UpsertFinOpsOverlay(ctx context.Context, o *models.FinOpsOverlay) error {
	if o == nil {
		return nil
	}
	id := o.ID
	if id == "" {
		id = models.FinOpsOverlayID
	}
	payload := o.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	ts := o.UpdatedAt
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO finops_overlays (id, payload, updated_at, updated_by)
		VALUES (?, CAST(? AS JSON), ?, ?)
		ON DUPLICATE KEY UPDATE
			payload = VALUES(payload),
			updated_at = VALUES(updated_at),
			updated_by = VALUES(updated_by)
	`, id, []byte(payload), ts, o.UpdatedBy)
	return err
}

func (s *Store) DeleteFinOpsOverlay(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM finops_overlays WHERE id = ?`, models.FinOpsOverlayID)
	return err
}
