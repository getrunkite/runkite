package sqlcompletion

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/pagecursor"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/google/uuid"
)

// ActionRows is the small read adapter needed by the shared action lifecycle.
type ActionRows interface {
	Next() bool
	Scan(...interface{}) error
	Err() error
	Close()
}

type actionQueryTx interface {
	ActionQuery(context.Context, string, ...interface{}) (ActionRows, error)
}

type actionSQLRows struct{ *sql.Rows }

func (r actionSQLRows) Close() { _ = r.Rows.Close() }
func (t sqlTx) ActionQuery(ctx context.Context, q string, a ...interface{}) (ActionRows, error) {
	r, err := t.QueryContext(ctx, q, a...)
	if err != nil {
		return nil, err
	}
	return actionSQLRows{r}, nil
}
func (t *sqliteTx) ActionQuery(ctx context.Context, q string, a ...interface{}) (ActionRows, error) {
	r, err := t.conn.QueryContext(ctx, q, a...)
	if err != nil {
		return nil, err
	}
	return actionSQLRows{r}, nil
}
func actionRows(ctx context.Context, tx Tx, q string, a ...interface{}) (ActionRows, error) {
	reader, ok := tx.(actionQueryTx)
	if !ok {
		return nil, fmt.Errorf("connector action query adapter unavailable")
	}
	return reader.ActionQuery(ctx, q, a...)
}

// ConnectorActionSchema has no run foreign key. Explicit action retention owns
// deletion; run or thread deletion must not silently erase effect evidence.
func ConnectorActionSchema(dialect string) []string {
	identity, id, timestamp, payload, suffix := "TEXT", "TEXT", "TEXT", "TEXT", ""
	indexCreate := "CREATE INDEX IF NOT EXISTS "
	if dialect == "postgres" {
		timestamp, payload = "TIMESTAMPTZ", "JSONB"
	}
	if dialect == "mysql" {
		indexCreate = "CREATE INDEX "
		identity = "VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin"
		id = "VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin"
		timestamp, payload, suffix = "DATETIME(6)", "JSON", " ENGINE=InnoDB CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin"
	}
	return []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS connector_actions (
			id %s PRIMARY KEY, tenant_id %s NOT NULL, run_id %s NOT NULL,
			generation BIGINT NOT NULL, agent_id %s NOT NULL, connector %s NOT NULL,
			state VARCHAR(64) NOT NULL, version BIGINT NOT NULL,
			created_at %s NOT NULL, updated_at %s NOT NULL, dispatch_deadline_at %s NULL,
			payload %s NOT NULL, UNIQUE(tenant_id,id))%s`, id, identity, identity, identity, identity, timestamp, timestamp, timestamp, payload, suffix),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS connector_action_events (
			tenant_id %s NOT NULL, action_id %s NOT NULL, sequence BIGINT NOT NULL,
			mutation_id %s NOT NULL, mutation_digest VARCHAR(64) NOT NULL,
			kind VARCHAR(64) NOT NULL, observed_at %s NOT NULL, payload %s NOT NULL,
			PRIMARY KEY(action_id,sequence), UNIQUE(tenant_id,mutation_id),
			FOREIGN KEY(tenant_id,action_id) REFERENCES connector_actions(tenant_id,id) ON DELETE CASCADE)%s`, identity, id, id, timestamp, payload, suffix),
		indexCreate + `idx_connector_actions_tenant_run ON connector_actions(tenant_id,run_id,created_at,id)`,
		indexCreate + `idx_connector_actions_state ON connector_actions(state,updated_at,id)`,
		indexCreate + `idx_connector_actions_tenant_created ON connector_actions(tenant_id,created_at,id)`,
	}
}

