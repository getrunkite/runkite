// Package vectorstore defines the VectorStore interface for semantic
// search over embeddings (master plan: "Vector/semantic store -- None
// (disabled) by default, pgvector/Qdrant/Weaviate/Pinecone in production").
// Disabled by default, same convention as every other opt-in platform
// feature (llm_cache, rate_limit, webhooks, cron) -- enabled explicitly via
// langgraph.json's vector_store block, never implicitly just because
// POSTGRES_DSN happens to be set (an existing Postgres deployment may not
// have the pgvector extension available or permitted).
package vectorstore

import (
	"context"

	"github.com/sharanharsoor/runkite/internal/models"
)

// VectorStore is the persistence + similarity-search interface every
// backend (pgvector today; Qdrant/Weaviate/Pinecone are Tier 2, not yet
// built) implements.
type VectorStore interface {
	// Upsert embeds/re-embeds an item under (namespace, id). Overwrites
	// content/embedding/metadata on a repeat call with the same id --
	// re-indexing a changed document is the common case, not a conflict.
	Upsert(ctx context.Context, item *models.VectorItem) error

	// Delete removes an item. No error if it never existed -- same
	// idempotent-delete convention as the key-value store.
	Delete(ctx context.Context, namespace, id string) error

	// Search returns the topK nearest neighbors to embedding by cosine
	// similarity, ordered best-first, optionally narrowed by an
	// exact-match Metadata filter.
	Search(ctx context.Context, req *models.VectorSearchRequest) ([]*models.VectorSearchResult, error)

	// Init creates the schema/extension/indexes needed. Called once at
	// control-plane startup, only when vector_store is configured.
	Init(ctx context.Context) error
	Close() error
}
