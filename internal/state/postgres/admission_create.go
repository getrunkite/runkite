package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlcompletion"
	"github.com/getrunkite/runkite/internal/tenant"
)

// CreateRunAdmitted locks admission scopes, COUNTs, then INSERTs in one
// transaction so concurrent creates serialize per tenant/agent.
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

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tid := tenant.FromContext(ctx)
	if caps.Enabled() && (caps.TenantConcurrent > 0 || caps.TenantDaily > 0) {
		if err := pgXactLock(ctx, tx, "rk:admit:t:"+tid); err != nil {
			return err
		}
	}
	if caps.Enabled() && (caps.AgentConcurrent > 0 || caps.AgentDaily > 0) {
		if err := pgXactLock(ctx, tx, "rk:admit:a:"+tid+":"+run.AgentID); err != nil {
			return err
		}
	}

	driver := s.completionDriver()
	locked := completionTx{tx}
	status, err := driver.LockThread(ctx, locked, run.ThreadID)
	if err != nil {
		return err
	}
	if claim && status == models.ThreadStatusBusy {
		return &state.ErrConflict{Resource: "thread", ID: run.ThreadID, Reason: "is busy"}
	}
	if claim {
		if err := locked.Exec(ctx, `UPDATE threads SET status = 'busy' WHERE thread_id = $1 AND tenant_id = $2`, run.ThreadID, tid); err != nil {
			return err
		}
	}

	countActive := func(agentID string) (int, error) {
		q := `SELECT COUNT(*) FROM runs WHERE status IN ('pending','running') AND tenant_id = $1`
		args := []interface{}{tid}
		if agentID != "" {
			q += ` AND agent_id = $2`
			args = append(args, agentID)
		}
		var n int
		err := tx.QueryRow(ctx, q, args...).Scan(&n)
		return n, err
	}
	countSince := func(since time.Time, agentID string) (int, error) {
		q := `SELECT COUNT(*) FROM runs WHERE created_at >= $1 AND tenant_id = $2` + state.SQLExcludeSimulationPostgres
		args := []interface{}{since.UTC(), tid}
		if agentID != "" {
			q += ` AND agent_id = $3`
			args = append(args, agentID)
		}
		var n int
		err := tx.QueryRow(ctx, q, args...).Scan(&n)
		return n, err
	}
	if err := state.EvaluateRunAdmission(caps, run.AgentID, countActive, countSince); err != nil {
		return err
	}

	meta, _ := json.Marshal(run.Metadata)
	_, err = tx.Exec(ctx, `
		INSERT INTO runs (tenant_id, run_id, thread_id, agent_id, status, metadata, input, config, created_at, updated_at, parent_run_id, root_run_id, depth)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	`, tid, run.RunID, run.ThreadID, run.AgentID, run.Status, meta,
		nullableJSON(run.Input), nullableJSON(run.Config),
		run.CreatedAt, run.UpdatedAt, run.ParentRunID, run.RootRunID, run.Depth)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return &state.ErrConflict{Resource: "run", ID: run.RunID}
		}
		return err
	}
	if afterInsert != nil {
		if err := afterInsert(locked); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func pgXactLock(ctx context.Context, tx pgx.Tx, key string) error {
	k1, k2 := advisoryPair(key)
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, k1, k2)
	if err != nil {
		return fmt.Errorf("admission lock %q: %w", key, err)
	}
	return nil
}

func advisoryPair(key string) (int32, int32) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	v := h.Sum64()
	return int32(v >> 32), int32(v)
}
