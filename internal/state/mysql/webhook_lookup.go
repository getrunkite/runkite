package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

func (s *Store) GetWebhookDeadLetter(ctx context.Context, id string) (*models.WebhookDeadLetter, error) {
	// Keep the primary-key predicate indexable, then enforce exact spelling
	// independently of the compatible backend's default text collation.
	query := `SELECT id, tenant_id, url, event_type, run_id, payload, error, attempts, failed_at FROM webhook_dead_letters WHERE id = ? AND BINARY id = BINARY ?`
	args := []interface{}{id, id}
	if !tenant.IsSystem(ctx) {
		query += ` AND BINARY tenant_id = BINARY ?`
		args = append(args, tenant.FromContext(ctx))
	}
	var dl models.WebhookDeadLetter
	var payload []byte
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&dl.ID, &dl.TenantID, &dl.URL, &dl.EventType, &dl.RunID, &payload, &dl.Error, &dl.Attempts, &dl.FailedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &state.ErrNotFound{Resource: "webhook_dead_letter", ID: id}
	}
	if err != nil {
		return nil, err
	}
	dl.Payload = json.RawMessage(payload)
	return &dl, nil
}

var _ state.WebhookDeadLetterLookup = (*Store)(nil)
