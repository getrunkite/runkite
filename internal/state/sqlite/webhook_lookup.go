package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

func (s *SQLiteStore) GetWebhookDeadLetter(ctx context.Context, id string) (*models.WebhookDeadLetter, error) {
	query := `SELECT id, tenant_id, url, event_type, run_id, payload, error, attempts, failed_at FROM webhook_dead_letters WHERE id = ?`
	args := []interface{}{id}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	var dl models.WebhookDeadLetter
	var payload, failedAt string
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&dl.ID, &dl.TenantID, &dl.URL, &dl.EventType, &dl.RunID, &payload, &dl.Error, &dl.Attempts, &failedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &state.ErrNotFound{Resource: "webhook_dead_letter", ID: id}
	}
	if err != nil {
		return nil, err
	}
	dl.Payload = json.RawMessage(payload)
	dl.FailedAt, err = time.Parse(time.RFC3339, failedAt)
	if err != nil {
		return nil, err
	}
	return &dl, nil
}

var _ state.WebhookDeadLetterLookup = (*SQLiteStore)(nil)
