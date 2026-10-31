package mysql

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state/migrate"
	"github.com/getrunkite/runkite/internal/tenant"
)

// Like the existing MySQL conformance suite, these tests own the dedicated
// MYSQL_DSN database. Never point them at a running deployment.
func legacyIdentityStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Skip("MYSQL_DSN not set")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Leave the dedicated schema ready for later independent package tests,
		// even when the fixture deliberately inserted an old case-alias FK.
		if err := s.TruncateAll(context.Background()); err != nil {
			t.Errorf("fixture cleanup: %v", err)
		}
		if err := s.Init(context.Background()); err != nil {
			t.Errorf("restore current schema: %v", err)
		}
		_ = s.Close()
	})
	err = s.withSchemaLock(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if err := s.baselineDown(ctx, conn); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `DROP TABLE IF EXISTS schema_migrations`); err != nil {
			return err
		}
		return migrate.Upgrade(ctx, migrate.NewSQL(conn, migrate.MySQL), s.migrations(conn)[:16], nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func assertIdentitySchema(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	version, err := migrate.NewSQL(s.db, migrate.MySQL).Current(ctx)
	if err != nil || version != 17 {
		t.Fatalf("identity schema version=%d err=%v", version, err)
	}
	for _, table := range identityColumns {
		for _, column := range table.columns {
			var actual string
			if err := s.db.QueryRowContext(ctx, `SELECT COLLATION_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`, table.table, column).Scan(&actual); err != nil || actual != identityCollation {
				t.Fatalf("identity comparison differs at %s.%s: %s err=%v", table.table, column, actual, err)
			}
		}
	}
	for _, fk := range identityForeignKeys {
		if err := validateIdentityForeignKey(ctx, s.db, fk, fk.name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIdentityCollationMigrationPreservesDefinitionsDataAndCascades(t *testing.T) {
	s := legacyIdentityStore(t)
	ctx := tenant.WithContext(context.Background(), "Acme")
	type definition struct {
		columnType, nullable, extra string
		defaultValue                sql.NullString
	}
	definitions := map[string]definition{}
	for _, table := range identityColumns {
		for _, column := range table.columns {
			var d definition
			if err := s.db.QueryRowContext(ctx, `SELECT COLUMN_TYPE, IS_NULLABLE, COLUMN_DEFAULT, EXTRA FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND COLUMN_NAME=?`, table.table, column).Scan(&d.columnType, &d.nullable, &d.defaultValue, &d.extra); err != nil {
				t.Fatal(err)
			}
			definitions[table.table+"."+column] = d
		}
	}
	var displayCollation, statusCollation string
	if err := s.db.QueryRowContext(ctx, `SELECT COLLATION_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='agents' AND COLUMN_NAME='name'`).Scan(&displayCollation); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COLLATION_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='threads' AND COLUMN_NAME='status'`).Scan(&statusCollation); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAgent(ctx, &models.Agent{AgentID: "AgentA", Name: "Display name"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAgentSchema(ctx, &models.AgentSchema{AgentID: "AgentA"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.CreateThread(ctx, &models.Thread{ThreadID: "ThreadA", Status: models.ThreadStatusBusy, Values: map[string]interface{}{"saved": "before migration"}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, &models.Run{RunID: "RunA", ThreadID: "ThreadA", AgentID: "AgentA", Status: models.RunStatusPending, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	created := now.Format(time.RFC3339)
	if err := s.SaveCheckpoint(ctx, "ThreadA", &models.ThreadState{Values: map[string]interface{}{"saved": "checkpoint"}, Checkpoint: models.ThreadCheckpoint{ThreadID: "ThreadA", CheckpointID: "CheckpointA"}, CreatedAt: &created}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutOpaqueCheckpoint(ctx, "ThreadA", "OpaqueA", []byte("native bytes"), "test", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	assertIdentitySchema(t, s)
	for key, old := range definitions {
		parts := strings.SplitN(key, ".", 2)
		var d definition
		if err := s.db.QueryRowContext(ctx, `SELECT COLUMN_TYPE, IS_NULLABLE, COLUMN_DEFAULT, EXTRA FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND COLUMN_NAME=?`, parts[0], parts[1]).Scan(&d.columnType, &d.nullable, &d.defaultValue, &d.extra); err != nil || d != old {
			t.Fatalf("identity definition changed beyond collation at %s: old=%+v now=%+v err=%v", key, old, d, err)
		}
	}
	for _, check := range []struct{ table, column, want string }{{"agents", "name", displayCollation}, {"threads", "status", statusCollation}} {
		var actual string
		if err := s.db.QueryRowContext(ctx, `SELECT COLLATION_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND COLUMN_NAME=?`, check.table, check.column).Scan(&actual); err != nil || actual != check.want {
			t.Fatalf("nonidentity collation changed at %s.%s: %s err=%v", check.table, check.column, actual, err)
		}
	}
	if agent, err := s.GetAgent(ctx, "AgentA"); err != nil || agent.Name != "Display name" {
		t.Fatalf("existing data changed: agent=%+v err=%v", agent, err)
	}
	if blob, err := s.GetOpaqueCheckpoint(ctx, "ThreadA", "OpaqueA"); err != nil || string(blob.Data) != "native bytes" {
		t.Fatalf("opaque data changed: blob=%+v err=%v", blob, err)
	}
	if err := s.UpsertAgent(tenant.WithContext(ctx, "acme"), &models.Agent{AgentID: "AgentA", Name: "Neighbor"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Downgrade(ctx); err == nil || !strings.Contains(err.Error(), "backup") {
		t.Fatalf("unsafe identity downgrade was not refused: %v", err)
	}
	assertIdentitySchema(t, s)
	if err := s.DeleteThread(ctx, "ThreadA"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"runs", "thread_checkpoints", "opaque_checkpoints"} {
		var count int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quotedIdentifier(table)+" WHERE thread_id='ThreadA'").Scan(&count); err != nil || count != 0 {
			t.Fatalf("thread cascade not restored for %s: count=%d err=%v", table, count, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM agents WHERE tenant_id='Acme' AND agent_id='AgentA'`); err != nil {
		t.Fatal(err)
	}
	var schemas int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_schemas WHERE tenant_id='Acme'`).Scan(&schemas); err != nil || schemas != 0 {
		t.Fatalf("agent schema cascade not restored: count=%d err=%v", schemas, err)
	}
}

func TestIdentityCollationMigrationResumesAfterInterruptedDDL(t *testing.T) {
	for _, phase := range []string{"partial_columns", "applied_before_stamp"} {
		t.Run(phase, func(t *testing.T) {
			s := legacyIdentityStore(t)
			ctx := context.Background()
			if err := s.withSchemaLock(ctx, func(ctx context.Context, conn *sql.Conn) error {
				if phase == "applied_before_stamp" {
					return s.upIdentityCollation(ctx, conn)
				}
				for _, fk := range identityForeignKeys {
					var name string
					if err := conn.QueryRowContext(ctx, `SELECT CONSTRAINT_NAME FROM INFORMATION_SCHEMA.REFERENTIAL_CONSTRAINTS WHERE CONSTRAINT_SCHEMA=DATABASE() AND TABLE_NAME=?`, fk.table).Scan(&name); err != nil {
						return err
					}
					if _, err := conn.ExecContext(ctx, "ALTER TABLE "+quotedIdentifier(fk.table)+" DROP FOREIGN KEY "+quotedIdentifier(name)); err != nil {
						return err
					}
				}
				_, err := conn.ExecContext(ctx, `ALTER TABLE agents MODIFY COLUMN tenant_id VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL DEFAULT 'default', MODIFY COLUMN agent_id VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if version, err := migrate.NewSQL(s.db, migrate.MySQL).Current(ctx); err != nil || version != 16 {
				t.Fatalf("fixture must remain unrecorded at16: version=%d err=%v", version, err)
			}
			if err := s.Init(ctx); err != nil {
				t.Fatal(err)
			}
			assertIdentitySchema(t, s)
			if err := s.Init(ctx); err != nil {
				t.Fatalf("completed migration is not idempotent: %v", err)
			}
		})
	}
}

