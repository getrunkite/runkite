// Package pgvector implements vectorstore.VectorStore using PostgreSQL's
// pgvector extension -- the vector/semantic store's Tier 1, stable
// backend. A dedicated pool/package rather than folding into
// internal/state/postgres: the vector extension and its own table are an
// entirely separate concern from control-plane metadata, opt-in
// (vector_store must be explicitly configured), and this keeps an existing
// Postgres-backed deployment that never touches vector_store from ever
// needing the extension to exist at all.
package pgvector

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"
	pgxvector "github.com/pgvector/pgvector-go/pgx"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state/migrate"
	"github.com/getrunkite/runkite/internal/tenant"
)

// schemaMigrationsTable is separate from the state store's
// schema_migrations -- both can share one Postgres database, and their
// version streams must not collide (state v1 stamped must not skip
// vector baseline).
const schemaMigrationsTable = "vector_schema_migrations"

// initAdvisoryLockKey serializes vector schema DDL across concurrent
// control-plane replicas. Distinct from the state store's lock key.
const initAdvisoryLockKey = 894127002

// Store implements vectorstore.VectorStore with PostgreSQL + pgvector.
type Store struct {
	pool       *pgxpool.Pool
	dimensions int
}

// New creates a pgvector-backed store. dimensions fixes the embedding
// column's width at schema-creation time (pgvector's vector(N) type is
// fixed-dimension) -- every item upserted must supply exactly this many
// floats, checked at the API layer before it ever reaches SQL.
func New(ctx context.Context, dsn string, dimensions int) (*Store, error) {
	if dimensions <= 0 {
		return nil, fmt.Errorf("pgvector: dimensions must be > 0, got %d", dimensions)
	}

	// The vector extension must exist before ANY connection can register
	// its types (AfterConnect below queries pg_type for "vector") --
	// create it first via a plain, short-lived connection that has no
	// such requirement itself. Doing this here (not in Init, which runs
	// after New returns) is what breaks the chicken-and-egg: New's own
	// pool.Ping below would otherwise try to register a type that
	// doesn't exist yet on a brand-new database.
	bootstrapConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgvector: connect: %w", err)
	}
	_, err = bootstrapConn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector")
	bootstrapConn.Close(ctx)
	if err != nil {
		return nil, fmt.Errorf("pgvector: CREATE EXTENSION vector (is the pgvector extension installed on this Postgres server?): %w", err)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("pgvector: parse dsn: %w", err)
	}
	// pgx encodes/decodes pgvector.Vector in its efficient binary wire
	// format only once the type is registered per-connection -- pgxpool
	// hands out different underlying *pgx.Conn per checkout, so this has
	// to run on every new physical connection, not once at pool creation.
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgxvector.RegisterTypes(ctx, conn)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pgvector: pgxpool.NewWithConfig: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgvector: ping: %w", err)
	}
	return &Store{pool: pool, dimensions: dimensions}, nil
}

// Init applies pending numbered schema migrations for vector_items
// (tracked in vector_schema_migrations, not the state store's
// schema_migrations). Safe to call concurrently from multiple processes
// -- wrapped in a session advisory lock like state.Store.Init. The
// vector extension itself is already created by New.
//
// Known limitation: the embedding column's dimension is fixed the FIRST
// time the table is created; CREATE TABLE IF NOT EXISTS never revisits
// it. Changing vector_store.dimensions after the table exists does not
// migrate rows or the column type -- Upsert fails with a Postgres
// dimension-mismatch error until the table is dropped/recreated. No
// in-place ALTER COLUMN TYPE step is registered for that.
func (s *Store) Init(ctx context.Context) error {
	return s.withSchemaLock(ctx, func(ctx context.Context, conn *pgxpool.Conn) error {
		bk := migrate.NewPgxTable(conn, schemaMigrationsTable)
		return migrate.Upgrade(ctx, bk, s.migrations(conn), func(ctx context.Context) (bool, error) {
			return migrate.PgxTableExists(ctx, conn, "vector_items")
		})
	})
}

// Downgrade rolls back the most recently applied vector migration under
// the same advisory lock Init uses. Baseline (v1) Down drops
// vector_items (destructive).
func (s *Store) Downgrade(ctx context.Context) error {
	return s.withSchemaLock(ctx, func(ctx context.Context, conn *pgxpool.Conn) error {
		bk := migrate.NewPgxTable(conn, schemaMigrationsTable)
		return migrate.Downgrade(ctx, bk, s.migrations(conn))
	})
}

func (s *Store) withSchemaLock(ctx context.Context, fn func(context.Context, *pgxpool.Conn) error) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("pgvector: acquire connection for schema lock: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", initAdvisoryLockKey); err != nil {
		return fmt.Errorf("pgvector: acquire schema advisory lock: %w", err)
	}
	defer func() {
		if _, unlockErr := conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", initAdvisoryLockKey); unlockErr != nil {
			return
		}
	}()
	return fn(ctx, conn)
}

