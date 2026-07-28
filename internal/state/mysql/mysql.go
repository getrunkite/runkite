// Package mysql will implement the state.Store interface using MySQL --
// the second SQL exemplar alongside Postgres/SQLite (master plan:
// "MySQL stays 'future SQL twin if someone needs it'"), same
// conformance suite (internal/state/conformance) as every other
// backend. Built in checkpoints: schema (New/Init/Close) plus Agents
// CRUD so far; remaining Store methods land before conformance wiring.
//
// A fresh backend (no pre-multi-tenancy/pre-versioning legacy schema
// to migrate the way Postgres/SQLite's Init() carries forward), so the
// schema below is written as its final shape directly -- no `ALTER
// TABLE ADD COLUMN IF NOT EXISTS` migration trail, and every index is
// declared inline on its CREATE TABLE rather than as separate `CREATE
// INDEX IF NOT EXISTS` statements (MySQL, unlike Postgres, has no
// portable `IF NOT EXISTS` for CREATE INDEX across all supported
// versions -- inline index declarations sidestep the question
// entirely, since CREATE TABLE IF NOT EXISTS is a no-op, indexes
// included, on a table that already exists).
//
// Two real dialect differences from Postgres/SQLite drove design
// choices here, not just syntax substitution:
//
//  1. No RETURNING clause on INSERT ... ON DUPLICATE KEY UPDATE
//     (confirmed against the MySQL 8.4 reference manual). Versioned
//     upserts still stay single-statement for the agents row itself:
//     LAST_INSERT_ID(expr) remembers the computed version on the
//     connection even without AUTO_INCREMENT, then a same-transaction
//     SELECT LAST_INSERT_ID() reads it back (see UpsertAgent). That is
//     stronger than MongoDB's separate read-then-write; checkpoint 1's
//     earlier "must use Mongo's pattern" note was wrong and is
//     superseded by the UpsertAgent implementation.
//  2. TEXT/BLOB columns cannot be part of a PRIMARY KEY or a plain
//     index without an explicit prefix length in InnoDB, unlike
//     Postgres/SQLite where a TEXT column is a perfectly normal
//     primary key. Every column used as a key anywhere (tenant_id,
//     agent_id, thread_id, run_id, name, checkpoint_id, cache_key,
//     schedule_name, namespace, store `key`) is VARCHAR(255) here
//     instead of TEXT; large freeform content (description,
//     error_msg, checkpoint_ns's occasional long value) stays TEXT
//     since it's never part of a key. store_items' composite PK of
//     three VARCHAR(255) columns sits near InnoDB's utf8mb4 index
//     byte limit (~3072) -- do not widen those columns casually.
package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/state"
	"github.com/sharanharsoor/runkite/internal/tenant"
)

// mysqlDuplicateKeyError is MySQL's ER_DUP_ENTRY code (1062), the
// equivalent of Postgres's "23505" unique_violation -- checked wherever
// a plain INSERT (not an ON DUPLICATE KEY UPDATE upsert) needs to
// report a real conflict to the caller instead of a generic error.
const mysqlDuplicateKeyError = 1062

func isDuplicateKeyError(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == mysqlDuplicateKeyError
}

// Store implements state.Store with a MySQL database.
type Store struct {
	db *sql.DB
}

// New creates a new MySQL store from a DSN in the go-sql-driver/mysql
// format (e.g. "user:password@tcp(host:3306)/dbname?parseTime=true").
// parseTime=true is required in the DSN (not defaulted here, so it's
// visible in config rather than a silent implicit behavior) -- without
// it the driver returns DATETIME/TIMESTAMP columns as []byte instead of
// time.Time, breaking every Scan(&someTime) call in this package.
func New(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{db: db}, nil
}