func actionConflict(id, reason string) error {
	return &state.ErrConflict{Resource: "connector_action", ID: id, Reason: reason}
}
func actionUUID(s string) bool { u, e := uuid.Parse(s); return e == nil && u.String() == s }
func actionDigest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == sha256.Size && strings.ToLower(s) == s
}
func actionText(s string, max int) bool {
	return utf8.ValidString(s) && utf8.RuneCountInString(s) <= max && !strings.ContainsRune(s, '\x00')
}
func actionHash(v interface{}) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func actionScope(ctx context.Context, tid string) error {
	if tid == "" || (!tenant.IsSystem(ctx) && tid != tenant.FromContext(ctx)) {
		return &state.ErrNotFound{Resource: "connector_action", ID: ""}
	}
	return nil
}
func (d Driver) actionTime(t time.Time) interface{} {
	if d.Dialect == "sqlite" {
		return t.UTC().Format("2006-01-02T15:04:05.000000000Z")
	}
	return t.UTC()
}

func (d Driver) loadConnectorAction(ctx context.Context, tx Tx, id string, locked bool) (*models.ConnectorAction, error) {
	q := `SELECT payload FROM connector_actions WHERE id = ?`
	args := []interface{}{id}
	if !tenant.IsSystem(ctx) {
		q += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	if locked {
		q += d.lock()
	}
	var raw []byte
	if err := tx.Row(ctx, d.query(q), args...).Scan(&raw); err != nil {
		if isNoRows(err) {
			return nil, &state.ErrNotFound{Resource: "connector_action", ID: id}
		}
		return nil, err
	}
	var a models.ConnectorAction
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("decode connector action metadata: %w", err)
	}
	if a.ID != id {
		return nil, fmt.Errorf("invalid stored connector action identity")
	}
	if err := actionScope(ctx, a.TenantID); err != nil {
		return nil, err
	}
	return &a, nil
}

func (d Driver) lockConnectorActionRun(ctx context.Context, tx Tx, a *models.ConnectorAction, capture bool) error {
	if err := actionScope(ctx, a.TenantID); err != nil {
		return err
	}
	if err := d.LockRunCheckpoint(ctx, tx, state.RunCheckpointBinding{RunID: a.RunID, Generation: a.Generation, ThreadID: a.ThreadID}); err != nil {
		return err
	}
	var tid, agent, incarnation string
	var created storedTime
	var meta []byte
	if err := tx.Row(ctx, d.query(`SELECT tenant_id,agent_id,created_at,metadata,incarnation_id FROM runs WHERE run_id = ? AND tenant_id = ?`), a.RunID, a.TenantID).Scan(&tid, &agent, &created, &meta, &incarnation); err != nil {
		return err
	}
	if tid != a.TenantID || agent != a.AgentID {
		return actionConflict(a.ID, "run identity does not match action")
	}
	if incarnation == "" || (!capture && (a.RunIncarnationID == "" || a.RunIncarnationID != incarnation)) {
		return actionConflict(a.ID, "run incarnation does not match action")
	}
	a.RunCreatedAt = created.Time
	a.RunIncarnationID = incarnation
	var metadata struct {
		Manifest *struct {
			AgentVersion int `json:"agent_version"`
		} `json:"run_manifest"`
	}
	if err := json.Unmarshal(meta, &metadata); err != nil {
		return fmt.Errorf("decode action run metadata: %w", err)
	}
	if metadata.Manifest != nil {
		a.AgentVersion = metadata.Manifest.AgentVersion
	} else {
		a.AgentVersion = 0
	}
	return nil
}

