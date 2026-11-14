package sqlcompletion

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/google/uuid"
)

func validActionSinkID(value string) bool {
	if len(value) > 64 {
		return false
	}
	for _, c := range value {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

func validDeliveryReason(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, c := range value {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// ActionDeliverySchema deliberately has no action/run foreign key. Delivery
// recovery remains possible after explicit action evidence retention.
func ActionDeliverySchema(dialect string) []string {
	identity, id, timestamp, payload, suffix := "TEXT", "TEXT", "TEXT", "TEXT", ""
	if dialect == "postgres" {
		timestamp = "TIMESTAMPTZ"
		payload = "JSONB"
	}
	if dialect == "mysql" {
		identity = "VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin"
		id = "VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin"
		timestamp = "DATETIME(6)"
		payload = "JSON"
		suffix = " ENGINE=InnoDB CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin"
	}
	table := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS action_deliveries (
 id %s PRIMARY KEY,sink_id %s NOT NULL,tenant_id %s NOT NULL,action_id %s NOT NULL,
 sequence BIGINT NOT NULL,payload %s NOT NULL,attempts INTEGER NOT NULL DEFAULT 0,
 status VARCHAR(32) NOT NULL,available_at %s NOT NULL,claim_token VARCHAR(64) NOT NULL DEFAULT '',
 claim_until %s NULL,last_reason_code VARCHAR(64) NOT NULL DEFAULT '',created_at %s NOT NULL,updated_at %s NOT NULL,
 UNIQUE(tenant_id,action_id,sequence,sink_id))%s`, id, id, identity, id, payload, timestamp, timestamp, timestamp, timestamp, suffix)
	// Index declarations are separated so MySQL's retryable migration can
	// independently recover from auto-committed DDL.
	return []string{table}
}

func (d Driver) appendActionDelivery(ctx context.Context, tx Tx, a *models.ConnectorAction, e *models.ConnectorActionEvent) error {
	if a.ExportSinkID == "" {
		return nil
	}
	if !validActionSinkID(a.ExportSinkID) {
		return fmt.Errorf("invalid action export sink identity")
	}
	id := uuid.NewSHA1(uuid.NameSpaceURL, []byte(a.TenantID+"\x00"+a.ID+"\x00"+strconv.FormatInt(e.Sequence, 10)+"\x00"+a.ExportSinkID)).String()
	raw, err := json.Marshal(state.ActionDeliveryPayload{Schema: "runkite.action-event.v1", DeliveryID: id, SinkID: a.ExportSinkID, Action: a, Event: e})
	if err != nil {
		return err
	}
	if len(raw) > state.MaxActionDeliveryPayloadBytes {
		return fmt.Errorf("action export metadata exceeds supported bounds")
	}
	return tx.Exec(ctx, d.query(`INSERT INTO action_deliveries(id,sink_id,tenant_id,action_id,sequence,payload,attempts,status,available_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`), id, a.ExportSinkID, a.TenantID, a.ID, e.Sequence, raw, 0, state.ActionDeliveryPending, d.actionTime(e.ObservedAt), d.actionTime(e.ObservedAt), d.actionTime(e.ObservedAt))
}

const actionDeliveryColumns = `id,sink_id,tenant_id,action_id,sequence,payload,attempts,status,available_at,claim_token,claim_until,last_reason_code,created_at,updated_at`

func scanActionDelivery(row Row) (*state.ActionDelivery, error) {
	var a state.ActionDelivery
	var available, created, updated storedTime
	var claimUntil nullablePendingTime
	if err := row.Scan(&a.ID, &a.SinkID, &a.TenantID, &a.ActionID, &a.Sequence, &a.PayloadRaw, &a.Attempts, &a.Status, &available, &a.ClaimToken, &claimUntil, &a.LastReasonCode, &created, &updated); err != nil {
		return nil, err
	}
	a.AvailableAt = available.Time
	a.CreatedAt = created.Time
	a.UpdatedAt = updated.Time
	if claimUntil.present {
		a.ClaimUntil = &claimUntil.Time
	}
	return &a, nil
}

func deliveryConflict(id string) error {
	return &state.ErrConflict{Resource: "action_delivery", ID: id, Reason: "delivery claim is no longer current"}
}
func requireDeliveryWorker(ctx context.Context) error {
	if !tenant.IsSystem(ctx) {
		return fmt.Errorf("action delivery worker requires system authority")
	}
	return nil
}

func (d Driver) ClaimActionDeliveries(ctx context.Context, sinkID string, now, leaseUntil time.Time, limit int) ([]*state.ActionDelivery, error) {
	if err := requireDeliveryWorker(ctx); err != nil {
		return nil, err
	}
	if sinkID == "" || !validActionSinkID(sinkID) || now.IsZero() || !leaseUntil.After(now) || limit < 1 || limit > 100 {
		return nil, fmt.Errorf("invalid action delivery claim")
	}
	tx, err := d.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	q := `SELECT ` + actionDeliveryColumns + ` FROM action_deliveries WHERE sink_id=? AND status=? AND available_at<=? AND (claim_until IS NULL OR claim_until<=?) ORDER BY available_at,id LIMIT ?`
	if d.Dialect != "sqlite" {
		q += ` FOR UPDATE SKIP LOCKED`
	}
	rows, err := actionRows(ctx, tx, d.query(q), sinkID, state.ActionDeliveryPending, d.actionTime(now), d.actionTime(now), limit)
	if err != nil {
		return nil, err
	}
	out := []*state.ActionDelivery{}
	for rows.Next() {
		a, err := scanActionDelivery(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for _, a := range out {
		a.ClaimToken = uuid.NewString()
		a.ClaimUntil = &leaseUntil
		a.Attempts++
		a.UpdatedAt = now
		if err := tx.Exec(ctx, d.query(`UPDATE action_deliveries SET claim_token=?,claim_until=?,attempts=attempts+1,updated_at=? WHERE id=?`), a.ClaimToken, d.actionTime(leaseUntil), d.actionTime(now), a.ID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

func (d Driver) currentActionDelivery(ctx context.Context, tx Tx, id, token string) (*state.ActionDelivery, error) {
	if !actionUUID(id) || !actionUUID(token) {
		return nil, deliveryConflict(id)
	}
	a, err := scanActionDelivery(tx.Row(ctx, d.query(`SELECT `+actionDeliveryColumns+` FROM action_deliveries WHERE id=? AND claim_token=? AND status=?`+d.lock()), id, token, state.ActionDeliveryPending))
	if isNoRows(err) {
		return nil, deliveryConflict(id)
	}
	if err != nil {
		return nil, err
	}
	if a.ClaimUntil == nil || !a.ClaimUntil.After(time.Now().UTC()) {
		return nil, deliveryConflict(id)
	}
	return a, nil
}

func (d Driver) AckActionDelivery(ctx context.Context, id, token string) error {
	if err := requireDeliveryWorker(ctx); err != nil {
		return err
	}
	tx, err := d.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := d.currentActionDelivery(ctx, tx, id, token); err != nil {
		return err
	}
	if err := tx.Exec(ctx, d.query(`UPDATE action_deliveries SET status=?,claim_token='',claim_until=NULL,last_reason_code='',updated_at=? WHERE id=? AND claim_token=?`), state.ActionDeliveryDelivered, d.actionTime(time.Now().UTC()), id, token); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (d Driver) FailActionDelivery(ctx context.Context, id, token string, nextAt time.Time, reason string, dead bool) error {
	if err := requireDeliveryWorker(ctx); err != nil {
		return err
	}
	if !validDeliveryReason(reason) || nextAt.IsZero() {
		return fmt.Errorf("invalid action delivery failure metadata")
	}
	tx, err := d.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := d.currentActionDelivery(ctx, tx, id, token); err != nil {
		return err
	}
	status := state.ActionDeliveryPending
	if dead {
		status = state.ActionDeliveryDeadLetter
	}
	if err := tx.Exec(ctx, d.query(`UPDATE action_deliveries SET status=?,available_at=?,claim_token='',claim_until=NULL,last_reason_code=?,updated_at=? WHERE id=? AND claim_token=?`), status, d.actionTime(nextAt), reason, d.actionTime(time.Now().UTC()), id, token); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (d Driver) SearchActionDeliveries(ctx context.Context, req *state.ActionDeliverySearchRequest) ([]*state.ActionDelivery, error) {
	if req == nil {
		req = &state.ActionDeliverySearchRequest{}
	}
	if req.Offset < 0 || req.Offset > 10000 || req.Limit < 0 || req.Limit > 100 {
		return nil, fmt.Errorf("invalid action delivery pagination")
	}
	limit := req.Limit
	if limit == 0 {
		limit = 50
	}
	if req.Status != "" && req.Status != state.ActionDeliveryPending && req.Status != state.ActionDeliveryDelivered && req.Status != state.ActionDeliveryDeadLetter {
		return nil, fmt.Errorf("invalid action delivery status")
	}
	tx, err := d.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	q := `SELECT ` + actionDeliveryColumns + ` FROM action_deliveries WHERE 1=1`
	args := []interface{}{}
	if !tenant.IsSystem(ctx) {
		if req.TenantID != "" && req.TenantID != tenant.FromContext(ctx) {
			return []*state.ActionDelivery{}, nil
		}
		q += ` AND tenant_id=?`
		args = append(args, tenant.FromContext(ctx))
	} else if req.TenantID != "" {
		q += ` AND tenant_id=?`
		args = append(args, req.TenantID)
	}
	if req.SinkID != "" {
		if !validActionSinkID(req.SinkID) {
			return nil, fmt.Errorf("invalid action export sink identity")
		}
		q += ` AND sink_id=?`
		args = append(args, req.SinkID)
	}
	if req.Status != "" {
		q += ` AND status=?`
		args = append(args, req.Status)
	}
	q += ` ORDER BY created_at DESC,id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, req.Offset)
	rows, err := actionRows(ctx, tx, d.query(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*state.ActionDelivery{}
	for rows.Next() {
		a, err := scanActionDelivery(rows)
		if err != nil {
			return nil, err
		}
		a.ClaimToken = ""
		out = append(out, a)
	}
	return out, rows.Err()
}

func (d Driver) RetryActionDelivery(ctx context.Context, tenantID, id string) error {
	if tenantID == "" || (!tenant.IsSystem(ctx) && tenantID != tenant.FromContext(ctx)) {
		return &state.ErrNotFound{Resource: "action_delivery", ID: id}
	}
	tx, err := d.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var status string
	err = tx.Row(ctx, d.query(`SELECT status FROM action_deliveries WHERE id=? AND tenant_id=?`+d.lock()), id, tenantID).Scan(&status)
	if isNoRows(err) {
		return &state.ErrNotFound{Resource: "action_delivery", ID: id}
	}
	if err != nil {
		return err
	}
	if status != state.ActionDeliveryDeadLetter {
		return deliveryConflict(id)
	}
	if err := tx.Exec(ctx, d.query(`UPDATE action_deliveries SET status=?,available_at=?,claim_token='',claim_until=NULL,last_reason_code='',updated_at=? WHERE id=? AND tenant_id=?`), state.ActionDeliveryPending, d.actionTime(time.Now().UTC()), d.actionTime(time.Now().UTC()), id, tenantID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