// Init creates all tables. Safe to call on every startup (CREATE TABLE
// IF NOT EXISTS throughout) -- same idempotent, non-versioned migration
// convention as every other backend's Init.
func (s *Store) Init(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS agents (
			tenant_id    VARCHAR(255) NOT NULL DEFAULT 'default',
			agent_id     VARCHAR(255) NOT NULL,
			name         TEXT NOT NULL,
			description  TEXT,
			metadata     JSON,
			capabilities JSON,
			version      INT NOT NULL DEFAULT 1,
			created_at   DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6),
			updated_at   DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6),
			PRIMARY KEY (tenant_id, agent_id)
		) ENGINE=InnoDB CHARSET=utf8mb4`,

		// Full agent versioning -- one immutable row per version ever
		// served, written by UpsertAgent itself the moment
		// agents.version bumps. Never updated or deleted afterward --
		// see models.AgentVersion's doc comment for why rollback
		// doesn't touch old rows here.
		`CREATE TABLE IF NOT EXISTS agent_versions (
			tenant_id    VARCHAR(255) NOT NULL DEFAULT 'default',
			agent_id     VARCHAR(255) NOT NULL,
			version      INT NOT NULL,
			name         TEXT,
			description  TEXT,
			metadata     JSON,
			capabilities JSON,
			created_at   DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6),
			PRIMARY KEY (tenant_id, agent_id, version)
		) ENGINE=InnoDB CHARSET=utf8mb4`,

		`CREATE TABLE IF NOT EXISTS agent_schemas (
			tenant_id     VARCHAR(255) NOT NULL DEFAULT 'default',
			agent_id      VARCHAR(255) NOT NULL,
			input_schema  JSON,
			output_schema JSON,
			state_schema  JSON,
			config_schema JSON,
			PRIMARY KEY (tenant_id, agent_id),
			FOREIGN KEY (tenant_id, agent_id) REFERENCES agents(tenant_id, agent_id) ON DELETE CASCADE
		) ENGINE=InnoDB CHARSET=utf8mb4`,

		// Agent marketplace / registry -- a metadata catalog, same
		// increment-on-change version pattern as agents/agent_versions
		// above, deliberately not storing or executing code.
		`CREATE TABLE IF NOT EXISTS registry_entries (
			tenant_id    VARCHAR(255) NOT NULL DEFAULT 'default',
			name         VARCHAR(255) NOT NULL,
			display_name TEXT,
			description  TEXT,
			author       TEXT,
			tags         JSON,
			source_type  VARCHAR(64) NOT NULL,
			source_ref   TEXT NOT NULL,
			metadata     JSON,
			version      INT NOT NULL DEFAULT 1,
			created_at   DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6),
			updated_at   DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6),
			PRIMARY KEY (tenant_id, name)
		) ENGINE=InnoDB CHARSET=utf8mb4`,

		`CREATE TABLE IF NOT EXISTS registry_entry_versions (
			tenant_id    VARCHAR(255) NOT NULL DEFAULT 'default',
			name         VARCHAR(255) NOT NULL,
			version      INT NOT NULL,
			display_name TEXT,
			description  TEXT,
			author       TEXT,
			tags         JSON,
			source_type  VARCHAR(64) NOT NULL,
			source_ref   TEXT NOT NULL,
			metadata     JSON,
			created_at   DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6),
			PRIMARY KEY (tenant_id, name, version)
		) ENGINE=InnoDB CHARSET=utf8mb4`,

		`CREATE TABLE IF NOT EXISTS threads (
			tenant_id   VARCHAR(255) NOT NULL DEFAULT 'default',
			thread_id   VARCHAR(255) NOT NULL,
			status      VARCHAR(64) DEFAULT 'idle',
			metadata    JSON,
			values_json JSON,
			created_at  DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6),
			updated_at  DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6),
			PRIMARY KEY (thread_id),
			KEY idx_threads_tenant (tenant_id)
		) ENGINE=InnoDB CHARSET=utf8mb4`,

		// Agent-to-Agent (A2A) delegation bookkeeping -- see models.Run's
		// doc comment. parent_run_id/root_run_id are nullable (a normal
		// top-level run has neither); depth defaults to 0.
		`CREATE TABLE IF NOT EXISTS runs (
			tenant_id     VARCHAR(255) NOT NULL DEFAULT 'default',
			run_id        VARCHAR(255) NOT NULL,
			thread_id     VARCHAR(255),
			agent_id      VARCHAR(255),
			status        VARCHAR(64) DEFAULT 'pending',
			metadata      JSON,
			input         JSON,
			config        JSON,
			output        JSON,
			error_msg     TEXT,
			created_at    DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6),
			updated_at    DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6),
			parent_run_id VARCHAR(255),
			root_run_id   VARCHAR(255),
			depth         INT NOT NULL DEFAULT 0,
			PRIMARY KEY (run_id),
			KEY idx_runs_thread (thread_id),
			KEY idx_runs_status (status),
			KEY idx_runs_tenant (tenant_id),
			KEY idx_runs_root (root_run_id),
			FOREIGN KEY (thread_id) REFERENCES threads(thread_id) ON DELETE CASCADE
		) ENGINE=InnoDB CHARSET=utf8mb4`,

		`CREATE TABLE IF NOT EXISTS thread_checkpoints (
			tenant_id     VARCHAR(255) NOT NULL DEFAULT 'default',
			checkpoint_id VARCHAR(255) NOT NULL,
			thread_id     VARCHAR(255) NOT NULL,
			checkpoint_ns VARCHAR(255) DEFAULT '',
			parent_id     VARCHAR(255),
			values_json   JSON,
			metadata      JSON,
			next_nodes    JSON,
			tasks         JSON,
			interrupts    JSON,
			created_at    DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			PRIMARY KEY (checkpoint_id),
			KEY idx_checkpoints_thread (thread_id, created_at),
			FOREIGN KEY (thread_id) REFERENCES threads(thread_id) ON DELETE CASCADE
		) ENGINE=InnoDB CHARSET=utf8mb4`,

		// `key` is a reserved word in MySQL (used in index syntax) --
		// backtick-quoted everywhere it appears as a column name, here
		// and in every query touching this table.
		//
		// Known ceiling: this composite primary key is 3 x VARCHAR(255)
		// on utf8mb4 (4 bytes/char) = 3060 bytes, against InnoDB's fixed
		// 3072-byte max index key length (DYNAMIC/COMPRESSED row
		// format, the MySQL 8.x default -- innodb_large_prefix, the old
		// escape hatch, was removed in 8.0). 12 bytes of headroom --
		// widening ANY of these three columns, even slightly, would
		// break table creation. Upgrade path if that's ever needed:
		// hash namespace/key into a fixed-width column (e.g. a
		// SHA-256 hex digest) instead of widening them, same shape as
		// swapping a natural key for a surrogate one.
		`CREATE TABLE IF NOT EXISTS store_items (
			tenant_id   VARCHAR(255) NOT NULL DEFAULT 'default',
			namespace   VARCHAR(255) NOT NULL,
			` + "`key`" + `       VARCHAR(255) NOT NULL,
			value       JSON NOT NULL,
			created_at  DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6),
			updated_at  DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6),
			ttl_minutes DOUBLE,
			expires_at  DATETIME(6),
			PRIMARY KEY (tenant_id, namespace, ` + "`key`" + `)
		) ENGINE=InnoDB CHARSET=utf8mb4`,

		`CREATE TABLE IF NOT EXISTS webhook_dead_letters (
			id         VARCHAR(255) NOT NULL,
			url        TEXT NOT NULL,
			event_type VARCHAR(255) NOT NULL,
			run_id     VARCHAR(255) NOT NULL,
			payload    JSON,
			error      TEXT,
			attempts   INT NOT NULL DEFAULT 0,
			failed_at  DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			PRIMARY KEY (id),
			KEY idx_dead_letters_failed_at (failed_at)
		) ENGINE=InnoDB CHARSET=utf8mb4`,

		// Composite PK so two tenants can cache the same logical input
		// under the same raw cache_key without colliding.
		// computeCacheKey also embeds tenant_id (defense in depth if a
		// WHERE clause is missed).
		`CREATE TABLE IF NOT EXISTS run_cache (
			tenant_id  VARCHAR(255) NOT NULL DEFAULT 'default',
			cache_key  VARCHAR(255) NOT NULL,
			agent_id   VARCHAR(255) NOT NULL,
			output     JSON,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			expires_at DATETIME(6) NOT NULL,
			PRIMARY KEY (tenant_id, cache_key),
			KEY idx_run_cache_expires (expires_at)
		) ENGINE=InnoDB CHARSET=utf8mb4`,

		`CREATE TABLE IF NOT EXISTS cron_schedules (
			tenant_id  VARCHAR(255) NOT NULL DEFAULT 'default',
			name       VARCHAR(255) NOT NULL,
			agent_id   VARCHAR(255) NOT NULL,
			expression VARCHAR(255) NOT NULL,
			timezone   VARCHAR(255) NOT NULL DEFAULT 'UTC',
			input      JSON,
			config     JSON,
			enabled    BOOLEAN NOT NULL DEFAULT TRUE,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			PRIMARY KEY (tenant_id, name)
		) ENGINE=InnoDB CHARSET=utf8mb4`,

		// Multi-instance-safe claiming (master plan: "cron-expression
		// scheduling with multi-instance-safe claiming (Postgres claim
		// window)") -- this table plus TryClaimCronFire's INSERT ...
		// ON DUPLICATE KEY UPDATE (a no-op update, since the row
		// existing at all is the signal another instance already
		// claimed this fire) is the MySQL equivalent of that claim
		// window.
		`CREATE TABLE IF NOT EXISTS cron_claims (
			tenant_id     VARCHAR(255) NOT NULL DEFAULT 'default',
			schedule_name VARCHAR(255) NOT NULL,
			fire_time     DATETIME(6) NOT NULL,
			claimed_at    DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			PRIMARY KEY (tenant_id, schedule_name, fire_time)
		) ENGINE=InnoDB CHARSET=utf8mb4`,
	}

	for _, stmt := range statements {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("mysql: init schema: %w\nstatement: %s", err, stmt)
		}
	}
	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// --------------------------------------------------------------------------
// Agents
// --------------------------------------------------------------------------

// UpsertAgent uses a single atomic INSERT ... ON DUPLICATE KEY UPDATE,
// the same one-statement shape Postgres's UpsertAgent uses (not the
// separate read-then-write transaction MongoDB's needs) -- verified
// live before writing this that MySQL's JSON columns support a real
// `!=` comparison here (normalized, not naive text comparison: an
// unchanged republish with different JSON whitespace/key order still
// correctly reports "unchanged"), and that LAST_INSERT_ID(expr) --
// normally an AUTO_INCREMENT trick -- works as a general "remember an
// arbitrary computed value for this connection" mechanism even though
// none of these tables use AUTO_INCREMENT: the version number becomes
// retrievable via a same-transaction `SELECT LAST_INSERT_ID()`
// immediately after, MySQL's only substitute for Postgres/SQLite's
// RETURNING clause.
//
// The version-history insert then always fires (not conditionally, the
// way Postgres/Mongo explicitly check "did this call actually bump the
// version"): agent_versions' own composite primary key
// (tenant_id, agent_id, version) makes an unchanged republish's insert
// collide with the row already there and become a same-content
// ON DUPLICATE KEY UPDATE no-op, which is indistinguishable in effect
// from not inserting at all -- correctness falls out of the key shape
// rather than needing a separate versionChanged bool.
//
// Known, accepted trade-off: uses the VALUES(col) function inside
// ON DUPLICATE KEY UPDATE, deprecated since MySQL 8.0.20 in favor of an
// explicit row-alias (`INSERT ... AS new ON DUPLICATE KEY UPDATE col =
// new.col`). Deliberately not migrated yet -- tried it directly against
// this exact statement and it requires qualifying EVERY bare column
// reference with either the target table name or the alias (found two
// real ambiguous-column errors doing so, both from a column appearing
// on both sides implicitly), which is meaningfully more error-prone
// than the deprecated-but-unambiguous VALUES() form for a statement
// this dense. VALUES() is still fully functional on 8.4 (the version
// tested against throughout this backend) -- worth revisiting only if
// upgrading to a MySQL version that actually removes it, not preemptively.
func (s *Store) UpsertAgent(ctx context.Context, agent *models.Agent) error {
	tenantID := tenant.FromContext(ctx)
	now := time.Now().UTC()
	meta, _ := json.Marshal(agent.Metadata)
	caps, _ := json.Marshal(agent.Capabilities)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op if committed

	// description != VALUES(description) (bare MySQL !=, not Postgres's
	// IS DISTINCT FROM) would treat a NULL-vs-'' transition as "no
	// change" -- not a real gap here since this column is never left
	// SQL NULL by any code path in this package (agent.Description is
	// always a real Go string, "" included, never nil), unlike
	// Postgres's schema which carries a genuine legacy-migration NULL
	// possibility IS DISTINCT FROM exists to handle.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO agents (tenant_id, agent_id, name, description, metadata, capabilities, version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, LAST_INSERT_ID(1), ?, ?)
		ON DUPLICATE KEY UPDATE
			version = LAST_INSERT_ID(IF(name != VALUES(name) OR description != VALUES(description)
			                              OR metadata != VALUES(metadata) OR capabilities != VALUES(capabilities),
			                            version + 1, version)),
			name = VALUES(name), description = VALUES(description),
			metadata = VALUES(metadata), capabilities = VALUES(capabilities), updated_at = VALUES(updated_at)
	`, tenantID, agent.AgentID, agent.Name, agent.Description, meta, caps, now, now)
	if err != nil {
		return fmt.Errorf("mysql: upsert agent: %w", err)
	}

	var newVersion int
	if err := tx.QueryRowContext(ctx, "SELECT LAST_INSERT_ID()").Scan(&newVersion); err != nil {
		return fmt.Errorf("mysql: read back computed version: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO agent_versions (tenant_id, agent_id, version, name, description, metadata, capabilities, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE agent_id = agent_id
	`, tenantID, agent.AgentID, newVersion, agent.Name, agent.Description, meta, caps, now); err != nil {
		return fmt.Errorf("mysql: write agent version snapshot: %w", err)
	}

	return tx.Commit()
}