func (d Driver) actionReplay(ctx context.Context, tx Tx, a *models.ConnectorAction, mutation, digest string) (bool, int64, error) {
	if !actionUUID(mutation) {
		return false, 0, fmt.Errorf("invalid connector action mutation ID")
	}
	var id, stored string
	var sequence int64
	err := tx.Row(ctx, d.query(`SELECT action_id,mutation_digest,sequence FROM connector_action_events WHERE tenant_id = ? AND mutation_id = ?`), a.TenantID, mutation).Scan(&id, &stored, &sequence)
	if isNoRows(err) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	if id != a.ID || stored != digest {
		return false, 0, actionConflict(a.ID, "mutation ID reused with different action metadata")
	}
	return true, sequence, nil
}
func actionEvent(a *models.ConnectorAction, kind, mutation string) *models.ConnectorActionEvent {
	observer := "runkite_connector_proxy"
	if kind == "observation_missing" {
		observer = "runkite_reconciler"
	}
	return &models.ConnectorActionEvent{ActionID: a.ID, TenantID: a.TenantID, Sequence: a.Version, MutationID: mutation, Kind: kind, ObservedAt: a.UpdatedAt, Observer: observer, State: a.State, Decision: a.Decision, ReasonCode: a.ReasonCode, RuleID: a.RuleID, AuthorizationSource: a.AuthorizationSource, BreakGlassID: a.BreakGlassID, PendingActionID: a.PendingActionID, DispatchID: a.DispatchID, ResponseKind: a.ResponseKind, HTTPStatus: a.HTTPStatus, RPCErrorCode: a.RPCErrorCode, ResponseDigest: a.ResponseDigest, ResponseBytes: a.ResponseBytes}
}
func (d Driver) saveConnectorAction(ctx context.Context, tx Tx, a *models.ConnectorAction, kind, mutation, digest string, create bool) error {
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	var deadline interface{}
	if a.DispatchDeadlineAt != nil {
		deadline = d.actionTime(*a.DispatchDeadlineAt)
	}
	if create {
		err = tx.Exec(ctx, d.query(`INSERT INTO connector_actions(id,tenant_id,run_id,generation,agent_id,connector,state,version,created_at,updated_at,dispatch_deadline_at,payload) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`), a.ID, a.TenantID, a.RunID, a.Generation, a.AgentID, a.Connector, a.State, a.Version, d.actionTime(a.CreatedAt), d.actionTime(a.UpdatedAt), deadline, raw)
	} else {
		err = tx.Exec(ctx, d.query(`UPDATE connector_actions SET state=?,version=?,updated_at=?,dispatch_deadline_at=?,payload=? WHERE id=? AND tenant_id=?`), a.State, a.Version, d.actionTime(a.UpdatedAt), deadline, raw, a.ID, a.TenantID)
	}
	if err != nil {
		return err
	}
	event := actionEvent(a, kind, mutation)
	eventRaw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return tx.Exec(ctx, d.query(`INSERT INTO connector_action_events(tenant_id,action_id,sequence,mutation_id,mutation_digest,kind,observed_at,payload) VALUES(?,?,?,?,?,?,?,?)`), a.TenantID, a.ID, a.Version, mutation, digest, kind, d.actionTime(a.UpdatedAt), eventRaw)
}

