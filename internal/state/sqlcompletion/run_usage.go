package sqlcompletion

import (
	"context"
	"encoding/json"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
)

func (d Driver) loadRunUsage(ctx context.Context, tx Tx, id, tenantID string) (*models.RunUsageSnapshot, error) {
	var raw []byte
	if err := tx.Row(ctx, d.query(`SELECT usage_snapshot FROM runs WHERE run_id=? AND tenant_id=?`), id, tenantID).Scan(&raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var snapshot models.RunUsageSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return nil, err
	}
	if err := snapshot.Validate(); err != nil || snapshot.Source != models.RunUsageSourceIsolated {
		return nil, &state.ErrConflict{Resource: "run_usage", ID: id, Reason: "invalid stored usage snapshot"}
	}
	return &snapshot, nil
}

func (d Driver) lockRunUsage(ctx context.Context, tx Tx, binding state.RunUsageBinding) (*models.RunUsageSnapshot, error) {
	if binding.TenantID == "" || binding.RunIncarnationID == "" || (!tenant.IsSystem(ctx) && tenant.FromContext(ctx) != binding.TenantID) {
		return nil, &state.ErrRunCheckpointFence{ReasonCode: "run_binding_required"}
	}
	tid, thread, status, metadata, err := d.load(ctx, tx, binding.RunID)
	if err != nil {
		return nil, err
	}
	if tid != binding.TenantID {
		return nil, &state.ErrRunCheckpointFence{ReasonCode: "run_binding_required"}
	}
	if err := state.CheckRunCheckpointBinding(&models.Run{ThreadID: thread, Status: status, Metadata: metadata}, binding.RunCheckpointBinding); err != nil {
		return nil, err
	}
	if err := state.CheckWorkloadOwner(ctx, metadata); err != nil {
		return nil, err
	}
	var incarnation string
	if err := tx.Row(ctx, d.query(`SELECT incarnation_id FROM runs WHERE run_id=? AND tenant_id=?`), binding.RunID, tid).Scan(&incarnation); err != nil {
		return nil, err
	}
	if incarnation != binding.RunIncarnationID {
		return nil, &state.ErrRunCheckpointFence{ReasonCode: "run_incarnation_mismatch"}
	}
	return d.loadRunUsage(ctx, tx, binding.RunID, tid)
}

func (d Driver) GetRunUsage(ctx context.Context, binding state.RunUsageBinding) (*models.RunUsageSnapshot, error) {
	tx, err := d.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	return d.lockRunUsage(ctx, tx, binding)
}

func (d Driver) ReportRunUsage(ctx context.Context, binding state.RunUsageBinding, report models.RunUsageReport) (*models.RunUsageSnapshot, bool, error) {
	if err := report.Validate(); err != nil {
		return nil, false, err
	}
	tx, err := d.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	previous, err := d.lockRunUsage(ctx, tx, binding)
	if err != nil {
		return nil, false, err
	}
	apply, err := report.Advances(previous)
	if err != nil {
		return nil, false, &state.ErrConflict{Resource: "run_usage", ID: binding.RunID, Reason: err.Error()}
	}
	if !apply {
		return previous, false, nil
	}
	snapshot := &models.RunUsageSnapshot{RunUsageReport: report, Source: models.RunUsageSourceIsolated, ReportedGeneration: binding.Generation, ReportedAt: time.Now().UTC(), WorkloadOwner: transport.WorkloadIdentityFromContext(ctx)}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Exec(ctx, d.query(`UPDATE runs SET usage_snapshot=? WHERE run_id=? AND tenant_id=?`), raw, binding.RunID, binding.TenantID); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return snapshot, true, nil
}
