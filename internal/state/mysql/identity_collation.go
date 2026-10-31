package mysql

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/getrunkite/runkite/internal/state/migrate"
)

const identityCollation = "utf8mb4_0900_bin"

// Only identity and security-scope columns change comparison semantics. Display
// names, descriptions, statuses and other human search fields retain theirs.
var identityColumns = []struct {
	table   string
	columns []string
}{
	{"agents", []string{"tenant_id", "agent_id"}},
	{"agent_versions", []string{"tenant_id", "agent_id"}},
	{"agent_schemas", []string{"tenant_id", "agent_id"}},
	{"registry_entries", []string{"tenant_id", "name"}},
	{"registry_entry_versions", []string{"tenant_id", "name"}},
	{"threads", []string{"tenant_id", "thread_id"}},
	{"runs", []string{"tenant_id", "run_id", "thread_id", "agent_id", "parent_run_id", "root_run_id"}},
	{"thread_checkpoints", []string{"tenant_id", "checkpoint_id", "thread_id", "checkpoint_ns", "parent_id"}},
	{"opaque_checkpoints", []string{"tenant_id", "thread_id", "checkpoint_id", "framework"}},
	{"store_items", []string{"tenant_id", "namespace", "key"}},
	{"webhook_dead_letters", []string{"id", "tenant_id", "run_id"}},
	{"run_cache", []string{"tenant_id", "cache_key", "agent_id"}},
	{"cron_schedules", []string{"tenant_id", "name", "agent_id"}},
	{"cron_claims", []string{"tenant_id", "schedule_name"}},
	{"terminal_hook_claims", []string{"run_id"}},
	{"audit_events", []string{"id", "tenant_id", "resource_id", "rule_id", "run_id", "agent_id", "connector", "tool", "trace_id"}},
	{"policy_grants", []string{"id", "tenant_id", "agent_id", "connector"}},
	{"pending_actions", []string{"id", "tenant_id", "run_id", "agent_id", "connector", "tool", "rule_id", "args_digest"}},
	{"kill_switches", []string{"id", "tenant_id", "agent_id"}},
	{"break_glass_windows", []string{"id", "tenant_id", "agent_id"}},
	{"mandatory_hitl_rules", []string{"id", "tenant_id", "agent_id", "connector"}},
	{"usage_events", []string{"id", "tenant_id", "run_id", "agent_id", "principal", "model", "source"}},
	{"usage_holds", []string{"run_id", "tenant_id", "agent_id"}},
	{"finops_overlays", []string{"id"}},
}

type identityForeignKey struct {
	table, name, parent string
	columns, references []string
}

var identityForeignKeys = []identityForeignKey{
	{"agent_schemas", "fk_agent_schemas_agent_identity", "agents", []string{"tenant_id", "agent_id"}, []string{"tenant_id", "agent_id"}},
	{"runs", "fk_runs_thread_identity", "threads", []string{"thread_id"}, []string{"thread_id"}},
	{"thread_checkpoints", "fk_thread_checkpoints_thread_identity", "threads", []string{"thread_id"}, []string{"thread_id"}},
	{"opaque_checkpoints", "fk_opaque_checkpoints_thread_identity", "threads", []string{"thread_id"}, []string{"thread_id"}},
}