func TestIdentityCollationMigrationRejectsAliasedForeignKeyBeforeDDL(t *testing.T) {
	s := legacyIdentityStore(t)
	ctx := tenant.WithContext(context.Background(), "owner")
	now := time.Now().UTC()
	if err := s.CreateThread(ctx, &models.Thread{ThreadID: "Parent", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, &models.Run{RunID: "child", ThreadID: "parent", Status: models.RunStatusPending, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("legacy case-insensitive fixture was not created: %v", err)
	}
	if err := s.Init(ctx); err == nil || !strings.Contains(err.Error(), "nonexact foreign-key references") {
		t.Fatalf("legacy alias reference was not refused: %v", err)
	}
	assertIdentityPreflightUnchanged(t, s, len(identityForeignKeys))
}

func assertIdentityPreflightUnchanged(t *testing.T, s *Store, wantConstraints int) {
	t.Helper()
	ctx := context.Background()
	var collation string
	if err := s.db.QueryRowContext(ctx, `SELECT COLLATION_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='agents' AND COLUMN_NAME='tenant_id'`).Scan(&collation); err != nil || collation == identityCollation {
		t.Fatalf("failed preflight modified columns: collation=%s err=%v", collation, err)
	}
	var constraints int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM INFORMATION_SCHEMA.REFERENTIAL_CONSTRAINTS WHERE CONSTRAINT_SCHEMA=DATABASE()`).Scan(&constraints); err != nil || constraints != wantConstraints {
		t.Fatalf("failed preflight removed foreign keys: count=%d err=%v", constraints, err)
	}
	if version, err := migrate.NewSQL(s.db, migrate.MySQL).Current(ctx); err != nil || version != 16 {
		t.Fatalf("failed preflight was stamped: version=%d err=%v", version, err)
	}
}

func TestIdentityCollationMigrationRejectsAliasedTenantBeforeDDL(t *testing.T) {
	for _, child := range []string{"runs", "thread_checkpoints", "opaque_checkpoints"} {
		t.Run(child, func(t *testing.T) {
			s := legacyIdentityStore(t)
			ctx := tenant.WithContext(context.Background(), "Acme")
			now := time.Now().UTC()
			if err := s.CreateThread(ctx, &models.Thread{ThreadID: "parent", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			// Seed the old FK shape directly: thread_id is byte-exact, but
			// ownership was a case alias under the old tenant comparison.
			var query string
			switch child {
			case "runs":
				query = `INSERT INTO runs (tenant_id, run_id, thread_id) VALUES ('acme', 'child', 'parent')`
			case "thread_checkpoints":
				query = `INSERT INTO thread_checkpoints (tenant_id, checkpoint_id, thread_id) VALUES ('acme', 'child', 'parent')`
			case "opaque_checkpoints":
				query = `INSERT INTO opaque_checkpoints (tenant_id, checkpoint_id, thread_id, data) VALUES ('acme', 'child', 'parent', X'01')`
			}
			if _, err := s.db.ExecContext(ctx, query); err != nil {
				t.Fatalf("legacy tenant-alias fixture was not created: %v", err)
			}
			if err := s.Init(ctx); err == nil || !strings.Contains(err.Error(), "nonexact foreign-key references in "+child) {
				t.Fatalf("legacy tenant-alias reference was not refused: %v", err)
			}
			assertIdentityPreflightUnchanged(t, s, len(identityForeignKeys))
		})
	}
}

func TestIdentityCollationMigrationRejectsCustomForeignKeysBeforeDDL(t *testing.T) {
	for _, shape := range []string{"incoming_reference", "canonical_name_collision"} {
		t.Run(shape, func(t *testing.T) {
			s := legacyIdentityStore(t)
			ctx := context.Background()
			const customTable = "runkite_test_custom_identity_fk"
			t.Cleanup(func() {
				if _, err := s.db.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+quotedIdentifier(customTable)); err != nil {
					t.Errorf("remove custom FK fixture: %v", err)
				}
			})
			query := "CREATE TABLE " + quotedIdentifier(customTable) + " (thread_id VARCHAR(255), FOREIGN KEY (thread_id) REFERENCES threads(thread_id)) ENGINE=InnoDB CHARSET=utf8mb4"
			want := "custom foreign-key references to threads"
			if shape == "canonical_name_collision" {
				query = "CREATE TABLE " + quotedIdentifier(customTable) + " (id BIGINT PRIMARY KEY, parent_id BIGINT, CONSTRAINT fk_runs_thread_identity FOREIGN KEY (parent_id) REFERENCES " + quotedIdentifier(customTable) + "(id)) ENGINE=InnoDB"
				want = "already used by a custom table"
			}
			if _, err := s.db.ExecContext(ctx, query); err != nil {
				t.Fatal(err)
			}
			if err := s.Init(ctx); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("custom schema conflict was not refused before DDL: %v", err)
			}
			assertIdentityPreflightUnchanged(t, s, len(identityForeignKeys)+1)
		})
	}
}
