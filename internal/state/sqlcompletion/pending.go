package sqlcompletion

import (
	"context"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
)

func (d Driver) pendingRunIncarnation(ctx context.Context, tx Tx, tenantID, agentID, runID string, locked bool) (string, error) {
	q := `SELECT incarnation_id,agent_id FROM runs WHERE run_id=? AND tenant_id=?`
	if locked {
		q += d.lock()
	}
	var incarnation, storedAgent string
	err := tx.Row(ctx, d.query(q), runID, tenantID).Scan(&incarnation, &storedAgent)
	if isNoRows(err) {
		// Legacy state-store fixtures may have approval tuples without a run.
		// They cannot match an action or a real run once one exists.
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if incarnation == "" || storedAgent != agentID {
		return "", &state.ErrConflict{Resource: "pending_action", ID: runID, Reason: "run binding changed"}
	}
	return incarnation, nil
}

func (d Driver) PendingRunIncarnation(ctx context.Context, tenantID, agentID, runID string) (string, error) {
	tx, err := d.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	return d.pendingRunIncarnation(ctx, tx, tenantID, agentID, runID, false)
}

// BindPendingAction derives managed authority from durable state. Historical
// approvals are not backfilled into authority for a newly encountered run.
func (d Driver) BindPendingAction(ctx context.Context, a *models.PendingAction) error {
	if a.ExpiresAt != nil {
		a.RunIncarnationID = ""
		return nil
	}
	incarnation, err := d.PendingRunIncarnation(ctx, a.TenantID, a.AgentID, a.RunID)
	if err != nil {
		return err
	}
	if a.RunIncarnationID != "" && a.RunIncarnationID != incarnation {
		return &state.ErrConflict{Resource: "pending_action", ID: a.ID, Reason: "run binding changed"}
	}
	a.RunIncarnationID = incarnation
	return nil
}

// ConsumeApprovedPendingAction retains the legacy store API while locking the
// current run before the approval. An old run's authority never transfers to a
// replacement that reused its external identifier and attempt generation.
func (d Driver) ConsumeApprovedPendingAction(ctx context.Context, tenantID, agentID, runID string, generation int64, connector, tool, digest string) (string, error) {
	if digest == "" {
		return "", nil
	}
	tx, err := d.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	incarnation, err := d.pendingRunIncarnation(ctx, tx, tenantID, agentID, runID, true)
	if err != nil {
		return "", err
	}
	q := `SELECT id FROM pending_actions WHERE run_id=? AND generation=? AND connector=? AND tool=? AND status=? AND args_digest=? AND tenant_id=? AND agent_id=? AND expires_at IS NULL AND run_incarnation_id=? ORDER BY created_at ASC LIMIT 1`
	var id string
	err = tx.Row(ctx, d.query(q+d.lock()), runID, generation, connector, tool, models.PendingStatusApproved, digest, tenantID, agentID, incarnation).Scan(&id)
	if isNoRows(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if err := tx.Exec(ctx, d.query(`UPDATE pending_actions SET status=?,updated_at=?,pending_key=NULL WHERE id=? AND status=?`), models.PendingStatusConsumed, d.now(time.Now().UTC()), id, models.PendingStatusApproved); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return id, nil
}
