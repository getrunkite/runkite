package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

// CreateRunAdmitted holds GET_LOCK scopes on one connection, then COUNT +
// INSERT on that same connection so concurrent creates serialize.
func (s *Store) CreateRunAdmitted(ctx context.Context, run *models.Run, caps *state.RunAdmissionCaps) error {
	if !caps.Enabled() {
		return s.CreateRun(ctx, run)
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	tid := tenant.FromContext(ctx)
	var locks []string
	if caps.TenantConcurrent > 0 || caps.TenantDaily > 0 {
		locks = append(locks, "rk:admit:t:"+tid)
	}
	if caps.AgentConcurrent > 0 || caps.AgentDaily > 0 {
		locks = append(locks, "rk:admit:a:"+tid+":"+run.AgentID)
	}
	for _, name := range locks {
		if err := mysqlGetLock(ctx, conn, name); err != nil {
			return err
		}
		defer mysqlReleaseLock(conn, name)
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
		q := `SELECT COUNT(*) FROM runs WHERE created_at >= ? AND tenant_id = ?`
		args := []interface{}{since.UTC(), tid}
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
		INSERT INTO runs (tenant_id, run_id, thread_id, agent_id, status, metadata, input, config, error_msg, created_at, updated_at, parent_run_id, root_run_id, depth)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, tid, run.RunID, run.ThreadID, run.AgentID, run.Status, meta,
		nullableJSON(run.Input), nullableJSON(run.Config), "",
		run.CreatedAt, run.UpdatedAt, nullableString(run.ParentRunID), nullableString(run.RootRunID), run.Depth)
	if isDuplicateKeyError(err) {
		return &state.ErrConflict{Resource: "run", ID: run.RunID}
	}
	return err
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
