// Package sqlite implements the state.Store interface using embedded SQLite.
// This is the zero-dependency default backend -- no external DB needed.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // Pure-Go SQLite driver (no CGo)

	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/state"
	"github.com/sharanharsoor/runkite/internal/tenant"
)

// SQLiteStore implements state.Store with an embedded SQLite database.
type SQLiteStore struct {
	db *sql.DB
}

// New creates a new SQLite store. If path is empty, uses an in-memory DB.
func New(path string) (*SQLiteStore, error) {
	if path == "" {
		path = ":memory:"
	}
	// modernc.org/sqlite only honors `_pragma=...` query params (see its
	// driver.go docs). The mattn/go-sqlite3 forms `_journal_mode` /
	// `_busy_timeout` / `_foreign_keys` are silently ignored — open
	// succeeds, pragmas never apply. Caught live: those mattn-style
	// params left file-backed DBs at journal_mode=delete + busy_timeout=0
	// for the entire project history until this was fixed. `:memory:`
	// can't actually be WAL (SQLite reports journal_mode=memory); still
	// set busy_timeout + foreign_keys there.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	if path == ":memory:" {
		dsn = ":memory:?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// Single connection for SQLite (avoids locking issues) -- kept
	// unconditionally after a four-stage investigation (bench/REPORT.md
	// section 6), each stage confirmed live with real benchmarks, not
	// just reasoned about:
	//
	// 1. CPU profiling under 100-way concurrent load pinned SQLite+
	//    in-memory being slower than Postgres+Redis (for the TypeScript
	//    runner) on "the pure-Go modernc.org/sqlite driver is
	//    syscall-heavy per query" (ruling out Go-level mutex contention
	//    first, via a real mutex/block profile). This diagnosis turned
	//    out itself confounded -- see stage 4.
	// 2. A first attempt at widening this pool to 8 produced a ~99.8%
	//    error rate ("SQLITE_BUSY: database is locked") under real
	//    concurrent write load -- but that attempt was against a DSN
	//    that silently never applied WAL mode or the busy_timeout it
	//    claimed to set (see the `_pragma=` comment above): every lock
	//    collision failed immediately with zero retry budget, not after
	//    a real 5s wait.
	// 3. Once the DSN was fixed to use `_pragma=...` (the only form
	//    modernc.org/sqlite actually honors), widening the pool to 8
	//    was re-tested and worked cleanly: 0 errors across 4
	//    independent 30s/concurrency-100 loadgen runs (2 TypeScript, 2
	//    Python), both runners faster than the original broken-DSN
	//    baseline.
	// 4. A direct isolation test -- DSN fixed, pool held at 1, no
	//    widening at all -- showed the fixed DSN ALONE was faster
	//    still on p50/p90 for both runners (~360-366ms p50 vs.
	//    ~390-400ms with pool=8, across 2 runs per runner per config).
	//    p99 is noisier and roughly comparable between pool=1 and
	//    pool=8 rather than a clean win either way (pool=8's own p99
	//    wasn't consistently worse than pool=1's across every run).
	//    Stage 1's "syscall-heavy driver" diagnosis was itself
	//    confounded: that CPU profile measured `journal_mode=delete`
	//    (the broken DSN's slow rollback-journal mode, mislabeled as
	//    WAL) -- once WAL is genuinely active, most of that cost
	//    disappears, and connection-pool tuning was never going to fix
	//    a journal-mode problem. Widening the pool added Go-side
	//    connection-checkout overhead for no offsetting p50/p90 benefit
	//    on this write-heavy workload (SQLite's single-writer nature
	//    caps write throughput at 1 regardless of pool size), so it was
	//    reverted a second time. See bench/REPORT.md section 6 for the
	//    full numbers from all four stages.
	db.SetMaxOpenConns(1)

	// Ensure foreign keys are enabled (required for ON DELETE CASCADE)
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}

	return &SQLiteStore{db: db}, nil
}

