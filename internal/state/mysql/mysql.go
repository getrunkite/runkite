// Package mysql will implement the state.Store interface using MySQL --
// the second SQL exemplar alongside Postgres/SQLite (master plan:
// "MySQL stays 'future SQL twin if someone needs it'"), same
// conformance suite (internal/state/conformance) as every other
// backend. Checkpoint 1 is schema-only (New/Init/Close); CRUD lands
// in later checkpoints before the Store interface is satisfied.
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
//  1. No RETURNING clause (confirmed against the MySQL 8.4 reference
//     manual -- INSERT ... ON DUPLICATE KEY UPDATE has no equivalent,
//     and the classic LAST_INSERT_ID(expr) workaround only helps
//     AUTO_INCREMENT columns, which this schema doesn't use --
//     every primary key here is an application-generated string).
//     Postgres/SQLite's UpsertAgent-style "one statement, RETURNING
//     the resulting version" pattern doesn't port. Versioned upserts
//     here use the same explicit read-compute-write-in-a-transaction
//     pattern already built for MongoDB instead (see mongo.go's
//     UpsertAgent) -- compute the target version in Go from a row
//     read inside the transaction, then write it, all under one
//     transaction for atomicity.
//  2. TEXT/BLOB columns cannot be part of a PRIMARY KEY or a plain
//     index without an explicit prefix length in InnoDB, unlike
//     Postgres/SQLite where a TEXT column is a perfectly normal
//     primary key. Every column used as a key anywhere (tenant_id,
//     agent_id, thread_id, run_id, name, checkpoint_id, cache_key,
//     schedule_name, namespace, store `key`) is VARCHAR(255) here
//     instead of TEXT; large freeform content (description,
//     error_msg, checkpoint_ns's occasional long value) stays TEXT
//     since it's never part of a key.
package mysql

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/go-sql-driver/mysql"
)

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
