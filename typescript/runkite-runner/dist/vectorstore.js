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
import { Document } from "@langchain/core/documents";
import { httpDispatcher } from "./tls.js";
function itemToDocument(item) {
    return new Document({ pageContent: item.content ?? "", metadata: item.metadata ?? {}, id: item.id });
}
export class RunkiteVectorStore extends VectorStore {
    namespace;
    baseUrl;
    headers;
    // See store.ts's identical field for the full rationale.
    dispatcher = httpDispatcher();
    constructor(embeddings, opts) {
        super(embeddings, opts);
        if (!opts.httpBaseUrl) {
            throw new Error("RunkiteVectorStore requires httpBaseUrl (proxy mode only -- see module doc comment)");
        }
        this.namespace = opts.namespace;
        this.baseUrl = opts.httpBaseUrl.replace(/\/+$/, "");
        this.headers = { "Content-Type": "application/json" };
        if (opts.runnerToken) {
            this.headers["X-Runner-Kind"] = "typescript-langgraphjs";
            this.headers["X-Runner-Token"] = opts.runnerToken;
        }
    }
    _vectorstoreType() {
        return "runkite";
    }
    async addVectors(vectors, documents, options) {
        if (vectors.length !== documents.length) {
            throw new Error(`addVectors: vectors.length (${vectors.length}) must equal documents.length (${documents.length})`);
        }
        // Fail loudly on a length mismatch, matching Python's
        // zip(ids, texts, metadatas, vectors, strict=True) -- a silently
        // shorter options.ids would otherwise leave later documents with an
        // undefined id, which only surfaces downstream as an opaque control
        // plane 400 rather than a clear error at the call site that caused it.
        if (options?.ids && options.ids.length !== documents.length) {
            throw new Error(`addVectors: options.ids.length (${options.ids.length}) must equal documents.length (${documents.length})`);
        }
        const ids = options?.ids ?? documents.map((d) => d.id ?? crypto.randomUUID());
        await Promise.all(documents.map((doc, i) => this.upsert(ids[i], doc.pageContent, doc.metadata ?? {}, vectors[i])));
        return ids;
    }
    async addDocuments(documents, options) {
        const texts = documents.map((d) => d.pageContent);
        const vectors = await this.embeddings.embedDocuments(texts);
        return this.addVectors(vectors, documents, options);
    }
    async similaritySearchVectorWithScore(query, k, filter) {
        const results = await this.search(query, k, filter);
        return results.map((r) => [itemToDocument(r.item), r.score]);
    }
    /** LangChain's base `delete(_params?)` is a documented no-op unless a
     * subclass overrides it -- this one deletes by id, same as Python's
     * `adelete(ids=...)`. Silently does nothing if `ids` is absent/empty,
     * matching Python's `if not ids: return None`. */
    async delete(params) {
        const ids = params?.ids;
        if (!ids || ids.length === 0)
            return;
        await Promise.all(ids.map((id) => this.deleteOne(id)));
    }
    // -- HTTP calls to the control plane's /internal/vectors/* API ----------
    // Not the client-facing /vectors/*: a runner authenticates with its
    // runner token, not a client API key/JWT it may not have. Same
    // handlers on the Go side, different auth boundary -- see
    // internal/auth/auth.go and store.ts's identical convention.
    async upsert(id, content, metadata, embedding) {
        const opts = {
            method: "PUT",
            headers: this.headers,
            body: JSON.stringify({ namespace: this.namespace, id, content, metadata, embedding }),
            dispatcher: this.dispatcher,
        };
        const resp = await fetch(`${this.baseUrl}/internal/vectors/items`, opts);
        if (!resp.ok)
            throw new Error(`upsert vector item failed: ${resp.status} ${await resp.text()}`);
    }
    async deleteOne(id) {
        const opts = {
            method: "DELETE",
            headers: this.headers,
            body: JSON.stringify({ namespace: this.namespace, id }),
            dispatcher: this.dispatcher,
        };
        const resp = await fetch(`${this.baseUrl}/internal/vectors/items`, opts);
        if (!resp.ok)
            throw new Error(`delete vector item failed: ${resp.status} ${await resp.text()}`);
    }
    async search(embedding, topK, filter) {
        const opts = {
            method: "POST",
            headers: this.headers,
            body: JSON.stringify({ namespace: this.namespace, embedding, top_k: topK, filter: filter ?? {} }),
            dispatcher: this.dispatcher,
        };
        const resp = await fetch(`${this.baseUrl}/internal/vectors/search`, opts);
        if (!resp.ok)
            throw new Error(`search vectors failed: ${resp.status} ${await resp.text()}`);
        const body = (await resp.json());
        return body.results ?? [];
    }
    /** Matches LangChain.js's static `fromTexts`/`fromDocuments` convention
     * (Python's `from_texts` classmethod equivalent) -- builds a store,
     * embeds + adds the given texts, and returns it ready to query. */
    static async fromTexts(texts, metadatas, embeddings, dbConfig) {
        const docs = texts.map((text, i) => new Document({
            pageContent: text,
            metadata: Array.isArray(metadatas) ? metadatas[i] : metadatas,
            id: dbConfig.ids?.[i],
        }));
        return RunkiteVectorStore.fromDocuments(docs, embeddings, dbConfig);
    }
    static async fromDocuments(docs, embeddings, dbConfig) {
        const store = new RunkiteVectorStore(embeddings, dbConfig);
        await store.addDocuments(docs, { ids: dbConfig.ids });
        return store;
    }
}
