package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlcompletion"
	"github.com/getrunkite/runkite/internal/tenant"
)

// CreateRunAdmitted holds GET_LOCK scopes on one connection, then COUNT +
// INSERT on that same connection so concurrent creates serialize.
func (s *Store) CreateRunAdmitted(ctx context.Context, run *models.Run, caps *state.RunAdmissionCaps) error {
	return s.createRun(ctx, run, caps, false, nil)
}

func (s *Store) CreateCachedRun(ctx context.Context, run *models.Run, caps *state.RunAdmissionCaps) error {
	pending, completion, err := state.PrepareCachedRun(run)
	if err != nil {
		return err
	}
	return s.createRun(ctx, pending, caps, true, func(tx sqlcompletion.Tx) error {
		result, err := s.completionDriver().CompleteRunInTx(ctx, tx, completion)
		if err != nil {
			return err
		}
		if !result.Applied {
			return fmt.Errorf("cache completion was not applied")
		}
		return nil
	})
}

func (s *Store) createRun(ctx context.Context, run *models.Run, caps *state.RunAdmissionCaps, claim bool, afterInsert func(sqlcompletion.Tx) error) error {
	for _, raw := range []json.RawMessage{run.Input, run.Config} {
		if err := s.validateApplicationJSON(ctx, raw); err != nil {
			return err
		}
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	tid := tenant.FromContext(ctx)
	var locks []string
	if caps.Enabled() && (caps.TenantConcurrent > 0 || caps.TenantDaily > 0) {
		locks = append(locks, "rk:admit:t:"+tid)
	}
	if caps.Enabled() && (caps.AgentConcurrent > 0 || caps.AgentDaily > 0) {
		locks = append(locks, "rk:admit:a:"+tid+":"+run.AgentID)
	}
	for _, name := range locks {
		if err := mysqlGetLock(ctx, conn, name); err != nil {
			return err
		}
		defer mysqlReleaseLock(conn, name)
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	locked := sqlcompletion.WrapSQLTransaction(tx)
	driver := s.completionDriver()
	status, err := driver.LockThread(ctx, locked, run.ThreadID)
	if err != nil {
		return err
	}
	if claim && status == models.ThreadStatusBusy {
		return &state.ErrConflict{Resource: "thread", ID: run.ThreadID, Reason: "is busy"}
	}
	if claim {
		if err := locked.Exec(ctx, `UPDATE threads SET status = 'busy' WHERE thread_id = ? AND tenant_id = ?`, run.ThreadID, tid); err != nil {
			return err
		}
	}

	countActive := func(agentID string) (int, error) {
		q := `SELECT COUNT(*) FROM runs WHERE status IN ('pending','running') AND tenant_id = ?`
		args := []interface{}{tid}
		if agentID != "" {
			q += ` AND agent_id = ?`
			args = append(args, agentID)
		}
		var n int
		err := tx.QueryRowContext(ctx, q, args...).Scan(&n)
		return n, err
	}
	countSince := func(since time.Time, agentID string) (int, error) {
		q := `SELECT COUNT(*) FROM runs WHERE created_at >= ? AND tenant_id = ?` + state.SQLExcludeSimulationMySQL
		args := []interface{}{since.UTC(), tid}
		if agentID != "" {
			q += ` AND agent_id = ?`
			args = append(args, agentID)
		}
		var n int
		err := tx.QueryRowContext(ctx, q, args...).Scan(&n)
		return n, err
	}
	if err := state.EvaluateRunAdmission(caps, run.AgentID, countActive, countSince); err != nil {
		return err
	}

	meta, _ := json.Marshal(run.Metadata)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO runs (tenant_id, run_id, thread_id, agent_id, status, metadata, input, config, error_msg, created_at, updated_at, parent_run_id, root_run_id, depth)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, tid, run.RunID, run.ThreadID, run.AgentID, run.Status, meta,
		nullableJSON(run.Input), nullableJSON(run.Config), "",
		run.CreatedAt, run.UpdatedAt, nullableString(run.ParentRunID), nullableString(run.RootRunID), run.Depth)
	if isDuplicateKeyError(err) {
		return &state.ErrConflict{Resource: "run", ID: run.RunID}
	}
	if err != nil {
		return err
	}
	if afterInsert != nil {
		if err := afterInsert(locked); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func mysqlGetLock(ctx context.Context, conn *sql.Conn, name string) error {
	var got sql.NullInt64
	// MySQL user-lock names are capped; keep keys short enough in practice.
	if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK(?, 10)`, name).Scan(&got); err != nil {
		return fmt.Errorf("admission GET_LOCK %q: %w", name, err)
	}
	if !got.Valid || got.Int64 != 1 {
		return fmt.Errorf("admission GET_LOCK %q: not acquired within 10s", name)
	}
	return nil
}

func mysqlReleaseLock(conn *sql.Conn, name string) {
	_, _ = conn.ExecContext(context.Background(), `SELECT RELEASE_LOCK(?)`, name)
}
