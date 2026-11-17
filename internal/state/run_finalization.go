package state

import (
	"context"

	"github.com/getrunkite/runkite/internal/models"
)

// RunUsageEventStore atomically binds terminal accounting to the original
// durable run record. Unbound legacy usage is never inferred from public IDs.
type RunUsageEventStore interface {
	WriteRunUsageEvent(context.Context, string, *models.UsageEvent) error
}

// RunUsageHoldStore serializes reservation mutations with run replacement.
// The final argument to ReleaseRunUsageHold is the original record identity.
type RunUsageHoldStore interface {
	UpsertRunUsageHold(context.Context, string, *models.UsageHold) error
	ReleaseRunUsageHold(context.Context, string, string) error
}
