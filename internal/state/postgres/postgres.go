// Package postgres implements the state.Store interface using PostgreSQL
// via pgx. This is the production backend for multi-node deployments.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/state"
	"github.com/sharanharsoor/runkite/internal/tenant"
)

// Store implements state.Store with a PostgreSQL database.
type Store struct {
	pool *pgxpool.Pool
}

// New creates a new Postgres store from a connection string (DSN).
func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgxpool.New: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// initAdvisoryLockKey is an arbitrary constant used as a Postgres session
// advisory lock key to serialize schema initialization across concurrent
// processes (multiple control-plane replicas starting simultaneously
// against the same fresh database -- see Init's own doc comment for the
// race this closes). Value has no meaning beyond being a fixed, unique-
// enough int64 unlikely to collide with any other advisory lock a user's
// own application might independently take on the same database.
const initAdvisoryLockKey = 894127001

// Init creates all tables. Safe to call concurrently from multiple
// processes against the same database (e.g. several `runkite serve`
// replicas starting up at once, all pointed at one fresh Postgres) --
// wrapped in a session-level advisory lock so only one process actually
// runs the DDL while the others wait, then find the schema already
// present. Without this, concurrent CREATE TABLE IF NOT EXISTS calls
// against a table that doesn't exist yet can race on Postgres's own
// catalog (confirmed live: "duplicate key value violates unique
// constraint \"pg_type_typname_nsp_index\"" when 3 replicas started
// together against a fresh database) -- IF NOT EXISTS makes each
// individual statement idempotent once the schema exists, but doesn't by
// itself make the very first, from-nothing creation race-free.
func (s *Store) Init(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for schema init lock: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", initAdvisoryLockKey); err != nil {
		return fmt.Errorf("acquire schema init advisory lock: %w", err)
	}
	defer func() {
		// Best-effort unlock on the same connection the lock was taken on
		// (required -- session-level advisory locks are connection-scoped).
		// A failed unlock isn't fatal: the lock releases automatically when
		// this connection is closed/returned to the pool, at worst
		// delaying (not blocking forever) any other replica's Init call.
		if _, unlockErr := conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", initAdvisoryLockKey); unlockErr != nil {
			return
		}
	}()

	return s.initSchemaLocked(ctx, conn)
}

// splitSchemaStatements splits a semicolon-delimited SQL script into
// individual statements. Comments are stripped *before* splitting on ";",
// not after -- a `-- comment` runs to the end of its line regardless of any
// semicolon characters that appear within the comment's own prose (several
// of this schema's comments do, e.g. "...covered by its composite PRIMARY
// KEY above; CREATE UNIQUE INDEX..." as explanatory text, not real SQL), so
// splitting the raw string first and only checking for comment-only lines
// afterward mis-splits mid-comment. Stripping first is correct and loses
// nothing -- Postgres doesn't need the comments to execute the statements.
// NOT a general-purpose SQL splitter -- it does not understand dollar-
// quoting (`$$ ... $$`), so it must only be used on scripts confirmed not
// to contain any (see initSchemaLocked's call site for that check), and it
// assumes "--" never appears inside a string literal, true for this DDL-
// only schema (identifiers and types, no string-valued data).
func splitSchemaStatements(script string) []string {
	var codeLines []string
	for _, line := range strings.Split(script, "\n") {
		if idx := strings.Index(line, "--"); idx >= 0 {
			line = line[:idx]
		}
		codeLines = append(codeLines, line)
	}
	codeOnly := strings.Join(codeLines, "\n")

	var out []string
	for _, part := range strings.Split(codeOnly, ";") {
		if strings.TrimSpace(part) != "" {
			out = append(out, strings.TrimSpace(part)+";")
		}
	}
	return out
}

// firstLine returns stmt's first non-blank, non-comment line, trimmed --
// used only to make a schema-statement error message identify which
// statement failed without dumping the whole (possibly multi-line) block.
func firstLine(stmt string) string {
	for _, line := range strings.Split(stmt, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "--") {
			return trimmed
		}
	}
	return strings.TrimSpace(stmt)
}

