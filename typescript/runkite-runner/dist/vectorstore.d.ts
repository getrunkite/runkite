/**
 * Vector Store client for the TypeScript runner. Implements LangChain.js's
 * `VectorStore` abstract class (`addVectors`/`addDocuments`/
 * `similaritySearchVectorWithScore`/`delete`) so it drops into existing
 * LangChain.js RAG code unchanged -- same role Python's
 * RunkiteVectorStore (vectorstore.py) plays for LangChain (Python).
 *
 * Proxy mode ONLY, deliberately -- unlike the Python client, which also
 * supports a direct-psycopg mode for same-process pgvector access. Two
 * reasons this port doesn't carry that mode over:
 *
 *  1. Direct mode is only ever correct when the control plane's vector
 *     store is actually pgvector (it queries `vector_items` with raw
 *     SQL) -- it silently writes to a table the control plane never
 *     reads if the CP is actually on Qdrant. Proxy mode is correct
 *     against EVERY VectorStore backend the control plane supports,
 *     because the control plane owns the backend choice, not the
 *     runner. That's true for Python too, which is why Python's own
 *     module docstring says "proxy wins when both are set" -- this
 *     port just always takes the branch Python treats as the safe
 *     default, without also offering the narrower one.
 *  2. RunkiteStore (store.ts)'s existing direct mode uses `pg` (the
 *     `node-postgres` package) for the key-value store; adding a
 *     second, vector-specific raw-SQL path (pgvector's `<=>` operator,
 *     `::vector` casts) for a mode that's a narrower special case even
 *     in Python isn't worth the surface area for this port. A future
 *     direct mode could reuse the same `pg.Pool`, but it's additive,
 *     not required for parity with the vector_agent example this
 *     mirrors (which itself only ever uses proxy mode -- see
 *     examples/vector_agent/graph.py).
 *
 * Namespace here is a single flat string (e.g. "docs", "faq"), NOT the
 * key-value store's \x1F-delimited hierarchical segments -- vector
 * collections are conventionally flat (an index/collection per
 * embedding space), same convention models.VectorItem uses on the Go
 * side.
 */
import { VectorStore } from "@langchain/core/vectorstores";
import { type DocumentInterface } from "@langchain/core/documents";
import type { EmbeddingsInterface } from "@langchain/core/embeddings";
export interface RunkiteVectorStoreOptions {
    namespace: string;
    httpBaseUrl: string;
    runnerToken?: string;
}
export declare class RunkiteVectorStore extends VectorStore {
    private readonly namespace;
    private readonly baseUrl;
    private readonly headers;
    private readonly dispatcher;
    constructor(embeddings: EmbeddingsInterface, opts: RunkiteVectorStoreOptions);
    _vectorstoreType(): string;
    addVectors(vectors: number[][], documents: DocumentInterface[], options?: {
        ids?: string[];
    }): Promise<string[]>;
    addDocuments(documents: DocumentInterface[], options?: {
        ids?: string[];
    }): Promise<string[]>;
    similaritySearchVectorWithScore(query: number[], k: number, filter?: Record<string, unknown>): Promise<[DocumentInterface, number][]>;
    /** LangChain's base `delete(_params?)` is a documented no-op unless a
     * subclass overrides it -- this one deletes by id, same as Python's
     * `adelete(ids=...)`. Silently does nothing if `ids` is absent/empty,
     * matching Python's `if not ids: return None`. */
    delete(params?: {
        ids?: string[];
    }): Promise<void>;
    private upsert;
    private deleteOne;
    private search;
    /** Matches LangChain.js's static `fromTexts`/`fromDocuments` convention
     * (Python's `from_texts` classmethod equivalent) -- builds a store,
     * embeds + adds the given texts, and returns it ready to query. */
    static fromTexts(texts: string[], metadatas: Record<string, unknown>[] | Record<string, unknown>, embeddings: EmbeddingsInterface, dbConfig: RunkiteVectorStoreOptions & {
        ids?: string[];
    }): Promise<RunkiteVectorStore>;
    static fromDocuments(docs: DocumentInterface[], embeddings: EmbeddingsInterface, dbConfig: RunkiteVectorStoreOptions & {
        ids?: string[];
    }): Promise<RunkiteVectorStore>;
}
