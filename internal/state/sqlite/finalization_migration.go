package sqlite

import "context"

// Existing usage rows remain explicitly unbound. Their original physical
// records cannot be reconstructed from a reusable public ID or timestamp.
func (s *SQLiteStore) upRunFinalization(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `CREATE TABLE usage_events_record_upgrade (
 id TEXT PRIMARY KEY,ts TEXT NOT NULL,tenant_id TEXT NOT NULL DEFAULT 'default',run_id TEXT NOT NULL DEFAULT '',
 run_incarnation_id TEXT NOT NULL DEFAULT '',agent_id TEXT NOT NULL DEFAULT '',principal TEXT NOT NULL DEFAULT '',model TEXT NOT NULL DEFAULT '',
 tokens_in INTEGER NOT NULL DEFAULT 0,tokens_out INTEGER NOT NULL DEFAULT 0,usd_estimate REAL NOT NULL DEFAULT 0,source TEXT NOT NULL DEFAULT '',
 UNIQUE(run_id,run_incarnation_id,source));
 INSERT INTO usage_events_record_upgrade(id,ts,tenant_id,run_id,agent_id,principal,model,tokens_in,tokens_out,usd_estimate,source)
 SELECT id,ts,tenant_id,run_id,agent_id,principal,model,tokens_in,tokens_out,usd_estimate,source FROM usage_events;
 DROP TABLE usage_events;
 ALTER TABLE usage_events_record_upgrade RENAME TO usage_events;
 CREATE INDEX idx_usage_events_tenant_ts ON usage_events(tenant_id,ts);
 CREATE INDEX idx_usage_events_tenant_agent_ts ON usage_events(tenant_id,agent_id,ts);
 CREATE INDEX idx_usage_events_run ON usage_events(run_id);
 ALTER TABLE usage_holds ADD COLUMN run_incarnation_id TEXT NOT NULL DEFAULT '';`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