// initSchemaLocked runs the actual schema DDL, called only while Init
// holds the advisory lock -- split out so the lock-acquisition wrapper
// above and the DDL itself aren't tangled together in one long function.
func (s *Store) initSchemaLocked(ctx context.Context, conn *pgxpool.Conn) error {
	schema := `
	CREATE TABLE IF NOT EXISTS agents (
		tenant_id    TEXT NOT NULL DEFAULT 'default',
		agent_id     TEXT NOT NULL,
		name         TEXT NOT NULL,
		description  TEXT DEFAULT '',
		metadata     JSONB DEFAULT '{}',
		capabilities JSONB DEFAULT '{}',
		version      INTEGER NOT NULL DEFAULT 1,
		created_at   TIMESTAMPTZ DEFAULT NOW(),
		updated_at   TIMESTAMPTZ DEFAULT NOW(),
		PRIMARY KEY (tenant_id, agent_id)
	);
	-- Postgres supports IF NOT EXISTS on ADD COLUMN (unlike SQLite's parser
	-- here -- see addColumnIfMissing in the sqlite package), so existing
	-- installs upgrading from a pre-version/pre-tenant schema get columns
	-- added in place with a single idempotent statement each. Pre-existing
	-- rows all become "default" tenant -- exactly today's single-tenant
	-- behavior, nothing reassigned or hidden. As in SQLite, a table
	-- created before this migration keeps its ORIGINAL primary key even
	-- after the column is added (ALTER TABLE can't retroactively widen a
	-- primary key here either) -- every query still filters by tenant_id
	-- regardless, so isolation holds; only the extra DB-level composite
	-- uniqueness constraint is unavailable on upgraded databases.
	ALTER TABLE agents ADD COLUMN IF NOT EXISTS version INTEGER NOT NULL DEFAULT 1;
	-- Every ALTER TABLE ... ADD COLUMN IF NOT EXISTS tenant_id below is
	-- placed immediately after its own table's CREATE, and BEFORE any
	-- later statement that depends on that column existing (an index on
	-- tenant_id, or another table's FOREIGN KEY into this one) -- on a
	-- pre-existing (pre-tenant) database, "agents" already exists so its
	-- CREATE is a no-op and tenant_id genuinely doesn't exist until this
	-- ALTER runs. Getting this ordering wrong is exactly the two bugs
	-- confirmed live while building this migration: agent_schemas'
	-- CREATE TABLE has a FOREIGN KEY into agents(tenant_id, agent_id),
	-- and threads/runs each have a CREATE INDEX on their own tenant_id
	-- column right after their CREATE TABLE -- both fail with "column
	-- tenant_id does not exist" if the ALTER for that table hasn't run
	-- yet. Batching all the ALTERs at the end of the script (the first
	-- attempt here) is the wrong pattern for exactly this reason.
	ALTER TABLE agents ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default';
	-- ON CONFLICT(tenant_id, agent_id) in UpsertAgent needs an ACTUAL
	-- unique constraint on exactly those two columns to target -- a
	-- pre-existing table's original PRIMARY KEY (agent_id alone, from
	-- before this migration) doesn't satisfy that, and ALTER TABLE can't
	-- widen a primary key in place. A fresh table already has this
	-- covered by its composite PRIMARY KEY above; CREATE UNIQUE INDEX IF
	-- NOT EXISTS with an explicit name makes this idempotent and correct
	-- for both cases uniformly, at the cost of one small redundant index
	-- on a fresh install (harmless). Confirmed live: without this, a
	-- pre-existing database fails every UpsertAgent with "no unique or
	-- exclusion constraint matching the ON CONFLICT specification".
	CREATE UNIQUE INDEX IF NOT EXISTS ux_agents_tenant_agent ON agents(tenant_id, agent_id);

	-- Full agent versioning, supporting version history browsing and
	-- rollback to arbitrary past versions -- one immutable row per
	-- version ever served, written by UpsertAgent itself the moment
	-- agents.version bumps. Never updated or deleted afterward -- see
	-- models.AgentVersion's doc comment for why rollback doesn't touch
	-- old rows here.
	CREATE TABLE IF NOT EXISTS agent_versions (
		tenant_id    TEXT NOT NULL DEFAULT 'default',
		agent_id     TEXT NOT NULL,
		version      INTEGER NOT NULL,
		name         TEXT,
		description  TEXT,
		metadata     JSONB DEFAULT '{}',
		capabilities JSONB DEFAULT '{}',
		created_at   TIMESTAMPTZ DEFAULT NOW(),
		PRIMARY KEY (tenant_id, agent_id, version)
	);

	CREATE TABLE IF NOT EXISTS agent_schemas (
		tenant_id     TEXT NOT NULL DEFAULT 'default',
		agent_id      TEXT NOT NULL,
		input_schema  JSONB DEFAULT '{}',
		output_schema JSONB DEFAULT '{}',
		state_schema  JSONB DEFAULT '{}',
		config_schema JSONB DEFAULT '{}',
		PRIMARY KEY (tenant_id, agent_id),
		FOREIGN KEY (tenant_id, agent_id) REFERENCES agents(tenant_id, agent_id) ON DELETE CASCADE
	);
	ALTER TABLE agent_schemas ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default';
	CREATE UNIQUE INDEX IF NOT EXISTS ux_agent_schemas_tenant_agent ON agent_schemas(tenant_id, agent_id);

	-- Agent marketplace / registry for publishing, discovering, and
	-- deploying agent definitions -- a metadata catalog, same
	-- increment-on-change version pattern as agents/agent_versions
	-- above, deliberately not storing or executing code.
	CREATE TABLE IF NOT EXISTS registry_entries (
		tenant_id    TEXT NOT NULL DEFAULT 'default',
		name         TEXT NOT NULL,
		display_name TEXT,
		description  TEXT,
		author       TEXT,
		tags         JSONB DEFAULT '[]',
		source_type  TEXT NOT NULL,
		source_ref   TEXT NOT NULL,
		metadata     JSONB DEFAULT '{}',
		version      INTEGER NOT NULL DEFAULT 1,
		created_at   TIMESTAMPTZ DEFAULT NOW(),
		updated_at   TIMESTAMPTZ DEFAULT NOW(),
		PRIMARY KEY (tenant_id, name)
	);
	CREATE TABLE IF NOT EXISTS registry_entry_versions (
		tenant_id    TEXT NOT NULL DEFAULT 'default',
		name         TEXT NOT NULL,
		version      INTEGER NOT NULL,
		display_name TEXT,
		description  TEXT,
		author       TEXT,
		tags         JSONB DEFAULT '[]',
		source_type  TEXT NOT NULL,
		source_ref   TEXT NOT NULL,
		metadata     JSONB DEFAULT '{}',
		created_at   TIMESTAMPTZ DEFAULT NOW(),
		PRIMARY KEY (tenant_id, name, version)
	);

	CREATE TABLE IF NOT EXISTS threads (
		tenant_id  TEXT NOT NULL DEFAULT 'default',
		thread_id  TEXT PRIMARY KEY,
		status     TEXT DEFAULT 'idle',
		metadata   JSONB DEFAULT '{}',
		values_json JSONB DEFAULT '{}',
		created_at TIMESTAMPTZ DEFAULT NOW(),
		updated_at TIMESTAMPTZ DEFAULT NOW()
	);
	ALTER TABLE threads ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default';
	CREATE INDEX IF NOT EXISTS idx_threads_tenant ON threads(tenant_id);
	-- Optimistic concurrency for PATCH /threads: a plain increment-on-write
	-- counter, not the updated_at timestamp -- comparing a client-round-tripped timestamp
	-- for exact equality across four backends with different column
	-- types and precisions (Postgres microseconds, SQLite text, MySQL,
	-- Mongo) is a real source of false-positive conflicts; an integer
	-- has no such ambiguity.
	ALTER TABLE threads ADD COLUMN IF NOT EXISTS version INTEGER NOT NULL DEFAULT 1;

	CREATE TABLE IF NOT EXISTS runs (
		tenant_id  TEXT NOT NULL DEFAULT 'default',
		run_id     TEXT PRIMARY KEY,
		thread_id  TEXT REFERENCES threads(thread_id) ON DELETE CASCADE,
		agent_id   TEXT,
		status     TEXT DEFAULT 'pending',
		metadata   JSONB DEFAULT '{}',
		input      JSONB,
		config     JSONB,
		output     JSONB,
		error_msg  TEXT DEFAULT '',
		created_at TIMESTAMPTZ DEFAULT NOW(),
		updated_at TIMESTAMPTZ DEFAULT NOW()
	);
	ALTER TABLE runs ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default';
	-- Agent-to-Agent (A2A) delegation bookkeeping -- see models.Run's doc
	-- comment. parent_run_id/root_run_id are nullable (a normal
	-- top-level run has neither); depth defaults to 0.
	ALTER TABLE runs ADD COLUMN IF NOT EXISTS parent_run_id TEXT;
	ALTER TABLE runs ADD COLUMN IF NOT EXISTS root_run_id TEXT;
	ALTER TABLE runs ADD COLUMN IF NOT EXISTS depth INTEGER NOT NULL DEFAULT 0;
	CREATE INDEX IF NOT EXISTS idx_runs_thread ON runs(thread_id);
	CREATE INDEX IF NOT EXISTS idx_runs_status ON runs(status);
	CREATE INDEX IF NOT EXISTS idx_runs_tenant ON runs(tenant_id);
	CREATE INDEX IF NOT EXISTS idx_runs_root ON runs(root_run_id);

	CREATE TABLE IF NOT EXISTS thread_checkpoints (
		tenant_id       TEXT NOT NULL DEFAULT 'default',
		checkpoint_id   TEXT PRIMARY KEY,
		thread_id       TEXT NOT NULL REFERENCES threads(thread_id) ON DELETE CASCADE,
		checkpoint_ns   TEXT DEFAULT '',
		parent_id       TEXT,
		values_json     JSONB DEFAULT '{}',
		metadata        JSONB DEFAULT '{}',
		next_nodes      JSONB DEFAULT '[]',
		tasks           JSONB DEFAULT '[]',
		interrupts      JSONB DEFAULT '[]',
		created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	ALTER TABLE thread_checkpoints ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default';
	CREATE INDEX IF NOT EXISTS idx_checkpoints_thread ON thread_checkpoints(thread_id, created_at DESC);

	CREATE TABLE IF NOT EXISTS store_items (
		tenant_id  TEXT NOT NULL DEFAULT 'default',
		namespace  TEXT NOT NULL,
		key        TEXT NOT NULL,
		value      JSONB NOT NULL DEFAULT '{}',
		created_at TIMESTAMPTZ DEFAULT NOW(),
		updated_at TIMESTAMPTZ DEFAULT NOW(),
		PRIMARY KEY (tenant_id, namespace, key)
	);
	ALTER TABLE store_items ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default';
	CREATE UNIQUE INDEX IF NOT EXISTS ux_store_items_tenant_ns_key ON store_items(tenant_id, namespace, key);
	ALTER TABLE store_items ADD COLUMN IF NOT EXISTS ttl_minutes DOUBLE PRECISION;
	ALTER TABLE store_items ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;

	CREATE TABLE IF NOT EXISTS webhook_dead_letters (
		id         TEXT PRIMARY KEY,
		url        TEXT NOT NULL,
		event_type TEXT NOT NULL,
		run_id     TEXT NOT NULL,
		payload    JSONB NOT NULL DEFAULT '{}',
		error      TEXT DEFAULT '',
		attempts   INTEGER NOT NULL DEFAULT 0,
		failed_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_dead_letters_failed_at ON webhook_dead_letters(failed_at DESC);

	-- Composite PK so two tenants can cache the same logical input under
	-- the same raw cache_key without colliding. computeCacheKey also
	-- embeds tenant_id (defense in depth if a WHERE clause is missed).
	CREATE TABLE IF NOT EXISTS run_cache (
		tenant_id  TEXT NOT NULL DEFAULT 'default',
		cache_key  TEXT NOT NULL,
		agent_id   TEXT NOT NULL,
		output     JSONB NOT NULL DEFAULT '{}',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		expires_at TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (tenant_id, cache_key)
	);
	ALTER TABLE run_cache ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default';
	CREATE INDEX IF NOT EXISTS idx_run_cache_expires ON run_cache(expires_at);
	CREATE UNIQUE INDEX IF NOT EXISTS ux_run_cache_tenant_key ON run_cache(tenant_id, cache_key);

	CREATE TABLE IF NOT EXISTS cron_schedules (
		tenant_id  TEXT NOT NULL DEFAULT 'default',
		name       TEXT NOT NULL,
		agent_id   TEXT NOT NULL,
		expression TEXT NOT NULL,
		timezone   TEXT NOT NULL DEFAULT 'UTC',
		input      JSONB NOT NULL DEFAULT '{}',
		config     JSONB NOT NULL DEFAULT '{}',
		enabled    BOOLEAN NOT NULL DEFAULT true,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (tenant_id, name)
	);
	ALTER TABLE cron_schedules ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default';
	CREATE UNIQUE INDEX IF NOT EXISTS ux_cron_schedules_tenant_name ON cron_schedules(tenant_id, name);

	-- Postgres is the deployment where "multi-instance-safe claiming"
	-- actually matters (multiple control-plane replicas, same DB) -- this
	-- table plus TryClaimCronFire's INSERT ... ON CONFLICT DO NOTHING is
	-- the "Postgres claim window" referenced by other backends' comments.
	CREATE TABLE IF NOT EXISTS cron_claims (
		tenant_id     TEXT NOT NULL DEFAULT 'default',
		schedule_name TEXT NOT NULL,
		fire_time     TIMESTAMPTZ NOT NULL,
		claimed_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (tenant_id, schedule_name, fire_time)
	);
	ALTER TABLE cron_claims ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default';
	CREATE UNIQUE INDEX IF NOT EXISTS ux_cron_claims_tenant_sched_fire ON cron_claims(tenant_id, schedule_name, fire_time);
	`
	// Executed as separate statements (not one conn.Exec(ctx, schema) call)
	// deliberately -- a second real race found via the same live
	// multi-instance test that found the advisory-lock issue above.
	// Postgres's simple-query protocol runs a multi-statement string as ONE
	// implicit transaction, so a single conn.Exec(ctx, schema) call holds
	// every table's DDL locks (many tables' worth) for the whole
	// transaction's duration. The advisory lock serializes this DDL against
	// *other Init() calls*, but not against a DIFFERENT, already-initialized
	// replica's normal application queries (cron polling, agent
	// registration, etc.) touching the same tables -- confirmed live:
	// "deadlock detected (SQLSTATE 40P01)" when a third replica's
	// still-open, multi-table DDL transaction crossed lock-acquisition
	// order with a second replica's already-running queries. Splitting into
	// individually-committing statements shrinks each one's lock footprint
	// to a single table for a few milliseconds, closing that window. Safe
	// here specifically because this schema string contains no dollar-
	// quoted (`DO $$ ... END $$`) blocks with internal semicolons -- see
	// the separate run_cache migration below, which does, and stays a
	// single conn.Exec call for exactly that reason.
	for _, stmt := range splitSchemaStatements(schema) {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("schema statement %q: %w", firstLine(stmt), err)
		}
	}
	// Pre-multi-tenancy (and early multi-tenancy) run_cache used
	// PRIMARY KEY (cache_key) alone. That makes ON CONFLICT(tenant_id,
	// cache_key) unable to insert the same raw key for two tenants --
	// widen the PK in place when the old shape is still present.
	// Fresh installs already have the composite PK from CREATE TABLE.
	_, err := conn.Exec(ctx, `
		DO $$ BEGIN
			IF EXISTS (
				SELECT 1
				FROM pg_constraint c
				JOIN pg_class t ON c.conrelid = t.oid
				WHERE t.relname = 'run_cache'
				  AND c.contype = 'p'
				  AND pg_get_constraintdef(c.oid) = 'PRIMARY KEY (cache_key)'
			) THEN
				ALTER TABLE run_cache DROP CONSTRAINT run_cache_pkey;
				ALTER TABLE run_cache ADD PRIMARY KEY (tenant_id, cache_key);
			END IF;
		END $$;
	`)
	return err
}

