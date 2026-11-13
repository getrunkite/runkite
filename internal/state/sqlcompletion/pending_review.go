package sqlcompletion

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

// PendingActionSelectColumns keeps the SQL backends and transactional approval
// core on the same metadata projection. Counts are derived, never caller supplied.
const PendingActionSelectColumns = `id,run_id,generation,tenant_id,agent_id,connector,tool,
 rule_id,reason,reason_code,status,created_at,updated_at,args_digest,args,decided_by,expires_at,
 run_incarnation_id,target_revision,policy_revision,tool_revision,requester_identity,
 required_approvals,exclude_requester,
 (SELECT COUNT(*) FROM pending_action_approvals AS pa WHERE pa.pending_action_id=pending_actions.id AND pa.tenant_id=pending_actions.tenant_id)`

// PendingApprovalSchema keeps approval history separate from run lifetime. A
// unique exact identity prevents concurrent retries becoming additional votes.
func PendingApprovalSchema(dialect string) []string {
	identity, timestamp, suffix := "TEXT", "TEXT", ""
	if dialect == "postgres" {
		timestamp = "TIMESTAMPTZ"
	}
	if dialect == "mysql" {
		identity = "VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin"
		timestamp = "DATETIME(6)"
		suffix = " ENGINE=InnoDB CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin"
	}
	return []string{fmt.Sprintf(`CREATE TABLE IF NOT EXISTS pending_action_approvals (
 tenant_id %s NOT NULL,pending_action_id %s NOT NULL,reviewer_identity %s NOT NULL,
 reviewed_at %s NOT NULL,PRIMARY KEY(tenant_id,pending_action_id,reviewer_identity))%s`, identity, identity, identity, timestamp, suffix)}
}

type nullablePendingTime struct {
	storedTime
	present bool
}

func (t *nullablePendingTime) Scan(value interface{}) error {
	if value == nil {
		return nil
	}
	t.present = true
	return t.storedTime.Scan(value)
}

func ScanPendingAction(row Row) (*models.PendingAction, error) {
	var a models.PendingAction
	var created, updated storedTime
	var expiry nullablePendingTime
	var reason sql.NullString
	var args []byte
	if err := row.Scan(&a.ID, &a.RunID, &a.Generation, &a.TenantID, &a.AgentID, &a.Connector, &a.Tool,
		&a.RuleID, &reason, &a.ReasonCode, &a.Status, &created, &updated, &a.ArgsDigest, &args, &a.DecidedBy, &expiry,
		&a.RunIncarnationID, &a.TargetRevision, &a.PolicyRevision, &a.ToolRevision, &a.RequesterIdentity, &a.RequiredApprovals, &a.ExcludeRequester, &a.ApprovalCount); err != nil {
		return nil, err
	}
	a.CreatedAt = created.Time
	a.UpdatedAt = updated.Time
	a.Reason = reason.String
	a.Args = models.UnmarshalPendingArgs(args)
	if expiry.present {
		a.ExpiresAt = &expiry.Time
	}
	return &a, nil
}

