package migrate

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PgxDB is the subset of *pgxpool.Pool / *pgxpool.Conn used for version
// bookkeeping under Postgres.
type PgxDB interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Pgx is a Bookkeeper backed by pgx.
//
// TableName selects the version table (default schema_migrations). Vector
// stores that share a Postgres database with the control-plane state store
// must use a distinct name (e.g. vector_schema_migrations) so the two
// version streams never collide.
type Pgx struct {
	DB        PgxDB
	TableName string
}

// NewPgx returns a Bookkeeper for Postgres writing to schema_migrations.
func NewPgx(db PgxDB) *Pgx {
	return &Pgx{DB: db}
}

// NewPgxTable returns a Bookkeeper writing to tableName. tableName must be
// a fixed safe identifier from code (never request/user input).
func NewPgxTable(db PgxDB, tableName string) *Pgx {
	return &Pgx{DB: db, TableName: tableName}
}

func (p *Pgx) table() string {
	if p.TableName == "" {
		return "schema_migrations"
	}
	return p.TableName
}

func (p *Pgx) Ensure(ctx context.Context) error {
	_, err := p.DB.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`, p.table()))
	return err
}

func (p *Pgx) Current(ctx context.Context) (int, error) {
	var v *int
	err := p.DB.QueryRow(ctx, fmt.Sprintf(`SELECT MAX(version) FROM %s`, p.table())).Scan(&v)
	if err != nil {
		return 0, err
	}
	if v == nil {
		return 0, nil
	}
	return *v, nil
}

func (p *Pgx) Insert(ctx context.Context, version int, name string) error {
	_, err := p.DB.Exec(ctx,
		fmt.Sprintf(`INSERT INTO %s (version, name, applied_at) VALUES ($1, $2, $3)`, p.table()),
		version, name, time.Now().UTC())
	return err
}

func (p *Pgx) Delete(ctx context.Context, version int) error {
	_, err := p.DB.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE version = $1`, p.table()), version)
	return err
}

// PgxTableExists reports whether a relation exists in the public schema.
func PgxTableExists(ctx context.Context, db PgxDB, table string) (bool, error) {
	var name *string
	err := db.QueryRow(ctx, `SELECT to_regclass($1)::text`, "public."+table).Scan(&name)
	if err != nil {
		return false, err
	}
	return name != nil, nil
}