// Close closes the connection pool.
func (s *Store) Close() error {
	s.pool.Close()
	return nil
}

// Ping verifies the pool can still reach Postgres right now.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// TruncateAll removes all rows from all tables. For testing only.
func (s *Store) TruncateAll(ctx context.Context) error {
	// webhook_dead_letters, run_cache, and agent_versions have no FK to
	// any of the other tables (run_id/agent_id are plain TEXT
	// references, not foreign keys), so TRUNCATE ... CASCADE from the
	// other tables would never reach them -- they must be listed
	// explicitly or a fresh conformance subtest sees leftover rows from
	// an earlier subtest (confirmed: list_empty_when_none_saved failed
	// with leftover rows from prior subtests before this fix was first
	// applied for webhook_dead_letters; agent_versions had the exact
	// same bug, found via audit and fixed here -- same class of gap as
	// the identical one already fixed for Mongo's TruncateAll).
	_, err := s.pool.Exec(ctx, `
		TRUNCATE store_items, runs, threads, agent_schemas, agents, agent_versions, registry_entries, registry_entry_versions, webhook_dead_letters, run_cache, cron_schedules, cron_claims CASCADE
	`)
	return err
}

// --------------------------------------------------------------------------
// Agents
// --------------------------------------------------------------------------

func (s *Store) UpsertAgent(ctx context.Context, agent *models.Agent) error {
	meta, _ := json.Marshal(agent.Metadata)
	caps, _ := json.Marshal(agent.Capabilities)
	now := time.Now().UTC()
	tenantID := tenant.FromContext(ctx)

	// A transaction, not a single UPSERT statement, because writing the
	// version-history snapshot (agent_versions) needs to know WHETHER
	// this call actually bumped the version -- doing that inside one
	// UPSERT's CASE expression (the pre-versioning-history approach)
	// has no way to also conditionally write a second table's row from
	// the same statement. Not a contended path: agent upserts happen at
	// config-bootstrap time, not per-request.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op if committed

	var newVersion int
	// version bumps only if the definition actually changed. JSONB equality
	// compares the parsed value (not the source string), so this is
	// robust to whitespace/key-order differences unlike a naive text
	// comparison -- see the SQLite equivalent for the TEXT-column version.
	err = tx.QueryRow(ctx, `
		INSERT INTO agents (tenant_id, agent_id, name, description, metadata, capabilities, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, 1, $7, $7)
		ON CONFLICT(tenant_id, agent_id) DO UPDATE SET
			name=EXCLUDED.name, description=EXCLUDED.description,
			metadata=EXCLUDED.metadata, capabilities=EXCLUDED.capabilities,
			updated_at=EXCLUDED.updated_at,
			version = CASE WHEN agents.name IS DISTINCT FROM EXCLUDED.name
			                 OR agents.description IS DISTINCT FROM EXCLUDED.description
			                 OR agents.metadata IS DISTINCT FROM EXCLUDED.metadata
			                 OR agents.capabilities IS DISTINCT FROM EXCLUDED.capabilities
			               THEN agents.version + 1
			               ELSE agents.version END
		RETURNING version
	`, tenantID, agent.AgentID, agent.Name, agent.Description, meta, caps, now).Scan(&newVersion)
	if err != nil {
		return err
	}

	// Only write a version snapshot if this call is the one that
	// created it -- an unchanged re-registration (e.g. every control
	// plane restart with an unchanged langgraph.json) must not create
	// duplicate agent_versions rows for the same version number.
	// version=1 always needs a snapshot on first insert; version>1
	// needs one only if it doesn't already exist (idempotent guard
	// against this same "unchanged" case at any version).
	_, err = tx.Exec(ctx, `
		INSERT INTO agent_versions (tenant_id, agent_id, version, name, description, metadata, capabilities, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (tenant_id, agent_id, version) DO NOTHING
	`, tenantID, agent.AgentID, newVersion, agent.Name, agent.Description, meta, caps, now)
	if err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func (s *Store) ListAgentVersions(ctx context.Context, agentID string) ([]*models.AgentVersion, error) {
	query := `SELECT tenant_id, agent_id, version, name, description, metadata, capabilities, created_at FROM agent_versions WHERE agent_id = $1`
	args := []interface{}{agentID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = $2`
		args = append(args, tenant.FromContext(ctx))
	}
	query += ` ORDER BY version DESC`

	rows, err := s.pool.Query(ctx, query, args...)
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
	query := `SELECT tenant_id, agent_id, version, name, description, metadata, capabilities, created_at FROM agent_versions WHERE agent_id = $1 AND version = $2`
	args := []interface{}{agentID, version}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = $3`
		args = append(args, tenant.FromContext(ctx))
	}
	row := s.pool.QueryRow(ctx, query, args...)
	v, err := scanAgentVersion(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, &state.ErrNotFound{Resource: "agent_version", ID: fmt.Sprintf("%s@v%d", agentID, version)}
		}
		return nil, err
	}
	return v, nil
}

// agentVersionScanner covers both pgx.Row (QueryRow) and pgx.Rows
// (Query) -- both expose an identical Scan method, so this one helper
// serves GetAgentVersion and ListAgentVersions without duplicating the
// column list or JSON-unmarshal logic between them.
type agentVersionScanner interface {
	Scan(dest ...interface{}) error
}

func scanAgentVersion(row agentVersionScanner) (*models.AgentVersion, error) {
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
// Registry: agent marketplace / registry
// --------------------------------------------------------------------------

// PublishRegistryEntry follows the exact same transaction + RETURNING +
// conditional-version-snapshot pattern as UpsertAgent above -- see its
// comment for the full rationale (a single UPSERT's CASE expression
// can't also conditionally write a second table's row).
func (s *Store) PublishRegistryEntry(ctx context.Context, entry *models.RegistryEntry) error {
	meta, _ := json.Marshal(entry.Metadata)
	tags, _ := json.Marshal(nonNilTags(entry.Tags))
	now := time.Now().UTC()
	tenantID := tenant.FromContext(ctx)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op if committed

	var newVersion int
	err = tx.QueryRow(ctx, `
		INSERT INTO registry_entries (tenant_id, name, display_name, description, author, tags, source_type, source_ref, metadata, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 1, $10, $10)
		ON CONFLICT(tenant_id, name) DO UPDATE SET
			display_name=EXCLUDED.display_name, description=EXCLUDED.description,
			author=EXCLUDED.author, tags=EXCLUDED.tags,
			source_type=EXCLUDED.source_type, source_ref=EXCLUDED.source_ref,
			metadata=EXCLUDED.metadata, updated_at=EXCLUDED.updated_at,
			version = CASE WHEN registry_entries.display_name IS DISTINCT FROM EXCLUDED.display_name
			                 OR registry_entries.description IS DISTINCT FROM EXCLUDED.description
			                 OR registry_entries.author IS DISTINCT FROM EXCLUDED.author
			                 OR registry_entries.tags IS DISTINCT FROM EXCLUDED.tags
			                 OR registry_entries.source_type IS DISTINCT FROM EXCLUDED.source_type
			                 OR registry_entries.source_ref IS DISTINCT FROM EXCLUDED.source_ref
			                 OR registry_entries.metadata IS DISTINCT FROM EXCLUDED.metadata
			               THEN registry_entries.version + 1
			               ELSE registry_entries.version END
		RETURNING version
	`, tenantID, entry.Name, entry.DisplayName, entry.Description, entry.Author, tags, entry.SourceType, entry.SourceRef, meta, now).Scan(&newVersion)
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO registry_entry_versions (tenant_id, name, version, display_name, description, author, tags, source_type, source_ref, metadata, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (tenant_id, name, version) DO NOTHING
	`, tenantID, entry.Name, newVersion, entry.DisplayName, entry.Description, entry.Author, tags, entry.SourceType, entry.SourceRef, meta, now)
	if err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func nonNilTags(tags []string) []string {
	if tags == nil {
		return []string{}
	}
	return tags
}

func (s *Store) GetRegistryEntry(ctx context.Context, name string) (*models.RegistryEntry, error) {
	query := `SELECT tenant_id, name, display_name, description, author, tags, source_type, source_ref, metadata, version, created_at, updated_at FROM registry_entries WHERE name = $1`
	args := []interface{}{name}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = $2`
		args = append(args, tenant.FromContext(ctx))
	}
	row := s.pool.QueryRow(ctx, query, args...)
	e, err := scanRegistryEntry(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, &state.ErrNotFound{Resource: "registry_entry", ID: name}
		}
		return nil, err
	}
	return e, nil
}

