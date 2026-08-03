// Package migrate is a small numbered up/down schema migration runner for
// the control-plane state stores. No external migrator dependency: each
// backend registers Go Migration steps (baseline = today's full Init DDL)
// and this package tracks applied versions in schema_migrations.
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// ErrNoMigration means Downgrade was asked to roll back but nothing is applied.
var ErrNoMigration = errors.New("no migration to roll back")

// Migration is one numbered schema step. Versions must be contiguous from 1.
type Migration struct {
	Version int
	Name    string
	Up      func(ctx context.Context) error
	Down    func(ctx context.Context) error
}

// Bookkeeper records which migration versions have been applied.
type Bookkeeper interface {
	Ensure(ctx context.Context) error
	Current(ctx context.Context) (int, error)
	Insert(ctx context.Context, version int, name string) error
	Delete(ctx context.Context, version int) error
}

// Upgrade ensures the migrations table exists, heals and stamps baseline
// v1 when an already-initialized legacy database has no version rows,
// then applies every pending Up in order.
//
// Legacy path (tables exist, schema_migrations empty): runs migration
// 1's Up before stamping. Baseline Up is written to be idempotent
// (CREATE IF NOT EXISTS / existence-checked ADD COLUMN), and those ADD
// COLUMN steps are exactly the self-healing the old unconditional Init
// ran on every boot -- skipping Up and only inserting the version row
// would leave older schemas missing columns like threads.version.
func Upgrade(ctx context.Context, bk Bookkeeper, migrations []Migration, legacyExists func(context.Context) (bool, error)) error {
	if err := validate(migrations); err != nil {
		return err
	}
	if err := bk.Ensure(ctx); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}
	cur, err := bk.Current(ctx)
	if err != nil {
		return fmt.Errorf("current schema version: %w", err)
	}
	if cur == 0 && legacyExists != nil {
		exists, err := legacyExists(ctx)
		if err != nil {
			return fmt.Errorf("legacy schema probe: %w", err)
		}
		if exists {
			m1 := byVersion(migrations, 1)
			if m1 == nil {
				return fmt.Errorf("legacy stamp requires migration version 1")
			}
			if err := m1.Up(ctx); err != nil {
				return fmt.Errorf("migrate up %d (%s) for legacy schema: %w", m1.Version, m1.Name, err)
			}
			if err := bk.Insert(ctx, m1.Version, m1.Name); err != nil {
				return fmt.Errorf("stamp baseline v1: %w", err)
			}
			cur = 1
		}
	}
	sorted := append([]Migration(nil), migrations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Version < sorted[j].Version })
	for _, m := range sorted {
		if m.Version <= cur {
			continue
		}
		if m.Version != cur+1 {
			return fmt.Errorf("migration version gap: current=%d next=%d", cur, m.Version)
		}
		if err := m.Up(ctx); err != nil {
			return fmt.Errorf("migrate up %d (%s): %w", m.Version, m.Name, err)
		}
		if err := bk.Insert(ctx, m.Version, m.Name); err != nil {
			return fmt.Errorf("record migration %d (%s): %w", m.Version, m.Name, err)
		}
		cur = m.Version
	}
	return nil
}

// Downgrade runs Down for the highest applied version and deletes its row.
func Downgrade(ctx context.Context, bk Bookkeeper, migrations []Migration) error {
	if err := validate(migrations); err != nil {
		return err
	}
	if err := bk.Ensure(ctx); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}
	cur, err := bk.Current(ctx)
	if err != nil {
		return fmt.Errorf("current schema version: %w", err)
	}
	if cur == 0 {
		return ErrNoMigration
	}
	m := byVersion(migrations, cur)
	if m == nil {
		return fmt.Errorf("applied version %d has no registered Down", cur)
	}
	if err := m.Down(ctx); err != nil {
		return fmt.Errorf("migrate down %d (%s): %w", m.Version, m.Name, err)
	}
	if err := bk.Delete(ctx, cur); err != nil {
		return fmt.Errorf("unrecord migration %d: %w", cur, err)
	}
	return nil
}

