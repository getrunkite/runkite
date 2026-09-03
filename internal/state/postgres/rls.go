package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/getrunkite/runkite/internal/tenant"
)

// Tables that carry tenant_id and should be covered when RLS is enabled.
// terminal_hook_claims is intentionally omitted (run_id PK only; no tenant column).
var rlsTables = []string{
	"agents",
	"agent_versions",
	"agent_schemas",
	"registry_entries",
	"registry_entry_versions",
	"threads",
	"runs",
	"thread_checkpoints",
	"opaque_checkpoints",
	"store_items",
	"webhook_dead_letters",
	"audit_events",
	"run_cache",
	"cron_schedules",
	"cron_claims",
	"policy_grants",
	"pending_actions",
	"kill_switches",
	"break_glass_windows",
	"mandatory_hitl_rules",
	"usage_events",
	"usage_holds",
}

// applyTenantGUC stamps session vars used by RLS policies onto conn.
// is_system=true (tenant.SystemContext) sees all rows; otherwise rows must
// match app.tenant_id. Uses session-level set_config (is_local=false) so
// the GUC is visible to the subsequent Query/Exec on the same acquired
// connection; AfterRelease clears it before the conn returns to the pool.
func applyTenantGUC(ctx context.Context, conn *pgx.Conn) error {
	if tenant.IsSystem(ctx) {
		_, err := conn.Exec(ctx,
			`SELECT set_config('app.tenant_id', '', false), set_config('app.is_system', 'true', false)`)
		return err
	}
	tid := tenant.FromContext(ctx)
	_, err := conn.Exec(ctx,
		`SELECT set_config('app.tenant_id', $1, false), set_config('app.is_system', 'false', false)`,
		tid)
	return err
}

func clearTenantGUC(conn *pgx.Conn) {
	// Best-effort reset on release so a leaked acquire path cannot carry
	// a prior tenant into the next checkout. Background ctx: release
	// already happened outside the request.
	_, _ = conn.Exec(context.Background(),
		`SELECT set_config('app.tenant_id', '', false), set_config('app.is_system', 'false', false)`)
}

// ensureRLS enables FORCE ROW LEVEL SECURITY and tenant policies on every
// tenant-scoped table. Idempotent. Policies allow system context through
// app.is_system; otherwise tenant_id must equal app.tenant_id.
func (s *Store) ensureRLS(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	for _, table := range rlsTables {
		stmts := []string{
			fmt.Sprintf(`ALTER TABLE %s ENABLE ROW LEVEL SECURITY`, table),
			fmt.Sprintf(`ALTER TABLE %s FORCE ROW LEVEL SECURITY`, table),
			fmt.Sprintf(`DROP POLICY IF EXISTS runkite_tenant_isolation ON %s`, table),
			fmt.Sprintf(`CREATE POLICY runkite_tenant_isolation ON %s
				FOR ALL
				USING (
					current_setting('app.is_system', true) = 'true'
					OR tenant_id = current_setting('app.tenant_id', true)
				)
				WITH CHECK (
					current_setting('app.is_system', true) = 'true'
					OR tenant_id = current_setting('app.tenant_id', true)
				)`, table),
		}
		for _, stmt := range stmts {
			if _, err := conn.Exec(ctx, stmt); err != nil {
				// Table may not exist yet on a partial migrate; skip missing.
				if strings.Contains(err.Error(), "does not exist") {
					continue
				}
				return fmt.Errorf("rls %s: %w", table, err)
			}
		}
	}
	return nil
}