func (s *Store) SearchRegistryEntries(ctx context.Context, req *models.RegistrySearchRequest) ([]*models.RegistryEntry, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	query := `SELECT tenant_id, name, display_name, description, author, tags, source_type, source_ref, metadata, version, created_at, updated_at FROM registry_entries`
	var args []interface{}
	var where []string
	argN := 1

	if !tenant.IsSystem(ctx) {
		where = append(where, fmt.Sprintf("tenant_id = $%d", argN))
		args = append(args, tenant.FromContext(ctx))
		argN++
	}
	if req.Name != "" {
		where = append(where, fmt.Sprintf("name ILIKE $%d", argN))
		args = append(args, "%"+req.Name+"%")
		argN++
	}
	if req.Author != "" {
		where = append(where, fmt.Sprintf("author = $%d", argN))
		args = append(args, req.Author)
		argN++
	}
	for _, tag := range req.Tags {
		where = append(where, fmt.Sprintf("tags @> $%d", argN))
		tagJSON, _ := json.Marshal([]string{tag})
		args = append(args, string(tagJSON))
		argN++
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += fmt.Sprintf(" ORDER BY name ASC LIMIT $%d OFFSET $%d", argN, argN+1)
	args = append(args, limit, req.Offset)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	entries := []*models.RegistryEntry{}
	for rows.Next() {
		e, err := scanRegistryEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// DeleteRegistryEntry also removes the entry's own version history, not
// just its current row -- real bug found via live testing: without
// this, a delete-then-republish of the same name hits the version
// snapshot table's "ON CONFLICT (tenant_id, name, version) DO NOTHING"
// against ORPHANED rows left over from before the delete, so the new
// publish's own v1 snapshot is silently discarded and
// ListRegistryEntryVersions keeps showing pre-delete content. Deleting
// history alongside the entry means a republish always starts from a
// genuinely clean version 1, matching what "deleted" should mean for a
// scoped-to-tenant, no-op cascading resource with no other backend to
// preserve the audit trail for once its own parent row is gone (unlike
// agent versioning, which never deletes an agent at all today).
func (s *Store) DeleteRegistryEntry(ctx context.Context, name string) error {
	tenantID := tenant.FromContext(ctx)

	// A transaction, not two independent Execs, so a crash/network
	// failure between the two deletes can't leave the entry gone but
	// its version history still present (same orphan-row class of bug
	// as REG-005b, just from a different failure mode -- a partial
	// crash instead of a missing statement).
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op if committed

	query := `DELETE FROM registry_entries WHERE name = $1`
	args := []interface{}{name}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = $2`
		args = append(args, tenantID)
	}
	tag, err := tx.Exec(ctx, query, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return &state.ErrNotFound{Resource: "registry_entry", ID: name}
	}

	versionsQuery := `DELETE FROM registry_entry_versions WHERE name = $1`
	versionsArgs := []interface{}{name}
	if !tenant.IsSystem(ctx) {
		versionsQuery += ` AND tenant_id = $2`
		versionsArgs = append(versionsArgs, tenantID)
	}
	if _, err := tx.Exec(ctx, versionsQuery, versionsArgs...); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func (s *Store) ListRegistryEntryVersions(ctx context.Context, name string) ([]*models.RegistryEntryVersion, error) {
	query := `SELECT tenant_id, name, version, display_name, description, author, tags, source_type, source_ref, metadata, created_at FROM registry_entry_versions WHERE name = $1`
	args := []interface{}{name}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = $2`
		args = append(args, tenant.FromContext(ctx))
	}
	query += ` ORDER BY version DESC`

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	versions := []*models.RegistryEntryVersion{}
	for rows.Next() {
		v, err := scanRegistryEntryVersion(rows)
		if err != nil {
			return nil, err
		}
		versions = append(versions, v)
	}
	return versions, rows.Err()
}

func (s *Store) GetRegistryEntryVersion(ctx context.Context, name string, version int) (*models.RegistryEntryVersion, error) {
	query := `SELECT tenant_id, name, version, display_name, description, author, tags, source_type, source_ref, metadata, created_at FROM registry_entry_versions WHERE name = $1 AND version = $2`
	args := []interface{}{name, version}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = $3`
		args = append(args, tenant.FromContext(ctx))
	}
	row := s.pool.QueryRow(ctx, query, args...)
	v, err := scanRegistryEntryVersion(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, &state.ErrNotFound{Resource: "registry_entry_version", ID: fmt.Sprintf("%s@v%d", name, version)}
		}
		return nil, err
	}
	return v, nil
}

func scanRegistryEntry(row agentVersionScanner) (*models.RegistryEntry, error) {
	var e models.RegistryEntry
	var tagsBytes, metaBytes []byte
	if err := row.Scan(&e.TenantID, &e.Name, &e.DisplayName, &e.Description, &e.Author, &tagsBytes, &e.SourceType, &e.SourceRef, &metaBytes, &e.Version, &e.CreatedAt, &e.UpdatedAt); err != nil {
		return nil, err
	}
	json.Unmarshal(tagsBytes, &e.Tags)
	json.Unmarshal(metaBytes, &e.Metadata)
	return &e, nil
}

func scanRegistryEntryVersion(row agentVersionScanner) (*models.RegistryEntryVersion, error) {
	var v models.RegistryEntryVersion
	var tagsBytes, metaBytes []byte
	if err := row.Scan(&v.TenantID, &v.Name, &v.Version, &v.DisplayName, &v.Description, &v.Author, &tagsBytes, &v.SourceType, &v.SourceRef, &metaBytes, &v.CreatedAt); err != nil {
		return nil, err
	}
	json.Unmarshal(tagsBytes, &v.Tags)
	json.Unmarshal(metaBytes, &v.Metadata)
	return &v, nil
}

func (s *Store) GetAgent(ctx context.Context, agentID string) (*models.Agent, error) {
	query := `SELECT tenant_id, agent_id, name, description, metadata, capabilities, version FROM agents WHERE agent_id = $1`
	args := []interface{}{agentID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = $2`
		args = append(args, tenant.FromContext(ctx))
	}
	row := s.pool.QueryRow(ctx, query, args...)

	var a models.Agent
	var metaBytes, capsBytes []byte
	if err := row.Scan(&a.TenantID, &a.AgentID, &a.Name, &a.Description, &metaBytes, &capsBytes, &a.Version); err != nil {
		if err == pgx.ErrNoRows {
			return nil, &state.ErrNotFound{Resource: "agent", ID: agentID}
		}
		return nil, err
	}
	json.Unmarshal(metaBytes, &a.Metadata)
	json.Unmarshal(capsBytes, &a.Capabilities)
	return &a, nil
}

func (s *Store) SearchAgents(ctx context.Context, req *models.AgentSearchRequest) ([]*models.Agent, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	offset := req.Offset
	if offset < 0 {
		offset = 0
	}

	query := `SELECT tenant_id, agent_id, name, description, metadata, capabilities, version FROM agents`
	var args []interface{}
	var where []string
	argN := 1

	if !tenant.IsSystem(ctx) {
		where = append(where, fmt.Sprintf("tenant_id = $%d", argN))
		args = append(args, tenant.FromContext(ctx))
		argN++
	}
	if req.Name != "" {
		where = append(where, fmt.Sprintf("name ILIKE $%d", argN))
		args = append(args, "%"+req.Name+"%")
		argN++
	}
	for k, v := range req.Metadata {
		where = append(where, fmt.Sprintf("metadata->>$%d = $%d", argN, argN+1))
		args = append(args, k)
		if sv, ok := v.(string); ok {
			args = append(args, sv)
		} else {
			valJSON, _ := json.Marshal(v)
			args = append(args, string(valJSON))
		}
		argN += 2
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += fmt.Sprintf(" ORDER BY name LIMIT $%d OFFSET $%d", argN, argN+1)
	args = append(args, limit, offset)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	agents := []*models.Agent{}
	for rows.Next() {
		var a models.Agent
		var metaBytes, capsBytes []byte
		if err := rows.Scan(&a.TenantID, &a.AgentID, &a.Name, &a.Description, &metaBytes, &capsBytes, &a.Version); err != nil {
			return nil, err
		}
		json.Unmarshal(metaBytes, &a.Metadata)
		json.Unmarshal(capsBytes, &a.Capabilities)
		agents = append(agents, &a)
	}
	return agents, nil
}

func (s *Store) UpsertAgentSchema(ctx context.Context, schema *models.AgentSchema) error {
	input, _ := json.Marshal(schema.InputSchema)
	output, _ := json.Marshal(schema.OutputSchema)
	st, _ := json.Marshal(schema.StateSchema)
	cfg, _ := json.Marshal(schema.ConfigSchema)

	_, err := s.pool.Exec(ctx, `
		INSERT INTO agent_schemas (tenant_id, agent_id, input_schema, output_schema, state_schema, config_schema)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT(tenant_id, agent_id) DO UPDATE SET
			input_schema=EXCLUDED.input_schema, output_schema=EXCLUDED.output_schema,
			state_schema=EXCLUDED.state_schema, config_schema=EXCLUDED.config_schema
	`, tenant.FromContext(ctx), schema.AgentID, input, output, st, cfg)
	return err
}

func (s *Store) GetAgentSchema(ctx context.Context, agentID string) (*models.AgentSchema, error) {
	query := `SELECT agent_id, input_schema, output_schema, state_schema, config_schema FROM agent_schemas WHERE agent_id = $1`
	args := []interface{}{agentID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = $2`
		args = append(args, tenant.FromContext(ctx))
	}
	row := s.pool.QueryRow(ctx, query, args...)

	var as models.AgentSchema
	var inBytes, outBytes, stBytes, cfgBytes []byte
	if err := row.Scan(&as.AgentID, &inBytes, &outBytes, &stBytes, &cfgBytes); err != nil {
		if err == pgx.ErrNoRows {
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

// --------------------------------------------------------------------------
// Threads
// --------------------------------------------------------------------------

func (s *Store) CreateThread(ctx context.Context, thread *models.Thread) error {
	meta, _ := json.Marshal(thread.Metadata)
	vals, _ := json.Marshal(thread.Values)

	_, err := s.pool.Exec(ctx, `
		INSERT INTO threads (tenant_id, thread_id, status, metadata, values_json, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, tenant.FromContext(ctx), thread.ThreadID, thread.Status, meta, vals, thread.CreatedAt, thread.UpdatedAt)

	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return &state.ErrConflict{Resource: "thread", ID: thread.ThreadID}
		}
		return err
	}
	// The column defaults to 1 in the DB (see the schema above), but the
	// caller's own struct -- which handlers echo straight back as the
	// create response, without a follow-up GetThread -- never had this
	// field set. Without this, a client that creates a thread and reads
	// "version" off THAT response to make an immediate IfMatchVersion
	// patch would see a stale 0 and needlessly get rejected.
	thread.Version = 1
	return nil
}

func (s *Store) GetThread(ctx context.Context, threadID string) (*models.Thread, error) {
	query := `SELECT tenant_id, thread_id, status, metadata, values_json, created_at, updated_at, version FROM threads WHERE thread_id = $1`
	args := []interface{}{threadID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = $2`
		args = append(args, tenant.FromContext(ctx))
	}
	row := s.pool.QueryRow(ctx, query, args...)

	var t models.Thread
	var metaBytes, valsBytes []byte
	if err := row.Scan(&t.TenantID, &t.ThreadID, &t.Status, &metaBytes, &valsBytes, &t.CreatedAt, &t.UpdatedAt, &t.Version); err != nil {
		if err == pgx.ErrNoRows {
			return nil, &state.ErrNotFound{Resource: "thread", ID: threadID}
		}
		return nil, err
	}
	json.Unmarshal(metaBytes, &t.Metadata)
	json.Unmarshal(valsBytes, &t.Values)
	return &t, nil
}

func (s *Store) UpdateThread(ctx context.Context, threadID string, patch *models.ThreadPatch) (*models.Thread, error) {
	existing, err := s.GetThread(ctx, threadID)
	if err != nil {
		return nil, err
	}

	if patch.Metadata != nil {
		for k, v := range patch.Metadata {
			if existing.Metadata == nil {
				existing.Metadata = make(map[string]interface{})
			}
			existing.Metadata[k] = v
		}
	}
	if patch.Values != nil {
		for k, v := range patch.Values {
			if existing.Values == nil {
				existing.Values = make(map[string]interface{})
			}
			existing.Values[k] = v
		}
	}

	existing.UpdatedAt = time.Now().UTC()
	meta, _ := json.Marshal(existing.Metadata)
	vals, _ := json.Marshal(existing.Values)

	// version = version + 1 in the SET clause (not existing.Version+1 as
	// a bound parameter) so this stays correct even under the IfMatchVersion
	// race fallback below, where existing.Version might be stale by the
	// time this statement actually runs -- the DB computes the increment
	// from whatever the CURRENT row has, atomically, in the same statement
	// that checks the WHERE-clause version match.
	query := `UPDATE threads SET metadata = $1, values_json = $2, updated_at = $3, version = version + 1 WHERE thread_id = $4`
	args := []interface{}{meta, vals, existing.UpdatedAt, threadID}
	if !tenant.IsSystem(ctx) {
		query += fmt.Sprintf(" AND tenant_id = $%d", len(args)+1)
		args = append(args, tenant.FromContext(ctx))
	}
	// Atomic optimistic-concurrency check: the version comparison lives
	// in the SAME UPDATE's WHERE clause, not a separate read-then-compare
	// in Go, which would still race under real concurrency (the same
	// TOCTOU class TryClaimThread's own doc comment calls out).
	if patch.IfMatchVersion != nil {
		query += fmt.Sprintf(" AND version = $%d", len(args)+1)
		args = append(args, *patch.IfMatchVersion)
	}
	// RETURNING version (not existing.Version++, an in-memory guess) so
	// the response reflects the row's REAL post-update value even under
	// concurrent unconditional writers -- two overlapping unconditional
	// patches both compute existing.Version+1 in memory without seeing
	// each other's write, so BOTH responses would falsely claim the same
	// version despite the DB ending up two higher than either started
	// from.
	query += " RETURNING version"
	var newVersion int
	err = s.pool.QueryRow(ctx, query, args...).Scan(&newVersion)
	if err != nil {
		if err == pgx.ErrNoRows {
			if patch.IfMatchVersion != nil {
				return nil, &state.ErrConflict{Resource: "thread", ID: threadID, Reason: "version mismatch (optimistic concurrency)"}
			}
			// Unconditional update matched no row (e.g. tenant mismatch
			// or a concurrent delete) -- preserve the pre-existing
			// behavior of returning the in-memory struct rather than
			// erroring; this path never checked rows-affected before.
			existing.Version++
			return existing, nil
		}
		return nil, err
	}
	existing.Version = newVersion
	return existing, nil
}

func (s *Store) DeleteThread(ctx context.Context, threadID string) error {
	query := `DELETE FROM threads WHERE thread_id = $1`
	args := []interface{}{threadID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = $2`
		args = append(args, tenant.FromContext(ctx))
	}
	tag, err := s.pool.Exec(ctx, query, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return &state.ErrNotFound{Resource: "thread", ID: threadID}
	}
	return nil
}

func (s *Store) SearchThreads(ctx context.Context, req *models.ThreadSearchRequest) ([]*models.Thread, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}

	query := `SELECT tenant_id, thread_id, status, metadata, values_json, created_at, updated_at, version FROM threads`
	var args []interface{}
	var where []string
	argN := 1

	if !tenant.IsSystem(ctx) {
		where = append(where, fmt.Sprintf("tenant_id = $%d", argN))
		args = append(args, tenant.FromContext(ctx))
		argN++
	}
	if req.Status != nil {
		where = append(where, fmt.Sprintf("status = $%d", argN))
		args = append(args, string(*req.Status))
		argN++
	}
	for k, v := range req.Metadata {
		where = append(where, fmt.Sprintf("metadata->>$%d = $%d", argN, argN+1))
		args = append(args, k)
		if sv, ok := v.(string); ok {
			args = append(args, sv)
		} else {
			valJSON, _ := json.Marshal(v)
			args = append(args, string(valJSON))
		}
		argN += 2
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d OFFSET $%d", argN, argN+1)
	args = append(args, limit, req.Offset)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	threads := []*models.Thread{}
	for rows.Next() {
		var t models.Thread
		var metaBytes, valsBytes []byte
		if err := rows.Scan(&t.TenantID, &t.ThreadID, &t.Status, &metaBytes, &valsBytes, &t.CreatedAt, &t.UpdatedAt, &t.Version); err != nil {
			return nil, err
		}
		json.Unmarshal(metaBytes, &t.Metadata)
		json.Unmarshal(valsBytes, &t.Values)
		threads = append(threads, &t)
	}
	return threads, nil
}

func (s *Store) SetThreadStatus(ctx context.Context, threadID string, status models.ThreadStatus) error {
	now := time.Now().UTC()
	query := `UPDATE threads SET status = $1, updated_at = $2 WHERE thread_id = $3`
	args := []interface{}{string(status), now, threadID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = $4`
		args = append(args, tenant.FromContext(ctx))
	}
	_, err := s.pool.Exec(ctx, query, args...)
	return err
}

func (s *Store) TryClaimThread(ctx context.Context, threadID string) (bool, error) {
	now := time.Now().UTC()
	query := `UPDATE threads SET status = $1, updated_at = $2 WHERE thread_id = $3 AND status != $1`
	args := []interface{}{string(models.ThreadStatusBusy), now, threadID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = $4`
		args = append(args, tenant.FromContext(ctx))
	}
	tag, err := s.pool.Exec(ctx, query, args...)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ReleaseThreadIfNoOtherActive is a single conditional UPDATE -- the
// inverse of TryClaimThread -- so a late StatusCallback cannot idle a
// thread that a newer run already claimed.
func (s *Store) ReleaseThreadIfNoOtherActive(ctx context.Context, threadID, excludeRunID string, status models.ThreadStatus) (bool, error) {
	now := time.Now().UTC()
	query := `UPDATE threads SET status = $1, updated_at = $2
		WHERE thread_id = $3 AND status = $4
		AND NOT EXISTS (
			SELECT 1 FROM runs r
			WHERE r.thread_id = threads.thread_id
			  AND r.status IN ('pending','running')
			  AND r.run_id <> $5
		)`
	args := []interface{}{string(status), now, threadID, string(models.ThreadStatusBusy), excludeRunID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = $6`
		args = append(args, tenant.FromContext(ctx))
	}
	tag, err := s.pool.Exec(ctx, query, args...)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
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

	_, err := s.pool.Exec(ctx, `
		INSERT INTO thread_checkpoints (tenant_id, checkpoint_id, thread_id, checkpoint_ns, parent_id, values_json, metadata, next_nodes, tasks, interrupts, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`, tenant.FromContext(ctx), ts.Checkpoint.CheckpointID, threadID, ts.Checkpoint.CheckpointNS,
		parentID, vals, meta, next, tasks, interrupts, createdAt)
	return err
}

func (s *Store) GetLatestCheckpoint(ctx context.Context, threadID string) (*models.ThreadState, error) {
	query := `SELECT checkpoint_id, thread_id, checkpoint_ns, parent_id, values_json, metadata, next_nodes, tasks, interrupts, created_at
		FROM thread_checkpoints WHERE thread_id = $1`
	args := []interface{}{threadID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = $2`
		args = append(args, tenant.FromContext(ctx))
	}
	query += ` ORDER BY created_at DESC LIMIT 1`
	row := s.pool.QueryRow(ctx, query, args...)

	var ts models.ThreadState
	var cpID, tID, cpNS string
	var parentID *string
	var valsBytes, metaBytes, nextBytes, tasksBytes, intBytes []byte
	var createdAt time.Time

	if err := row.Scan(&cpID, &tID, &cpNS, &parentID, &valsBytes, &metaBytes, &nextBytes, &tasksBytes, &intBytes, &createdAt); err != nil {
		if err == pgx.ErrNoRows {
			return nil, &state.ErrNotFound{Resource: "checkpoint", ID: "latest"}
		}
		return nil, err
	}
	fillPgCheckpoint(&ts, cpID, tID, cpNS, parentID, valsBytes, metaBytes, nextBytes, tasksBytes, intBytes, createdAt)
	return &ts, nil
}

func (s *Store) ListCheckpoints(ctx context.Context, threadID string, limit int, before string) ([]*models.ThreadState, error) {
	if limit <= 0 {
		limit = 10
	}

	query := `SELECT checkpoint_id, thread_id, checkpoint_ns, parent_id, values_json, metadata, next_nodes, tasks, interrupts, created_at
		FROM thread_checkpoints WHERE thread_id = $1`
	args := []interface{}{threadID}
	argN := 2

	if !tenant.IsSystem(ctx) {
		query += fmt.Sprintf(` AND tenant_id = $%d`, argN)
		args = append(args, tenant.FromContext(ctx))
		argN++
	}
	if before != "" {
		query += fmt.Sprintf(` AND created_at < (SELECT created_at FROM thread_checkpoints WHERE checkpoint_id = $%d)`, argN)
		args = append(args, before)
		argN++
	}
	query += fmt.Sprintf(` ORDER BY created_at DESC LIMIT $%d`, argN)
	args = append(args, limit)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	states := []*models.ThreadState{}
	for rows.Next() {
		var ts models.ThreadState
		var cpID, tID, cpNS string
		var parentID *string
		var valsBytes, metaBytes, nextBytes, tasksBytes, intBytes []byte
		var createdAt time.Time

		if err := rows.Scan(&cpID, &tID, &cpNS, &parentID, &valsBytes, &metaBytes, &nextBytes, &tasksBytes, &intBytes, &createdAt); err != nil {
			return nil, err
		}
		fillPgCheckpoint(&ts, cpID, tID, cpNS, parentID, valsBytes, metaBytes, nextBytes, tasksBytes, intBytes, createdAt)
		states = append(states, &ts)
	}
	return states, nil
}

func fillPgCheckpoint(ts *models.ThreadState, cpID, tID, cpNS string, parentID *string, valsBytes, metaBytes, nextBytes, tasksBytes, intBytes []byte, createdAt time.Time) {
	ts.Checkpoint = models.ThreadCheckpoint{
		CheckpointID: cpID,
		ThreadID:     tID,
		CheckpointNS: cpNS,
	}
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
	if parentID != nil {
		ts.ParentCheckpoint = &models.ThreadCheckpoint{
			CheckpointID: *parentID,
			ThreadID:     tID,
			CheckpointNS: cpNS,
		}
	}
}

// --------------------------------------------------------------------------
// Runs
// --------------------------------------------------------------------------

func (s *Store) CreateRun(ctx context.Context, run *models.Run) error {
	meta, _ := json.Marshal(run.Metadata)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO runs (tenant_id, run_id, thread_id, agent_id, status, metadata, input, config, created_at, updated_at, parent_run_id, root_run_id, depth)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	`, tenant.FromContext(ctx), run.RunID, run.ThreadID, run.AgentID, run.Status, meta,
		nullableJSON(run.Input), nullableJSON(run.Config),
		run.CreatedAt, run.UpdatedAt, run.ParentRunID, run.RootRunID, run.Depth)
	if err != nil {
		// run_id is the primary key; wrap a unique violation into
		// state.ErrConflict, same idiom CreateThread already uses --
		// otherwise a cross-tenant run_id collision (GetRun is
		// tenant-scoped, so createRunCtx's own retry-race fallback
		// can't find the OTHER tenant's row to dispatch through) falls
		// all the way back to the API layer as a raw, unwrapped
		// *pgconn.PgError, surfacing as a generic 500 instead of a
		// clean 409.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return &state.ErrConflict{Resource: "run", ID: run.RunID}
		}
	}
	return err
}

func (s *Store) GetRun(ctx context.Context, runID string) (*models.Run, error) {
	query := `SELECT tenant_id, run_id, thread_id, agent_id, status, metadata, input, config, output, error_msg, created_at, updated_at, parent_run_id, root_run_id, depth FROM runs WHERE run_id = $1`
	args := []interface{}{runID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = $2`
		args = append(args, tenant.FromContext(ctx))
	}
	row := s.pool.QueryRow(ctx, query, args...)

	var r models.Run
	var metaBytes, inputBytes, configBytes, outputBytes []byte
	if err := row.Scan(&r.TenantID, &r.RunID, &r.ThreadID, &r.AgentID, &r.Status, &metaBytes, &inputBytes, &configBytes, &outputBytes, &r.Error, &r.CreatedAt, &r.UpdatedAt, &r.ParentRunID, &r.RootRunID, &r.Depth); err != nil {
		if err == pgx.ErrNoRows {
			return nil, &state.ErrNotFound{Resource: "run", ID: runID}
		}
		return nil, err
	}
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

func (s *Store) UpdateRunStatus(ctx context.Context, runID string, status models.RunStatus, output []byte, errMsg string) error {
	now := time.Now().UTC()
	query := `UPDATE runs SET status = $1, output = $2, error_msg = $3, updated_at = $4 WHERE run_id = $5`
	args := []interface{}{string(status), nullableJSON(output), errMsg, now, runID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = $6`
		args = append(args, tenant.FromContext(ctx))
	}
	_, err := s.pool.Exec(ctx, query, args...)
	return err
}

func (s *Store) ListActiveRunsCreatedBefore(ctx context.Context, before time.Time, limit int) ([]*models.Run, error) {
	if limit <= 0 {
		limit = 100
	}
	query := `SELECT tenant_id, run_id, thread_id, agent_id, status, metadata, input, config, output, error_msg, created_at, updated_at, parent_run_id, root_run_id, depth FROM runs WHERE status IN ('pending','running') AND created_at < $1`
	args := []interface{}{before.UTC()}
	argN := 2
	if !tenant.IsSystem(ctx) {
		query += fmt.Sprintf(` AND tenant_id = $%d`, argN)
		args = append(args, tenant.FromContext(ctx))
		argN++
	}
	query += fmt.Sprintf(` ORDER BY created_at ASC LIMIT $%d`, argN)
	args = append(args, limit)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	runs := []*models.Run{}
	for rows.Next() {
		var r models.Run
		var metaBytes, inputBytes, configBytes, outputBytes []byte
		if err := rows.Scan(&r.TenantID, &r.RunID, &r.ThreadID, &r.AgentID, &r.Status, &metaBytes, &inputBytes, &configBytes, &outputBytes, &r.Error, &r.CreatedAt, &r.UpdatedAt, &r.ParentRunID, &r.RootRunID, &r.Depth); err != nil {
			return nil, err
		}
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
		r.AssistantID = r.AgentID
		runs = append(runs, &r)
	}
	return runs, rows.Err()
}

func (s *Store) TryMarkRunTimeout(ctx context.Context, runID string, errMsg string) (bool, error) {
	now := time.Now().UTC()
	query := `UPDATE runs SET status = $1, error_msg = $2, updated_at = $3 WHERE run_id = $4 AND status IN ('pending','running')`
	args := []interface{}{string(models.RunStatusTimeout), errMsg, now, runID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = $5`
		args = append(args, tenant.FromContext(ctx))
	}
	tag, err := s.pool.Exec(ctx, query, args...)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (s *Store) DeleteRun(ctx context.Context, runID string) error {
	query := `DELETE FROM runs WHERE run_id = $1`
	args := []interface{}{runID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = $2`
		args = append(args, tenant.FromContext(ctx))
	}
	tag, err := s.pool.Exec(ctx, query, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return &state.ErrNotFound{Resource: "run", ID: runID}
	}
	return nil
}

// pruneableRunStatuses matches api.isTerminalStatus's definition
// (internal/api/runs.go) -- duplicated here rather than imported to keep
// internal/state free of a dependency on internal/api. "running" and
// "pending" are never pruneable regardless of age; "interrupted" runs
// (paused for human-in-the-loop resume) are included because resumption
// operates on the thread's checkpoint state, not this run row.
const pruneableRunStatusesSQL = `('success','error','interrupted','timeout')`

func (s *Store) PruneRuns(ctx context.Context, olderThan time.Time) (int64, error) {
	query := `DELETE FROM runs WHERE status IN ` + pruneableRunStatusesSQL + ` AND updated_at < $1`
	args := []interface{}{olderThan}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = $2`
		args = append(args, tenant.FromContext(ctx))
	}
	tag, err := s.pool.Exec(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *Store) PruneCheckpoints(ctx context.Context, keepLast int) (int64, error) {
	if keepLast <= 0 {
		return 0, nil
	}
	// tenantArg stays nil (SQL NULL) for a system context, matching
	// "$2::text IS NULL" below -- meaning "every tenant", not "no
	// tenant". Ranking (ROW_NUMBER) is computed per-thread so a busy
	// thread's history doesn't starve a quiet one's retention window.
	var tenantArg interface{}
	if !tenant.IsSystem(ctx) {
		tenantArg = tenant.FromContext(ctx)
	}
	query := `
		DELETE FROM thread_checkpoints
		WHERE checkpoint_id IN (
			SELECT checkpoint_id FROM (
				SELECT checkpoint_id,
					ROW_NUMBER() OVER (PARTITION BY thread_id ORDER BY created_at DESC) AS rn
				FROM thread_checkpoints
				WHERE ($2::text IS NULL OR tenant_id = $2)
			) ranked
			WHERE rn > $1
		)`
	tag, err := s.pool.Exec(ctx, query, keepLast, tenantArg)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *Store) PruneCronClaims(ctx context.Context, olderThan time.Time) (int64, error) {
	query := `DELETE FROM cron_claims WHERE fire_time < $1`
	args := []interface{}{olderThan}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = $2`
		args = append(args, tenant.FromContext(ctx))
	}
	tag, err := s.pool.Exec(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *Store) PruneExpiredStoreItems(ctx context.Context) (int64, error) {
	query := `DELETE FROM store_items WHERE expires_at IS NOT NULL AND expires_at <= $1`
	args := []interface{}{time.Now().UTC()}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = $2`
		args = append(args, tenant.FromContext(ctx))
	}
	tag, err := s.pool.Exec(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *Store) SearchRuns(ctx context.Context, req *models.RunSearchRequest) ([]*models.Run, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}

	query := `SELECT tenant_id, run_id, thread_id, agent_id, status, metadata, input, config, output, error_msg, created_at, updated_at, parent_run_id, root_run_id, depth FROM runs`
	var args []interface{}
	var where []string
	argN := 1

	if !tenant.IsSystem(ctx) {
		where = append(where, fmt.Sprintf("tenant_id = $%d", argN))
		args = append(args, tenant.FromContext(ctx))
		argN++
	}
	if req.Status != nil {
		where = append(where, fmt.Sprintf("status = $%d", argN))
		args = append(args, string(*req.Status))
		argN++
	}
	if req.ThreadID != "" {
		where = append(where, fmt.Sprintf("thread_id = $%d", argN))
		args = append(args, req.ThreadID)
		argN++
	}
	if req.AgentID != "" {
		where = append(where, fmt.Sprintf("agent_id = $%d", argN))
		args = append(args, req.AgentID)
		argN++
	}
	if req.RootRunID != "" {
		where = append(where, fmt.Sprintf("root_run_id = $%d", argN))
		args = append(args, req.RootRunID)
		argN++
	}
	for k, v := range req.Metadata {
		where = append(where, fmt.Sprintf("metadata->>$%d = $%d", argN, argN+1))
		args = append(args, k)
		if sv, ok := v.(string); ok {
			args = append(args, sv)
		} else {
			valJSON, _ := json.Marshal(v)
			args = append(args, string(valJSON))
		}
		argN += 2
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d OFFSET $%d", argN, argN+1)
	args = append(args, limit, req.Offset)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	runs := []*models.Run{}
	for rows.Next() {
		var r models.Run
		var metaBytes, inputBytes, configBytes, outputBytes []byte
		if err := rows.Scan(&r.TenantID, &r.RunID, &r.ThreadID, &r.AgentID, &r.Status, &metaBytes, &inputBytes, &configBytes, &outputBytes, &r.Error, &r.CreatedAt, &r.UpdatedAt, &r.ParentRunID, &r.RootRunID, &r.Depth); err != nil {
			return nil, err
		}
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
		runs = append(runs, &r)
	}
	return runs, nil
}

// --------------------------------------------------------------------------
// Store (key-value)
// --------------------------------------------------------------------------

// Same namespace encoding as SQLite — \x1F delimited, wrapped in leading
// and trailing delimiters for boundary-safe prefix matching.
const nsDelim = "\x1F"

func nsToString(ns []string) string {
	return nsDelim + strings.Join(ns, nsDelim) + nsDelim
}

func stringToNs(s string) []string {
	trimmed := strings.Trim(s, nsDelim)
	if trimmed == "" {
		return []string{}
	}
	return strings.Split(trimmed, nsDelim)
}

func nsPrefixPattern(prefix []string) string {
	if len(prefix) == 0 {
		return "%"
	}
	return nsDelim + strings.Join(prefix, nsDelim) + nsDelim + "%"
}

// storeItemExpiresAt computes the absolute expiry from a TTL in minutes,
// nil if ttlMinutes is nil (no expiration) -- shared by PutItem and the
// refresh-on-read path in GetItem/SearchItems so both compute the same
// way from the same "now."
func storeItemExpiresAt(now time.Time, ttlMinutes *float64) *time.Time {
	if ttlMinutes == nil {
		return nil
	}
	t := now.Add(time.Duration(*ttlMinutes * float64(time.Minute)))
	return &t
}

func (s *Store) PutItem(ctx context.Context, item *models.StoreItem) error {
	val, _ := json.Marshal(item.Value)
	ns := nsToString(item.Namespace)
	now := time.Now().UTC()
	expiresAt := storeItemExpiresAt(now, item.TTLMinutes)

	_, err := s.pool.Exec(ctx, `
		INSERT INTO store_items (tenant_id, namespace, key, value, created_at, updated_at, ttl_minutes, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT(tenant_id, namespace, key) DO UPDATE SET
			value=EXCLUDED.value, updated_at=EXCLUDED.updated_at,
			ttl_minutes=EXCLUDED.ttl_minutes, expires_at=EXCLUDED.expires_at
	`, tenant.FromContext(ctx), ns, item.Key, val, now, now, item.TTLMinutes, expiresAt)
	return err
}

func (s *Store) GetItem(ctx context.Context, namespace []string, key string, refreshTTL bool) (*models.StoreItem, error) {
	ns := nsToString(namespace)
	now := time.Now().UTC()
	query := `SELECT tenant_id, namespace, key, value, created_at, updated_at, ttl_minutes FROM store_items
		WHERE namespace = $1 AND key = $2 AND (expires_at IS NULL OR expires_at > $3)`
	args := []interface{}{ns, key, now}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = $4`
		args = append(args, tenant.FromContext(ctx))
	}
	row := s.pool.QueryRow(ctx, query, args...)

	var item models.StoreItem
	var tenantID, nsStr string
	var valBytes []byte
	var ttlMinutes *float64
	if err := row.Scan(&tenantID, &nsStr, &item.Key, &valBytes, &item.CreatedAt, &item.UpdatedAt, &ttlMinutes); err != nil {
		if err == pgx.ErrNoRows {
			return nil, &state.ErrNotFound{Resource: "store_item", ID: key}
		}
		return nil, err
	}
	item.Namespace = stringToNs(nsStr)
	json.Unmarshal(valBytes, &item.Value)

	if refreshTTL && ttlMinutes != nil {
		// Use the row's own tenant_id, not tenant.FromContext(ctx) --
		// for a system-context caller reading across tenants, those can
		// differ, which would otherwise silently match zero rows here.
		newExpiry := storeItemExpiresAt(now, ttlMinutes)
		_, _ = s.pool.Exec(ctx, `UPDATE store_items SET expires_at = $1 WHERE tenant_id = $2 AND namespace = $3 AND key = $4`,
			newExpiry, tenantID, ns, key)
	}
	return &item, nil
}

func (s *Store) DeleteItem(ctx context.Context, namespace []string, key string) error {
	ns := nsToString(namespace)
	query := `DELETE FROM store_items WHERE namespace = $1 AND key = $2`
	args := []interface{}{ns, key}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = $3`
		args = append(args, tenant.FromContext(ctx))
	}
	tag, err := s.pool.Exec(ctx, query, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return &state.ErrNotFound{Resource: "store_item", ID: key}
	}
	return nil
}

func (s *Store) SearchItems(ctx context.Context, req *models.StoreSearchRequest) ([]*models.StoreItem, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	now := time.Now().UTC()

	query := `SELECT tenant_id, namespace, key, value, created_at, updated_at, ttl_minutes FROM store_items`
	var args []interface{}
	where := []string{"(expires_at IS NULL OR expires_at > $1)"}
	args = append(args, now)
	argN := 2

	if !tenant.IsSystem(ctx) {
		where = append(where, fmt.Sprintf("tenant_id = $%d", argN))
		args = append(args, tenant.FromContext(ctx))
		argN++
	}
	if len(req.NamespacePrefix) > 0 {
		where = append(where, fmt.Sprintf("namespace LIKE $%d", argN))
		args = append(args, nsPrefixPattern(req.NamespacePrefix))
		argN++
	}
	for k, v := range req.Filter {
		where = append(where, fmt.Sprintf("value->>$%d = $%d", argN, argN+1))
		args = append(args, k)
		if sv, ok := v.(string); ok {
			args = append(args, sv)
		} else {
			valJSON, _ := json.Marshal(v)
			args = append(args, string(valJSON))
		}
		argN += 2
	}
	query += " WHERE " + strings.Join(where, " AND ")
	query += fmt.Sprintf(" ORDER BY updated_at DESC LIMIT $%d OFFSET $%d", argN, argN+1)
	args = append(args, limit, req.Offset)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Non-nil so a no-results search JSON-encodes to "items": [] rather
	// than "items": null -- SDK clients call .map() on it unconditionally.
	items := []*models.StoreItem{}
	// (tenantID, namespace-string, key, ttlMinutes) rows to refresh after
	// rows.Close() below -- can't run UPDATEs against the same connection
	// while still iterating a live pgx result set.
	type refreshRow struct {
		tenantID, ns, key string
		ttlMinutes        float64
	}
	var toRefresh []refreshRow
	for rows.Next() {
		var item models.StoreItem
		var tenantID, nsStr string
		var valBytes []byte
		var ttlMinutes *float64
		if err := rows.Scan(&tenantID, &nsStr, &item.Key, &valBytes, &item.CreatedAt, &item.UpdatedAt, &ttlMinutes); err != nil {
			return nil, err
		}
		item.Namespace = stringToNs(nsStr)
		json.Unmarshal(valBytes, &item.Value)
		items = append(items, &item)
		if req.RefreshTTLOrDefault() && ttlMinutes != nil {
			toRefresh = append(toRefresh, refreshRow{tenantID, nsStr, item.Key, *ttlMinutes})
		}
	}
	rows.Close()
	for _, rr := range toRefresh {
		newExpiry := storeItemExpiresAt(now, &rr.ttlMinutes)
		_, _ = s.pool.Exec(ctx, `UPDATE store_items SET expires_at = $1 WHERE tenant_id = $2 AND namespace = $3 AND key = $4`,
			newExpiry, rr.tenantID, rr.ns, rr.key)
	}
	return items, nil
}

func (s *Store) ListNamespaces(ctx context.Context, req *models.StoreListNamespacesRequest) ([][]string, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}

	query := `SELECT DISTINCT namespace FROM store_items`
	var args []interface{}
	var where []string
	argN := 1

	if !tenant.IsSystem(ctx) {
		where = append(where, fmt.Sprintf("tenant_id = $%d", argN))
		args = append(args, tenant.FromContext(ctx))
		argN++
	}
	if len(req.Prefix) > 0 {
		where = append(where, fmt.Sprintf("namespace LIKE $%d", argN))
		args = append(args, nsPrefixPattern(req.Prefix))
		argN++
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += fmt.Sprintf(" ORDER BY namespace LIMIT $%d OFFSET $%d", argN, argN+1)
	args = append(args, limit, req.Offset)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Non-nil so a no-results list JSON-encodes to [] rather than null.
	namespaces := [][]string{}
	for rows.Next() {
		var nsStr string
		if err := rows.Scan(&nsStr); err != nil {
			return nil, err
		}
		namespaces = append(namespaces, stringToNs(nsStr))
	}
	return namespaces, nil
}

// --------------------------------------------------------------------------
// Webhook dead-letter
// --------------------------------------------------------------------------

func (s *Store) SaveWebhookDeadLetter(ctx context.Context, dl *models.WebhookDeadLetter) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO webhook_dead_letters (id, url, event_type, run_id, payload, error, attempts, failed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, dl.ID, dl.URL, dl.EventType, dl.RunID, []byte(dl.Payload), dl.Error, dl.Attempts, dl.FailedAt)
	return err
}

func (s *Store) ListWebhookDeadLetters(ctx context.Context, limit int) ([]*models.WebhookDeadLetter, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, url, event_type, run_id, payload, error, attempts, failed_at
		FROM webhook_dead_letters ORDER BY failed_at DESC LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*models.WebhookDeadLetter{}
	for rows.Next() {
		var dl models.WebhookDeadLetter
		var payloadBytes []byte
		if err := rows.Scan(&dl.ID, &dl.URL, &dl.EventType, &dl.RunID, &payloadBytes, &dl.Error, &dl.Attempts, &dl.FailedAt); err != nil {
			return nil, err
		}
		dl.Payload = json.RawMessage(payloadBytes)
		out = append(out, &dl)
	}
	return out, nil
}

// --------------------------------------------------------------------------
// Run cache (LLM response caching)
// --------------------------------------------------------------------------

func (s *Store) GetCachedRunResult(ctx context.Context, cacheKey string) (*models.CachedRunResult, error) {
	// cacheKey incorporates tenant_id via computeCacheKey; this WHERE
	// clause is defense in depth on top of the composite PK.
	query := `SELECT cache_key, agent_id, output, created_at, expires_at FROM run_cache WHERE cache_key = $1 AND expires_at > NOW()`
	args := []interface{}{cacheKey}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = $2`
		args = append(args, tenant.FromContext(ctx))
	}
	row := s.pool.QueryRow(ctx, query, args...)

	var r models.CachedRunResult
	var outputBytes []byte
	if err := row.Scan(&r.CacheKey, &r.AgentID, &outputBytes, &r.CreatedAt, &r.ExpiresAt); err != nil {
		if err == pgx.ErrNoRows {
			return nil, &state.ErrNotFound{Resource: "run_cache", ID: cacheKey}
		}
		return nil, err
	}
	json.Unmarshal(outputBytes, &r.Output)
	return &r, nil
}

// --------------------------------------------------------------------------
// Cron scheduler
// --------------------------------------------------------------------------

func (s *Store) UpsertCronSchedule(ctx context.Context, sched *models.CronSchedule) error {
	input, _ := json.Marshal(sched.Input)
	config, _ := json.Marshal(sched.Config)
	now := time.Now().UTC()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO cron_schedules (tenant_id, name, agent_id, expression, timezone, input, config, enabled, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)
		ON CONFLICT(tenant_id, name) DO UPDATE SET
			agent_id=EXCLUDED.agent_id, expression=EXCLUDED.expression, timezone=EXCLUDED.timezone,
			input=EXCLUDED.input, config=EXCLUDED.config, enabled=EXCLUDED.enabled, updated_at=EXCLUDED.updated_at
	`, tenant.FromContext(ctx), sched.Name, sched.AgentID, sched.Expression, sched.Timezone, input, config, sched.Enabled, now)
	return err
}

// ListCronSchedules -- see the SQLite equivalent's doc comment: always
// called from a system context in practice (the scheduler loop must see
// every tenant's schedules), TenantID is always populated on the returned
// rows so the caller can dispatch each fire under its own tenant.
func (s *Store) ListCronSchedules(ctx context.Context) ([]*models.CronSchedule, error) {
	query := `SELECT tenant_id, name, agent_id, expression, timezone, input, config, enabled, created_at, updated_at FROM cron_schedules`
	var args []interface{}
	if !tenant.IsSystem(ctx) {
		query += ` WHERE tenant_id = $1`
		args = append(args, tenant.FromContext(ctx))
	}
	query += ` ORDER BY tenant_id, name`

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*models.CronSchedule{}
	for rows.Next() {
		var sc models.CronSchedule
		var inputBytes, configBytes []byte
		if err := rows.Scan(&sc.TenantID, &sc.Name, &sc.AgentID, &sc.Expression, &sc.Timezone, &inputBytes, &configBytes, &sc.Enabled, &sc.CreatedAt, &sc.UpdatedAt); err != nil {
			return nil, err
		}
		sc.Input = json.RawMessage(inputBytes)
		sc.Config = json.RawMessage(configBytes)
		out = append(out, &sc)
	}
	return out, nil
}

func (s *Store) DeleteCronSchedule(ctx context.Context, name string) error {
	query := `DELETE FROM cron_schedules WHERE name = $1`
	args := []interface{}{name}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = $2`
		args = append(args, tenant.FromContext(ctx))
	}
	_, err := s.pool.Exec(ctx, query, args...)
	return err
}

func (s *Store) TryClaimCronFire(ctx context.Context, scheduleName string, fireTime time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO cron_claims (tenant_id, schedule_name, fire_time, claimed_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, schedule_name, fire_time) DO NOTHING
	`, tenant.FromContext(ctx), scheduleName, fireTime.UTC(), time.Now().UTC())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (s *Store) ReleaseCronClaim(ctx context.Context, scheduleName string, fireTime time.Time) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM cron_claims WHERE tenant_id = $1 AND schedule_name = $2 AND fire_time = $3
	`, tenant.FromContext(ctx), scheduleName, fireTime.UTC())
	return err
}

func (s *Store) GetLastCronFireTime(ctx context.Context, scheduleName string) (time.Time, bool, error) {
	var fireTime time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT fire_time FROM cron_claims WHERE tenant_id = $1 AND schedule_name = $2 ORDER BY fire_time DESC LIMIT 1
	`, tenant.FromContext(ctx), scheduleName).Scan(&fireTime)
	if err == pgx.ErrNoRows {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return fireTime, true, nil
}

func (s *Store) SaveCachedRunResult(ctx context.Context, result *models.CachedRunResult) error {
	output, _ := json.Marshal(result.Output)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO run_cache (tenant_id, cache_key, agent_id, output, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT(tenant_id, cache_key) DO UPDATE SET
			output=EXCLUDED.output, created_at=EXCLUDED.created_at, expires_at=EXCLUDED.expires_at
	`, tenant.FromContext(ctx), result.CacheKey, result.AgentID, output, result.CreatedAt, result.ExpiresAt)
	return err
}

// nullableJSON returns nil for empty/null JSON, otherwise the raw bytes.
func nullableJSON(data []byte) interface{} {
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	return data
}
