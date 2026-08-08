package mysql_test

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/state/mysql"
)

// Live schema smoke test for checkpoint 1. Skips when MySQL isn't up
// (CI/local without docker-compose.test.yml's mysql service).
func TestInit_CreatesCoreAndGovernanceTablesIdempotently(t *testing.T) {
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		dsn = "runkite:runkite@tcp(127.0.0.1:3307)/runkite_test?parseTime=true"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	store, err := mysql.New(ctx, dsn)
	if err != nil {
		t.Skipf("mysql not available: %v", err)
	}
	defer store.Close()

	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init #1: %v", err)
	}
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init #2 (idempotent): %v", err)
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	want := []string{
		"agents", "agent_versions", "agent_schemas",
		"registry_entries", "registry_entry_versions",
		"threads", "runs", "thread_checkpoints",
		"store_items", "webhook_dead_letters", "run_cache",
		"cron_schedules", "cron_claims",
		"audit_events", "policy_grants", "pending_actions",
		"kill_switches", "break_glass_windows", "mandatory_hitl_rules",
	}
	for _, table := range want {
		var name string
		err := db.QueryRowContext(ctx, `
			SELECT table_name FROM information_schema.tables
			WHERE table_schema = DATABASE() AND table_name = ?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("missing table %s: %v", table, err)
		}
	}

	var tbl, create string
	if err := db.QueryRowContext(ctx, "SHOW CREATE TABLE runs").Scan(&tbl, &create); err != nil {
		t.Fatalf("SHOW CREATE TABLE runs: %v", err)
	}
	for _, needle := range []string{
		"PRIMARY KEY (`run_id`)",
		"KEY `idx_runs_status` (`status`)",
		"KEY `idx_runs_tenant` (`tenant_id`)",
		"KEY `idx_runs_root` (`root_run_id`)",
		"KEY `idx_runs_parent` (`parent_run_id`)",
		"FOREIGN KEY (`thread_id`) REFERENCES `threads` (`thread_id`) ON DELETE CASCADE",
		"utf8mb4",
	} {
		if !strings.Contains(create, needle) {
			t.Fatalf("runs DDL missing %q\n%s", needle, create)
		}
	}
}
