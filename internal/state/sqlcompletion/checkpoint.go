package sqlcompletion

import (
	"context"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

// LockRunCheckpoint uses the same run-row lock as attempt advancement and
// completion. The caller must keep tx open until its blob mutation commits.
func (d Driver) LockRunCheckpoint(ctx context.Context, tx Tx, binding state.RunCheckpointBinding) error {
	_, threadID, status, metadata, err := d.load(ctx, tx, binding.RunID)
	if isNoRows(err) {
		return &state.ErrRunCheckpointFence{ReasonCode: "run_not_inflight"}
	}
	if err != nil {
		return err
	}
	return state.CheckRunCheckpointBinding(&models.Run{ThreadID: threadID, Status: status, Metadata: metadata}, binding)
}

func (d Driver) WithRunCheckpoint(ctx context.Context, binding state.RunCheckpointBinding, mutate func(Tx) error) error {
	tx, err := d.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := d.LockRunCheckpoint(ctx, tx, binding); err != nil {
		return err
	}
	if err := mutate(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (d Driver) DeleteBoundOpaqueCheckpoint(ctx context.Context, binding state.RunCheckpointBinding, checkpointID string) error {
	tx, err := d.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := d.LockRunCheckpoint(ctx, tx, binding); err != nil {
		return err
	}
	tid := tenant.FromContext(ctx)
	var id string
	err = tx.Row(ctx, d.query(`SELECT checkpoint_id FROM opaque_checkpoints WHERE tenant_id = ? AND thread_id = ? AND checkpoint_id = ?`+d.lock()), tid, binding.ThreadID, checkpointID).Scan(&id)
	if isNoRows(err) {
		return &state.ErrNotFound{Resource: "opaque_checkpoint", ID: checkpointID}
	}
	if err != nil {
		return err
	}
	if err := tx.Exec(ctx, d.query(`DELETE FROM opaque_checkpoints WHERE tenant_id = ? AND thread_id = ? AND checkpoint_id = ?`), tid, binding.ThreadID, checkpointID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