func quotedIdentifier(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

func quotedColumns(columns []string) string {
	out := make([]string, len(columns))
	for i, column := range columns {
		out[i] = quotedIdentifier(column)
	}
	return strings.Join(out, ", ")
}

func (s *Store) upIdentityCollation(ctx context.Context, db migrate.DB) error {
	var available int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLLATIONS WHERE COLLATION_NAME = ? AND CHARACTER_SET_NAME = 'utf8mb4' AND PAD_ATTRIBUTE = 'NO PAD'`, identityCollation).Scan(&available); err != nil {
		return fmt.Errorf("mysql: identity migration requires MySQL 8 with %s NO PAD: %w", identityCollation, err)
	}
	if available != 1 {
		return fmt.Errorf("mysql: identity migration requires %s with NO PAD", identityCollation)
	}
	if err := preflightIdentityReferences(ctx, db); err != nil {
		return err
	}

	// Inspect all definitions and existing references before dropping anything.
	// Old insensitive FKs may have accepted case-alias links. Never silently
	// change those links or resume service without enforceable constraints.
	type existingKey struct{ table, name string }
	var drop []existingKey
	for _, fk := range identityForeignKeys {
		last := ""
		for {
			var name string
			err := db.QueryRowContext(ctx, `SELECT CONSTRAINT_NAME FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE WHERE BINARY CONSTRAINT_SCHEMA = BINARY DATABASE() AND BINARY TABLE_NAME = BINARY ? AND REFERENCED_TABLE_NAME IS NOT NULL AND ORDINAL_POSITION = 1 AND BINARY CONSTRAINT_NAME > BINARY ? ORDER BY BINARY CONSTRAINT_NAME LIMIT 1`, fk.table, last).Scan(&name)
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				return err
			}
			if err := validateIdentityForeignKey(ctx, db, fk, name); err != nil {
				return err
			}
			drop = append(drop, existingKey{fk.table, name})
			last = name
		}
		var joins, present []string
		for i, column := range fk.columns {
			joins = append(joins, "BINARY c."+quotedIdentifier(column)+" = BINARY p."+quotedIdentifier(fk.references[i]))
			present = append(present, "c."+quotedIdentifier(column)+" IS NOT NULL")
		}
		if fk.parent == "threads" {
			// These legacy FKs contain only thread_id. Their owners must also
			// match exactly before formerly insensitive tenants become distinct.
			joins = append(joins, "BINARY c.`tenant_id` = BINARY p.`tenant_id`")
		}
		query := "SELECT COUNT(*) FROM " + quotedIdentifier(fk.table) + " c LEFT JOIN " + quotedIdentifier(fk.parent) + " p ON " + strings.Join(joins, " AND ") + " WHERE " + strings.Join(present, " AND ") + " AND p." + quotedIdentifier(fk.references[0]) + " IS NULL"
		var aliases int
		if err := db.QueryRowContext(ctx, query).Scan(&aliases); err != nil {
			return err
		}
		if aliases != 0 {
			return fmt.Errorf("mysql: identity migration found %d nonexact foreign-key references in %s; repair explicitly before upgrading", aliases, fk.table)
		}
	}
	// Validate and prepare column alterations before any constraint is removed.
	type alteration struct{ table, clauses string }
	var alters []alteration
	for _, table := range identityColumns {
		var clauses []string
		for _, column := range table.columns {
			clause, err := identityColumnAlteration(ctx, db, table.table, column)
			if err != nil {
				return err
			}
			if clause != "" {
				clauses = append(clauses, clause)
			}
		}
		if len(clauses) > 0 {
			alters = append(alters, alteration{table.table, strings.Join(clauses, ", ")})
		}
	}
	for _, fk := range drop {
		if _, err := db.ExecContext(ctx, "ALTER TABLE "+quotedIdentifier(fk.table)+" DROP FOREIGN KEY "+quotedIdentifier(fk.name)); err != nil {
			return fmt.Errorf("mysql: drop identity constraint %s.%s: %w", fk.table, fk.name, err)
		}
	}
	for _, alter := range alters {
		if _, err := db.ExecContext(ctx, "ALTER TABLE "+quotedIdentifier(alter.table)+" "+alter.clauses); err != nil {
			return fmt.Errorf("mysql: alter identity columns in %s: %w", alter.table, err)
		}
	}
	for _, fk := range identityForeignKeys {
		statement := "ALTER TABLE " + quotedIdentifier(fk.table) + " ADD CONSTRAINT " + quotedIdentifier(fk.name) + " FOREIGN KEY (" + quotedColumns(fk.columns) + ") REFERENCES " + quotedIdentifier(fk.parent) + " (" + quotedColumns(fk.references) + ") ON DELETE CASCADE"
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("mysql: restore identity constraint %s.%s: %w", fk.table, fk.name, err)
		}
	}
	return nil
}

func preflightIdentityReferences(ctx context.Context, db migrate.DB) error {
	for _, fk := range identityForeignKeys {
		var collisions int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM INFORMATION_SCHEMA.REFERENTIAL_CONSTRAINTS WHERE BINARY CONSTRAINT_SCHEMA = BINARY DATABASE() AND CONSTRAINT_NAME = ? AND BINARY TABLE_NAME <> BINARY ?`, fk.name, fk.table).Scan(&collisions); err != nil {
			return err
		}
		if collisions != 0 {
			return fmt.Errorf("mysql: identity constraint name %s is already used by a custom table; repair explicitly before upgrading", fk.name)
		}
	}
	for _, table := range identityColumns {
		var selected, allowed []string
		args := []any{table.table}
		for _, column := range table.columns {
			selected = append(selected, "?")
			args = append(args, column)
		}
		for _, fk := range identityForeignKeys {
			if fk.parent != table.table {
				continue
			}
			for i, column := range fk.columns {
				allowed = append(allowed, "(BINARY TABLE_SCHEMA = BINARY DATABASE() AND BINARY TABLE_NAME = BINARY ? AND BINARY COLUMN_NAME = BINARY ? AND BINARY REFERENCED_COLUMN_NAME = BINARY ?)")
				args = append(args, fk.table, column, fk.references[i])
			}
		}
		query := "SELECT COUNT(*) FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE WHERE BINARY REFERENCED_TABLE_SCHEMA = BINARY DATABASE() AND BINARY REFERENCED_TABLE_NAME = BINARY ? AND BINARY REFERENCED_COLUMN_NAME IN (" + strings.Join(selected, ",") + ")"
		if len(allowed) > 0 {
			query += " AND NOT (" + strings.Join(allowed, " OR ") + ")"
		}
		var unknown int
		if err := db.QueryRowContext(ctx, query, args...).Scan(&unknown); err != nil {
			return err
		}
		if unknown != 0 {
			return fmt.Errorf("mysql: identity migration found custom foreign-key references to %s; repair explicitly before upgrading", table.table)
		}
	}
	return nil
}

