package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlcompletion"
	"github.com/getrunkite/runkite/internal/tenant"
)

// CreateRunAdmitted runs COUNT + INSERT under BEGIN IMMEDIATE so concurrent
// writers serialize (SQLite write lock) before either observes the other.
func (s *SQLiteStore) CreateRunAdmitted(ctx context.Context, run *models.Run, caps *state.RunAdmissionCaps) error {
	return s.createRun(ctx, run, caps, false, nil)
}

func (s *SQLiteStore) CreateCachedRun(ctx context.Context, run *models.Run, caps *state.RunAdmissionCaps) error {
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

func (s *SQLiteStore) createRun(ctx context.Context, run *models.Run, caps *state.RunAdmissionCaps, claim bool, afterInsert func(sqlcompletion.Tx) error) error {

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	tid := tenant.FromContext(ctx)

	driver := s.completionDriver()
	locked := sqlcompletion.WrapSQLiteConnection(conn)
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
		err := conn.QueryRowContext(ctx, q, args...).Scan(&n)
		return n, err
	}
	countSince := func(since time.Time, agentID string) (int, error) {
		q := `SELECT COUNT(*) FROM runs WHERE created_at >= ? AND tenant_id = ?` + state.SQLExcludeSimulationSQLite
		args := []interface{}{since.UTC().Format(time.RFC3339), tid}
		if agentID != "" {
			q += ` AND agent_id = ?`
			args = append(args, agentID)
		}
		var n int
		err := conn.QueryRowContext(ctx, q, args...).Scan(&n)
		return n, err
	}
	if err := state.EvaluateRunAdmission(caps, run.AgentID, countActive, countSince); err != nil {
		return err
	}

	meta, _ := json.Marshal(run.Metadata)
	_, err = conn.ExecContext(ctx, `
		INSERT INTO runs (tenant_id, run_id, thread_id, agent_id, status, metadata, input, config, created_at, updated_at, parent_run_id, root_run_id, depth)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, tid, run.RunID, run.ThreadID, run.AgentID, run.Status, string(meta),
		string(run.Input), string(run.Config),
		run.CreatedAt.Format(time.RFC3339), run.UpdatedAt.Format(time.RFC3339),
		nullableString(run.ParentRunID), nullableString(run.RootRunID), run.Depth)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint") {
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
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}
