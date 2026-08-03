package migrate

import (
	"context"
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
type Pgx struct {
	DB PgxDB
}

// NewPgx returns a Bookkeeper for Postgres.
func NewPgx(db PgxDB) *Pgx {
	return &Pgx{DB: db}
}

func (p *Pgx) Ensure(ctx context.Context) error {
	_, err := p.DB.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`)
	return err
}

func (p *Pgx) Current(ctx context.Context) (int, error) {
	var v *int
	err := p.DB.QueryRow(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v)
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
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES ($1, $2, $3)`,
		version, name, time.Now().UTC())
	return err
}

func (p *Pgx) Delete(ctx context.Context, version int) error {
	_, err := p.DB.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, version)
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