func (s *Store) GetAgent(ctx context.Context, agentID string) (*models.Agent, error) {
	query := `SELECT tenant_id, agent_id, name, description, metadata, capabilities, version FROM agents WHERE agent_id = ?`
	args := []interface{}{agentID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	row := s.db.QueryRowContext(ctx, query, args...)
	a, err := scanAgent(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, &state.ErrNotFound{Resource: "agent", ID: agentID}
		}
		return nil, err
	}
	return a, nil
}

func (s *Store) SearchAgents(ctx context.Context, req *models.AgentSearchRequest) ([]*models.Agent, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	query := `SELECT tenant_id, agent_id, name, description, metadata, capabilities, version FROM agents`
	var args []interface{}
	var where []string

	if !tenant.IsSystem(ctx) {
		where = append(where, "tenant_id = ?")
		args = append(args, tenant.FromContext(ctx))
	}
	if req.Name != "" {
		where = append(where, "name LIKE ?")
		args = append(args, "%"+req.Name+"%")
	}
	for k, v := range req.Metadata {
		where = append(where, "JSON_EXTRACT(metadata, ?) = ?")
		path := "$." + k
		if sv, ok := v.(string); ok {
			args = append(args, path, sv)
		} else {
			valJSON, _ := json.Marshal(v)
			args = append(args, path, string(valJSON))
		}
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY name ASC LIMIT ? OFFSET ?"
	args = append(args, limit, req.Offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	agents := []*models.Agent{}
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

func (s *Store) GetAgentSchema(ctx context.Context, agentID string) (*models.AgentSchema, error) {
	query := `SELECT agent_id, input_schema, output_schema, state_schema, config_schema FROM agent_schemas WHERE agent_id = ?`
	args := []interface{}{agentID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	row := s.db.QueryRowContext(ctx, query, args...)

	var as models.AgentSchema
	var inBytes, outBytes, stBytes, cfgBytes []byte
	if err := row.Scan(&as.AgentID, &inBytes, &outBytes, &stBytes, &cfgBytes); err != nil {
		if err == sql.ErrNoRows {
			return nil, &state.ErrNotFound{Resource: "agent_schema", ID: agentID}
		}
		return nil, err
	}
	json.Unmarshal(inBytes, &as.InputSchema)
	json.Unmarshal(outBytes, &as.OutputSchema)
	json.Unmarshal(stBytes, &as.StateSchema)
	json.Unmarshal(cfgBytes, &as.ConfigSchema)
	return &as, nil
}

func (s *Store) UpsertAgentSchema(ctx context.Context, schema *models.AgentSchema) error {
	input, _ := json.Marshal(schema.InputSchema)
	output, _ := json.Marshal(schema.OutputSchema)
	st, _ := json.Marshal(schema.StateSchema)
	cfg, _ := json.Marshal(schema.ConfigSchema)

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_schemas (tenant_id, agent_id, input_schema, output_schema, state_schema, config_schema)
		VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			input_schema = VALUES(input_schema), output_schema = VALUES(output_schema),
			state_schema = VALUES(state_schema), config_schema = VALUES(config_schema)
	`, tenant.FromContext(ctx), schema.AgentID, input, output, st, cfg)
	return err
}

func (s *Store) ListAgentVersions(ctx context.Context, agentID string) ([]*models.AgentVersion, error) {
	query := `SELECT tenant_id, agent_id, version, name, description, metadata, capabilities, created_at FROM agent_versions WHERE agent_id = ?`
	args := []interface{}{agentID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	query += ` ORDER BY version DESC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	versions := []*models.AgentVersion{}
	for rows.Next() {
		v, err := scanAgentVersion(rows)
		if err != nil {
			return nil, err
		}
		versions = append(versions, v)
	}
	return versions, rows.Err()
}

func (s *Store) GetAgentVersion(ctx context.Context, agentID string, version int) (*models.AgentVersion, error) {
	query := `SELECT tenant_id, agent_id, version, name, description, metadata, capabilities, created_at FROM agent_versions WHERE agent_id = ? AND version = ?`
	args := []interface{}{agentID, version}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	row := s.db.QueryRowContext(ctx, query, args...)
	v, err := scanAgentVersion(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, &state.ErrNotFound{Resource: "agent_version", ID: fmt.Sprintf("%s@v%d", agentID, version)}
		}
		return nil, err
	}
	return v, nil
}

// rowScanner covers both *sql.Row (QueryRowContext) and *sql.Rows
// (QueryContext) -- both expose an identical Scan method, so this one
// helper serves every Get.../List... pair below without duplicating
// the column list or JSON-unmarshal logic between them.
type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanAgent(row rowScanner) (*models.Agent, error) {
	var a models.Agent
	var metaBytes, capsBytes []byte
	if err := row.Scan(&a.TenantID, &a.AgentID, &a.Name, &a.Description, &metaBytes, &capsBytes, &a.Version); err != nil {
		return nil, err
	}
	json.Unmarshal(metaBytes, &a.Metadata)
	json.Unmarshal(capsBytes, &a.Capabilities)
	return &a, nil
}

func scanAgentVersion(row rowScanner) (*models.AgentVersion, error) {
	var v models.AgentVersion
	var metaBytes, capsBytes []byte
	if err := row.Scan(&v.TenantID, &v.AgentID, &v.Version, &v.Name, &v.Description, &metaBytes, &capsBytes, &v.CreatedAt); err != nil {
		return nil, err
	}
	json.Unmarshal(metaBytes, &v.Metadata)
	json.Unmarshal(capsBytes, &v.Capabilities)
	return &v, nil
}

// --------------------------------------------------------------------------
// Threads
// --------------------------------------------------------------------------

func (s *Store) CreateThread(ctx context.Context, thread *models.Thread) error {
	meta, _ := json.Marshal(thread.Metadata)
	vals, _ := json.Marshal(thread.Values)

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO threads (tenant_id, thread_id, status, metadata, values_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, tenant.FromContext(ctx), thread.ThreadID, thread.Status, meta, vals, thread.CreatedAt, thread.UpdatedAt)
	if err != nil {
		if isDuplicateKeyError(err) {
			return &state.ErrConflict{Resource: "thread", ID: thread.ThreadID}
		}
		return err
	}
	return nil
}

func (s *Store) GetThread(ctx context.Context, threadID string) (*models.Thread, error) {
	query := `SELECT tenant_id, thread_id, status, metadata, values_json, created_at, updated_at FROM threads WHERE thread_id = ?`
	args := []interface{}{threadID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	row := s.db.QueryRowContext(ctx, query, args...)
	t, err := scanThread(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, &state.ErrNotFound{Resource: "thread", ID: threadID}
		}
		return nil, err
	}
	return t, nil
}

func (s *Store) UpdateThread(ctx context.Context, threadID string, patch *models.ThreadPatch) (*models.Thread, error) {
	existing, err := s.GetThread(ctx, threadID)
	if err != nil {
		return nil, err
	}

	if patch.Metadata != nil {
		if existing.Metadata == nil {
			existing.Metadata = make(map[string]interface{})
		}
		for k, v := range patch.Metadata {
			existing.Metadata[k] = v
		}
	}
	if patch.Values != nil {
		if existing.Values == nil {
			existing.Values = make(map[string]interface{})
		}
		for k, v := range patch.Values {
			existing.Values[k] = v
		}
	}

	existing.UpdatedAt = time.Now().UTC()
	meta, _ := json.Marshal(existing.Metadata)
	vals, _ := json.Marshal(existing.Values)

	query := `UPDATE threads SET metadata = ?, values_json = ?, updated_at = ? WHERE thread_id = ?`
	args := []interface{}{meta, vals, existing.UpdatedAt, threadID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	_, err = s.db.ExecContext(ctx, query, args...)
	return existing, err
}

func (s *Store) DeleteThread(ctx context.Context, threadID string) error {
	query := `DELETE FROM threads WHERE thread_id = ?`
	args := []interface{}{threadID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return &state.ErrNotFound{Resource: "thread", ID: threadID}
	}
	return nil
}

func (s *Store) SearchThreads(ctx context.Context, req *models.ThreadSearchRequest) ([]*models.Thread, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	query := `SELECT tenant_id, thread_id, status, metadata, values_json, created_at, updated_at FROM threads`
	var args []interface{}
	var where []string

	if !tenant.IsSystem(ctx) {
		where = append(where, "tenant_id = ?")
		args = append(args, tenant.FromContext(ctx))
	}
	if req.Status != nil {
		where = append(where, "status = ?")
		args = append(args, string(*req.Status))
	}
	for k, v := range req.Metadata {
		where = append(where, "JSON_EXTRACT(metadata, ?) = ?")
		path := "$." + k
		if sv, ok := v.(string); ok {
			args = append(args, path, sv)
		} else {
			valJSON, _ := json.Marshal(v)
			args = append(args, path, string(valJSON))
		}
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY created_at DESC LIMIT ? OFFSET ?"
	args = append(args, limit, req.Offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	threads := []*models.Thread{}
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		threads = append(threads, t)
	}
	return threads, rows.Err()
}

func (s *Store) SetThreadStatus(ctx context.Context, threadID string, status models.ThreadStatus) error {
	now := time.Now().UTC()
	query := `UPDATE threads SET status = ?, updated_at = ? WHERE thread_id = ?`
	args := []interface{}{string(status), now, threadID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	_, err := s.db.ExecContext(ctx, query, args...)
	return err
}

// TryClaimThread is a single atomic conditional UPDATE (idle/interrupted/
// etc -> busy), same TOCTOU-safe shape as Postgres/SQLite -- two
// concurrent callers can never both see RowsAffected() > 0, since
// InnoDB's row lock during the UPDATE serializes them and the second
// one's WHERE status != 'busy' no longer matches once the first commits.
func (s *Store) TryClaimThread(ctx context.Context, threadID string) (bool, error) {
	now := time.Now().UTC()
	query := `UPDATE threads SET status = ?, updated_at = ? WHERE thread_id = ? AND status != ?`
	args := []interface{}{string(models.ThreadStatusBusy), now, threadID, string(models.ThreadStatusBusy)}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func scanThread(row rowScanner) (*models.Thread, error) {
	var t models.Thread
	var metaBytes, valsBytes []byte
	if err := row.Scan(&t.TenantID, &t.ThreadID, &t.Status, &metaBytes, &valsBytes, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	json.Unmarshal(metaBytes, &t.Metadata)
	json.Unmarshal(valsBytes, &t.Values)
	return &t, nil
}

// nullableJSON returns nil (SQL NULL) for empty/absent JSON data instead
// of passing an empty byte slice through -- MySQL's JSON column type
// rejects an empty string as invalid JSON syntax, but happily accepts
// SQL NULL for a nullable column. Non-empty data (including the 4-byte
// literal "null") passes through unchanged.
func nullableJSON(data []byte) interface{} {
	if len(data) == 0 {
		return nil
	}
	return []byte(data)
}

// nullableString/nullStringToPtr convert between a *string model field
// (nil = absent, e.g. Run.ParentRunID on a top-level run) and the
// sql.NullString round-trip database/sql requires for nullable TEXT/
// VARCHAR columns. Unlike pgx (used by the Postgres backend), the
// standard database/sql driver interface go-sql-driver/mysql implements
// has no built-in support for scanning directly into a **string, so
// every nullable *string column here goes through sql.NullString
// instead -- same pattern the SQLite backend uses for the same reason
// (also a database/sql driver).
func nullableString(s *string) interface{} {
	if s == nil {
		return nil
	}
	return *s
}

func nullStringToPtr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	v := s.String
	return &v
}

// --------------------------------------------------------------------------
// Runs
// --------------------------------------------------------------------------

func (s *Store) CreateRun(ctx context.Context, run *models.Run) error {
	meta, _ := json.Marshal(run.Metadata)
	// error_msg is written explicitly as '' rather than left to a
	// schema-level DEFAULT: unlike Postgres, plain MySQL TEXT columns
	// reject a literal string DEFAULT outright (ER_BLOB_CANT_HAVE_DEFAULT,
	// confirmed against a live MySQL 8.4 container) -- only an
	// expression-syntax default like `DEFAULT ('')` is accepted, and
	// that's a more obscure dialect quirk to lean on than just writing
	// the value explicitly here, where every other Run field is
	// already written explicitly. scanRun's plain string destination
	// for this column (not sql.NullString) depends on this column
	// never actually being SQL NULL for an app-created row.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (tenant_id, run_id, thread_id, agent_id, status, metadata, input, config, error_msg, created_at, updated_at, parent_run_id, root_run_id, depth)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, tenant.FromContext(ctx), run.RunID, run.ThreadID, run.AgentID, run.Status, meta,
		nullableJSON(run.Input), nullableJSON(run.Config), "",
		run.CreatedAt, run.UpdatedAt, nullableString(run.ParentRunID), nullableString(run.RootRunID), run.Depth)
	return err
}

func (s *Store) GetRun(ctx context.Context, runID string) (*models.Run, error) {
	query := `SELECT tenant_id, run_id, thread_id, agent_id, status, metadata, input, config, output, error_msg, created_at, updated_at, parent_run_id, root_run_id, depth FROM runs WHERE run_id = ?`
	args := []interface{}{runID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	row := s.db.QueryRowContext(ctx, query, args...)
	r, err := scanRun(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, &state.ErrNotFound{Resource: "run", ID: runID}
		}
		return nil, err
	}
	return r, nil
}

func (s *Store) UpdateRunStatus(ctx context.Context, runID string, status models.RunStatus, output []byte, errMsg string) error {
	now := time.Now().UTC()
	query := `UPDATE runs SET status = ?, output = ?, error_msg = ?, updated_at = ? WHERE run_id = ?`
	args := []interface{}{string(status), nullableJSON(output), errMsg, now, runID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	_, err := s.db.ExecContext(ctx, query, args...)
	return err
}

func (s *Store) DeleteRun(ctx context.Context, runID string) error {
	query := `DELETE FROM runs WHERE run_id = ?`
	args := []interface{}{runID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return &state.ErrNotFound{Resource: "run", ID: runID}
	}
	return nil
}

func (s *Store) SearchRuns(ctx context.Context, req *models.RunSearchRequest) ([]*models.Run, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	query := `SELECT tenant_id, run_id, thread_id, agent_id, status, metadata, input, config, output, error_msg, created_at, updated_at, parent_run_id, root_run_id, depth FROM runs`
	var args []interface{}
	var where []string

	if !tenant.IsSystem(ctx) {
		where = append(where, "tenant_id = ?")
		args = append(args, tenant.FromContext(ctx))
	}
	if req.Status != nil {
		where = append(where, "status = ?")
		args = append(args, string(*req.Status))
	}
	if req.ThreadID != "" {
		where = append(where, "thread_id = ?")
		args = append(args, req.ThreadID)
	}
	if req.AgentID != "" {
		where = append(where, "agent_id = ?")
		args = append(args, req.AgentID)
	}
	// Same MySQL scalar-vs-JSON comparison rule SearchThreads relies
	// on: JSON_EXTRACT(...) returns a JSON value, and comparing it with
	// = against a non-JSON operand implicitly casts that operand to
	// JSON first, so a plain Go string on the right still matches a
	// JSON string on the left without needing JSON_UNQUOTE.
	for k, v := range req.Metadata {
		where = append(where, "JSON_EXTRACT(metadata, ?) = ?")
		path := "$." + k
		if sv, ok := v.(string); ok {
			args = append(args, path, sv)
		} else {
			valJSON, _ := json.Marshal(v)
			args = append(args, path, string(valJSON))
		}
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY created_at DESC LIMIT ? OFFSET ?"
	args = append(args, limit, req.Offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	runs := []*models.Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

func scanRun(row rowScanner) (*models.Run, error) {
	var r models.Run
	var metaBytes, inputBytes, configBytes, outputBytes []byte
	var parentRunID, rootRunID sql.NullString
	if err := row.Scan(&r.TenantID, &r.RunID, &r.ThreadID, &r.AgentID, &r.Status, &metaBytes, &inputBytes, &configBytes, &outputBytes, &r.Error, &r.CreatedAt, &r.UpdatedAt, &parentRunID, &rootRunID, &r.Depth); err != nil {
		return nil, err
	}
	r.ParentRunID = nullStringToPtr(parentRunID)
	r.RootRunID = nullStringToPtr(rootRunID)
	if metaBytes != nil {
		json.Unmarshal(metaBytes, &r.Metadata)
	}
	if inputBytes != nil {
		r.Input = json.RawMessage(inputBytes)
	}
	if configBytes != nil {
		r.Config = json.RawMessage(configBytes)
	}
	if outputBytes != nil {
		r.Output = json.RawMessage(outputBytes)
	}
	r.AssistantID = r.AgentID // SDK compat
	return &r, nil
}

// --------------------------------------------------------------------------
// Checkpoints
// --------------------------------------------------------------------------

func (s *Store) SaveCheckpoint(ctx context.Context, threadID string, ts *models.ThreadState) error {
	vals, _ := json.Marshal(ts.Values)
	meta, _ := json.Marshal(ts.Metadata)
	next, _ := json.Marshal(ts.Next)
	tasks, _ := json.Marshal(ts.Tasks)
	interrupts, _ := json.Marshal(ts.Interrupts)

	var parentID *string
	if ts.ParentCheckpoint != nil {
		parentID = &ts.ParentCheckpoint.CheckpointID
	}

	createdAt := time.Now().UTC()
	if ts.CreatedAt != nil && *ts.CreatedAt != "" {
		if parsed, err := time.Parse(time.RFC3339, *ts.CreatedAt); err == nil {
			createdAt = parsed
		}
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO thread_checkpoints (tenant_id, checkpoint_id, thread_id, checkpoint_ns, parent_id, values_json, metadata, next_nodes, tasks, interrupts, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, tenant.FromContext(ctx), ts.Checkpoint.CheckpointID, threadID, ts.Checkpoint.CheckpointNS,
		nullableString(parentID), vals, meta, next, tasks, interrupts, createdAt)
	return err
}

func (s *Store) GetLatestCheckpoint(ctx context.Context, threadID string) (*models.ThreadState, error) {
	query := `SELECT checkpoint_id, thread_id, checkpoint_ns, parent_id, values_json, metadata, next_nodes, tasks, interrupts, created_at
		FROM thread_checkpoints WHERE thread_id = ?`
	args := []interface{}{threadID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	query += ` ORDER BY created_at DESC LIMIT 1`
	row := s.db.QueryRowContext(ctx, query, args...)

	ts, err := scanCheckpoint(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, &state.ErrNotFound{Resource: "checkpoint", ID: "latest"}
		}
		return nil, err
	}
	return ts, nil
}

// ListCheckpoints' "before" filter looks up created_at by checkpoint_id
// via a subquery rather than joining tenant_id into it -- same as
// Postgres/SQLite: checkpoint_id is a global primary key (a UUID-like
// value from the caller, not tenant-scoped), so a plain lookup by ID is
// enough, and the outer query's own tenant filter already bounds the
// result set to the caller's tenant regardless of what the subquery
// matches.
func (s *Store) ListCheckpoints(ctx context.Context, threadID string, limit int, before string) ([]*models.ThreadState, error) {
	if limit <= 0 {
		limit = 10
	}
	query := `SELECT checkpoint_id, thread_id, checkpoint_ns, parent_id, values_json, metadata, next_nodes, tasks, interrupts, created_at
		FROM thread_checkpoints WHERE thread_id = ?`
	args := []interface{}{threadID}

	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	if before != "" {
		query += ` AND created_at < (SELECT created_at FROM thread_checkpoints WHERE checkpoint_id = ?)`
		args = append(args, before)
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	states := []*models.ThreadState{}
	for rows.Next() {
		ts, err := scanCheckpoint(rows)
		if err != nil {
			return nil, err
		}
		states = append(states, ts)
	}
	return states, rows.Err()
}

func scanCheckpoint(row rowScanner) (*models.ThreadState, error) {
	var ts models.ThreadState
	var cpID, tID, cpNS string
	var parentID sql.NullString
	var valsBytes, metaBytes, nextBytes, tasksBytes, intBytes []byte
	var createdAt time.Time

	if err := row.Scan(&cpID, &tID, &cpNS, &parentID, &valsBytes, &metaBytes, &nextBytes, &tasksBytes, &intBytes, &createdAt); err != nil {
		return nil, err
	}
	ts.Checkpoint = models.ThreadCheckpoint{CheckpointID: cpID, ThreadID: tID, CheckpointNS: cpNS}
	if valsBytes != nil {
		json.Unmarshal(valsBytes, &ts.Values)
	}
	if metaBytes != nil {
		json.Unmarshal(metaBytes, &ts.Metadata)
	}
	if nextBytes != nil {
		json.Unmarshal(nextBytes, &ts.Next)
	}
	if tasksBytes != nil {
		json.Unmarshal(tasksBytes, &ts.Tasks)
	}
	if intBytes != nil {
		json.Unmarshal(intBytes, &ts.Interrupts)
	}
	cat := createdAt.Format(time.RFC3339)
	ts.CreatedAt = &cat
	if parentID.Valid {
		ts.ParentCheckpoint = &models.ThreadCheckpoint{CheckpointID: parentID.String, ThreadID: tID, CheckpointNS: cpNS}
	}
	return &ts, nil
}
