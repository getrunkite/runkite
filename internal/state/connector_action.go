package state

import (
	"context"
	"time"

	"github.com/getrunkite/runkite/internal/models"
)

// ConnectorActionDispatch is an internal persistence receipt, never a token
// for executing or re-executing a downstream operation.
type ConnectorActionDispatch struct {
	ActionID   string
	TenantID   string
	DispatchID string
	Version    int64
	// Applied is true only for this transaction's new dispatch claim. A
	// persistence replay returns the receipt with Applied false and must not
	// cause another downstream request.
	Applied bool
}

// ProtectedActionStore is optional. SQL backends implement it; unsupported
// deployments must reject enabled action recording instead of dropping evidence.
type ProtectedActionStore interface {
	// Creation captures the actual stored run incarnation, version and identity.
	CreateConnectorAction(context.Context, *models.ConnectorAction, string) (*models.ConnectorAction, error)
	RecordConnectorActionDecision(context.Context, string, int64, models.ConnectorActionDecision, string) (*models.ConnectorAction, error)
	// Peek never consumes approval. Prepare locks and validates it again.
	PeekApprovedConnectorAction(context.Context, string) (*models.PendingAction, error)
	// Approval consumption, dispatch intent and its event commit atomically.
	PrepareConnectorActionDispatch(context.Context, string, int64, string, string, time.Time, string) (*ConnectorActionDispatch, error)
	// Empty dispatch ID permits only a proven not_dispatched observation.
	// An original receipt remains usable for evidence after run cancellation or
	// reclaim, including refining outcome_unknown with its late response.
	FinishConnectorAction(context.Context, string, int64, string, models.ConnectorActionObservation, string) (*models.ConnectorAction, error)
	GetConnectorAction(context.Context, string) (*models.ConnectorAction, error)
	SearchConnectorActions(context.Context, *models.ConnectorActionSearchRequest) ([]*models.ConnectorAction, error)
	ListConnectorActionEvents(context.Context, string, int64, int) ([]*models.ConnectorActionEvent, error)
	MarkExpiredConnectorActionsUnknown(context.Context, time.Time, int) (int, error)
	// Only resolved observations may be explicitly pruned. Unknown outcomes,
	// active intents and unresolved approvals survive ordinary run retention.
	PruneConnectorActions(context.Context, time.Time, int) (int, error)
}
