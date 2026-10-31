package mongo

import (
	"context"
	"errors"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func (s *Store) GetWebhookDeadLetter(ctx context.Context, id string) (*models.WebhookDeadLetter, error) {
	var doc webhookDeadLetterDoc
	err := s.col("webhook_dead_letters").FindOne(ctx, tenantFilter(ctx, bson.M{"id": id})).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, &state.ErrNotFound{Resource: "webhook_dead_letter", ID: id}
	}
	if err != nil {
		return nil, err
	}
	return &models.WebhookDeadLetter{
		ID: doc.ID, TenantID: doc.TenantID, URL: doc.URL, EventType: doc.EventType, RunID: doc.RunID,
		Payload: bsonToJSON(doc.Payload), Error: doc.Error, Attempts: doc.Attempts, FailedAt: doc.FailedAt,
	}, nil
}

var _ state.WebhookDeadLetterLookup = (*Store)(nil)