func (s *Store) migrations(conn *pgxpool.Conn) []migrate.Migration {
	return []migrate.Migration{{
		Version: 1,
		Name:    "baseline",
		Up: func(ctx context.Context) error {
			return s.initSchemaLocked(ctx, conn)
		},
		Down: func(ctx context.Context) error {
			_, err := conn.Exec(ctx, `DROP TABLE IF EXISTS vector_items CASCADE`)
			return err
		},
	}}
}

func (s *Store) initSchemaLocked(ctx context.Context, conn *pgxpool.Conn) error {
	schema := fmt.Sprintf(`
	CREATE TABLE IF NOT EXISTS vector_items (
		tenant_id  TEXT NOT NULL DEFAULT 'default',
		namespace  TEXT NOT NULL,
		id         TEXT NOT NULL,
		content    TEXT DEFAULT '',
		embedding  vector(%d) NOT NULL,
		metadata   JSONB DEFAULT '{}',
		created_at TIMESTAMPTZ DEFAULT NOW(),
		updated_at TIMESTAMPTZ DEFAULT NOW(),
		PRIMARY KEY (tenant_id, namespace, id)
	);
	`, s.dimensions)
	if _, err := conn.Exec(ctx, schema); err != nil {
		return fmt.Errorf("pgvector: create table: %w", err)
	}

	// HNSW (not IVFFlat): builds incrementally as rows are inserted, so it
	// stays correct for a store that grows continuously -- IVFFlat's
	// clusters are trained once from whatever data exists at CREATE INDEX
	// time and degrade as the data distribution drifts, needing a manual
	// REINDEX. vector_cosine_ops matches Search's `<=>` operator below.
	if _, err := conn.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_vector_items_hnsw ON vector_items USING hnsw (embedding vector_cosine_ops)`); err != nil {
		return fmt.Errorf("pgvector: create hnsw index: %w", err)
	}
	return nil
}

func (s *Store) Close() error {
	s.pool.Close()
	return nil
}

func (s *Store) Upsert(ctx context.Context, item *models.VectorItem) error {
	if len(item.Embedding) != s.dimensions {
		return fmt.Errorf("pgvector: embedding has %d dimensions, store configured for %d", len(item.Embedding), s.dimensions)
	}
	meta, _ := json.Marshal(item.Metadata)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO vector_items (tenant_id, namespace, id, content, embedding, metadata, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW(), NOW())
		ON CONFLICT (tenant_id, namespace, id) DO UPDATE SET
			content = EXCLUDED.content, embedding = EXCLUDED.embedding,
			metadata = EXCLUDED.metadata, updated_at = NOW()
	`, tenant.FromContext(ctx), item.Namespace, item.ID, item.Content, pgvec.NewVector(item.Embedding), string(meta))
	return err
}

func (s *Store) Delete(ctx context.Context, namespace, id string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM vector_items WHERE tenant_id = $1 AND namespace = $2 AND id = $3`,
		tenant.FromContext(ctx), namespace, id)
	return err
}

func (s *Store) Search(ctx context.Context, req *models.VectorSearchRequest) ([]*models.VectorSearchResult, error) {
	if len(req.Embedding) != s.dimensions {
		return nil, fmt.Errorf("pgvector: query embedding has %d dimensions, store configured for %d", len(req.Embedding), s.dimensions)
	}
	topK := req.TopK
	if topK <= 0 {
		topK = 10
	}

	query := `
		SELECT tenant_id, namespace, id, content, embedding, metadata, created_at, updated_at,
		       1 - (embedding <=> $1) AS score
		FROM vector_items
		WHERE tenant_id = $2 AND namespace = $3`
	args := []interface{}{pgvec.NewVector(req.Embedding), tenant.FromContext(ctx), req.Namespace}
	argN := 4
	for k, v := range req.Filter {
		query += fmt.Sprintf(" AND metadata->>$%d = $%d", argN, argN+1)
		valJSON, _ := json.Marshal(v)
		if sv, ok := v.(string); ok {
			args = append(args, k, sv)
		} else {
			args = append(args, k, string(valJSON))
		}
		argN += 2
	}
	query += fmt.Sprintf(" ORDER BY embedding <=> $1 LIMIT $%d", argN)
	args = append(args, topK)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Non-nil so a no-results search JSON-encodes to [] rather than null.
	results := []*models.VectorSearchResult{}
	for rows.Next() {
		var item models.VectorItem
		var tenantID string
		var embedding pgvec.Vector
		var metaBytes []byte
		var score float64
		if err := rows.Scan(&tenantID, &item.Namespace, &item.ID, &item.Content, &embedding, &metaBytes, &item.CreatedAt, &item.UpdatedAt, &score); err != nil {
			return nil, err
		}
		item.Embedding = embedding.Slice()
		if metaBytes != nil {
			json.Unmarshal(metaBytes, &item.Metadata)
		}
		results = append(results, &models.VectorSearchResult{Item: &item, Score: score})
	}
	return results, rows.Err()
}
