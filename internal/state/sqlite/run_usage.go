package sqlite

import (
	"context"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
)

func (*SQLiteStore) SupportsWorkloadIdentity() bool { return true }
func (s *SQLiteStore) upRunUsage(ctx context.Context) error {
	return addColumnIfMissing(ctx, s.db, "runs", "usage_snapshot", "TEXT")
}
func (s *SQLiteStore) ReportRunUsage(ctx context.Context, binding state.RunUsageBinding, report models.RunUsageReport) (*models.RunUsageSnapshot, bool, error) {
	return s.completionDriver().ReportRunUsage(ctx, binding, report)
}
func (s *SQLiteStore) GetRunUsage(ctx context.Context, binding state.RunUsageBinding) (*models.RunUsageSnapshot, error) {
	return s.completionDriver().GetRunUsage(ctx, binding)
}
