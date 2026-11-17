package sqlcompletion

import (
	"context"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
)

// TryClaimRunTerminalHook keeps original-record validation and claim insertion
// under the same run lock, including against concurrent deletion/recreation.
func (d Driver) TryClaimRunTerminalHook(ctx context.Context, runID, incarnation string) (bool, error) {
	if incarnation == "" {
		return false, &state.ErrConflict{Resource: "run", ID: runID, Reason: "original record identity required for terminal hook claim"}
	}
	tx, err := d.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	query := `SELECT incarnation_id, status FROM runs WHERE run_id = ?`
	args := []interface{}{runID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	var current string
	var status models.RunStatus
	if err := tx.Row(ctx, d.query(query+d.lock()), args...).Scan(&current, &status); err != nil {
		if isNoRows(err) {
			return false, &state.ErrNotFound{Resource: "run", ID: runID}
		}
		return false, err
	}
	if current != incarnation {
		return false, &state.ErrConflict{Resource: "run", ID: runID, Reason: "retained run incarnation changed"}
	}
	if !state.TerminalRunStatus(status) {
		return false, &state.ErrConflict{Resource: "run", ID: runID, Reason: "terminal hook claim requires a terminal run"}
	}
	var existing string
	if err := tx.Row(ctx, d.query(`SELECT run_id FROM terminal_hook_claims WHERE run_id = ?`), runID).Scan(&existing); err == nil {
		return false, &state.ErrConflict{Resource: "run", ID: runID, Reason: "legacy terminal hook claim is ambiguous; reconcile and use fresh operation IDs"}
	} else if !isNoRows(err) {
		return false, err
	}
	key := transport.RunRecordKey(runID, incarnation)
	if err := tx.Row(ctx, d.query(`SELECT run_id FROM terminal_hook_claims WHERE run_id = ?`), key).Scan(&existing); err == nil {
		return false, nil
	} else if !isNoRows(err) {
		return false, err
	}
	claimedAt := interface{}(time.Now().UTC())
	if d.Dialect == "sqlite" {
		claimedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if err := tx.Exec(ctx, d.query(`INSERT INTO terminal_hook_claims (run_id, claimed_at) VALUES (?, ?)`), key, claimedAt); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