func (d Driver) loadPendingAction(ctx context.Context, tx Tx, id string, locked bool) (*models.PendingAction, error) {
	query := `SELECT ` + PendingActionSelectColumns + ` FROM pending_actions WHERE id=?`
	args := []interface{}{id}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id=?`
		args = append(args, tenant.FromContext(ctx))
	}
	if locked {
		query += d.lock()
	}
	a, err := ScanPendingAction(tx.Row(ctx, d.query(query), args...))
	if isNoRows(err) {
		return nil, &state.ErrNotFound{Resource: "pending_action", ID: id}
	}
	return a, err
}

func (d Driver) pendingApprovalHistory(ctx context.Context, tx Tx, a *models.PendingAction) ([]models.PendingActionApproval, error) {
	rows, err := actionRows(ctx, tx, d.query(`SELECT reviewer_identity,reviewed_at FROM pending_action_approvals WHERE tenant_id=? AND pending_action_id=? ORDER BY reviewed_at,reviewer_identity`), a.TenantID, a.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.PendingActionApproval{}
	for rows.Next() {
		var entry models.PendingActionApproval
		var at storedTime
		entry.PendingActionID = a.ID
		if err := rows.Scan(&entry.ReviewerIdentity, &at); err != nil {
			return nil, err
		}
		entry.ReviewedAt = at.Time
		out = append(out, entry)
	}
	return out, rows.Err()
}

func (d Driver) GetPendingAction(ctx context.Context, id string) (*models.PendingAction, error) {
	tx, err := d.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	a, err := d.loadPendingAction(ctx, tx, id, true)
	if err != nil {
		return nil, err
	}
	a.Approvals, err = d.pendingApprovalHistory(ctx, tx, a)
	if err != nil {
		return nil, err
	}
	a.ApprovalCount = len(a.Approvals)
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

func (d Driver) ListPendingActionApprovals(ctx context.Context, id string) ([]models.PendingActionApproval, error) {
	a, err := d.GetPendingAction(ctx, id)
	if err != nil {
		return nil, err
	}
	return a.Approvals, nil
}

func validPendingIdentity(value string) bool {
	return utf8.ValidString(value) && utf8.RuneCountInString(value) <= 255 && !strings.ContainsRune(value, 0)
}

// ApprovePendingAction serializes votes with capability consumption. Managed
// paths lock the run before the pending row, matching dispatch's lock order.
func (d Driver) ApprovePendingAction(ctx context.Context, id, actor string) error {
	tx, err := d.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	a, err := d.loadPendingAction(ctx, tx, id, false)
	if err != nil {
		return err
	}
	var incarnation, requester string
	if a.ExpiresAt == nil {
		incarnation, requester, err = d.pendingRunAuthority(ctx, tx, a.TenantID, a.AgentID, a.RunID, true, a.ExcludeRequester)
		if err != nil {
			return err
		}
	}
	a, err = d.loadPendingAction(ctx, tx, id, true)
	if err != nil {
		return err
	}
	conflict := func(reason string) error {
		return &state.ErrConflict{Resource: "pending_action", ID: id, Reason: reason}
	}
	if a.Status != models.PendingStatusPending {
		return conflict("status changed")
	}
	now := time.Now().UTC()
	if a.ExpiresAt != nil && !now.Before(*a.ExpiresAt) {
		return conflict("approval expired")
	}
	if a.RequiredApprovals < 1 || a.RequiredApprovals > models.MaxRequiredApprovals {
		return conflict("invalid approval quorum")
	}
	if !validPendingIdentity(actor) {
		return conflict("invalid reviewer identity")
	}
	strict := a.ExcludeRequester || a.RequiredApprovals > 1
	if strict && strings.TrimSpace(actor) == "" {
		return conflict("authenticated reviewer required")
	}
	if a.ExcludeRequester && (strings.TrimSpace(a.RequesterIdentity) == "" || actor == a.RequesterIdentity) {
		return conflict("requester cannot approve this operation")
	}
	if a.ExpiresAt == nil {
		if !models.ValidConnectorTargetRevision(a.TargetRevision) || !models.ValidPolicyRevision(a.PolicyRevision) || !models.ValidToolRevision(a.ToolRevision) {
			return conflict("historical approval binding requires fresh review")
		}
		if a.RunIncarnationID != incarnation || a.RequesterIdentity != requester {
			return conflict("run binding changed")
		}
	}
	insert := `INSERT INTO pending_action_approvals(tenant_id,pending_action_id,reviewer_identity,reviewed_at) VALUES(?,?,?,?) ON CONFLICT(tenant_id,pending_action_id,reviewer_identity) DO NOTHING`
	if d.Dialect == "mysql" {
		insert = `INSERT INTO pending_action_approvals(tenant_id,pending_action_id,reviewer_identity,reviewed_at) VALUES(?,?,?,?) ON DUPLICATE KEY UPDATE reviewer_identity=reviewer_identity`
	}
	if err := tx.Exec(ctx, d.query(insert), a.TenantID, a.ID, actor, d.now(now)); err != nil {
		return err
	}
	var count int
	if err := tx.Row(ctx, d.query(`SELECT COUNT(*) FROM pending_action_approvals WHERE tenant_id=? AND pending_action_id=?`), a.TenantID, a.ID).Scan(&count); err != nil {
		return err
	}
	status, key := models.PendingStatusPending, interface{}(models.PendingActionKey(a))
	if count >= a.RequiredApprovals {
		status = models.PendingStatusApproved
		key = nil
	}
	// DecidedBy remains the identity which reached quorum. Partial votes are
	// represented by retained history, without claiming a completed decision.
	decidedBy := ""
	if status == models.PendingStatusApproved {
		decidedBy = actor
	}
	if err := tx.Exec(ctx, d.query(`UPDATE pending_actions SET status=?,decided_by=?,updated_at=?,pending_key=? WHERE id=? AND tenant_id=? AND status=?`), status, decidedBy, d.now(now), key, a.ID, a.TenantID, models.PendingStatusPending); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// FindOpenPendingCall shares exact revision and immutable-plan matching across
// all SQL dialects. Public lookups omit plan; creation supplies the frozen plan.
func (d Driver) FindOpenPendingCall(ctx context.Context, tenantID, agentID, runID string, generation int64, connector, tool, digest string, embedded bool, incarnation, targetRevision, policyRevision, toolRevision string, plan *models.PendingAction) (*models.PendingAction, error) {
	if !tenant.IsSystem(ctx) && tenant.FromContext(ctx) != tenantID {
		return nil, nil
	}
	tx, err := d.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	q := `SELECT ` + PendingActionSelectColumns + ` FROM pending_actions WHERE run_id=? AND generation=? AND connector=? AND tool=? AND status=? AND args_digest=? AND tenant_id=? AND agent_id=? AND (expires_at IS NOT NULL)=? AND run_incarnation_id=? AND target_revision=? AND policy_revision=? AND tool_revision=?`
	args := []interface{}{runID, generation, connector, tool, models.PendingStatusPending, digest, tenantID, agentID, embedded, incarnation, targetRevision, policyRevision, toolRevision}
	if plan != nil {
		q += ` AND requester_identity=? AND required_approvals=? AND exclude_requester=?`
		args = append(args, plan.RequesterIdentity, plan.RequiredApprovals, plan.ExcludeRequester)
	}
	q += ` ORDER BY created_at ASC LIMIT 1`
	a, err := ScanPendingAction(tx.Row(ctx, d.query(q), args...))
	if isNoRows(err) {
		return nil, nil
	}
	return a, err
}
