package models

import (
	"encoding/json"
	"time"
)

// FinOpsOverlayID is the singleton overlay row id. One live overlay
// document per control plane (file finops remains the bootstrap baseline).
const FinOpsOverlayID = "default"

// FinOpsOverlay is the durable Admin-editable FinOps patch stored in SQL.
// Payload is a partial finops.Config JSON (pricebook/budgets/alerts/
// reservation/routing/on_hard_breach). Overlay map keys win over the
// file baseline; clearing the row restores file-only effective config.
type FinOpsOverlay struct {
	ID        string          `json:"id"`
	Payload   json.RawMessage `json:"payload"`
	UpdatedAt time.Time       `json:"updated_at"`
	UpdatedBy string          `json:"updated_by,omitempty"`
}
