package sqlcompletion

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
)

func (d Driver) pendingRunAuthority(ctx context.Context, tx Tx, tenantID, agentID, runID string, locked bool, requireVerified ...bool) (string, string, error) {
	strict := len(requireVerified) > 0 && requireVerified[0]
	q := `SELECT incarnation_id,agent_id,metadata FROM runs WHERE run_id=? AND tenant_id=?`
	if locked {
		q += d.lock()
	}
	var incarnation, storedAgent string
	var metadata []byte
	err := tx.Row(ctx, d.query(q), runID, tenantID).Scan(&incarnation, &storedAgent, &metadata)
	if isNoRows(err) {
		if strict {
			return "", "", &state.ErrConflict{Resource: "pending_action", ID: runID, Reason: "verified requester provenance required"}
		}
		// Legacy store-only fixtures have no run authority. A later real run
		// always has a nonempty incarnation, so those rows cannot transfer to it.
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	if incarnation == "" || storedAgent != agentID {
		return "", "", &state.ErrConflict{Resource: "pending_action", ID: runID, Reason: "run binding changed"}
	}
	var stored struct {
		Manifest *models.RunManifest `json:"run_manifest"`
	}
	if len(metadata) > 0 {
		if err := json.Unmarshal(metadata, &stored); err != nil {
			return "", "", err
		}
	}
	requester := ""
	if stored.Manifest != nil && stored.Manifest.Principal != nil {
		requester = stored.Manifest.Principal.Identity
	}
	if strict && (stored.Manifest == nil || stored.Manifest.Principal == nil || !stored.Manifest.Principal.VerifiedIdentity || strings.TrimSpace(requester) == "") {
		return "", "", &state.ErrConflict{Resource: "pending_action", ID: runID, Reason: "verified requester provenance required"}
	}
	return incarnation, requester, nil
}

func (d Driver) pendingRunIncarnation(ctx context.Context, tx Tx, tenantID, agentID, runID string, locked bool) (string, error) {
	incarnation, _, err := d.pendingRunAuthority(ctx, tx, tenantID, agentID, runID, locked)
	return incarnation, err
}

func (d Driver) PendingRunIncarnation(ctx context.Context, tenantID, agentID, runID string) (string, error) {
	tx, err := d.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	return d.pendingRunIncarnation(ctx, tx, tenantID, agentID, runID, false)
}

// BindPendingAction freezes the approval plan and derives managed requester
// identity from the durable run, never from submitted tool arguments.
func (d Driver) BindPendingAction(ctx context.Context, a *models.PendingAction) error {
	conflict := func(reason string) error {
		return &state.ErrConflict{Resource: "pending_action", ID: a.ID, Reason: reason}
	}
	if a.RequiredApprovals == 0 {
		a.RequiredApprovals = 1
	}
	if a.RequiredApprovals < 1 || a.RequiredApprovals > models.MaxRequiredApprovals {
		return conflict("invalid approval quorum")
	}
	if !validPendingIdentity(a.RequesterIdentity) {
		return conflict("invalid requester identity")
	}
	a.ApprovalCount = 0
	a.Approvals = nil
	if a.ExpiresAt != nil {
		a.RunIncarnationID = ""
		a.TargetRevision = ""
		a.PolicyRevision = ""
		a.ToolRevision = ""
	} else {
		if !models.ValidConnectorTargetRevision(a.TargetRevision) || !models.ValidPolicyRevision(a.PolicyRevision) || !models.ValidToolRevision(a.ToolRevision) {
			return conflict("managed approval revision binding required")
		}
		tx, err := d.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		incarnation, requester, err := d.pendingRunAuthority(ctx, tx, a.TenantID, a.AgentID, a.RunID, false, a.ExcludeRequester)
		if err != nil {
			return err
		}
		if (a.RunIncarnationID != "" && a.RunIncarnationID != incarnation) || (a.RequesterIdentity != "" && a.RequesterIdentity != requester) {
			return conflict("run binding changed")
		}
		if !validPendingIdentity(requester) {
			return conflict("invalid requester identity")
		}
		a.RunIncarnationID = incarnation
		a.RequesterIdentity = requester
	}
	if a.ExcludeRequester && strings.TrimSpace(a.RequesterIdentity) == "" {
		return conflict("requester identity required for separation")
	}
	return nil
}

// ConsumeApprovedPendingAction locks the current run before approval authority.
// A status flip alone never supplies votes for a quorum-bound capability.
func (d Driver) ConsumeApprovedPendingAction(ctx context.Context, tenantID, agentID, runID string, generation int64, connector, tool, digest, targetRevision, policyRevision, toolRevision string) (string, error) {
	if digest == "" || !models.ValidConnectorTargetRevision(targetRevision) || !models.ValidPolicyRevision(policyRevision) || !models.ValidToolRevision(toolRevision) {
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
	q := `SELECT id,exclude_requester,requester_identity FROM pending_actions WHERE run_id=? AND generation=? AND connector=? AND tool=? AND status=? AND args_digest=? AND tenant_id=? AND agent_id=? AND expires_at IS NULL AND run_incarnation_id=? AND target_revision=? AND policy_revision=? AND tool_revision=? AND required_approvals BETWEEN 1 AND 5 AND (SELECT COUNT(*) FROM pending_action_approvals AS pa WHERE pa.pending_action_id=pending_actions.id AND pa.tenant_id=pending_actions.tenant_id)>=required_approvals ORDER BY created_at ASC LIMIT 1`
	var id, requester string
	var excludeRequester bool
	err = tx.Row(ctx, d.query(q+d.lock()), runID, generation, connector, tool, models.PendingStatusApproved, digest, tenantID, agentID, incarnation, targetRevision, policyRevision, toolRevision).Scan(&id, &excludeRequester, &requester)
	if isNoRows(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if excludeRequester {
		_, currentRequester, err := d.pendingRunAuthority(ctx, tx, tenantID, agentID, runID, true, true)
		if err != nil {
			return "", err
		}
		if currentRequester != requester {
			return "", &state.ErrConflict{Resource: "pending_action", ID: id, Reason: "requester provenance changed"}
		}
	}
	if err := tx.Exec(ctx, d.query(`UPDATE pending_actions SET status=?,updated_at=?,pending_key=NULL WHERE id=? AND tenant_id=? AND status=?`), models.PendingStatusConsumed, d.now(time.Now().UTC()), id, tenantID, models.PendingStatusApproved); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return id, nil
}
