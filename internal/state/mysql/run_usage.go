package mysql

import (
	"context"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/migrate"
	"github.com/getrunkite/runkite/internal/state/sqlcompletion"
)

func (*Store) SupportsWorkloadIdentity() bool { return true }
func (s *Store) upRunUsage(ctx context.Context, db migrate.DB) error {
	return s.addColumnIfMissing(ctx, db, "runs", "usage_snapshot", "JSON NULL")
}
func (s *Store) ReportRunUsage(ctx context.Context, binding state.RunUsageBinding, report models.RunUsageReport) (*models.RunUsageSnapshot, bool, error) {
	return sqlcompletion.DatabaseSQL(s.db, "mysql").ReportRunUsage(ctx, binding, report)
}
func (s *Store) GetRunUsage(ctx context.Context, binding state.RunUsageBinding) (*models.RunUsageSnapshot, error) {
	return sqlcompletion.DatabaseSQL(s.db, "mysql").GetRunUsage(ctx, binding)
}