func validate(migrations []Migration) error {
	if len(migrations) == 0 {
		return fmt.Errorf("no migrations registered")
	}
	seen := map[int]string{}
	for _, m := range migrations {
		if m.Version < 1 {
			return fmt.Errorf("migration version must be >= 1, got %d", m.Version)
		}
		if m.Name == "" {
			return fmt.Errorf("migration %d: empty name", m.Version)
		}
		if m.Up == nil || m.Down == nil {
			return fmt.Errorf("migration %d (%s): Up and Down are required", m.Version, m.Name)
		}
		if prev, ok := seen[m.Version]; ok {
			return fmt.Errorf("duplicate migration version %d (%s and %s)", m.Version, prev, m.Name)
		}
		seen[m.Version] = m.Name
	}
	for v := 1; v <= len(migrations); v++ {
		if _, ok := seen[v]; !ok {
			return fmt.Errorf("migrations must be contiguous from 1: missing %d", v)
		}
	}
	if len(seen) != len(migrations) {
		return fmt.Errorf("migrations must be contiguous from 1: have %d entries, expected versions 1..%d", len(migrations), len(migrations))
	}
	return nil
}

func byVersion(migrations []Migration, version int) *Migration {
	for i := range migrations {
		if migrations[i].Version == version {
			return &migrations[i]
		}
	}
	return nil
}

// Dialect selects CREATE TABLE / time literals for schema_migrations.
type Dialect int

const (
	SQLite Dialect = iota
	Postgres
	MySQL
)

// DB is the subset of *sql.DB / *sql.Conn used for version bookkeeping and
// helpers. Postgres uses a separate pgx-backed Bookkeeper.
type DB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// SQL is a Bookkeeper backed by database/sql (SQLite and MySQL).
type SQL struct {
	DB      DB
	Dialect Dialect
}

// NewSQL returns a Bookkeeper for SQLite or MySQL.
func NewSQL(db DB, d Dialect) *SQL {
	return &SQL{DB: db, Dialect: d}
}

func (s *SQL) Ensure(ctx context.Context) error {
	var ddl string
	switch s.Dialect {
	case SQLite:
		ddl = `CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`
	case MySQL:
		ddl = `CREATE TABLE IF NOT EXISTS schema_migrations (
			version INT NOT NULL,
			name VARCHAR(255) NOT NULL,
			applied_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			PRIMARY KEY (version)
		) ENGINE=InnoDB CHARSET=utf8mb4`
	default:
		return fmt.Errorf("SQL bookkeeper: unsupported dialect %v (use pgx bookkeeper for Postgres)", s.Dialect)
	}
	_, err := s.DB.ExecContext(ctx, ddl)
	return err
}

func (s *SQL) Current(ctx context.Context) (int, error) {
	var v sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	if err != nil {
		return 0, err
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}

func (s *SQL) Insert(ctx context.Context, version int, name string) error {
	switch s.Dialect {
	case MySQL:
		_, err := s.DB.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			version, name, time.Now().UTC())
		return err
	default:
		_, err := s.DB.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, datetime('now'))`,
			version, name)
		return err
	}
}

func (s *SQL) Delete(ctx context.Context, version int) error {
	var q string
	switch s.Dialect {
	case MySQL:
		q = `DELETE FROM schema_migrations WHERE version = ?`
	default:
		q = `DELETE FROM schema_migrations WHERE version = ?`
	}
	_, err := s.DB.ExecContext(ctx, q, version)
	return err
}

// TableExists reports whether a user table exists (legacy-schema probe).
func TableExists(ctx context.Context, db DB, d Dialect, table string) (bool, error) {
	var q string
	var args []any
	switch d {
	case SQLite:
		q = `SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ? LIMIT 1`
		args = []any{table}
	case MySQL:
		q = `SELECT 1 FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? LIMIT 1`
		args = []any{table}
	default:
		return false, fmt.Errorf("TableExists: unsupported dialect %v", d)
	}
	var one int
	err := db.QueryRowContext(ctx, q, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
