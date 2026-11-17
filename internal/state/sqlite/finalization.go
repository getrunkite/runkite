package sqlite

import (
	"context"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
)

func (s *SQLiteStore) WriteRunUsageEvent(ctx context.Context, inc string, event *models.UsageEvent) error {
	return s.completionDriver().WriteRunUsageEvent(ctx, inc, event)
}
func (s *SQLiteStore) UpsertRunUsageHold(ctx context.Context, inc string, hold *models.UsageHold) error {
	return s.completionDriver().UpsertRunUsageHold(ctx, inc, hold)
}
func (s *SQLiteStore) ReleaseRunUsageHold(ctx context.Context, id, inc string) error {
	return s.completionDriver().ReleaseRunUsageHold(ctx, id, inc)
}

var _ state.RunUsageEventStore = (*SQLiteStore)(nil)
var _ state.RunUsageHoldStore = (*SQLiteStore)(nil)
