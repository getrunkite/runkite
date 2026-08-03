# Vector Store

> Deep dive moved from the root README. For a 60-second overview see the [root README](../README.md).

Semantic search over embeddings, backed by **pgvector** (**Supported** when the state backend is Postgres -- see [Backend support tiers](architecture.md#backend-support-tiers)), **Qdrant**, **Weaviate**, or **Pinecone** (**Compatible** non-SQL exemplars, same role Mongo plays for the state store -- proof the `VectorStore` interface is implementable against a real standalone vector database, not just a Postgres extension). Disabled entirely by default, same opt-in convention as `llm_cache`/`rate_limit`/`webhooks`/`cron` -- never implicitly enabled just because `POSTGRES_DSN` is set, since an existing Postgres deployment may not have the pgvector extension installed or permitted.

```json
{
  "vector_store": {
    "type": "pgvector",
    "dimensions": 1536
  }
}
```

```json
{
  "vector_store": {
    "type": "qdrant",
    "url": "http://localhost:6333",
    "collection": "vector_items",
    "dimensions": 1536
  }
}
```

```json
{
  "vector_store": {
    "type": "weaviate",
    "url": "http://localhost:8080",
    "class": "VectorItems",
    "dimensions": 1536
  }
}
```

```json
{
  "vector_store": {
    "type": "pinecone",
    "index": "vector-items",
    "dimensions": 1536
  }
}
```

`dimensions` fixes the embedding vector's width at creation time (pgvector's `vector(N)` column / Qdrant's collection vector size / Pinecone's index dimension are all fixed-dimension; Weaviate has no such native constraint, so that backend's width is enforced by Runkite's own code on every Upsert/Search instead) -- defaults to 1536 (a common text-embedding width) when omitted. `pgvector` requires `POSTGRES_DSN` to be set on the control plane; the extension itself (`CREATE EXTENSION IF NOT EXISTS vector`) is created automatically on startup, but the Postgres server must have the pgvector extension binary available -- the `pgvector/pgvector:pg16` image (used by this repo's `docker-compose.yml`/`docker-compose.test.yml`) ships it; a bare `postgres:16` image does not. `qdrant` requires `vector_store.url` or `QDRANT_URL` -- one Qdrant collection holds every tenant/namespace (`tenant_id`/`namespace` are stored as payload fields and included in every filter, not one collection per tenant), and a caller's `(tenant_id, namespace, id)` is mapped to a deterministic UUID v5 for Qdrant's point ID (which must be an integer or UUID, never an arbitrary string). `weaviate` requires `vector_store.url` or `WEAVIATE_URL` -- one Weaviate class holds every tenant/namespace, the same shared-collection convention as Qdrant's, and object IDs are derived the same UUID v5 way; unlike Qdrant's freeform JSON payload, Weaviate requires every property's type to be declared up front, so arbitrary caller metadata is stored as a single JSON-encoded `metadata_json` property rather than one Weaviate property per key.

`pinecone` defaults `vector_store.url` to Pinecone's own fixed control-plane host (`https://api.pinecone.io`) if left unset -- only set `url`/`PINECONE_URL` explicitly to point at a self-hosted [Pinecone Local](https://docs.pinecone.io/guides/operations/local-development) instance for development/testing (this repo's own `docker-compose.test.yml` does exactly that). A real Pinecone account needs `vector_store.api_key`/`PINECONE_API_KEY` set; Pinecone Local ignores whatever key is sent entirely. Unlike Qdrant/Weaviate, no derived UUID or JSON-encoding workaround is needed here: Pinecone vector IDs accept arbitrary strings and its metadata is natively a freeform, directly-queryable object -- a caller's Namespace maps straight onto Pinecone's own first-class namespace concept (not onto a payload/property field the way Qdrant/Weaviate reuse it), while `tenant_id` still goes into metadata and every query's filter, since there's no second Pinecone-native partitioning dimension to spend on it. Search's `filter` is pushed directly into Pinecone's own native `$eq` filter alongside the tenant scope -- no over-fetch-then-filter-in-Go workaround needed the way Weaviate's JSON-blob metadata requires.

**API**: `PUT /vectors/items` (upsert -- overwrites on a repeat `id`, re-embedding a changed document is the common case, not a conflict), `DELETE /vectors/items`, `POST /vectors/search` (top-K cosine similarity, optional exact-match `filter` over metadata) -- identical regardless of which backend is configured. Same dual-mode convention as the key-value store: mirrored under `/internal/vectors/*` for a runner's proxy-mode client. 501s (not 404s) when `vector_store` isn't configured -- "this feature isn't turned on" is a more actionable signal than "this route doesn't exist" for something opt-in.

**Python SDK**: `RunkiteVectorStore` (`python/runkite_runner/vectorstore.py`) implements LangChain's `VectorStore` interface (`add_texts`, `similarity_search`, `similarity_search_with_score`, `from_texts`), so it drops into existing LangChain/LangGraph RAG code unchanged. Prefer proxy mode (`http_base_url` / `RUNKITE_HTTP_URL`) -- it talks to the control plane's `/vectors/*` API and works for every backend (pgvector, Qdrant, …). Direct mode (`postgres_dsn` only, no HTTP URL) queries `vector_items` over `psycopg` and is correct **only** when the control plane is on pgvector; when both DSN and HTTP URL are provided, proxy wins so a runner with `POSTGRES_DSN` set for checkpoints doesn't silently write vectors to Postgres while the CP is on Qdrant. See `examples/vector_agent/` for a working retrieval demo (fake, deterministic embeddings -- no API key needed; always uses proxy).

**TypeScript SDK**: `RunkiteVectorStore` (`typescript/runkite-runner/src/vectorstore.ts`) implements LangChain.js's `VectorStore` abstract class (`addDocuments`/`addVectors`/`similaritySearchVectorWithScore`/`delete`/`fromTexts`/`fromDocuments`) -- same role, **proxy mode only**, deliberately, not a port-in-progress omission: direct mode is only ever correct when the control plane's vector store happens to be pgvector specifically, and proxy mode is correct against every backend the control plane supports because the control plane -- not the runner -- owns that choice. Same reasoning Python's own module docstring gives for why proxy wins whenever both are configured there; this port just always takes the branch Python treats as the safe default.

```typescript
import { RunkiteVectorStore } from "runkite-runner";

const store = new RunkiteVectorStore(embeddings, {
  namespace: "docs",
  httpBaseUrl: process.env.RUNKITE_HTTP_URL ?? "http://localhost:2026",
  runnerToken: process.env.RUNNER_TOKEN,
});
await store.addDocuments([{ pageContent: "...", metadata: {} }]);
const results = await store.similaritySearchWithScore("query text", 4);
```

**Schema**: pgvector applies numbered Up/Down migrations tracked in `vector_schema_migrations` (separate from the state store's `schema_migrations` so the two version streams never collide on a shared Postgres). `serve`/`dev` run them via `Init`; offline: `runkite vector upgrade` / `vector downgrade`. Qdrant, Weaviate, and Pinecone keep create-if-missing `Init` (no evolvable DDL trail); `vector downgrade` refuses those backends.

**Known limitations, stated plainly**:
- **Dimension is fixed at first creation, not migrated**, for all four backends. Changing `vector_store.dimensions` after the table/collection/class/index already exists does not migrate existing rows -- `Upsert` starts failing with a clear dimension-mismatch error (not silent corruption) until it's manually dropped or recreated at the new width.
- **Cosine similarity only.** All four backends support other distance metrics (pgvector: L2, inner-product; Qdrant: Euclidean, dot product; Weaviate: dot product, L2-squared, hamming, manhattan; Pinecone: Euclidean, dot product); only cosine is wired up today, the most common choice for text embeddings.
- **Direct mode is pgvector-only, and Python-only.** There is no runner-side Qdrant, Weaviate, or Pinecone client in either language, and the TypeScript client has no direct-pgvector mode at all (proxy-only by design -- see above), so a non-pgvector-backed deployment or any TypeScript runner always goes through the control plane's HTTP API. Functionally identical to Python's own proxy mode, just always one network hop instead of sometimes zero.
- **Qdrant's, Weaviate's, and Pinecone's `created_at` reset on re-index.** None has a built-in insert-vs-update distinction the way Postgres's `ON CONFLICT DO UPDATE` does, so pinning down "first write time" would need a read before every write (Qdrant, Pinecone), or Weaviate's own upsert-via-delete-then-create (see the package's own doc comment for why: Weaviate's PUT replaces an existing object only, and POST fails if one already exists -- neither alone is a true upsert; Pinecone's own upsert genuinely does overwrite the whole record atomically, but still with no partial-update path that would let created_at survive a resupplied value). `created_at` is set to "now" on every `Upsert` call, including re-indexing an already-existing `id` -- correct for a fresh item, but re-indexing an existing document resets rather than preserves its original `created_at`.
- **Weaviate's metadata `Filter` isn't pushed down into a native query.** Weaviate requires every property's type to be declared up front, so arbitrary Metadata is stored as a single JSON-encoded string property rather than one property per key (see above) -- Weaviate can't filter *inside* that JSON blob. Search instead pages through the tenant/namespace-scoped candidate set in the backend's own similarity order (via GraphQL `offset`), applying the exact-match filter in Go to each page, until `top_k` matches are found or the corpus is exhausted -- exact, not a fixed-size over-fetch window. An earlier implementation used a single fixed window (`top_k * 20`, capped at 500) instead of paging, which is confirmed live to silently return an empty result when a filter's only match sits outside that window (many closer, non-matching vectors + one distant matching one) -- fixed, with a permanent regression test reproducing exactly that shape. The one real remaining bound is Weaviate's own server-side `QUERY_MAXIMUM_RESULTS` default (10,000 combined offset+limit) -- paging that far without a match means Search is honestly no longer exact, a Weaviate-imposed ceiling on offset-based pagination itself, not something this package can page around. Less index-efficient than a native filter for a namespace where the filter matches rarely across a huge corpus (each rare match costs a full page scan). Pinecone doesn't share this limitation -- its metadata is natively freeform and queryable, so its `Filter` is pushed straight into Pinecone's own `$eq` filter.
- **Pinecone's tests never delete their throwaway indexes.** Confirmed live against Pinecone Local that its index deletion is asynchronous (`DELETE` returns `202 Accepted`, not `200`/`204`), and a newly created index can be assigned the exact same host:port a very recently deleted one had before that old index's data is actually cleared -- even after polling `GET` until it 404s, since the index name disappearing from the registry and the underlying port's data actually being torn down turned out to be two separate, unsynchronized events. Rather than fight that race, `internal/vectorstore/pinecone`'s own tests simply never delete what they create -- harmless for Pinecone Local specifically (in-memory, discarded on container teardown), same "a little sprawl instead of any risk of cross-test leakage" trade-off Qdrant's/Weaviate's own tests already accept for their collections/classes.
