package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/jackc/pgx/v5"
)

func (s *Store) GetWebhookDeadLetter(ctx context.Context, id string) (*models.WebhookDeadLetter, error) {
	query := `SELECT id, tenant_id, url, event_type, run_id, payload, error, attempts, failed_at FROM webhook_dead_letters WHERE id = $1`
	args := []interface{}{id}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = $2`
		args = append(args, tenant.FromContext(ctx))
	}
	var dl models.WebhookDeadLetter
	var payload []byte
	err := s.pool.QueryRow(ctx, query, args...).Scan(&dl.ID, &dl.TenantID, &dl.URL, &dl.EventType, &dl.RunID, &payload, &dl.Error, &dl.Attempts, &dl.FailedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, &state.ErrNotFound{Resource: "webhook_dead_letter", ID: id}
	}
	if err != nil {
		return nil, err
	}
	dl.Payload = json.RawMessage(payload)
	return &dl, nil
}

var _ state.WebhookDeadLetterLookup = (*Store)(nil)
