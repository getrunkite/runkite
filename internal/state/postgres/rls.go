package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/getrunkite/runkite/internal/tenant"
)

// rlsAppRole is a NOBYPASSRLS role SET ROLE'd on tenant acquires so FORCE
// RLS applies even when POSTGRES_DSN is a superuser (common in CI/dev).
const rlsAppRole = "runkite_app"

// Tables that carry tenant_id and should be covered when RLS is enabled.
// terminal_hook_claims is intentionally omitted (run_id PK only; no tenant column).
// Keep in sync with ensureRLS / TestRLS_TableListMatchesTenantColumns.
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
// System context keeps the login role (RESET ROLE) so migrations/admin
// see every row. Tenant context SETs ROLE runkite_app so FORCE RLS applies
// even when the DSN user is a superuser.
//
// After Init+ensureRLS, failClosed is true: SET ROLE must succeed (no
// silent bypass if the role is missing).
func (s *Store) applyTenantGUC(ctx context.Context, conn *pgx.Conn) error {
	if tenant.IsSystem(ctx) {
		_, err := conn.Exec(ctx, `
			SELECT set_config('app.tenant_id', '', false),
			       set_config('app.is_system', 'true', false);
			RESET ROLE`)
		return err
	}
	tid := tenant.FromContext(ctx)
	_, err := conn.Exec(ctx,
		`SELECT set_config('app.tenant_id', $1, false), set_config('app.is_system', 'false', false)`,
		tid)
	if err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `SET ROLE `+rlsAppRole); err != nil {
		// Before ensureRLS, the role may not exist yet (Ping/early acquire).
		if !s.rlsReady && strings.Contains(err.Error(), "does not exist") {
			return nil
		}
		return fmt.Errorf("SET ROLE %s: %w (RUNKITE_POSTGRES_RLS requires role membership; see docs)", rlsAppRole, err)
	}
	return nil
}

func clearTenantGUC(conn *pgx.Conn) {
	_, _ = conn.Exec(context.Background(), `
		SELECT set_config('app.tenant_id', '', false),
		       set_config('app.is_system', 'false', false);
		RESET ROLE`)
}

// ensureRLS enables FORCE ROW LEVEL SECURITY and tenant policies on every
// tenant-scoped table, and ensures runkite_app exists for tenant acquires.
func (s *Store) ensureRLS(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	// CREATE ROLE needs CREATEROLE (or superuser). GRANT … SET TRUE is
	// required on Postgres 16+ so the login role can SET ROLE the app role
	// (CREATEROLE auto-grants ADMIN on the created role, but not SET).
	// Do not also request ADMIN TRUE: for a CREATEROLE non-superuser, the
	// creator is already admin of the new role and re-granting ADMIN to
	// yourself fails with "ADMIN option cannot be granted back to your
	// own grantor", rolling back the whole bootstrap transaction.
	_, err = conn.Exec(ctx, `
		DO $$ BEGIN
			CREATE ROLE `+rlsAppRole+` NOINHERIT NOBYPASSRLS;
		EXCEPTION WHEN duplicate_object THEN NULL;
		END $$;
		GRANT USAGE ON SCHEMA public TO `+rlsAppRole+`;
		GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO `+rlsAppRole+`;
		GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA public TO `+rlsAppRole+`;
		ALTER DEFAULT PRIVILEGES IN SCHEMA public
			GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO `+rlsAppRole+`;
		ALTER DEFAULT PRIVILEGES IN SCHEMA public
			GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO `+rlsAppRole+`;
		GRANT `+rlsAppRole+` TO CURRENT_USER WITH SET TRUE;
	`)
	if err != nil {
		return fmt.Errorf("rls app role bootstrap (DSN needs CREATEROLE or superuser): %w", err)
	}

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
				if strings.Contains(err.Error(), "does not exist") {
					continue
				}
				return fmt.Errorf("rls %s: %w", table, err)
			}
		}
	}
	s.rlsReady = true
	return nil
}

// disableRLS drops Runkite's tenant policies and turns off FORCE/ENABLE RLS
// on known tables. Called from Init when RUNKITE_POSTGRES_RLS is off so a
// prior enable does not leave sticky FORCE (deny-all for non-BYPASS users).
// No-op when no runkite_tenant_isolation policy exists (avoids ALTER noise
// on DBs that never opted into RLS).
func (s *Store) disableRLS(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	var n int
	if err := conn.QueryRow(ctx,
		`SELECT COUNT(*) FROM pg_policies WHERE policyname = 'runkite_tenant_isolation'`,
	).Scan(&n); err != nil {
		return fmt.Errorf("detect rls policies: %w", err)
	}
	if n == 0 {
		s.rlsReady = false
		return nil
	}

	for _, table := range rlsTables {
		stmts := []string{
			fmt.Sprintf(`DROP POLICY IF EXISTS runkite_tenant_isolation ON %s`, table),
			fmt.Sprintf(`ALTER TABLE %s NO FORCE ROW LEVEL SECURITY`, table),
			fmt.Sprintf(`ALTER TABLE %s DISABLE ROW LEVEL SECURITY`, table),
		}
		for _, stmt := range stmts {
			if _, err := conn.Exec(ctx, stmt); err != nil {
				if strings.Contains(err.Error(), "does not exist") {
					continue
				}
				return fmt.Errorf("disable rls %s: %w", table, err)
			}
		}
	}
	s.rlsReady = false
	return nil
}
