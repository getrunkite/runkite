package sqlcompletion

import (
	"context"
	"database/sql"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

// WithRunIdentity keeps the original run row locked for the entire mutation.
// A second read outside this transaction cannot provide the same guarantee.
func (d Driver) WithRunIdentity(ctx context.Context, runID, incarnation string, mutate func(Tx, string) error) error {
	if incarnation == "" {
		return &state.ErrConflict{Resource: "run", ID: runID, Reason: "original record identity required for finalization"}
	}
	tx, err := d.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	tid, _, _, _, err := d.load(ctx, tx, runID)
	if err != nil {
		return err
	}
	var current string
	if err := tx.Row(ctx, d.query(`SELECT incarnation_id FROM runs WHERE run_id=? AND tenant_id=?`), runID, tid).Scan(&current); err != nil {
		return err
	}
	if current != incarnation {
		return &state.ErrConflict{Resource: "run", ID: runID, Reason: "original record identity changed during finalization"}
	}
	if err := mutate(tx, tid); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func legacyFinalizationConflict(resource, id string) error {
	return &state.ErrConflict{Resource: resource, ID: id, Reason: "legacy unbound record blocks finalization; use a fresh run ID or explicitly reconcile legacy accounting"}
}

func (d Driver) WriteRunUsageEvent(ctx context.Context, incarnation string, event *models.UsageEvent) error {
	if event == nil {
		return nil
	}
	return d.WithRunIdentity(ctx, event.RunID, incarnation, func(tx Tx, tid string) error {
		if event.TenantID != "" && event.TenantID != tid {
			return &state.ErrConflict{Resource: "usage event", ID: event.RunID, Reason: "run tenant mismatch"}
		}
		var legacy int
		if err := tx.Row(ctx, d.query(`SELECT COUNT(*) FROM usage_events WHERE run_incarnation_id='' AND (run_id=? OR id=?)`), event.RunID, event.ID).Scan(&legacy); err != nil {
			return err
		}
		if legacy != 0 {
			return legacyFinalizationConflict("usage event", event.RunID)
		}
		copy := *event
		copy.RunIncarnationID = incarnation
		copy.TenantID = tid
		return d.writeUsageEvent(ctx, tx, &copy)
	})
}

// WriteUsageEvent retains the trusted, unbound Go-store compatibility path.
// API finalization must use WriteRunUsageEvent instead.
func (d Driver) WriteUsageEvent(ctx context.Context, event *models.UsageEvent) error {
	if event == nil {
		return nil
	}
	copy := *event
	copy.RunIncarnationID = ""
	if copy.TenantID == "" {
		copy.TenantID = tenant.FromContext(ctx)
	}
	tx, err := d.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := d.writeUsageEvent(ctx, tx, &copy); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Usage tables retain their existing encoding because their half-open range
// readers format SQLite bounds with RFC3339Nano as well.
func (d Driver) usageTimestamp(ts time.Time) interface{} {
	if d.Dialect == "sqlite" {
		return ts.UTC().Format(time.RFC3339Nano)
	}
	return ts.UTC()
}

func (d Driver) writeUsageEvent(ctx context.Context, tx Tx, event *models.UsageEvent) error {
	ts := event.TS
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	source := event.Source
	if source == "" {
		source = models.UsageSourceTerminalOutput
	}
	q := `INSERT INTO usage_events(id,ts,tenant_id,run_id,run_incarnation_id,agent_id,principal,model,tokens_in,tokens_out,usd_estimate,source) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`
	if d.Dialect == "mysql" {
		// The API supplies an identity-specific primary key. Guard its ownership
		// explicitly because MySQL's duplicate-key clause covers every unique key.
		var idRun, idInc, idSource string
		err := tx.Row(ctx, `SELECT run_id,run_incarnation_id,source FROM usage_events WHERE id=?`, event.ID).Scan(&idRun, &idInc, &idSource)
		if err == nil && (idRun != event.RunID || idInc != event.RunIncarnationID || idSource != source) {
			return &state.ErrConflict{Resource: "usage event", ID: event.ID, Reason: "usage identity already belongs to another record"}
		}
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		q += ` ON DUPLICATE KEY UPDATE ts=VALUES(ts),tenant_id=VALUES(tenant_id),agent_id=VALUES(agent_id),principal=VALUES(principal),model=VALUES(model),tokens_in=VALUES(tokens_in),tokens_out=VALUES(tokens_out),usd_estimate=VALUES(usd_estimate)`
	} else {
		q += ` ON CONFLICT(run_id,run_incarnation_id,source) DO UPDATE SET ts=excluded.ts,tenant_id=excluded.tenant_id,agent_id=excluded.agent_id,principal=excluded.principal,model=excluded.model,tokens_in=excluded.tokens_in,tokens_out=excluded.tokens_out,usd_estimate=excluded.usd_estimate`
	}
	return tx.Exec(ctx, d.query(q), event.ID, d.usageTimestamp(ts), event.TenantID, event.RunID, event.RunIncarnationID, event.AgentID, event.Principal, event.Model, event.TokensIn, event.TokensOut, event.USDEstimate, source)
}

func (d Driver) UpsertRunUsageHold(ctx context.Context, incarnation string, hold *models.UsageHold) error {
	if hold == nil {
		return nil
	}
	return d.WithRunIdentity(ctx, hold.RunID, incarnation, func(tx Tx, tid string) error {
		if hold.TenantID != "" && hold.TenantID != tid {
			return &state.ErrConflict{Resource: "usage hold", ID: hold.RunID, Reason: "run tenant mismatch"}
		}
		var status models.RunStatus
		if err := tx.Row(ctx, d.query(`SELECT status FROM runs WHERE run_id=? AND tenant_id=?`), hold.RunID, tid).Scan(&status); err != nil {
			return err
		}
		// Admission may race cancellation after inserting the run. Its late
		// reservation must not recreate a hold that finalization released.
		if state.TerminalRunStatus(status) {
			return nil
		}
		var legacy int
		if err := tx.Row(ctx, d.query(`SELECT COUNT(*) FROM usage_holds WHERE run_id=? AND run_incarnation_id=''`), hold.RunID).Scan(&legacy); err != nil {
			return err
		}
		if legacy != 0 {
			return legacyFinalizationConflict("usage hold", hold.RunID)
		}
		ts := hold.CreatedAt
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		q := `INSERT INTO usage_holds(run_id,run_incarnation_id,tenant_id,agent_id,usd_hold,tokens_hold,created_at) VALUES(?,?,?,?,?,?,?)`
		if d.Dialect == "mysql" {
			q += ` ON DUPLICATE KEY UPDATE run_incarnation_id=VALUES(run_incarnation_id),tenant_id=VALUES(tenant_id),agent_id=VALUES(agent_id),usd_hold=VALUES(usd_hold),tokens_hold=VALUES(tokens_hold),created_at=VALUES(created_at)`
		} else {
			q += ` ON CONFLICT(run_id) DO UPDATE SET run_incarnation_id=excluded.run_incarnation_id,tenant_id=excluded.tenant_id,agent_id=excluded.agent_id,usd_hold=excluded.usd_hold,tokens_hold=excluded.tokens_hold,created_at=excluded.created_at`
		}
		return tx.Exec(ctx, d.query(q), hold.RunID, incarnation, tid, hold.AgentID, hold.USDHold, hold.TokensHold, d.usageTimestamp(ts))
	})
}

func (d Driver) ReleaseRunUsageHold(ctx context.Context, runID, incarnation string) error {
	return d.WithRunIdentity(ctx, runID, incarnation, func(tx Tx, _ string) error {
		var legacy int
		if err := tx.Row(ctx, d.query(`SELECT COUNT(*) FROM usage_holds WHERE run_id=? AND run_incarnation_id=''`), runID).Scan(&legacy); err != nil {
			return err
		}
		if legacy != 0 {
			return legacyFinalizationConflict("usage hold", runID)
		}
		return tx.Exec(ctx, d.query(`DELETE FROM usage_holds WHERE run_id=? AND run_incarnation_id=?`), runID, incarnation)
	})
}
