package credentials

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Dialect string

const (
	SQLite   Dialect = "sqlite"
	Postgres Dialect = "postgres"
	MySQL    Dialect = "mysql"
)

// Schema is installed by the control plane's ordinary migration runner. There
// is deliberately no startup auto-migration or separate database of secrets.
func Schema(d Dialect) ([]string, error) {
	if d != SQLite && d != Postgres && d != MySQL {
		return nil, ErrUnavailable
	}
	idType, hashType, blobType, tenantType, revisionType := "VARCHAR(64)", "VARCHAR(64)", "TEXT", "TEXT", "VARCHAR(128)"
	if d == MySQL {
		idType = "VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin"
		hashType = idType
		blobType = "LONGTEXT"
		tenantType = "VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin"
		revisionType = "VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin"
	}
	table := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS credential_leases (
lease_id %s PRIMARY KEY,
tenant_id %s NOT NULL,
tenant_hash %s NOT NULL,
owner_key %s NOT NULL UNIQUE,
binding_revision %s NOT NULL,
version BIGINT NOT NULL,
state VARCHAR(32) NOT NULL,
updated_at VARCHAR(40) NOT NULL,
record_json %s NOT NULL,
protected_handle %s NOT NULL
)`, idType, tenantType, hashType, hashType, revisionType, blobType, blobType)
	return []string{table}, nil
}

type scanRow interface{ Scan(...any) error }
type scanRows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close()
}
type sqlAccess interface {
	exec(context.Context, string, ...any) (int64, error)
	row(context.Context, string, ...any) scanRow
	rows(context.Context, string, ...any) (scanRows, error)
}

type stdAccess struct{ db *sql.DB }

func (a stdAccess) exec(ctx context.Context, q string, args ...any) (int64, error) {
	r, e := a.db.ExecContext(ctx, q, args...)
	if e != nil {
		return 0, e
	}
	return r.RowsAffected()
}
func (a stdAccess) row(ctx context.Context, q string, args ...any) scanRow {
	return a.db.QueryRowContext(ctx, q, args...)
}

type stdRows struct{ *sql.Rows }

func (r stdRows) Close() { _ = r.Rows.Close() }
func (a stdAccess) rows(ctx context.Context, q string, args ...any) (scanRows, error) {
	r, e := a.db.QueryContext(ctx, q, args...)
	if e != nil {
		return nil, e
	}
	return stdRows{r}, nil
}

type pgAccess struct{ pool *pgxpool.Pool }

func (a pgAccess) exec(ctx context.Context, q string, args ...any) (int64, error) {
	r, e := a.pool.Exec(ctx, q, args...)
	return r.RowsAffected(), e
}
func (a pgAccess) row(ctx context.Context, q string, args ...any) scanRow {
	return a.pool.QueryRow(ctx, q, args...)
}
func (a pgAccess) rows(ctx context.Context, q string, args ...any) (scanRows, error) {
	return a.pool.Query(ctx, q, args...)
}

type SQLStore struct {
	db      sqlAccess
	dialect Dialect
}

func NewSQLStore(db *sql.DB, d Dialect) (*SQLStore, error) {
	if db == nil || (d != SQLite && d != MySQL && d != Postgres) {
		return nil, ErrUnavailable
	}
	return &SQLStore{stdAccess{db}, d}, nil
}
func NewPGXStore(pool *pgxpool.Pool) (*SQLStore, error) {
	if pool == nil {
		return nil, ErrUnavailable
	}
	return &SQLStore{pgAccess{pool}, Postgres}, nil
}

func (s *SQLStore) bind(q string) string {
	if s.dialect != Postgres {
		return q
	}
	i := 0
	return replaceBinds(q, &i)
}
func replaceBinds(q string, i *int) string {
	var b strings.Builder
	for _, r := range q {
		if r == '?' {
			*i = *i + 1
			fmt.Fprintf(&b, "$%d", *i)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func tenantHash(tenant string) string {
	sum := sha256.Sum256([]byte(tenant))
	return hex.EncodeToString(sum[:])
}

func decodeRecord(row scanRow) (*Record, error) {
	var metadata, handle string
	if err := row.Scan(&metadata, &handle); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, ErrUnavailable
	}
	var r Record
	if json.Unmarshal([]byte(metadata), &r) != nil || r.ID == "" || !r.Owner.valid() {
		return nil, ErrUnavailable
	}
	r.ProtectedHandle = handle
	return &r, nil
}

func (s *SQLStore) Create(ctx context.Context, r *Record) (*Record, bool, error) {
	if r == nil || r.ID == "" || !r.Owner.valid() || r.Version != 1 || r.State != StateIssuing || r.BindingID == "" || r.BindingRevision == "" || r.ProtectedHandle != "" {
		return nil, false, ErrUnavailable
	}
	metadata, err := json.Marshal(r)
	if err != nil {
		return nil, false, ErrUnavailable
	}
	q := `INSERT INTO credential_leases (lease_id,tenant_id,tenant_hash,owner_key,binding_revision,version,state,updated_at,record_json,protected_handle) VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`
	if s.dialect == MySQL {
		q = `INSERT IGNORE INTO credential_leases (lease_id,tenant_id,tenant_hash,owner_key,binding_revision,version,state,updated_at,record_json,protected_handle) VALUES (?,?,?,?,?,?,?,?,?,?)`
	}
	affected, err := s.db.exec(ctx, s.bind(q), r.ID, r.Owner.TenantID, tenantHash(r.Owner.TenantID), ownerKey(r.Owner, r.BindingID), r.BindingRevision, r.Version, string(r.State), r.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z"), string(metadata), "")
	if err != nil {
		return nil, false, ErrUnavailable
	}
	actual, err := decodeRecord(s.db.row(ctx, s.bind(`SELECT record_json,protected_handle FROM credential_leases WHERE owner_key=?`), ownerKey(r.Owner, r.BindingID)))
	if err != nil || actual.Owner != r.Owner || actual.BindingID != r.BindingID {
		return nil, false, ErrUnavailable
	}
	return actual, affected == 1 && actual.ID == r.ID, nil
}

func (s *SQLStore) Get(ctx context.Context, tenant, id string) (*Record, error) {
	r, err := decodeRecord(s.db.row(ctx, s.bind(`SELECT record_json,protected_handle FROM credential_leases WHERE tenant_id=? AND tenant_hash=? AND lease_id=?`), tenant, tenantHash(tenant), id))
	if err != nil {
		return nil, err
	}
	if r.Owner.TenantID != tenant {
		return nil, ErrNotFound
	}
	return r, nil
}

func (s *SQLStore) CAS(ctx context.Context, r *Record, expected int64) (*Record, error) {
	if r == nil || expected < 1 || r.Version != expected+1 || !r.Owner.valid() {
		return nil, ErrConflict
	}
	metadata, err := json.Marshal(r)
	if err != nil {
		return nil, ErrUnavailable
	}
	affected, err := s.db.exec(ctx, s.bind(`UPDATE credential_leases SET version=?,state=?,updated_at=?,record_json=?,protected_handle=? WHERE lease_id=? AND tenant_id=? AND tenant_hash=? AND owner_key=? AND binding_revision=? AND version=?`), r.Version, string(r.State), r.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z"), string(metadata), r.ProtectedHandle, r.ID, r.Owner.TenantID, tenantHash(r.Owner.TenantID), ownerKey(r.Owner, r.BindingID), r.BindingRevision, expected)
	if err != nil {
		return nil, ErrUnavailable
	}
	if affected != 1 {
		return nil, ErrConflict
	}
	return clone(r), nil
}

func (s *SQLStore) ListUnresolved(ctx context.Context, limit int) ([]*Record, error) {
	if limit < 1 || limit > 1000 {
		return nil, ErrUnavailable
	}
	// Lost issuance handles cannot be recovered through a broad Vault listing.
	// Keep those records, but do not let them starve known-handle cleanup.
	rows, err := s.db.rows(ctx, s.bind(`SELECT record_json,protected_handle FROM credential_leases WHERE state NOT IN (?,?) AND (state<>? OR protected_handle<>'') ORDER BY updated_at,lease_id LIMIT ?`), string(StateRevoked), string(StateNotIssued), string(StateIssuanceUnknown), limit)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer rows.Close()
	var result []*Record
	for rows.Next() {
		r, err := decodeRecord(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	if rows.Err() != nil {
		return nil, ErrUnavailable
	}
	return result, nil
}

// Search is tenant-scoped in SQL, not a post-filter of a global bounded page.
// It intentionally never selects the private protected_handle column.
func (s *SQLStore) Search(ctx context.Context, tenant string, limit int) ([]*Record, error) {
	if tenant == "" || limit < 1 || limit > 100 {
		return nil, ErrUnavailable
	}
	rows, err := s.db.rows(ctx, s.bind(`SELECT record_json FROM credential_leases WHERE tenant_id=? AND tenant_hash=? ORDER BY updated_at DESC,lease_id DESC LIMIT ?`), tenant, tenantHash(tenant), limit)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer rows.Close()
	result := make([]*Record, 0)
	for rows.Next() {
		var raw string
		if rows.Scan(&raw) != nil {
			return nil, ErrUnavailable
		}
		var r Record
		if json.Unmarshal([]byte(raw), &r) != nil || r.Owner.TenantID != tenant {
			return nil, ErrUnavailable
		}
		result = append(result, &r)
	}
	if rows.Err() != nil {
		return nil, ErrUnavailable
	}
	return result, nil
}