func (d Driver) CreateConnectorAction(ctx context.Context, input *models.ConnectorAction, mutation string) (*models.ConnectorAction, error) {
	if input == nil {
		return nil, fmt.Errorf("connector action required")
	}
	a := *input
	if a.TenantID == "" {
		a.TenantID = tenant.FromContext(ctx)
	}
	if !actionUUID(a.ID) || !actionUUID(mutation) || a.RunID == "" || a.ThreadID == "" || a.AgentID == "" || a.Connector == "" || a.Tool == "" || a.Generation < 0 || !actionDigest(a.ArgsDigest) {
		return nil, fmt.Errorf("invalid connector action binding")
	}
	for _, v := range []string{a.TenantID, a.RunID, a.ThreadID, a.AgentID, a.Connector, a.Tool, a.Principal, a.TraceID} {
		if !actionText(v, 255) {
			return nil, fmt.Errorf("connector action identity exceeds supported bounds")
		}
	}
	if a.Transport == "" {
		a.Transport = "json"
	}
	if a.Transport != "json" && a.Transport != "streamable_http" {
		return nil, fmt.Errorf("unsupported connector action transport")
	}
	if a.TargetRevision == nil || !models.ValidConnectorTargetRevision(*a.TargetRevision) {
		return nil, fmt.Errorf("connector action target binding required")
	}
	for _, v := range []*string{a.PolicyRevision, a.TargetRevision, a.ToolRevision, a.CredentialRef} {
		if v != nil && !actionText(*v, 255) {
			return nil, fmt.Errorf("invalid connector action provenance")
		}
	}
	// Caller data cannot seed state, response or dispatch authority.
	a.RunCreatedAt = time.Time{}
	a.RunIncarnationID = ""
	a.AgentVersion = 0
	a.State = models.ConnectorActionReceived
	a.Version = 1
	a.CreatedAt = time.Time{}
	a.UpdatedAt = time.Time{}
	a.Decision = ""
	a.ReasonCode = ""
	a.RuleID = ""
	a.AuthorizationSource = ""
	a.BreakGlassID = ""
	a.PendingActionID = ""
	a.DispatchID = ""
	a.DispatchDeadlineAt = nil
	a.ResponseKind = ""
	a.HTTPStatus = nil
	a.RPCErrorCode = nil
	a.ResponseDigest = ""
	a.ResponseBytes = nil
	digest := actionHash(a)
	tx, err := d.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := d.lockConnectorActionRun(ctx, tx, &a, true); err != nil {
		return nil, err
	}
	existing, err := d.loadConnectorAction(ctx, tx, a.ID, true)
	if err == nil {
		if existing.RunIncarnationID == "" || existing.RunIncarnationID != a.RunIncarnationID {
			return nil, actionConflict(a.ID, "run incarnation changed")
		}
		replayed, _, err := d.actionReplay(ctx, tx, existing, mutation, digest)
		if err != nil {
			return nil, err
		}
		if !replayed {
			return nil, actionConflict(a.ID, "action ID already exists")
		}
		return existing, nil
	}
	var missing *state.ErrNotFound
	if !errors.As(err, &missing) {
		return nil, err
	}
	if replayed, _, err := d.actionReplay(ctx, tx, &a, mutation, digest); err != nil || replayed {
		if err != nil {
			return nil, err
		}
		return nil, actionConflict(a.ID, "mutation already exists")
	}
	a.CreatedAt = time.Now().UTC().Truncate(time.Microsecond)
	a.UpdatedAt = a.CreatedAt
	if err := d.saveConnectorAction(ctx, tx, &a, "received", mutation, digest, true); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &a, nil
}