func validateIdentityForeignKey(ctx context.Context, db migrate.DB, fk identityForeignKey, name string) error {
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE WHERE BINARY CONSTRAINT_SCHEMA = BINARY DATABASE() AND BINARY TABLE_NAME = BINARY ? AND BINARY CONSTRAINT_NAME = BINARY ?`, fk.table, name).Scan(&count); err != nil {
		return err
	}
	if count != len(fk.columns) {
		return fmt.Errorf("mysql: unsupported foreign-key shape on %s.%s", fk.table, name)
	}
	for i, column := range fk.columns {
		var actual, parent, referenced string
		var sameSchema bool
		if err := db.QueryRowContext(ctx, `SELECT COLUMN_NAME, REFERENCED_TABLE_NAME, REFERENCED_COLUMN_NAME, BINARY REFERENCED_TABLE_SCHEMA = BINARY DATABASE() FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE WHERE BINARY CONSTRAINT_SCHEMA = BINARY DATABASE() AND BINARY TABLE_NAME = BINARY ? AND BINARY CONSTRAINT_NAME = BINARY ? AND ORDINAL_POSITION = ?`, fk.table, name, i+1).Scan(&actual, &parent, &referenced, &sameSchema); err != nil {
			return err
		}
		if actual != column || parent != fk.parent || referenced != fk.references[i] || !sameSchema {
			return fmt.Errorf("mysql: unsupported foreign-key shape on %s.%s", fk.table, name)
		}
	}
	var onDelete, onUpdate string
	if err := db.QueryRowContext(ctx, `SELECT DELETE_RULE, UPDATE_RULE FROM INFORMATION_SCHEMA.REFERENTIAL_CONSTRAINTS WHERE BINARY CONSTRAINT_SCHEMA = BINARY DATABASE() AND BINARY TABLE_NAME = BINARY ? AND BINARY CONSTRAINT_NAME = BINARY ?`, fk.table, name).Scan(&onDelete, &onUpdate); err != nil {
		return err
	}
	if onDelete != "CASCADE" || (onUpdate != "RESTRICT" && onUpdate != "NO ACTION") {
		return fmt.Errorf("mysql: unsupported foreign-key actions on %s.%s", fk.table, name)
	}
	return nil
}

func identityColumnAlteration(ctx context.Context, db migrate.DB, table, column string) (string, error) {
	var columnType, nullable, extra, charset, collation string
	var defaultValue sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT COLUMN_TYPE, IS_NULLABLE, COLUMN_DEFAULT, EXTRA, CHARACTER_SET_NAME, COLLATION_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE BINARY TABLE_SCHEMA = BINARY DATABASE() AND BINARY TABLE_NAME = BINARY ? AND BINARY COLUMN_NAME = BINARY ?`, table, column).Scan(&columnType, &nullable, &defaultValue, &extra, &charset, &collation); err != nil {
		return "", fmt.Errorf("mysql: inspect identity column %s.%s: %w", table, column, err)
	}
	if collation == identityCollation {
		return "", nil
	}
	width := strings.TrimSuffix(strings.TrimPrefix(columnType, "varchar("), ")")
	n, err := strconv.Atoi(width)
	if err != nil || n <= 0 || columnType != "varchar("+width+")" || charset != "utf8mb4" || extra != "" || (nullable != "YES" && nullable != "NO") {
		return "", fmt.Errorf("mysql: unsupported definition for identity column %s.%s", table, column)
	}
	clause := "MODIFY COLUMN " + quotedIdentifier(column) + " " + columnType + " CHARACTER SET utf8mb4 COLLATE " + identityCollation
	if nullable == "NO" {
		clause += " NOT NULL"
	} else {
		clause += " NULL"
	}
	if defaultValue.Valid {
		// Hex literals retain arbitrary UTF-8 default bytes independently of
		// connection SQL modes and quote/backslash escaping conventions.
		clause += " DEFAULT X'" + hex.EncodeToString([]byte(defaultValue.String)) + "'"
	} else if nullable == "YES" {
		clause += " DEFAULT NULL"
	}
	return clause, nil
}
