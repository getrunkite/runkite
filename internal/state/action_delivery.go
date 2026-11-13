package state

import (
	"context"
	"encoding/json"
	"time"

	"github.com/getrunkite/runkite/internal/models"
)

const (
	ActionDeliveryPending         = "pending"
	ActionDeliveryDelivered       = "delivered"
	ActionDeliveryDeadLetter      = "dead_letter"
	MaxActionDeliveryPayloadBytes = 64 * 1024
)

// ActionDeliveryPayload is metadata for an existing action event. Receiving or
// retrying it never authorizes dispatching the associated tool operation.
type ActionDeliveryPayload struct {
	Schema     string                       `json:"schema"`
	DeliveryID string                       `json:"delivery_id"`
	SinkID     string                       `json:"sink_id"`
	Action     *models.ConnectorAction      `json:"action"`
	Event      *models.ConnectorActionEvent `json:"event"`
}

type ActionDelivery struct {
	ID             string          `json:"id"`
	SinkID         string          `json:"sink_id"`
	TenantID       string          `json:"tenant_id"`
	ActionID       string          `json:"action_id"`
	Sequence       int64           `json:"sequence"`
	PayloadRaw     json.RawMessage `json:"payload"`
	Attempts       int             `json:"attempts"`
	Status         string          `json:"status"`
	AvailableAt    time.Time       `json:"available_at"`
	ClaimToken     string          `json:"-"`
	ClaimUntil     *time.Time      `json:"claim_until,omitempty"`
	LastReasonCode string          `json:"last_reason_code,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

type ActionDeliverySearchRequest struct {
	TenantID string
	SinkID   string
	Status   string
	Limit    int
	Offset   int
}

// ActionDeliveryStore exports evidence at least once. Workers require a system
// context; reads and explicit dead-letter retries retain tenant scoping.
type ActionDeliveryStore interface {
	ClaimActionDeliveries(context.Context, string, time.Time, time.Time, int) ([]*ActionDelivery, error)
	AckActionDelivery(context.Context, string, string) error
	FailActionDelivery(context.Context, string, string, time.Time, string, bool) error
	SearchActionDeliveries(context.Context, *ActionDeliverySearchRequest) ([]*ActionDelivery, error)
	RetryActionDelivery(context.Context, string, string) error
}
