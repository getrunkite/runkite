package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getrunkite/runkite/internal/models"
)

func (s *Store) upFinOpsOverlays(ctx context.Context, conn *pgxpool.Conn) error {
	_, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS finops_overlays (
			id         TEXT PRIMARY KEY,
			payload    JSONB NOT NULL DEFAULT '{}',
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_by TEXT NOT NULL DEFAULT ''
		);
	`)
	return err
}

// GetFinOpsOverlay returns the singleton overlay row, or (nil, nil) when absent.
func (s *Store) GetFinOpsOverlay(ctx context.Context) (*models.FinOpsOverlay, error) {
	var (
		id, updatedBy string
		payload       []byte
		updatedAt     time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, payload, updated_at, updated_by FROM finops_overlays WHERE id = $1
	`, models.FinOpsOverlayID).Scan(&id, &payload, &updatedAt, &updatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
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

// UpsertFinOpsOverlay replaces the singleton overlay document.
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
	_, err := s.pool.Exec(ctx, `
		INSERT INTO finops_overlays (id, payload, updated_at, updated_by)
		VALUES ($1, $2::jsonb, $3, $4)
		ON CONFLICT (id) DO UPDATE SET
			payload = EXCLUDED.payload,
			updated_at = EXCLUDED.updated_at,
			updated_by = EXCLUDED.updated_by
	`, id, []byte(payload), ts, o.UpdatedBy)
	return err
}

// DeleteFinOpsOverlay removes the singleton overlay (file baseline only).
func (s *Store) DeleteFinOpsOverlay(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM finops_overlays WHERE id = $1`, models.FinOpsOverlayID)
	return err
}
