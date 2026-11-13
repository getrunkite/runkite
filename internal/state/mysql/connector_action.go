package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/migrate"
	"github.com/getrunkite/runkite/internal/state/sqlcompletion"
	"github.com/google/uuid"
)

var _ state.ProtectedActionStore = (*Store)(nil)

// MySQL's default repeatable-read snapshot can hide an event that committed
// while a concurrent dispatcher waited for the run lock. Action transactions
// use read committed so mutation replay sees the just-committed receipt.
func (s *Store) connectorActionDriver() sqlcompletion.Driver {
	driver := s.completionDriver()
	driver.Begin = func(ctx context.Context) (sqlcompletion.Tx, error) {
		tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if err != nil {
			return nil, err
		}
		return sqlcompletion.WrapSQLTransaction(tx), nil
	}
	return driver
}

func (s *Store) CreateConnectorAction(ctx context.Context, a *models.ConnectorAction, mutation string) (*models.ConnectorAction, error) {
	return s.connectorActionDriver().CreateConnectorAction(ctx, a, mutation)
}

func (s *Store) RecordConnectorActionDecision(ctx context.Context, id string, version int64, dec models.ConnectorActionDecision, mutation string) (*models.ConnectorAction, error) {
	return s.connectorActionDriver().RecordConnectorActionDecision(ctx, id, version, dec, mutation)
}

func (s *Store) PeekApprovedConnectorAction(ctx context.Context, id string) (*models.PendingAction, error) {
	return s.connectorActionDriver().PeekApprovedConnectorAction(ctx, id)
}

func (s *Store) PrepareConnectorActionDispatch(ctx context.Context, id string, version int64, approval, source string, deadline time.Time, mutation string) (*state.ConnectorActionDispatch, error) {
	return s.connectorActionDriver().PrepareConnectorActionDispatch(ctx, id, version, approval, source, deadline, mutation)
}

func (s *Store) FinishConnectorAction(ctx context.Context, id string, version int64, dispatch string, obs models.ConnectorActionObservation, mutation string) (*models.ConnectorAction, error) {
	return s.connectorActionDriver().FinishConnectorAction(ctx, id, version, dispatch, obs, mutation)
}

func (s *Store) GetConnectorAction(ctx context.Context, id string) (*models.ConnectorAction, error) {
	return s.connectorActionDriver().GetConnectorAction(ctx, id)
}

func (s *Store) SearchConnectorActions(ctx context.Context, req *models.ConnectorActionSearchRequest) ([]*models.ConnectorAction, error) {
	return s.connectorActionDriver().SearchConnectorActions(ctx, req)
}

func (s *Store) ListConnectorActionEvents(ctx context.Context, id string, after int64, limit int) ([]*models.ConnectorActionEvent, error) {
	return s.connectorActionDriver().ListConnectorActionEvents(ctx, id, after, limit)
}

func (s *Store) MarkExpiredConnectorActionsUnknown(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	return s.connectorActionDriver().MarkExpiredConnectorActionsUnknown(ctx, cutoff, limit)
}

func (s *Store) PruneConnectorActions(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	return s.connectorActionDriver().PruneConnectorActions(ctx, cutoff, limit)
}

func (s *Store) upConnectorActions(ctx context.Context, db migrate.DB) error {
	var columns int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND BINARY TABLE_NAME=BINARY 'runs' AND BINARY COLUMN_NAME=BINARY 'incarnation_id'`).Scan(&columns); err != nil {
		return err
	}
	if columns == 0 {
		if _, err := db.ExecContext(ctx, `ALTER TABLE runs ADD COLUMN incarnation_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	// DDL may have committed before an interrupted migration. Reset its default
	// even on retry, and preserve every already assigned incarnation below.
	if _, err := db.ExecContext(ctx, `ALTER TABLE runs ALTER COLUMN incarnation_id SET DEFAULT ''`); err != nil {
		return err
	}
	if err := backfillRunIncarnations(ctx, db); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND BINARY TABLE_NAME=BINARY 'pending_actions' AND BINARY COLUMN_NAME=BINARY 'run_incarnation_id'`).Scan(&columns); err != nil {
		return err
	}
	if columns == 0 {
		// Old capabilities remain unbound and require a new review. Never infer
		// their authority from a run that happens to exist during upgrade.
		if _, err := db.ExecContext(ctx, `ALTER TABLE pending_actions ADD COLUMN run_incarnation_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	for _, q := range sqlcompletion.ConnectorActionSchema("mysql")[:2] {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	for _, index := range []struct{ name, columns string }{{"idx_connector_actions_tenant_run", "tenant_id, run_id, created_at, id"}, {"idx_connector_actions_state", "state, updated_at, id"}, {"idx_connector_actions_tenant_created", "tenant_id, created_at, id"}} {
		if err := s.addIndexIfMissing(ctx, db, "connector_actions", index.name, index.columns); err != nil {
			return err
		}
	}
	return nil
}

// Backfill literal values rather than UUID() expressions. A pinned migration
// connection must close each bounded result set before issuing its updates.
func backfillRunIncarnations(ctx context.Context, db migrate.DB) error {
	queryDB, ok := db.(interface {
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	})
	if !ok {
		return fmt.Errorf("run incarnation backfill requires query support")
	}
	var last string
	first := true
	for {
		q := `SELECT run_id FROM runs WHERE incarnation_id=''`
		var args []any
		if !first {
			q += ` AND run_id > ?`
			args = append(args, last)
		}
		q += ` ORDER BY run_id ASC LIMIT 256`
		rows, err := queryDB.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		ids := make([]string, 0, 256)
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(ids) == 0 {
			break
		}
		for _, id := range ids {
			if _, err := db.ExecContext(ctx, `UPDATE runs SET incarnation_id=? WHERE BINARY run_id=BINARY ? AND incarnation_id=''`, uuid.NewString(), id); err != nil {
				return err
			}
		}
		last, first = ids[len(ids)-1], false
	}
	var missing int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE incarnation_id=''`).Scan(&missing); err != nil {
		return err
	}
	if missing != 0 {
		return fmt.Errorf("run incarnation backfill left %d unassigned rows; stop all writers and retry", missing)
	}
	return nil
}
