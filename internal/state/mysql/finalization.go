package mysql

import (
	"context"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
)

func (s *Store) WriteRunUsageEvent(ctx context.Context, inc string, event *models.UsageEvent) error {
	return s.completionDriver().WriteRunUsageEvent(ctx, inc, event)
}
func (s *Store) UpsertRunUsageHold(ctx context.Context, inc string, hold *models.UsageHold) error {
	return s.completionDriver().UpsertRunUsageHold(ctx, inc, hold)
}
func (s *Store) ReleaseRunUsageHold(ctx context.Context, id, inc string) error {
	return s.completionDriver().ReleaseRunUsageHold(ctx, id, inc)
}

var _ state.RunUsageEventStore = (*Store)(nil)
var _ state.RunUsageHoldStore = (*Store)(nil)
