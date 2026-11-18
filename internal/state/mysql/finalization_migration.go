package mysql

import (
	"context"
	"fmt"

	"github.com/getrunkite/runkite/internal/state/migrate"
)

func (s *Store) upRunFinalization(ctx context.Context, db migrate.DB) error {
	for _, table := range []string{"usage_events", "usage_holds"} {
		if err := s.addColumnIfMissing(ctx, db, table, "run_incarnation_id", `VARBINARY(256) NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	// Install the wider unique key before dropping the old one. MySQL DDL
	// commits implicitly, so this order also keeps a partially applied upgrade safe.
	var modern int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='usage_events' AND index_name='uq_usage_events_record_source'`).Scan(&modern); err != nil {
		return err
	}
	if modern == 0 {
		if _, err := db.ExecContext(ctx, `ALTER TABLE usage_events ADD UNIQUE KEY uq_usage_events_record_source(run_id,run_incarnation_id,source)`); err != nil {
			return err
		}
	} else {
		var nonunique int
		if err := db.QueryRowContext(ctx, `SELECT MAX(non_unique) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='usage_events' AND index_name='uq_usage_events_record_source'`).Scan(&nonunique); err != nil {
			return err
		}
		if nonunique != 0 {
			return fmt.Errorf("usage record index must be unique; stop writers and repair uq_usage_events_record_source before retrying")
		}
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='usage_events' AND index_name='uq_usage_events_run_source'`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		if _, err := db.ExecContext(ctx, `ALTER TABLE usage_events DROP INDEX uq_usage_events_run_source`); err != nil {
			return fmt.Errorf("drop legacy usage uniqueness: %w", err)
		}
	}
	return nil
}
