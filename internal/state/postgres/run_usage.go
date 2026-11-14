package postgres

import (
	"context"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/jackc/pgx/v5/pgxpool"
)

func (*Store) SupportsWorkloadIdentity() bool { return true }
func (s *Store) upRunUsage(ctx context.Context, conn *pgxpool.Conn) error {
	_, err := conn.Exec(ctx, `ALTER TABLE runs ADD COLUMN IF NOT EXISTS usage_snapshot JSONB`)
	return err
}
func (s *Store) ReportRunUsage(ctx context.Context, binding state.RunUsageBinding, report models.RunUsageReport) (*models.RunUsageSnapshot, bool, error) {
	return s.completionDriver().ReportRunUsage(ctx, binding, report)
}
func (s *Store) GetRunUsage(ctx context.Context, binding state.RunUsageBinding) (*models.RunUsageSnapshot, error) {
	return s.completionDriver().GetRunUsage(ctx, binding)
}
