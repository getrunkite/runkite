package sqlite

import (
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state/migrate"
	"github.com/getrunkite/runkite/internal/tenant"
)

func TestFinalizationMigrationPreservesUnboundHistory(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := tenant.SystemContext(t.Context())
	migrations := store.migrations()
	if err := migrate.Upgrade(ctx, migrate.NewSQL(store.db, migrate.SQLite), migrations[:len(migrations)-1], nil); err != nil {
		t.Fatal(err)
	}
	ts := formatTS(time.Now().UTC())
	if _, err := store.db.ExecContext(ctx, `INSERT INTO usage_events(id,ts,tenant_id,run_id,agent_id,tokens_in,source) VALUES(?,?,?,?,?,?,?)`, "legacy-usage", ts, "acme", "reused", "agent", 13, models.UsageSourceTerminalOutput); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO usage_holds(run_id,tenant_id,agent_id,tokens_hold,created_at) VALUES(?,?,?,?,?)`, "reused", "acme", "agent", 17, ts); err != nil {
		t.Fatal(err)
	}
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	var usageIdentity, holdIdentity string
	var tokens, hold int64
	if err := store.db.QueryRowContext(ctx, `SELECT run_incarnation_id,tokens_in FROM usage_events WHERE id='legacy-usage'`).Scan(&usageIdentity, &tokens); err != nil || usageIdentity != "" || tokens != 13 {
		t.Fatal("migration changed or guessed legacy accounting", usageIdentity, tokens, err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT run_incarnation_id,tokens_hold FROM usage_holds WHERE run_id='reused'`).Scan(&holdIdentity, &hold); err != nil || holdIdentity != "" || hold != 17 {
		t.Fatal("migration changed or guessed legacy hold", holdIdentity, hold, err)
	}
	if err := store.Init(ctx); err != nil {
		t.Fatal("migration replay was not idempotent", err)
	}
}
