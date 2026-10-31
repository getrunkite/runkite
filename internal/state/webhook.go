package state

import (
	"context"

	"github.com/getrunkite/runkite/internal/models"
)

// WebhookDeadLetterLookup is an optional indexed lookup for a known dead-letter
// ID. Built-in stores implement it so redelivery does not depend on list limits.
// The caller's tenant is enforced unless ctx carries SystemContext.
type WebhookDeadLetterLookup interface {
	GetWebhookDeadLetter(ctx context.Context, id string) (*models.WebhookDeadLetter, error)
}