func (d Driver) RecordConnectorActionDecision(ctx context.Context, id string, version int64, dec models.ConnectorActionDecision, mutation string) (*models.ConnectorAction, error) {
	if dec.Effect != "allow" && dec.Effect != "deny" && dec.Effect != "pending" {
		return nil, fmt.Errorf("invalid connector action decision")
	}
	if dec.AuthorizationSource == "" {
		dec.AuthorizationSource = "policy"
	}
	if dec.AuthorizationSource != "policy" && dec.AuthorizationSource != "break_glass" {
		return nil, fmt.Errorf("invalid connector action authority source")
	}
	if dec.AuthorizationSource == "break_glass" && dec.BreakGlassID == "" {
		return nil, fmt.Errorf("break-glass evidence required")
	}
	for _, v := range []string{dec.ReasonCode, dec.RuleID, dec.PendingActionID, dec.BreakGlassID} {
		if !actionText(v, 255) {
			return nil, fmt.Errorf("invalid connector action decision metadata")
		}
	}
	if dec.PolicyRevision != nil && !actionText(*dec.PolicyRevision, 255) {
		return nil, fmt.Errorf("invalid connector policy revision")
	}
	digest := actionHash(struct {
		ID       string
		Version  int64
		Decision models.ConnectorActionDecision
	}{id, version, dec})
	tx, err := d.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	a, err := d.loadConnectorAction(ctx, tx, id, true)
	if err != nil {
		return nil, err
	}
	if replay, _, err := d.actionReplay(ctx, tx, a, mutation, digest); err != nil || replay {
		return a, err
	}
	if a.Version != version || a.State != models.ConnectorActionReceived {
		return nil, actionConflict(id, "decision transition is no longer current")
	}
	a.Decision = dec.Effect
	a.ReasonCode = dec.ReasonCode
	a.RuleID = dec.RuleID
	a.AuthorizationSource = dec.AuthorizationSource
	a.BreakGlassID = dec.BreakGlassID
	a.PendingActionID = dec.PendingActionID
	a.PolicyRevision = dec.PolicyRevision
	a.State = models.ConnectorActionReady
	if dec.AuthorizationSource != "break_glass" {
		if dec.Effect == "deny" {
			a.State = models.ConnectorActionBlocked
		}
		if dec.Effect == "pending" {
			a.State = models.ConnectorActionApprovalRequired
		}
	}
	a.Version++
	a.UpdatedAt = time.Now().UTC().Truncate(time.Microsecond)
	if err := d.saveConnectorAction(ctx, tx, a, "decision", mutation, digest, false); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

func (d Driver) peekActionApproval(ctx context.Context, tx Tx, a *models.ConnectorAction, id string, locked bool) (*models.PendingAction, error) {
	if a.TargetRevision == nil || !models.ValidConnectorTargetRevision(*a.TargetRevision) {
		return nil, nil
	}
	q := `SELECT id,decided_by FROM pending_actions WHERE tenant_id=? AND agent_id=? AND run_id=? AND generation=? AND connector=? AND tool=? AND args_digest=? AND status=? AND expires_at IS NULL AND run_incarnation_id=? AND target_revision=?`
	args := []interface{}{a.TenantID, a.AgentID, a.RunID, a.Generation, a.Connector, a.Tool, a.ArgsDigest, models.PendingStatusApproved, a.RunIncarnationID, *a.TargetRevision}
	if id != "" {
		q += ` AND id=?`
		args = append(args, id)
	}
	q += ` ORDER BY created_at ASC LIMIT 1`
	if locked {
		q += d.lock()
	}
	var found, reviewer string
	err := tx.Row(ctx, d.query(q), args...).Scan(&found, &reviewer)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &models.PendingAction{ID: found, DecidedBy: reviewer, TenantID: a.TenantID, AgentID: a.AgentID, RunID: a.RunID, RunIncarnationID: a.RunIncarnationID, TargetRevision: *a.TargetRevision, Generation: a.Generation, Connector: a.Connector, Tool: a.Tool, ArgsDigest: a.ArgsDigest, Status: models.PendingStatusApproved}, nil
}
func (d Driver) PeekApprovedConnectorAction(ctx context.Context, id string) (*models.PendingAction, error) {
	tx, err := d.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	a, err := d.loadConnectorAction(ctx, tx, id, false)
	if err != nil {
		return nil, err
	}
	if err := d.lockConnectorActionRun(ctx, tx, a, false); err != nil {
		return nil, err
	}
	return d.peekActionApproval(ctx, tx, a, "", false)
}

func (d Driver) PrepareConnectorActionDispatch(ctx context.Context, id string, version int64, approvalID, source string, deadline time.Time, mutation string) (*state.ConnectorActionDispatch, error) {
	deadline = deadline.UTC()
	digest := actionHash(struct {
		ID               string
		Version          int64
		Approval, Source string
		Deadline         time.Time
	}{id, version, approvalID, source, deadline})
	tx, err := d.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	a, err := d.loadConnectorAction(ctx, tx, id, false)
	if err != nil {
		return nil, err
	}
	// Replays return no new execution authority, even after the run became
	// terminal. New dispatches use the same durable run fence as checkpoints.
	if replay, seq, err := d.actionReplay(ctx, tx, a, mutation, digest); err != nil || replay {
		if err != nil {
			return nil, err
		}
		a, err = d.loadConnectorAction(ctx, tx, id, true)
		if err != nil {
			return nil, err
		}
		return &state.ConnectorActionDispatch{ActionID: a.ID, TenantID: a.TenantID, DispatchID: a.DispatchID, Version: seq}, nil
	}
	if err := d.lockConnectorActionRun(ctx, tx, a, false); err != nil {
		return nil, err
	}
	a, err = d.loadConnectorAction(ctx, tx, id, true)
	if err != nil {
		return nil, err
	}
	if replay, seq, err := d.actionReplay(ctx, tx, a, mutation, digest); err != nil || replay {
		if err != nil {
			return nil, err
		}
		return &state.ConnectorActionDispatch{ActionID: a.ID, TenantID: a.TenantID, DispatchID: a.DispatchID, Version: seq}, nil
	}
	if a.TargetRevision == nil || !models.ValidConnectorTargetRevision(*a.TargetRevision) {
		return nil, actionConflict(id, "connector target binding required")
	}
	if a.Version != version || (a.State != models.ConnectorActionReady && a.State != models.ConnectorActionApprovalRequired) {
		return nil, actionConflict(id, "action is not ready for dispatch")
	}
	if deadline.IsZero() || !deadline.After(time.Now().UTC()) {
		return nil, fmt.Errorf("connector dispatch deadline must be in the future")
	}
	if approvalID != "" {
		approved, err := d.peekActionApproval(ctx, tx, a, approvalID, true)
		if err != nil {
			return nil, err
		}
		if approved == nil {
			return nil, actionConflict(id, "approved capability is unavailable")
		}
		if err := tx.Exec(ctx, d.query(`UPDATE pending_actions SET status=?,updated_at=?,pending_key=NULL WHERE id=? AND tenant_id=? AND status=?`), models.PendingStatusConsumed, d.now(time.Now().UTC()), approved.ID, a.TenantID, models.PendingStatusApproved); err != nil {
			return nil, err
		}
		a.PendingActionID = approved.ID
		a.AuthorizationSource = "approved_capability"
	} else {
		if a.Decision != "allow" && (a.AuthorizationSource != "break_glass" || a.BreakGlassID == "") {
			return nil, actionConflict(id, "action requires approved authority")
		}
		if source != a.AuthorizationSource {
			return nil, actionConflict(id, "authorization source changed")
		}
	}
	if a.Decision == "deny" && a.AuthorizationSource != "break_glass" {
		return nil, actionConflict(id, "a current denial cannot be overridden by approval")
	}
	a.State = models.ConnectorActionDispatchIntent
	a.DispatchID = uuid.NewString()
	a.DispatchDeadlineAt = &deadline
	a.Version++
	a.UpdatedAt = time.Now().UTC().Truncate(time.Microsecond)
	if err := d.saveConnectorAction(ctx, tx, a, "dispatch_intent", mutation, digest, false); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &state.ConnectorActionDispatch{ActionID: a.ID, TenantID: a.TenantID, DispatchID: a.DispatchID, Version: a.Version, Applied: true}, nil
}

func (d Driver) FinishConnectorAction(ctx context.Context, id string, version int64, dispatchID string, obs models.ConnectorActionObservation, mutation string) (*models.ConnectorAction, error) {
	if obs.State != models.ConnectorActionNotDispatched && obs.State != models.ConnectorActionResponseObserved && obs.State != models.ConnectorActionOutcomeUnknown {
		return nil, fmt.Errorf("invalid connector action observation")
	}
	if !actionText(obs.ReasonCode, 255) || !actionText(obs.ResponseKind, 64) || (obs.ResponseDigest != "" && !actionDigest(obs.ResponseDigest)) {
		return nil, fmt.Errorf("invalid connector observation metadata")
	}
	if obs.HTTPStatus != nil && (*obs.HTTPStatus < 100 || *obs.HTTPStatus > 599) || obs.ResponseBytes != nil && *obs.ResponseBytes < 0 {
		return nil, fmt.Errorf("invalid connector observation measurements")
	}
	digest := actionHash(struct {
		ID          string
		Version     int64
		Dispatch    string
		Observation models.ConnectorActionObservation
	}{id, version, dispatchID, obs})
	tx, err := d.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	a, err := d.loadConnectorAction(ctx, tx, id, true)
	if err != nil {
		return nil, err
	}
	if replay, _, err := d.actionReplay(ctx, tx, a, mutation, digest); err != nil || replay {
		return a, err
	}
	if dispatchID == "" {
		if obs.State != models.ConnectorActionNotDispatched || a.DispatchID != "" || a.Version != version || (a.State != models.ConnectorActionReceived && a.State != models.ConnectorActionReady && a.State != models.ConnectorActionApprovalRequired) {
			return nil, actionConflict(id, "action cannot be marked not dispatched")
		}
	} else {
		if !actionUUID(dispatchID) || a.DispatchID != dispatchID || obs.State == models.ConnectorActionNotDispatched {
			return nil, actionConflict(id, "observation does not match original dispatch")
		}
		if a.State != models.ConnectorActionDispatchIntent && a.State != models.ConnectorActionOutcomeUnknown {
			return nil, actionConflict(id, "dispatch observation is already resolved")
		}
		if a.State == models.ConnectorActionDispatchIntent && a.Version != version {
			return nil, actionConflict(id, "dispatch observation version changed")
		}
		// The original receipt may refine a sweep's unknown outcome. It does
		// not need, or receive, authority from the run's replacement attempt.
	}
	a.State = obs.State
	a.ReasonCode = obs.ReasonCode
	a.ResponseKind = obs.ResponseKind
	a.HTTPStatus = obs.HTTPStatus
	a.RPCErrorCode = obs.RPCErrorCode
	a.ResponseDigest = obs.ResponseDigest
	a.ResponseBytes = obs.ResponseBytes
	a.Version++
	a.UpdatedAt = time.Now().UTC().Truncate(time.Microsecond)
	if err := d.saveConnectorAction(ctx, tx, a, "observation", mutation, digest, false); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

func (d Driver) GetConnectorAction(ctx context.Context, id string) (*models.ConnectorAction, error) {
	tx, err := d.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	return d.loadConnectorAction(ctx, tx, id, false)
}
func actionLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	if limit > 1000 {
		return 1000
	}
	return limit
}
func (d Driver) SearchConnectorActions(ctx context.Context, request *models.ConnectorActionSearchRequest) ([]*models.ConnectorAction, error) {
	if request == nil {
		request = &models.ConnectorActionSearchRequest{}
	}
	q := `SELECT payload FROM connector_actions WHERE 1=1`
	var args []interface{}
	tid := request.TenantID
	if !tenant.IsSystem(ctx) {
		if tid != "" && tid != tenant.FromContext(ctx) {
			return []*models.ConnectorAction{}, nil
		}
		tid = tenant.FromContext(ctx)
	}
	for _, filter := range []struct{ column, value string }{{"tenant_id", tid}, {"run_id", request.RunID}, {"agent_id", request.AgentID}, {"connector", request.Connector}, {"state", request.State}} {
		if filter.value != "" {
			q += ` AND ` + filter.column + `=?`
			args = append(args, filter.value)
		}
	}
	if request.Cursor != "" {
		cur, err := pagecursor.DecodeTime(request.Cursor)
		if err != nil {
			return nil, err
		}
		q += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
		args = append(args, d.actionTime(cur.Time), d.actionTime(cur.Time), cur.ID)
	}
	q += ` ORDER BY created_at DESC,id DESC LIMIT ?`
	args = append(args, actionLimit(request.Limit))
	if request.Offset > 0 && request.Cursor == "" {
		q += ` OFFSET ?`
		args = append(args, request.Offset)
	}
	tx, err := d.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	rows, err := actionRows(ctx, tx, d.query(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []*models.ConnectorAction{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var a models.ConnectorAction
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
		result = append(result, &a)
	}
	return result, rows.Err()
}
func (d Driver) ListConnectorActionEvents(ctx context.Context, id string, after int64, limit int) ([]*models.ConnectorActionEvent, error) {
	tx, err := d.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	a, err := d.loadConnectorAction(ctx, tx, id, false)
	if err != nil {
		return nil, err
	}
	rows, err := actionRows(ctx, tx, d.query(`SELECT payload FROM connector_action_events WHERE tenant_id=? AND action_id=? AND sequence>? ORDER BY sequence ASC LIMIT ?`), a.TenantID, id, after, actionLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []*models.ConnectorActionEvent{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var e models.ConnectorActionEvent
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, err
		}
		result = append(result, &e)
	}
	return result, rows.Err()
}

func (d Driver) actionCandidates(ctx context.Context, condition string, cutoff time.Time, limit int) ([]string, error) {
	tx, err := d.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	q := `SELECT id FROM connector_actions WHERE ` + condition
	args := []interface{}{d.actionTime(cutoff)}
	if !tenant.IsSystem(ctx) {
		q += ` AND tenant_id=?`
		args = append(args, tenant.FromContext(ctx))
	}
	q += ` ORDER BY updated_at,id LIMIT ?`
	args = append(args, actionLimit(limit))
	rows, err := actionRows(ctx, tx, d.query(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
func (d Driver) MarkExpiredConnectorActionsUnknown(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	ids, err := d.actionCandidates(ctx, `state='dispatch_intent' AND dispatch_deadline_at<=?`, cutoff, limit)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, id := range ids {
		tx, err := d.Begin(ctx)
		if err != nil {
			return count, err
		}
		a, err := d.loadConnectorAction(ctx, tx, id, true)
		if err != nil {
			_ = tx.Rollback(context.Background())
			var missing *state.ErrNotFound
			if errors.As(err, &missing) {
				continue
			}
			return count, err
		}
		if a.State != models.ConnectorActionDispatchIntent || a.DispatchDeadlineAt == nil || a.DispatchDeadlineAt.After(cutoff) {
			_ = tx.Rollback(context.Background())
			continue
		}
		a.State = models.ConnectorActionOutcomeUnknown
		a.ReasonCode = "dispatch_observation_missing"
		a.Version++
		a.UpdatedAt = time.Now().UTC().Truncate(time.Microsecond)
		mutation := uuid.NewString()
		digest := actionHash(struct {
			ID      string
			Version int64
		}{id, a.Version})
		if err := d.saveConnectorAction(ctx, tx, a, "observation_missing", mutation, digest, false); err != nil {
			_ = tx.Rollback(context.Background())
			return count, err
		}
		if err := tx.Commit(ctx); err != nil {
			_ = tx.Rollback(context.Background())
			return count, err
		}
		count++
	}
	return count, nil
}
func (d Driver) PruneConnectorActions(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	ids, err := d.actionCandidates(ctx, `state IN ('blocked','not_dispatched','response_observed') AND updated_at<?`, cutoff, limit)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, id := range ids {
		tx, err := d.Begin(ctx)
		if err != nil {
			return count, err
		}
		a, err := d.loadConnectorAction(ctx, tx, id, true)
		if err != nil {
			_ = tx.Rollback(context.Background())
			var missing *state.ErrNotFound
			if errors.As(err, &missing) {
				continue
			}
			return count, err
		}
		if !a.UpdatedAt.Before(cutoff) || (a.State != models.ConnectorActionBlocked && a.State != models.ConnectorActionNotDispatched && a.State != models.ConnectorActionResponseObserved) {
			_ = tx.Rollback(context.Background())
			continue
		}
		if err := tx.Exec(ctx, d.query(`DELETE FROM connector_actions WHERE id=? AND tenant_id=?`), id, a.TenantID); err != nil {
			_ = tx.Rollback(context.Background())
			return count, err
		}
		if err := tx.Commit(ctx); err != nil {
			_ = tx.Rollback(context.Background())
			return count, err
		}
		count++
	}
	return count, nil
}