// Init creates all tables.
func (s *SQLiteStore) Init(ctx context.Context) error {
	schema := `
	CREATE TABLE IF NOT EXISTS agents (
		tenant_id    TEXT NOT NULL DEFAULT 'default',
		agent_id     TEXT NOT NULL,
		name         TEXT NOT NULL,
		description  TEXT DEFAULT '',
		metadata     TEXT DEFAULT '{}',
		capabilities TEXT DEFAULT '{}',
		version      INTEGER NOT NULL DEFAULT 1,
		created_at   TEXT DEFAULT (datetime('now')),
		updated_at   TEXT DEFAULT (datetime('now')),
		PRIMARY KEY (tenant_id, agent_id)
	);

	-- Full agent versioning, supporting version history browsing and
	-- rollback to arbitrary past versions -- one immutable row per
	-- version ever served. Never updated or deleted afterward.
	CREATE TABLE IF NOT EXISTS agent_versions (
		tenant_id    TEXT NOT NULL DEFAULT 'default',
		agent_id     TEXT NOT NULL,
		version      INTEGER NOT NULL,
		name         TEXT,
		description  TEXT,
		metadata     TEXT DEFAULT '{}',
		capabilities TEXT DEFAULT '{}',
		created_at   TEXT DEFAULT (datetime('now')),
		PRIMARY KEY (tenant_id, agent_id, version)
	);

	CREATE TABLE IF NOT EXISTS agent_schemas (
		tenant_id     TEXT NOT NULL DEFAULT 'default',
		agent_id      TEXT NOT NULL,
		input_schema  TEXT DEFAULT '{}',
		output_schema TEXT DEFAULT '{}',
		state_schema  TEXT DEFAULT '{}',
		config_schema TEXT DEFAULT '{}',
		PRIMARY KEY (tenant_id, agent_id),
		FOREIGN KEY (tenant_id, agent_id) REFERENCES agents(tenant_id, agent_id) ON DELETE CASCADE
	);

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
		tags         TEXT DEFAULT '[]',
		source_type  TEXT NOT NULL,
		source_ref   TEXT NOT NULL,
		metadata     TEXT DEFAULT '{}',
		version      INTEGER NOT NULL DEFAULT 1,
		created_at   TEXT DEFAULT (datetime('now')),
		updated_at   TEXT DEFAULT (datetime('now')),
		PRIMARY KEY (tenant_id, name)
	);
	CREATE TABLE IF NOT EXISTS registry_entry_versions (
		tenant_id    TEXT NOT NULL DEFAULT 'default',
		name         TEXT NOT NULL,
		version      INTEGER NOT NULL,
		display_name TEXT,
		description  TEXT,
		author       TEXT,
		tags         TEXT DEFAULT '[]',
		source_type  TEXT NOT NULL,
		source_ref   TEXT NOT NULL,
		metadata     TEXT DEFAULT '{}',
		created_at   TEXT DEFAULT (datetime('now')),
		PRIMARY KEY (tenant_id, name, version)
	);

	-- thread_id/run_id/checkpoint_id are system-generated UUIDs, kept as
	-- sole primary keys (collision-safe) rather than composite-keyed with
	-- tenant_id like the human-chosen IDs above -- every query still
	-- filters by tenant_id (see the Go query helpers), this is purely
	-- about which columns form uniqueness, not which columns are checked.
	CREATE TABLE IF NOT EXISTS threads (
		tenant_id  TEXT NOT NULL DEFAULT 'default',
		thread_id  TEXT PRIMARY KEY,
		status     TEXT DEFAULT 'idle',
		metadata   TEXT DEFAULT '{}',
		values_json TEXT DEFAULT '{}',
		created_at TEXT DEFAULT (datetime('now')),
		updated_at TEXT DEFAULT (datetime('now'))
	);

	CREATE TABLE IF NOT EXISTS runs (
		tenant_id  TEXT NOT NULL DEFAULT 'default',
		run_id     TEXT PRIMARY KEY,
		thread_id  TEXT REFERENCES threads(thread_id) ON DELETE CASCADE,
		agent_id   TEXT,
		status     TEXT DEFAULT 'pending',
		metadata   TEXT DEFAULT '{}',
		input      TEXT,
		config     TEXT,
		output     TEXT,
		error_msg  TEXT DEFAULT '',
		created_at TEXT DEFAULT (datetime('now')),
		updated_at TEXT DEFAULT (datetime('now'))
	);
	CREATE INDEX IF NOT EXISTS idx_runs_thread ON runs(thread_id);
	CREATE INDEX IF NOT EXISTS idx_runs_status ON runs(status);

	CREATE TABLE IF NOT EXISTS thread_checkpoints (
		tenant_id       TEXT NOT NULL DEFAULT 'default',
		checkpoint_id   TEXT PRIMARY KEY,
		thread_id       TEXT NOT NULL REFERENCES threads(thread_id) ON DELETE CASCADE,
		checkpoint_ns   TEXT DEFAULT '',
		parent_id       TEXT,
		values_json     TEXT DEFAULT '{}',
		metadata        TEXT DEFAULT '{}',
		next_nodes      TEXT DEFAULT '[]',
		tasks           TEXT DEFAULT '[]',
		interrupts      TEXT DEFAULT '[]',
		created_at      TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_checkpoints_thread ON thread_checkpoints(thread_id, created_at DESC);

	CREATE TABLE IF NOT EXISTS store_items (
		tenant_id TEXT NOT NULL DEFAULT 'default',
		namespace TEXT NOT NULL,
		key       TEXT NOT NULL,
		value     TEXT NOT NULL DEFAULT '{}',
		created_at TEXT DEFAULT (datetime('now')),
		updated_at TEXT DEFAULT (datetime('now')),
		PRIMARY KEY (tenant_id, namespace, key)
	);

	CREATE TABLE IF NOT EXISTS webhook_dead_letters (
		id         TEXT PRIMARY KEY,
		url        TEXT NOT NULL,
		event_type TEXT NOT NULL,
		run_id     TEXT NOT NULL,
		payload    TEXT NOT NULL DEFAULT '{}',
		error      TEXT DEFAULT '',
		attempts   INTEGER NOT NULL DEFAULT 0,
		failed_at  TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_dead_letters_failed_at ON webhook_dead_letters(failed_at DESC);

	-- Composite PK so two tenants can cache the same logical input under
	-- the same raw cache_key without colliding. computeCacheKey also
	-- embeds tenant_id (defense in depth if a WHERE clause is missed).
	CREATE TABLE IF NOT EXISTS run_cache (
		tenant_id  TEXT NOT NULL DEFAULT 'default',
		cache_key  TEXT NOT NULL,
		agent_id   TEXT NOT NULL,
		output     TEXT NOT NULL DEFAULT '{}',
		created_at TEXT NOT NULL,
		expires_at TEXT NOT NULL,
		PRIMARY KEY (tenant_id, cache_key)
	);
	CREATE INDEX IF NOT EXISTS idx_run_cache_expires ON run_cache(expires_at);

	CREATE TABLE IF NOT EXISTS cron_schedules (
		tenant_id  TEXT NOT NULL DEFAULT 'default',
		name       TEXT NOT NULL,
		agent_id   TEXT NOT NULL,
		expression TEXT NOT NULL,
		timezone   TEXT NOT NULL DEFAULT 'UTC',
		input      TEXT NOT NULL DEFAULT '{}',
		config     TEXT NOT NULL DEFAULT '{}',
		enabled    INTEGER NOT NULL DEFAULT 1,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (tenant_id, name)
	);

	-- ponytail: no active cleanup for old fire-claim rows -- growth is
	-- bounded by (number of schedules) x (fires per schedule ever), which
	-- for typical cron cadences (hourly/daily) is negligible even over
	-- years. Upgrade path if it ever matters: DELETE WHERE fire_time <
	-- some retention cutoff, same idea as run_cache's expiry but without
	-- needing correctness (claims are only ever read by exact key).
	CREATE TABLE IF NOT EXISTS cron_claims (
		tenant_id     TEXT NOT NULL DEFAULT 'default',
		schedule_name TEXT NOT NULL,
		fire_time     TEXT NOT NULL,
		claimed_at    TEXT NOT NULL,
		PRIMARY KEY (tenant_id, schedule_name, fire_time)
	);
	`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return err
	}
	if err := addColumnIfMissing(ctx, s.db, "agents", "version", "INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	// tenant_id on pre-existing databases created before multi-tenancy:
	// existing rows all become "default" tenant (i.e. exactly today's
	// single-tenant behavior, nothing reassigned or hidden). Note this
	// can't retroactively upgrade agents/agent_schemas/store_items/
	// cron_schedules/cron_claims's PRIMARY KEY to the new composite form
	// (SQLite can't ALTER a primary key in place) -- a pre-existing
	// database keeps its original single-column uniqueness even after
	// this migration runs. Every query still filters by tenant_id
	// regardless, so isolation holds either way; only the extra defense
	// of a DB-level composite uniqueness constraint is unavailable on
	// upgraded (as opposed to freshly created) SQLite files.
	for _, table := range []string{"agents", "agent_schemas", "threads", "runs", "thread_checkpoints", "store_items", "run_cache", "cron_schedules", "cron_claims"} {
		if err := addColumnIfMissing(ctx, s.db, table, "tenant_id", "TEXT NOT NULL DEFAULT 'default'"); err != nil {
			return err
		}
	}
	if err := addColumnIfMissing(ctx, s.db, "store_items", "ttl_minutes", "REAL"); err != nil {
		return err
	}
	if err := addColumnIfMissing(ctx, s.db, "store_items", "expires_at", "TEXT"); err != nil {
		return err
	}
	// Agent-to-Agent (A2A) delegation bookkeeping -- see models.Run's doc
	// comment. parent_run_id/root_run_id are nullable (a normal
	// top-level run has neither); depth defaults to 0.
	if err := addColumnIfMissing(ctx, s.db, "runs", "parent_run_id", "TEXT"); err != nil {
		return err
	}
	if err := addColumnIfMissing(ctx, s.db, "runs", "root_run_id", "TEXT"); err != nil {
		return err
	}
	if err := addColumnIfMissing(ctx, s.db, "runs", "depth", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// Optimistic concurrency for PATCH /threads -- see postgres.go's
	// identical column for the full rationale (a plain counter, not the
	// updated_at timestamp, to avoid cross-backend precision-matching issues).
	if err := addColumnIfMissing(ctx, s.db, "threads", "version", "INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS idx_runs_root ON runs(root_run_id)"); err != nil {
		return err
	}
	// These indexes reference tenant_id, so (unlike the CREATE INDEX
	// statements in the schema string above, which only reference columns
	// that are part of that same CREATE TABLE) they must run AFTER the
	// addColumnIfMissing loop, not inside the initial schema string --
	// confirmed live as a real bug in the Postgres equivalent of this
	// migration (same mistake, caught there first: "column tenant_id does
	// not exist" on a pre-existing database whose CREATE TABLE was a
	// no-op and whose ALTER hadn't run yet).
	if _, err := s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_threads_tenant ON threads(tenant_id)`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_runs_tenant ON runs(tenant_id)`); err != nil {
		return err
	}
	// ON CONFLICT(tenant_id, <col>) in Upsert*/PutItem/TryClaimCronFire
	// needs an ACTUAL unique constraint on exactly those columns to
	// target -- a pre-existing table's original PRIMARY KEY (single
	// column, from before this migration) doesn't satisfy that, same
	// issue confirmed live in the Postgres equivalent first. A fresh
	// table already has this covered by its composite PRIMARY KEY;
	// these are redundant-but-harmless on a fresh install, required for
	// an upgraded one.
	for _, stmt := range []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS ux_agents_tenant_agent ON agents(tenant_id, agent_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS ux_agent_schemas_tenant_agent ON agent_schemas(tenant_id, agent_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS ux_store_items_tenant_ns_key ON store_items(tenant_id, namespace, key)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS ux_cron_schedules_tenant_name ON cron_schedules(tenant_id, name)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS ux_cron_claims_tenant_sched_fire ON cron_claims(tenant_id, schedule_name, fire_time)`,
		// Upgraded DBs may still have cache_key as sole PRIMARY KEY; this
		// unique index lets ON CONFLICT(tenant_id, cache_key) target the
		// right columns. Same-raw-key rows across tenants still can't
		// coexist on those upgraded files (old sole PK) -- computeCacheKey
		// embedding tenant_id is what makes production keys never collide.
		`CREATE UNIQUE INDEX IF NOT EXISTS ux_run_cache_tenant_key ON run_cache(tenant_id, cache_key)`,
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// addColumnIfMissing runs an idempotent ALTER TABLE ADD COLUMN for
// databases created before a column existed. modernc.org/sqlite's parser
// rejects the standard "ADD COLUMN IF NOT EXISTS" syntax outright (a
// syntax error, not a runtime check), so this detects "already exists" by
// string-matching the error instead -- the portable idempotency mechanism
// for a "single idempotent migration" schema (see db downgrade's honest
// non-support): existing installs get new columns added in place, no
// versioned up/down migration system needed for purely additive changes.
func addColumnIfMissing(ctx context.Context, db *sql.DB, table, column, ddl string) error {
	_, err := db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, ddl))
	if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	return nil
}

// nullableString converts a *string model field (nil means "no value",
// e.g. models.Run.ParentRunID for a top-level run) to a driver value
// that binds to a NULL column, rather than storing the literal string
// "<nil>" or failing the scan.
func nullableString(s *string) interface{} {
	if s == nil {
		return nil
	}
	return *s
}

// nullStringToPtr is nullableString's inverse, for scanning a nullable
// TEXT column back into a *string model field.
func nullStringToPtr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}

// Close closes the database connection.
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

// Ping verifies the database file is still reachable/openable. Mostly a
// formality for SQLite (an embedded file, not a network service that can
// go down independently of this process) but kept for interface
// symmetry with the network-backed stores, and it does catch a real
// failure mode: the file being deleted or its directory becoming
// unwritable out from under a running process.
func (s *SQLiteStore) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// --------------------------------------------------------------------------
// Agents
// --------------------------------------------------------------------------

func (s *SQLiteStore) UpsertAgent(ctx context.Context, agent *models.Agent) error {
	meta, _ := json.Marshal(agent.Metadata)
	caps, _ := json.Marshal(agent.Capabilities)
	now := time.Now().UTC().Format(time.RFC3339)
	tenantID := tenant.FromContext(ctx)

	// A transaction, not a single statement, because writing the
	// version-history snapshot (agent_versions) needs to know WHETHER
	// this call actually bumped the version -- same rationale as the
	// Postgres backend's identical restructuring, see its comment for
	// the full explanation. Not a contended path: agent upserts happen
	// at config-bootstrap time, not per-request.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op if committed

	// version bumps only if the definition actually changed -- comparing
	// against the pre-update row (unqualified/table-qualified columns) vs
	// the proposed one (excluded.*) in a single atomic statement, so a
	// bootstrap re-running UpsertAgent with an unchanged langgraph.json on
	// every restart doesn't inflate the version number.
	var newVersion int
	err = tx.QueryRowContext(ctx, `
		INSERT INTO agents (tenant_id, agent_id, name, description, metadata, capabilities, version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT(tenant_id, agent_id) DO UPDATE SET
			name=excluded.name, description=excluded.description,
			metadata=excluded.metadata, capabilities=excluded.capabilities,
			updated_at=excluded.updated_at,
			version = CASE WHEN agents.name != excluded.name
			                 OR agents.description != excluded.description
			                 OR agents.metadata != excluded.metadata
			                 OR agents.capabilities != excluded.capabilities
			               THEN agents.version + 1
			               ELSE agents.version END
		RETURNING version
	`, tenantID, agent.AgentID, agent.Name, agent.Description, string(meta), string(caps), now, now).Scan(&newVersion)
	if err != nil {
		return err
	}

	// Only write a version snapshot if this call is the one that
	// created it -- see the Postgres backend's identical comment for
	// why an unchanged re-registration must not duplicate a row.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO agent_versions (tenant_id, agent_id, version, name, description, metadata, capabilities, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (tenant_id, agent_id, version) DO NOTHING
	`, tenantID, agent.AgentID, newVersion, agent.Name, agent.Description, string(meta), string(caps), now)
	if err != nil {
		return err
	}

	return tx.Commit()
}

func (s *SQLiteStore) ListAgentVersions(ctx context.Context, agentID string) ([]*models.AgentVersion, error) {
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

func (s *SQLiteStore) GetAgentVersion(ctx context.Context, agentID string, version int) (*models.AgentVersion, error) {
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

// agentVersionScanner covers both *sql.Row (QueryRowContext) and
// *sql.Rows (QueryContext) -- both expose an identical Scan method, so
// this one helper serves GetAgentVersion and ListAgentVersions without
// duplicating the column list or timestamp-parsing logic between them.
type agentVersionScanner interface {
	Scan(dest ...interface{}) error
}

func scanAgentVersion(row agentVersionScanner) (*models.AgentVersion, error) {
	var v models.AgentVersion
	var metaStr, capsStr, createdStr string
	if err := row.Scan(&v.TenantID, &v.AgentID, &v.Version, &v.Name, &v.Description, &metaStr, &capsStr, &createdStr); err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(metaStr), &v.Metadata)
	json.Unmarshal([]byte(capsStr), &v.Capabilities)
	v.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
	return &v, nil
}

// --------------------------------------------------------------------------
// Registry: agent marketplace / registry
// --------------------------------------------------------------------------

func (s *SQLiteStore) PublishRegistryEntry(ctx context.Context, entry *models.RegistryEntry) error {
	meta, _ := json.Marshal(entry.Metadata)
	tags, _ := json.Marshal(nonNilTags(entry.Tags))
	now := time.Now().UTC().Format(time.RFC3339)
	tenantID := tenant.FromContext(ctx)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op if committed

	var newVersion int
	err = tx.QueryRowContext(ctx, `
		INSERT INTO registry_entries (tenant_id, name, display_name, description, author, tags, source_type, source_ref, metadata, version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT(tenant_id, name) DO UPDATE SET
			display_name=excluded.display_name, description=excluded.description,
			author=excluded.author, tags=excluded.tags,
			source_type=excluded.source_type, source_ref=excluded.source_ref,
			metadata=excluded.metadata, updated_at=excluded.updated_at,
			version = CASE WHEN registry_entries.display_name != excluded.display_name
			                 OR registry_entries.description != excluded.description
			                 OR registry_entries.author != excluded.author
			                 OR registry_entries.tags != excluded.tags
			                 OR registry_entries.source_type != excluded.source_type
			                 OR registry_entries.source_ref != excluded.source_ref
			                 OR registry_entries.metadata != excluded.metadata
			               THEN registry_entries.version + 1
			               ELSE registry_entries.version END
		RETURNING version
	`, tenantID, entry.Name, entry.DisplayName, entry.Description, entry.Author, string(tags), entry.SourceType, entry.SourceRef, string(meta), now, now).Scan(&newVersion)
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO registry_entry_versions (tenant_id, name, version, display_name, description, author, tags, source_type, source_ref, metadata, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (tenant_id, name, version) DO NOTHING
	`, tenantID, entry.Name, newVersion, entry.DisplayName, entry.Description, entry.Author, string(tags), entry.SourceType, entry.SourceRef, string(meta), now)
	if err != nil {
		return err
	}

	return tx.Commit()
}

func (s *SQLiteStore) GetRegistryEntry(ctx context.Context, name string) (*models.RegistryEntry, error) {
	query := `SELECT tenant_id, name, display_name, description, author, tags, source_type, source_ref, metadata, version, created_at, updated_at FROM registry_entries WHERE name = ?`
	args := []interface{}{name}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	row := s.db.QueryRowContext(ctx, query, args...)
	e, err := scanRegistryEntry(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, &state.ErrNotFound{Resource: "registry_entry", ID: name}
		}
		return nil, err
	}
	return e, nil
}

func (s *SQLiteStore) SearchRegistryEntries(ctx context.Context, req *models.RegistrySearchRequest) ([]*models.RegistryEntry, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	query := `SELECT tenant_id, name, display_name, description, author, tags, source_type, source_ref, metadata, version, created_at, updated_at FROM registry_entries`
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
	if req.Author != "" {
		where = append(where, "author = ?")
		args = append(args, req.Author)
	}
	for _, tag := range req.Tags {
		where = append(where, "tags LIKE ?")
		tagJSON, _ := json.Marshal(tag)
		args = append(args, "%"+string(tagJSON)+"%")
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

// DeleteRegistryEntry also removes the entry's own version history --
// see the Postgres backend's identical fix for the full rationale
// (delete-then-republish otherwise resurrects stale pre-delete version
// snapshots via ON CONFLICT DO NOTHING against orphaned rows).
func (s *SQLiteStore) DeleteRegistryEntry(ctx context.Context, name string) error {
	tenantID := tenant.FromContext(ctx)

	// A transaction, not two independent Execs -- see the Postgres
	// backend's identical fix for the full rationale.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op if committed

	query := `DELETE FROM registry_entries WHERE name = ?`
	args := []interface{}{name}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenantID)
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return &state.ErrNotFound{Resource: "registry_entry", ID: name}
	}

	versionsQuery := `DELETE FROM registry_entry_versions WHERE name = ?`
	versionsArgs := []interface{}{name}
	if !tenant.IsSystem(ctx) {
		versionsQuery += ` AND tenant_id = ?`
		versionsArgs = append(versionsArgs, tenantID)
	}
	if _, err := tx.ExecContext(ctx, versionsQuery, versionsArgs...); err != nil {
		return err
	}

	return tx.Commit()
}

func (s *SQLiteStore) ListRegistryEntryVersions(ctx context.Context, name string) ([]*models.RegistryEntryVersion, error) {
	query := `SELECT tenant_id, name, version, display_name, description, author, tags, source_type, source_ref, metadata, created_at FROM registry_entry_versions WHERE name = ?`
	args := []interface{}{name}
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

func (s *SQLiteStore) GetRegistryEntryVersion(ctx context.Context, name string, version int) (*models.RegistryEntryVersion, error) {
	query := `SELECT tenant_id, name, version, display_name, description, author, tags, source_type, source_ref, metadata, created_at FROM registry_entry_versions WHERE name = ? AND version = ?`
	args := []interface{}{name, version}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	row := s.db.QueryRowContext(ctx, query, args...)
	v, err := scanRegistryEntryVersion(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, &state.ErrNotFound{Resource: "registry_entry_version", ID: fmt.Sprintf("%s@v%d", name, version)}
		}
		return nil, err
	}
	return v, nil
}

func nonNilTags(tags []string) []string {
	if tags == nil {
		return []string{}
	}
	return tags
}

func scanRegistryEntry(row agentVersionScanner) (*models.RegistryEntry, error) {
	var e models.RegistryEntry
	var tagsStr, metaStr, createdStr, updatedStr string
	if err := row.Scan(&e.TenantID, &e.Name, &e.DisplayName, &e.Description, &e.Author, &tagsStr, &e.SourceType, &e.SourceRef, &metaStr, &e.Version, &createdStr, &updatedStr); err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(tagsStr), &e.Tags)
	json.Unmarshal([]byte(metaStr), &e.Metadata)
	e.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
	e.UpdatedAt, _ = time.Parse(time.RFC3339, updatedStr)
	return &e, nil
}

func scanRegistryEntryVersion(row agentVersionScanner) (*models.RegistryEntryVersion, error) {
	var v models.RegistryEntryVersion
	var tagsStr, metaStr, createdStr string
	if err := row.Scan(&v.TenantID, &v.Name, &v.Version, &v.DisplayName, &v.Description, &v.Author, &tagsStr, &v.SourceType, &v.SourceRef, &metaStr, &createdStr); err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(tagsStr), &v.Tags)
	json.Unmarshal([]byte(metaStr), &v.Metadata)
	v.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
	return &v, nil
}

func (s *SQLiteStore) GetAgent(ctx context.Context, agentID string) (*models.Agent, error) {
	query := `SELECT tenant_id, agent_id, name, description, metadata, capabilities, version FROM agents WHERE agent_id = ?`
	args := []interface{}{agentID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	row := s.db.QueryRowContext(ctx, query, args...)

	var a models.Agent
	var metaStr, capsStr string
	if err := row.Scan(&a.TenantID, &a.AgentID, &a.Name, &a.Description, &metaStr, &capsStr, &a.Version); err != nil {
		if err == sql.ErrNoRows {
			return nil, &state.ErrNotFound{Resource: "agent", ID: agentID}
		}
		return nil, err
	}
	json.Unmarshal([]byte(metaStr), &a.Metadata)
	json.Unmarshal([]byte(capsStr), &a.Capabilities)
	return &a, nil
}

func (s *SQLiteStore) SearchAgents(ctx context.Context, req *models.AgentSearchRequest) ([]*models.Agent, error) {
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

	if !tenant.IsSystem(ctx) {
		where = append(where, "tenant_id = ?")
		args = append(args, tenant.FromContext(ctx))
	}
	if req.Name != "" {
		where = append(where, "name LIKE ?")
		args = append(args, "%"+req.Name+"%")
	}
	for k, v := range req.Metadata {
		where = append(where, "json_extract(metadata, ?) = ?")
		args = append(args, "$."+k)
		valJSON, _ := json.Marshal(v)
		// json_extract returns JSON-typed values; strings need to match as unquoted
		if s, ok := v.(string); ok {
			args = append(args, s)
		} else {
			args = append(args, string(valJSON))
		}
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY name LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	agents := []*models.Agent{}
	for rows.Next() {
		var a models.Agent
		var metaStr, capsStr string
		if err := rows.Scan(&a.TenantID, &a.AgentID, &a.Name, &a.Description, &metaStr, &capsStr, &a.Version); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(metaStr), &a.Metadata)
		json.Unmarshal([]byte(capsStr), &a.Capabilities)
		agents = append(agents, &a)
	}
	return agents, nil
}

func (s *SQLiteStore) UpsertAgentSchema(ctx context.Context, schema *models.AgentSchema) error {
	input, _ := json.Marshal(schema.InputSchema)
	output, _ := json.Marshal(schema.OutputSchema)
	st, _ := json.Marshal(schema.StateSchema)
	cfg, _ := json.Marshal(schema.ConfigSchema)

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_schemas (tenant_id, agent_id, input_schema, output_schema, state_schema, config_schema)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(tenant_id, agent_id) DO UPDATE SET
			input_schema=excluded.input_schema, output_schema=excluded.output_schema,
			state_schema=excluded.state_schema, config_schema=excluded.config_schema
	`, tenant.FromContext(ctx), schema.AgentID, string(input), string(output), string(st), string(cfg))
	return err
}

func (s *SQLiteStore) GetAgentSchema(ctx context.Context, agentID string) (*models.AgentSchema, error) {
	query := `SELECT agent_id, input_schema, output_schema, state_schema, config_schema FROM agent_schemas WHERE agent_id = ?`
	args := []interface{}{agentID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	row := s.db.QueryRowContext(ctx, query, args...)

	var as models.AgentSchema
	var inStr, outStr, stStr, cfgStr string
	if err := row.Scan(&as.AgentID, &inStr, &outStr, &stStr, &cfgStr); err != nil {
		if err == sql.ErrNoRows {
			return nil, &state.ErrNotFound{Resource: "agent_schema", ID: agentID}
		}
		return nil, err
	}
	json.Unmarshal([]byte(inStr), &as.InputSchema)
	json.Unmarshal([]byte(outStr), &as.OutputSchema)
	json.Unmarshal([]byte(stStr), &as.StateSchema)
	json.Unmarshal([]byte(cfgStr), &as.ConfigSchema)
	return &as, nil
}

// --------------------------------------------------------------------------
// Threads
// --------------------------------------------------------------------------

func (s *SQLiteStore) CreateThread(ctx context.Context, thread *models.Thread) error {
	meta, _ := json.Marshal(thread.Metadata)
	vals, _ := json.Marshal(thread.Values)

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO threads (tenant_id, thread_id, status, metadata, values_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, tenant.FromContext(ctx), thread.ThreadID, thread.Status, string(meta), string(vals),
		thread.CreatedAt.Format(time.RFC3339), thread.UpdatedAt.Format(time.RFC3339))

	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			return &state.ErrConflict{Resource: "thread", ID: thread.ThreadID}
		}
		return err
	}
	// See postgres.go's identical CreateThread for why this is set here
	// (the DB default the caller's struct never saw, but handlers echo
	// straight back as the create response).
	thread.Version = 1
	return nil
}

func (s *SQLiteStore) GetThread(ctx context.Context, threadID string) (*models.Thread, error) {
	query := `SELECT tenant_id, thread_id, status, metadata, values_json, created_at, updated_at, version FROM threads WHERE thread_id = ?`
	args := []interface{}{threadID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	row := s.db.QueryRowContext(ctx, query, args...)

	var t models.Thread
	var metaStr, valsStr, createdStr, updatedStr string
	if err := row.Scan(&t.TenantID, &t.ThreadID, &t.Status, &metaStr, &valsStr, &createdStr, &updatedStr, &t.Version); err != nil {
		if err == sql.ErrNoRows {
			return nil, &state.ErrNotFound{Resource: "thread", ID: threadID}
		}
		return nil, err
	}
	json.Unmarshal([]byte(metaStr), &t.Metadata)
	json.Unmarshal([]byte(valsStr), &t.Values)
	t.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
	t.UpdatedAt, _ = time.Parse(time.RFC3339, updatedStr)
	return &t, nil
}

func (s *SQLiteStore) UpdateThread(ctx context.Context, threadID string, patch *models.ThreadPatch) (*models.Thread, error) {
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

	// See postgres.go's identical UpdateThread for why version = version + 1
	// (not existing.Version+1 as a bound value) and why the IfMatchVersion
	// check lives in this same statement's WHERE clause.
	query := `UPDATE threads SET metadata = ?, values_json = ?, updated_at = ?, version = version + 1 WHERE thread_id = ?`
	args := []interface{}{string(meta), string(vals), existing.UpdatedAt.Format(time.RFC3339), threadID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	if patch.IfMatchVersion != nil {
		query += ` AND version = ?`
		args = append(args, *patch.IfMatchVersion)
	}
	// RETURNING version (not existing.Version++, an in-memory guess) --
	// see postgres.go's identical UpdateThread for why: two overlapping
	// unconditional writers would otherwise both report the same
	// falsely-computed version despite the DB ending up higher than
	// either saw.
	query += " RETURNING version"
	var newVersion int
	err = s.db.QueryRowContext(ctx, query, args...).Scan(&newVersion)
	if err != nil {
		if err == sql.ErrNoRows {
			if patch.IfMatchVersion != nil {
				return nil, &state.ErrConflict{Resource: "thread", ID: threadID, Reason: "version mismatch (optimistic concurrency)"}
			}
			// Unconditional update matched no row -- preserve the
			// pre-existing behavior of returning the in-memory struct
			// rather than erroring; this path never checked
			// rows-affected before.
			existing.Version++
			return existing, nil
		}
		return nil, err
	}
	existing.Version = newVersion
	return existing, nil
}

func (s *SQLiteStore) DeleteThread(ctx context.Context, threadID string) error {
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
	n, _ := result.RowsAffected()
	if n == 0 {
		return &state.ErrNotFound{Resource: "thread", ID: threadID}
	}
	return nil
}

func (s *SQLiteStore) SearchThreads(ctx context.Context, req *models.ThreadSearchRequest) ([]*models.Thread, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}

	query := `SELECT tenant_id, thread_id, status, metadata, values_json, created_at, updated_at, version FROM threads`
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
		where = append(where, "json_extract(metadata, ?) = ?")
		args = append(args, "$."+k)
		if sv, ok := v.(string); ok {
			args = append(args, sv)
		} else {
			valJSON, _ := json.Marshal(v)
			args = append(args, string(valJSON))
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
		var t models.Thread
		var metaStr, valsStr, createdStr, updatedStr string
		if err := rows.Scan(&t.TenantID, &t.ThreadID, &t.Status, &metaStr, &valsStr, &createdStr, &updatedStr, &t.Version); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(metaStr), &t.Metadata)
		json.Unmarshal([]byte(valsStr), &t.Values)
		t.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
		t.UpdatedAt, _ = time.Parse(time.RFC3339, updatedStr)
		threads = append(threads, &t)
	}
	return threads, nil
}

func (s *SQLiteStore) SetThreadStatus(ctx context.Context, threadID string, status models.ThreadStatus) error {
	now := time.Now().UTC().Format(time.RFC3339)
	query := `UPDATE threads SET status = ?, updated_at = ? WHERE thread_id = ?`
	args := []interface{}{string(status), now, threadID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	_, err := s.db.ExecContext(ctx, query, args...)
	return err
}

// TryClaimThread does the busy-check and the busy-write as one atomic
// UPDATE ... WHERE status != 'busy', so two concurrent callers can never
// both see success -- exactly one wins the race, the other gets rowsAffected=0.
func (s *SQLiteStore) TryClaimThread(ctx context.Context, threadID string) (bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
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

// ReleaseThreadIfNoOtherActive is a single conditional UPDATE -- the
// inverse of TryClaimThread -- so a late StatusCallback cannot idle a
// thread that a newer run already claimed.
func (s *SQLiteStore) ReleaseThreadIfNoOtherActive(ctx context.Context, threadID, excludeRunID string, status models.ThreadStatus) (bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	query := `UPDATE threads SET status = ?, updated_at = ?
		WHERE thread_id = ? AND status = ?
		AND NOT EXISTS (
			SELECT 1 FROM runs r
			WHERE r.thread_id = threads.thread_id
			  AND r.status IN ('pending','running')
			  AND r.run_id <> ?
		)`
	args := []interface{}{string(status), now, threadID, string(models.ThreadStatusBusy), excludeRunID}
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

// --------------------------------------------------------------------------
// Checkpoints
// --------------------------------------------------------------------------

func (s *SQLiteStore) SaveCheckpoint(ctx context.Context, threadID string, ts *models.ThreadState) error {
	vals, _ := json.Marshal(ts.Values)
	meta, _ := json.Marshal(ts.Metadata)
	next, _ := json.Marshal(ts.Next)
	tasks, _ := json.Marshal(ts.Tasks)
	interrupts, _ := json.Marshal(ts.Interrupts)

	var parentID *string
	if ts.ParentCheckpoint != nil {
		parentID = &ts.ParentCheckpoint.CheckpointID
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO thread_checkpoints (tenant_id, checkpoint_id, thread_id, checkpoint_ns, parent_id, values_json, metadata, next_nodes, tasks, interrupts, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, tenant.FromContext(ctx), ts.Checkpoint.CheckpointID, threadID, ts.Checkpoint.CheckpointNS,
		parentID, string(vals), string(meta), string(next), string(tasks), string(interrupts),
		nilOrNow(ts.CreatedAt))
	return err
}

func (s *SQLiteStore) GetLatestCheckpoint(ctx context.Context, threadID string) (*models.ThreadState, error) {
	query := `SELECT checkpoint_id, thread_id, checkpoint_ns, parent_id, values_json, metadata, next_nodes, tasks, interrupts, created_at
		FROM thread_checkpoints WHERE thread_id = ?`
	args := []interface{}{threadID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	query += ` ORDER BY created_at DESC LIMIT 1`
	row := s.db.QueryRowContext(ctx, query, args...)
	return scanCheckpointRow(row)
}

func (s *SQLiteStore) ListCheckpoints(ctx context.Context, threadID string, limit int, before string) ([]*models.ThreadState, error) {
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
		ts, err := scanCheckpointRows(rows)
		if err != nil {
			return nil, err
		}
		states = append(states, ts)
	}
	return states, nil
}

// scanCheckpointRow scans a single *sql.Row into a ThreadState.
func scanCheckpointRow(row *sql.Row) (*models.ThreadState, error) {
	var ts models.ThreadState
	var cpID, tID, cpNS, valsStr, metaStr, nextStr, tasksStr, intStr, createdStr string
	var parentID sql.NullString

	if err := row.Scan(&cpID, &tID, &cpNS, &parentID, &valsStr, &metaStr, &nextStr, &tasksStr, &intStr, &createdStr); err != nil {
		if err == sql.ErrNoRows {
			return nil, &state.ErrNotFound{Resource: "checkpoint", ID: "latest"}
		}
		return nil, err
	}
	fillCheckpoint(&ts, cpID, tID, cpNS, parentID, valsStr, metaStr, nextStr, tasksStr, intStr, createdStr)
	return &ts, nil
}

// scanCheckpointRows scans the current row of *sql.Rows into a ThreadState.
func scanCheckpointRows(rows *sql.Rows) (*models.ThreadState, error) {
	var ts models.ThreadState
	var cpID, tID, cpNS, valsStr, metaStr, nextStr, tasksStr, intStr, createdStr string
	var parentID sql.NullString

	if err := rows.Scan(&cpID, &tID, &cpNS, &parentID, &valsStr, &metaStr, &nextStr, &tasksStr, &intStr, &createdStr); err != nil {
		return nil, err
	}
	fillCheckpoint(&ts, cpID, tID, cpNS, parentID, valsStr, metaStr, nextStr, tasksStr, intStr, createdStr)
	return &ts, nil
}

func fillCheckpoint(ts *models.ThreadState, cpID, tID, cpNS string, parentID sql.NullString, valsStr, metaStr, nextStr, tasksStr, intStr, createdStr string) {
	ts.Checkpoint = models.ThreadCheckpoint{
		CheckpointID: cpID,
		ThreadID:     tID,
		CheckpointNS: cpNS,
	}
	json.Unmarshal([]byte(valsStr), &ts.Values)
	json.Unmarshal([]byte(metaStr), &ts.Metadata)
	json.Unmarshal([]byte(nextStr), &ts.Next)
	json.Unmarshal([]byte(tasksStr), &ts.Tasks)
	json.Unmarshal([]byte(intStr), &ts.Interrupts)
	ts.CreatedAt = &createdStr
	if parentID.Valid {
		ts.ParentCheckpoint = &models.ThreadCheckpoint{
			CheckpointID: parentID.String,
			ThreadID:     tID,
			CheckpointNS: cpNS,
		}
	}
}

func nilOrNow(t *string) string {
	if t != nil && *t != "" {
		return *t
	}
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// --------------------------------------------------------------------------
// Runs
// --------------------------------------------------------------------------

func (s *SQLiteStore) CreateRun(ctx context.Context, run *models.Run) error {
	meta, _ := json.Marshal(run.Metadata)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (tenant_id, run_id, thread_id, agent_id, status, metadata, input, config, created_at, updated_at, parent_run_id, root_run_id, depth)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, tenant.FromContext(ctx), run.RunID, run.ThreadID, run.AgentID, run.Status, string(meta),
		string(run.Input), string(run.Config),
		run.CreatedAt.Format(time.RFC3339), run.UpdatedAt.Format(time.RFC3339),
		nullableString(run.ParentRunID), nullableString(run.RootRunID), run.Depth)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint") {
		// run_id is the primary key; wrap into state.ErrConflict, same
		// idiom CreateThread already uses -- otherwise a cross-tenant
		// run_id collision (GetRun is tenant-scoped, so createRunCtx's
		// own retry-race fallback can't find the OTHER tenant's row to
		// dispatch through) falls all the way back to the API layer as
		// a raw, unwrapped driver error, surfacing as a generic 500
		// instead of a clean 409.
		return &state.ErrConflict{Resource: "run", ID: run.RunID}
	}
	return err
}

func (s *SQLiteStore) GetRun(ctx context.Context, runID string) (*models.Run, error) {
	query := `SELECT tenant_id, run_id, thread_id, agent_id, status, metadata, input, config, output, error_msg, created_at, updated_at, parent_run_id, root_run_id, depth FROM runs WHERE run_id = ?`
	args := []interface{}{runID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	row := s.db.QueryRowContext(ctx, query, args...)

	var r models.Run
	var metaStr, inputStr, configStr, outputStr, parentRunIDStr, rootRunIDStr sql.NullString
	var createdStr, updatedStr string
	if err := row.Scan(&r.TenantID, &r.RunID, &r.ThreadID, &r.AgentID, &r.Status, &metaStr, &inputStr, &configStr, &outputStr, &r.Error, &createdStr, &updatedStr, &parentRunIDStr, &rootRunIDStr, &r.Depth); err != nil {
		if err == sql.ErrNoRows {
			return nil, &state.ErrNotFound{Resource: "run", ID: runID}
		}
		return nil, err
	}
	r.ParentRunID = nullStringToPtr(parentRunIDStr)
	r.RootRunID = nullStringToPtr(rootRunIDStr)
	if metaStr.Valid {
		json.Unmarshal([]byte(metaStr.String), &r.Metadata)
	}
	if inputStr.Valid {
		r.Input = json.RawMessage(inputStr.String)
	}
	if configStr.Valid {
		r.Config = json.RawMessage(configStr.String)
	}
	if outputStr.Valid {
		r.Output = json.RawMessage(outputStr.String)
	}
	r.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
	r.UpdatedAt, _ = time.Parse(time.RFC3339, updatedStr)
	r.AssistantID = r.AgentID // SDK compat
	return &r, nil
}

func (s *SQLiteStore) UpdateRunStatus(ctx context.Context, runID string, status models.RunStatus, output []byte, errMsg string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	query := `UPDATE runs SET status = ?, output = ?, error_msg = ?, updated_at = ? WHERE run_id = ?`
	args := []interface{}{string(status), string(output), errMsg, now, runID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	_, err := s.db.ExecContext(ctx, query, args...)
	return err
}

func (s *SQLiteStore) ListActiveRunsCreatedBefore(ctx context.Context, before time.Time, limit int) ([]*models.Run, error) {
	if limit <= 0 {
		limit = 100
	}
	query := `SELECT tenant_id, run_id, thread_id, agent_id, status, metadata, input, config, output, error_msg, created_at, updated_at, parent_run_id, root_run_id, depth FROM runs WHERE status IN ('pending','running') AND created_at < ?`
	args := []interface{}{before.UTC().Format(time.RFC3339)}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	query += ` ORDER BY created_at ASC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	runs := []*models.Run{}
	for rows.Next() {
		var r models.Run
		var metaStr, inputStr, configStr, outputStr, parentRunIDStr, rootRunIDStr sql.NullString
		var createdStr, updatedStr string
		if err := rows.Scan(&r.TenantID, &r.RunID, &r.ThreadID, &r.AgentID, &r.Status, &metaStr, &inputStr, &configStr, &outputStr, &r.Error, &createdStr, &updatedStr, &parentRunIDStr, &rootRunIDStr, &r.Depth); err != nil {
			return nil, err
		}
		r.ParentRunID = nullStringToPtr(parentRunIDStr)
		r.RootRunID = nullStringToPtr(rootRunIDStr)
		if metaStr.Valid {
			json.Unmarshal([]byte(metaStr.String), &r.Metadata)
		}
		if inputStr.Valid {
			r.Input = json.RawMessage(inputStr.String)
		}
		if configStr.Valid {
			r.Config = json.RawMessage(configStr.String)
		}
		if outputStr.Valid {
			r.Output = json.RawMessage(outputStr.String)
		}
		r.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
		r.UpdatedAt, _ = time.Parse(time.RFC3339, updatedStr)
		r.AssistantID = r.AgentID
		runs = append(runs, &r)
	}
	return runs, rows.Err()
}

func (s *SQLiteStore) TryMarkRunTimeout(ctx context.Context, runID string, errMsg string) (bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	query := `UPDATE runs SET status = ?, error_msg = ?, updated_at = ? WHERE run_id = ? AND status IN ('pending','running')`
	args := []interface{}{string(models.RunStatusTimeout), errMsg, now, runID}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

func (s *SQLiteStore) DeleteRun(ctx context.Context, runID string) error {
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
	n, _ := result.RowsAffected()
	if n == 0 {
		return &state.ErrNotFound{Resource: "run", ID: runID}
	}
	return nil
}

func (s *SQLiteStore) PruneRuns(ctx context.Context, olderThan time.Time) (int64, error) {
	// RFC3339 string comparison matches how every other timestamp write
	// in this store is formatted (see the earlier "SQLite timestamp
	// zero-value bug" fix) -- lexicographic ordering on RFC3339 strings
	// is equivalent to chronological ordering.
	// Matches api.isTerminalStatus's definition (internal/api/runs.go) --
	// duplicated rather than imported to keep internal/state free of a
	// dependency on internal/api.
	query := `DELETE FROM runs WHERE status IN ('success','error','interrupted','timeout') AND updated_at < ?`
	args := []interface{}{olderThan.UTC().Format(time.RFC3339)}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *SQLiteStore) PruneCheckpoints(ctx context.Context, keepLast int) (int64, error) {
	if keepLast <= 0 {
		return 0, nil
	}
	// modernc.org/sqlite bundles a SQLite version with window function
	// support (3.25+), so this uses the same ROW_NUMBER() approach as
	// the Postgres implementation for identical semantics across
	// backends -- ranking is per-thread so a busy thread doesn't starve
	// a quiet one's retention window.
	query := `
		DELETE FROM thread_checkpoints
		WHERE checkpoint_id IN (
			SELECT checkpoint_id FROM (
				SELECT checkpoint_id,
					ROW_NUMBER() OVER (PARTITION BY thread_id ORDER BY created_at DESC) AS rn
				FROM thread_checkpoints
				WHERE (? IS NULL OR tenant_id = ?)
			) ranked
			WHERE rn > ?
		)`
	var tenantArg interface{}
	if !tenant.IsSystem(ctx) {
		tenantArg = tenant.FromContext(ctx)
	}
	result, err := s.db.ExecContext(ctx, query, tenantArg, tenantArg, keepLast)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *SQLiteStore) PruneCronClaims(ctx context.Context, olderThan time.Time) (int64, error) {
	query := `DELETE FROM cron_claims WHERE fire_time < ?`
	args := []interface{}{olderThan.UTC().Format(time.RFC3339)}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *SQLiteStore) PruneExpiredStoreItems(ctx context.Context) (int64, error) {
	query := `DELETE FROM store_items WHERE expires_at IS NOT NULL AND expires_at <= ?`
	args := []interface{}{formatStoreExpires(time.Now().UTC())}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *SQLiteStore) SearchRuns(ctx context.Context, req *models.RunSearchRequest) ([]*models.Run, error) {
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
	if req.RootRunID != "" {
		where = append(where, "root_run_id = ?")
		args = append(args, req.RootRunID)
	}
	for k, v := range req.Metadata {
		where = append(where, "json_extract(metadata, ?) = ?")
		args = append(args, "$."+k)
		if sv, ok := v.(string); ok {
			args = append(args, sv)
		} else {
			valJSON, _ := json.Marshal(v)
			args = append(args, string(valJSON))
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
		var r models.Run
		var metaStr, inputStr, configStr, outputStr, parentRunIDStr, rootRunIDStr sql.NullString
		var createdStr, updatedStr string
		if err := rows.Scan(&r.TenantID, &r.RunID, &r.ThreadID, &r.AgentID, &r.Status, &metaStr, &inputStr, &configStr, &outputStr, &r.Error, &createdStr, &updatedStr, &parentRunIDStr, &rootRunIDStr, &r.Depth); err != nil {
			return nil, err
		}
		r.ParentRunID = nullStringToPtr(parentRunIDStr)
		r.RootRunID = nullStringToPtr(rootRunIDStr)
		if metaStr.Valid {
			json.Unmarshal([]byte(metaStr.String), &r.Metadata)
		}
		if inputStr.Valid {
			r.Input = json.RawMessage(inputStr.String)
		}
		if configStr.Valid {
			r.Config = json.RawMessage(configStr.String)
		}
		if outputStr.Valid {
			r.Output = json.RawMessage(outputStr.String)
		}
		r.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
		r.UpdatedAt, _ = time.Parse(time.RFC3339, updatedStr)
		r.AssistantID = r.AgentID // SDK compat
		runs = append(runs, &r)
	}
	return runs, nil
}

// --------------------------------------------------------------------------
// Store (key-value)
// --------------------------------------------------------------------------

// nsDelim separates namespace segments in storage. Wrapping every encoded
// namespace in a leading AND trailing delimiter (not just joining) means a
// prefix-match pattern built the same way (see nsPrefixPattern) always ends
// on a segment boundary -- "team-a" can never LIKE-match "team-abc" -- and
// segments containing "/" round-trip correctly since "/" is no longer the
// separator. \x1F (ASCII unit separator) is chosen specifically because it
// essentially never appears in real namespace segments.
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

// nsPrefixPattern builds a SQL LIKE pattern matching only namespaces whose
// first len(prefix) segments equal prefix exactly -- never a same-string-prefix
// sibling like "team-abc" matching prefix ["team-a"].
func nsPrefixPattern(prefix []string) string {
	if len(prefix) == 0 {
		return "%"
	}
	return nsDelim + strings.Join(prefix, nsDelim) + nsDelim + "%"
}

// storeExpiresLayout is a fixed-width UTC timestamp used ONLY for
// store_items.expires_at string comparisons. Plain time.RFC3339
// truncates to whole seconds (broke sub-second TTLs). Plain
// time.RFC3339Nano is also wrong for lexicographic compares: Go trims
// trailing fractional zeros, so "...00.1Z" > "...00.11Z" as strings
// even though 100ms < 110ms chronologically -- items could read as
// still-alive (or already-expired) depending on the exact fractional
// digits. Fixed 9-digit fractional seconds keep string order == time
// order. nil ttlMinutes means no expiration.
const storeExpiresLayout = "2006-01-02T15:04:05.000000000Z07:00"

func formatStoreExpires(t time.Time) string {
	return t.UTC().Format(storeExpiresLayout)
}

func storeItemExpiresAt(now time.Time, ttlMinutes *float64) *string {
	if ttlMinutes == nil {
		return nil
	}
	s := formatStoreExpires(now.Add(time.Duration(*ttlMinutes * float64(time.Minute))))
	return &s
}

func (s *SQLiteStore) PutItem(ctx context.Context, item *models.StoreItem) error {
	val, _ := json.Marshal(item.Value)
	ns := nsToString(item.Namespace)
	nowT := time.Now().UTC()
	now := nowT.Format(time.RFC3339)
	expiresAt := storeItemExpiresAt(nowT, item.TTLMinutes)

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO store_items (tenant_id, namespace, key, value, created_at, updated_at, ttl_minutes, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(tenant_id, namespace, key) DO UPDATE SET
			value=excluded.value, updated_at=excluded.updated_at,
			ttl_minutes=excluded.ttl_minutes, expires_at=excluded.expires_at
	`, tenant.FromContext(ctx), ns, item.Key, string(val), now, now, item.TTLMinutes, expiresAt)
	return err
}

func (s *SQLiteStore) GetItem(ctx context.Context, namespace []string, key string, refreshTTL bool) (*models.StoreItem, error) {
	ns := nsToString(namespace)
	nowT := time.Now().UTC()
	query := `SELECT tenant_id, namespace, key, value, created_at, updated_at, ttl_minutes FROM store_items
		WHERE namespace = ? AND key = ? AND (expires_at IS NULL OR expires_at > ?)`
	args := []interface{}{ns, key, formatStoreExpires(nowT)}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	row := s.db.QueryRowContext(ctx, query, args...)

	var item models.StoreItem
	var tenantID, nsStr, valStr, createdStr, updatedStr string
	var ttlMinutes *float64
	if err := row.Scan(&tenantID, &nsStr, &item.Key, &valStr, &createdStr, &updatedStr, &ttlMinutes); err != nil {
		if err == sql.ErrNoRows {
			return nil, &state.ErrNotFound{Resource: "store_item", ID: key}
		}
		return nil, err
	}
	item.Namespace = stringToNs(nsStr)
	json.Unmarshal([]byte(valStr), &item.Value)
	item.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
	item.UpdatedAt, _ = time.Parse(time.RFC3339, updatedStr)

	if refreshTTL && ttlMinutes != nil {
		// Use the row's own tenant_id, not tenant.FromContext(ctx) --
		// for a system-context caller reading across tenants, those can
		// differ, which would otherwise silently match zero rows here.
		newExpiry := storeItemExpiresAt(nowT, ttlMinutes)
		_, _ = s.db.ExecContext(ctx, `UPDATE store_items SET expires_at = ? WHERE tenant_id = ? AND namespace = ? AND key = ?`,
			newExpiry, tenantID, ns, key)
	}
	return &item, nil
}

func (s *SQLiteStore) DeleteItem(ctx context.Context, namespace []string, key string) error {
	ns := nsToString(namespace)
	query := `DELETE FROM store_items WHERE namespace = ? AND key = ?`
	args := []interface{}{ns, key}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return &state.ErrNotFound{Resource: "store_item", ID: key}
	}
	return nil
}

func (s *SQLiteStore) SearchItems(ctx context.Context, req *models.StoreSearchRequest) ([]*models.StoreItem, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	nowT := time.Now().UTC()

	query := `SELECT tenant_id, namespace, key, value, created_at, updated_at, ttl_minutes FROM store_items`
	where := []string{"(expires_at IS NULL OR expires_at > ?)"}
	args := []interface{}{formatStoreExpires(nowT)}

	if !tenant.IsSystem(ctx) {
		where = append(where, "tenant_id = ?")
		args = append(args, tenant.FromContext(ctx))
	}
	if len(req.NamespacePrefix) > 0 {
		where = append(where, "namespace LIKE ?")
		args = append(args, nsPrefixPattern(req.NamespacePrefix))
	}
	for k, v := range req.Filter {
		where = append(where, "json_extract(value, ?) = ?")
		args = append(args, "$."+k)
		if sv, ok := v.(string); ok {
			args = append(args, sv)
		} else {
			valJSON, _ := json.Marshal(v)
			args = append(args, string(valJSON))
		}
	}
	query += " WHERE " + strings.Join(where, " AND ")
	query += " ORDER BY updated_at DESC LIMIT ? OFFSET ?"
	args = append(args, limit, req.Offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Non-nil so a no-results search JSON-encodes to "items": [] rather
	// than "items": null -- SDK clients call .map() on it unconditionally.
	items := []*models.StoreItem{}
	// (tenantID, namespace-string, key, ttlMinutes) rows to refresh after
	// rows.Close() below -- some sqlite drivers don't allow another
	// query/exec on the same connection while a result set is open.
	type refreshRow struct {
		tenantID, ns, key string
		ttlMinutes        float64
	}
	var toRefresh []refreshRow
	for rows.Next() {
		var item models.StoreItem
		var tenantID, nsStr, valStr, createdStr, updatedStr string
		var ttlMinutes *float64
		if err := rows.Scan(&tenantID, &nsStr, &item.Key, &valStr, &createdStr, &updatedStr, &ttlMinutes); err != nil {
			return nil, err
		}
		item.Namespace = stringToNs(nsStr)
		json.Unmarshal([]byte(valStr), &item.Value)
		item.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
		item.UpdatedAt, _ = time.Parse(time.RFC3339, updatedStr)
		items = append(items, &item)
		if req.RefreshTTLOrDefault() && ttlMinutes != nil {
			toRefresh = append(toRefresh, refreshRow{tenantID, nsStr, item.Key, *ttlMinutes})
		}
	}
	rows.Close()
	for _, rr := range toRefresh {
		newExpiry := storeItemExpiresAt(nowT, &rr.ttlMinutes)
		_, _ = s.db.ExecContext(ctx, `UPDATE store_items SET expires_at = ? WHERE tenant_id = ? AND namespace = ? AND key = ?`,
			newExpiry, rr.tenantID, rr.ns, rr.key)
	}
	return items, nil
}

func (s *SQLiteStore) ListNamespaces(ctx context.Context, req *models.StoreListNamespacesRequest) ([][]string, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}

	query := `SELECT DISTINCT namespace FROM store_items`
	var args []interface{}
	var where []string

	if !tenant.IsSystem(ctx) {
		where = append(where, "tenant_id = ?")
		args = append(args, tenant.FromContext(ctx))
	}
	if len(req.Prefix) > 0 {
		where = append(where, "namespace LIKE ?")
		args = append(args, nsPrefixPattern(req.Prefix))
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY namespace LIMIT ? OFFSET ?"
	args = append(args, limit, req.Offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
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

func (s *SQLiteStore) SaveWebhookDeadLetter(ctx context.Context, dl *models.WebhookDeadLetter) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO webhook_dead_letters (id, url, event_type, run_id, payload, error, attempts, failed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, dl.ID, dl.URL, dl.EventType, dl.RunID, string(dl.Payload), dl.Error, dl.Attempts, dl.FailedAt.UTC().Format(time.RFC3339))
	return err
}

func (s *SQLiteStore) ListWebhookDeadLetters(ctx context.Context, limit int) ([]*models.WebhookDeadLetter, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, url, event_type, run_id, payload, error, attempts, failed_at
		FROM webhook_dead_letters ORDER BY failed_at DESC LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*models.WebhookDeadLetter{}
	for rows.Next() {
		var dl models.WebhookDeadLetter
		var payloadStr, failedAtStr string
		if err := rows.Scan(&dl.ID, &dl.URL, &dl.EventType, &dl.RunID, &payloadStr, &dl.Error, &dl.Attempts, &failedAtStr); err != nil {
			return nil, err
		}
		dl.Payload = json.RawMessage(payloadStr)
		dl.FailedAt, _ = time.Parse(time.RFC3339, failedAtStr)
		out = append(out, &dl)
	}
	return out, nil
}

// --------------------------------------------------------------------------
// Run cache (LLM response caching)
// --------------------------------------------------------------------------

func (s *SQLiteStore) GetCachedRunResult(ctx context.Context, cacheKey string) (*models.CachedRunResult, error) {
	// cacheKey incorporates tenant_id via computeCacheKey; this WHERE
	// clause is defense in depth on top of the composite PK.
	now := time.Now().UTC().Format(time.RFC3339)
	query := `SELECT cache_key, agent_id, output, created_at, expires_at FROM run_cache WHERE cache_key = ? AND expires_at > ?`
	args := []interface{}{cacheKey, now}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	row := s.db.QueryRowContext(ctx, query, args...)

	var r models.CachedRunResult
	var outputStr, createdAtStr, expiresAtStr string
	if err := row.Scan(&r.CacheKey, &r.AgentID, &outputStr, &createdAtStr, &expiresAtStr); err != nil {
		if err == sql.ErrNoRows {
			return nil, &state.ErrNotFound{Resource: "run_cache", ID: cacheKey}
		}
		return nil, err
	}
	json.Unmarshal([]byte(outputStr), &r.Output)
	r.CreatedAt, _ = time.Parse(time.RFC3339, createdAtStr)
	r.ExpiresAt, _ = time.Parse(time.RFC3339, expiresAtStr)
	return &r, nil
}

// --------------------------------------------------------------------------
// Cron scheduler
// --------------------------------------------------------------------------

func (s *SQLiteStore) UpsertCronSchedule(ctx context.Context, sched *models.CronSchedule) error {
	input, _ := json.Marshal(sched.Input)
	config, _ := json.Marshal(sched.Config)
	now := time.Now().UTC().Format(time.RFC3339)
	enabled := 0
	if sched.Enabled {
		enabled = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO cron_schedules (tenant_id, name, agent_id, expression, timezone, input, config, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(tenant_id, name) DO UPDATE SET
			agent_id=excluded.agent_id, expression=excluded.expression, timezone=excluded.timezone,
			input=excluded.input, config=excluded.config, enabled=excluded.enabled, updated_at=excluded.updated_at
	`, tenant.FromContext(ctx), sched.Name, sched.AgentID, sched.Expression, sched.Timezone, string(input), string(config), enabled, now, now)
	return err
}

// ListCronSchedules is always called from a system context in practice
// (the scheduler loop must see every tenant's schedules to service them
// all -- see cmd/cron.go), but honors a regular tenant context too for
// completeness/testability: TenantID is always populated on the returned
// rows either way, since the scheduler needs it to dispatch each fire
// under its own schedule's tenant, not whichever context listed it.
func (s *SQLiteStore) ListCronSchedules(ctx context.Context) ([]*models.CronSchedule, error) {
	query := `SELECT tenant_id, name, agent_id, expression, timezone, input, config, enabled, created_at, updated_at FROM cron_schedules`
	var args []interface{}
	if !tenant.IsSystem(ctx) {
		query += ` WHERE tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	query += ` ORDER BY tenant_id, name`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*models.CronSchedule{}
	for rows.Next() {
		var sc models.CronSchedule
		var inputStr, configStr, createdAtStr, updatedAtStr string
		var enabled int
		if err := rows.Scan(&sc.TenantID, &sc.Name, &sc.AgentID, &sc.Expression, &sc.Timezone, &inputStr, &configStr, &enabled, &createdAtStr, &updatedAtStr); err != nil {
			return nil, err
		}
		sc.Input = json.RawMessage(inputStr)
		sc.Config = json.RawMessage(configStr)
		sc.Enabled = enabled != 0
		sc.CreatedAt, _ = time.Parse(time.RFC3339, createdAtStr)
		sc.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAtStr)
		out = append(out, &sc)
	}
	return out, nil
}

func (s *SQLiteStore) DeleteCronSchedule(ctx context.Context, name string) error {
	query := `DELETE FROM cron_schedules WHERE name = ?`
	args := []interface{}{name}
	if !tenant.IsSystem(ctx) {
		query += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	_, err := s.db.ExecContext(ctx, query, args...)
	return err
}

func (s *SQLiteStore) TryClaimCronFire(ctx context.Context, scheduleName string, fireTime time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO cron_claims (tenant_id, schedule_name, fire_time, claimed_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(tenant_id, schedule_name, fire_time) DO NOTHING
	`, tenant.FromContext(ctx), scheduleName, fireTime.UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *SQLiteStore) ReleaseCronClaim(ctx context.Context, scheduleName string, fireTime time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM cron_claims WHERE tenant_id = ? AND schedule_name = ? AND fire_time = ?
	`, tenant.FromContext(ctx), scheduleName, fireTime.UTC().Format(time.RFC3339))
	return err
}

func (s *SQLiteStore) GetLastCronFireTime(ctx context.Context, scheduleName string) (time.Time, bool, error) {
	var fireTimeStr string
	err := s.db.QueryRowContext(ctx, `
		SELECT fire_time FROM cron_claims WHERE tenant_id = ? AND schedule_name = ? ORDER BY fire_time DESC LIMIT 1
	`, tenant.FromContext(ctx), scheduleName).Scan(&fireTimeStr)
	if err == sql.ErrNoRows {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	t, err := time.Parse(time.RFC3339, fireTimeStr)
	if err != nil {
		return time.Time{}, false, err
	}
	return t, true, nil
}

func (s *SQLiteStore) SaveCachedRunResult(ctx context.Context, result *models.CachedRunResult) error {
	output, _ := json.Marshal(result.Output)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO run_cache (tenant_id, cache_key, agent_id, output, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(tenant_id, cache_key) DO UPDATE SET
			output=excluded.output, created_at=excluded.created_at, expires_at=excluded.expires_at
	`, tenant.FromContext(ctx), result.CacheKey, result.AgentID, string(output),
		result.CreatedAt.UTC().Format(time.RFC3339), result.ExpiresAt.UTC().Format(time.RFC3339))
	return err
}
