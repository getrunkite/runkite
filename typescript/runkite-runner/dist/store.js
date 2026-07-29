/**
 * Store Dual Mode for the TypeScript runner. Direct TypeScript mirror of
 * the Python runner's store.py -- see that file's docstring for the full
 * rationale; repeated briefly here:
 *
 * - direct mode (POSTGRES_DSN set): queries the control plane's own
 *   store_items table straight over `pg` -- same schema, same \x1F
 *   namespace encoding as internal/state/postgres/postgres.go. Zero HTTP
 *   hop. Always operates in the "default" tenant (see TENANT_ID below) --
 *   same documented Direct Mode Trust Model trade-off as the Python
 *   runner: direct mode bypasses control-plane tenant scoping entirely.
 * - proxy mode (no POSTGRES_DSN): calls the control plane's /internal/
 *   store/* HTTP API. Works against any backend (SQLite, Postgres).
 *
 * Both modes read/write the exact same rows as the Go control plane and
 * the Python runner -- one store, not three competing systems.
 *
 * BaseStore only requires `batch` to be implemented; every other method
 * (get/put/delete/search/listNamespaces) is provided by langgraph's base
 * class in terms of it (see node_modules/@langchain/langgraph-checkpoint's
 * store/base.d.ts).
 */
import { BaseStore } from "@langchain/langgraph-checkpoint";
import pg from "pg";
export const NS_DELIM = "\x1f";
/** Exported for direct unit testing of the round-trip/boundary-safety
 * properties -- same \x1F-delimited encoding as internal/state/postgres's
 * Go implementation and the Python runner's store.py, so all three must
 * agree on every edge case (empty namespace, segments containing "/",
 * prefix-vs-sibling boundary matching). */
export function nsToString(ns) {
    return NS_DELIM + ns.join(NS_DELIM) + NS_DELIM;
}
export function stringToNs(s) {
    const trimmed = s.replace(new RegExp(`^${NS_DELIM}+|${NS_DELIM}+$`, "g"), "");
    return trimmed === "" ? [] : trimmed.split(NS_DELIM);
}
export function nsPrefixPattern(prefix) {
    if (prefix.length === 0)
        return "%";
    return NS_DELIM + prefix.join(NS_DELIM) + NS_DELIM + "%";
}
function parseTs(value) {
    if (value instanceof Date)
        return value;
    if (typeof value === "string" && value)
        return new Date(value);
    return new Date();
}
function itemFromRow(row) {
    return {
        namespace: stringToNs(row.namespace),
        key: row.key,
        value: (typeof row.value === "string" ? JSON.parse(row.value || "{}") : row.value) ?? {},
        createdAt: parseTs(row.created_at),
        updatedAt: parseTs(row.updated_at),
    };
}
function isGetOp(op) {
    return "namespace" in op && !("value" in op);
}
function isPutOp(op) {
    return "namespace" in op && "value" in op;
}
function isSearchOp(op) {
    return "namespacePrefix" in op;
}
function isListNamespacesOp(op) {
    return !("namespace" in op) && !("namespacePrefix" in op);
}
export class RunkiteStore extends BaseStore {
    mode;
    pool = null;
    baseUrl = null;
    headers = {};
    // Direct mode has no per-request tenant identity to work with (it's a
    // raw DB connection, not an authenticated HTTP call) -- see the module
    // docstring's Direct Mode Trust Model note. Must match
    // internal/tenant.DefaultTenant on the Go side exactly.
    static TENANT_ID = "default";
    constructor(opts) {
        super();
        if (!opts.postgresDsn && !opts.httpBaseUrl) {
            throw new Error("RunkiteStore requires postgresDsn or httpBaseUrl");
        }
        this.mode = opts.postgresDsn ? "direct" : "proxy";
        if (opts.postgresDsn) {
            // poolSize mirrors the Python runner's pool_size=concurrency --
            // see checkpoint.ts's CheckpointerManager.start() for the full
            // rationale. Undefined leaves node-postgres's own default (10).
            this.pool = new pg.Pool({ connectionString: opts.postgresDsn, ...(opts.poolSize ? { max: opts.poolSize } : {}) });
        }
        if (opts.httpBaseUrl) {
            this.baseUrl = opts.httpBaseUrl.replace(/\/+$/, "");
        }
        if (opts.runnerToken) {
            this.headers["X-Runner-Kind"] = "typescript-langgraphjs";
            this.headers["X-Runner-Token"] = opts.runnerToken;
        }
    }
    async close() {
        if (this.pool)
            await this.pool.end();
    }
    async batch(operations) {
        const results = await Promise.all(operations.map((op) => (this.mode === "direct" ? this.directOne(op) : this.proxyOne(op))));
        return results;
    }
    // -- direct mode: pg straight to store_items -----------------------------
    async directOne(op) {
        const pool = this.pool;
        if (isGetOp(op)) {
            const res = await pool.query("SELECT namespace, key, value, created_at, updated_at FROM store_items WHERE tenant_id = $1 AND namespace = $2 AND key = $3", [RunkiteStore.TENANT_ID, nsToString(op.namespace), op.key]);
            return res.rows[0] ? itemFromRow(res.rows[0]) : null;
        }
        if (isPutOp(op)) {
            const ns = nsToString(op.namespace);
            if (op.value === null) {
                await pool.query("DELETE FROM store_items WHERE tenant_id = $1 AND namespace = $2 AND key = $3", [
                    RunkiteStore.TENANT_ID,
                    ns,
                    op.key,
                ]);
            }
            else {
                await pool.query(`INSERT INTO store_items (tenant_id, namespace, key, value, created_at, updated_at)
           VALUES ($1, $2, $3, $4, NOW(), NOW())
           ON CONFLICT (tenant_id, namespace, key)
           DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()`, [RunkiteStore.TENANT_ID, ns, op.key, JSON.stringify(op.value)]);
            }
            return undefined;
        }
        if (isSearchOp(op)) {
            const where = ["tenant_id = $1", "namespace LIKE $2"];
            const args = [RunkiteStore.TENANT_ID, nsPrefixPattern(op.namespacePrefix)];
            let argN = 3;
            for (const [k, v] of Object.entries(op.filter ?? {})) {
                where.push(`value->>$${argN} = $${argN + 1}`);
                args.push(k, typeof v === "string" ? v : JSON.stringify(v));
                argN += 2;
            }
            const limit = op.limit ?? 10;
            const query = `SELECT namespace, key, value, created_at, updated_at FROM store_items ` +
                `WHERE ${where.join(" AND ")} ORDER BY updated_at DESC LIMIT $${argN} OFFSET $${argN + 1}`;
            args.push(limit, op.offset ?? 0);
            const res = await pool.query(query, args);
            return res.rows.map((r) => itemFromRow(r));
        }
        if (isListNamespacesOp(op)) {
            const prefix = op.matchConditions?.find((c) => c.matchType === "prefix")?.path;
            const where = ["tenant_id = $1"];
            const args = [RunkiteStore.TENANT_ID];
            let argN = 2;
            if (prefix) {
                where.push(`namespace LIKE $${argN}`);
                args.push(nsPrefixPattern(prefix));
                argN++;
            }
            const query = `SELECT DISTINCT namespace FROM store_items WHERE ${where.join(" AND ")} ORDER BY namespace LIMIT $${argN} OFFSET $${argN + 1}`;
            args.push(op.limit ?? 100, op.offset ?? 0);
            const res = await pool.query(query, args);
            return res.rows.map((r) => stringToNs(r.namespace));
        }
        throw new TypeError(`unsupported store op: ${JSON.stringify(op)}`);
    }
    // -- proxy mode: HTTP calls to the control plane -------------------------
    async proxyOne(op) {
        // /internal/store/* (not the client-facing /store/*): a runner
        // authenticates with its runner token, not a client API key/JWT it
        // may not have. Same handlers on the Go side, different auth
        // boundary -- see internal/auth/auth.go.
        if (isGetOp(op)) {
            const url = `${this.baseUrl}/internal/store/items?namespace=${encodeURIComponent(op.namespace.join(","))}&key=${encodeURIComponent(op.key)}`;
            const resp = await fetch(url, { headers: this.headers });
            if (resp.status === 404)
                return null;
            if (!resp.ok)
                throw new Error(`GET store item failed: ${resp.status} ${await resp.text()}`);
            return itemFromJson(await resp.json());
        }
        if (isPutOp(op)) {
            if (op.value === null) {
                const resp = await fetch(`${this.baseUrl}/internal/store/items`, {
                    method: "DELETE",
                    headers: { ...this.headers, "Content-Type": "application/json" },
                    body: JSON.stringify({ namespace: op.namespace, key: op.key }),
                });
                if (!resp.ok)
                    throw new Error(`DELETE store item failed: ${resp.status} ${await resp.text()}`);
            }
            else {
                const resp = await fetch(`${this.baseUrl}/internal/store/items`, {
                    method: "PUT",
                    headers: { ...this.headers, "Content-Type": "application/json" },
                    body: JSON.stringify({ namespace: op.namespace, key: op.key, value: op.value }),
                });
                if (!resp.ok)
                    throw new Error(`PUT store item failed: ${resp.status} ${await resp.text()}`);
            }
            return undefined;
        }
        if (isSearchOp(op)) {
            const resp = await fetch(`${this.baseUrl}/internal/store/items/search`, {
                method: "POST",
                headers: { ...this.headers, "Content-Type": "application/json" },
                body: JSON.stringify({
                    namespace_prefix: op.namespacePrefix,
                    filter: op.filter ?? {},
                    limit: op.limit,
                    offset: op.offset,
                }),
            });
            if (!resp.ok)
                throw new Error(`search store items failed: ${resp.status} ${await resp.text()}`);
            const body = (await resp.json());
            return (body.items ?? []).map((i) => itemFromJson(i));
        }
        if (isListNamespacesOp(op)) {
            const prefix = op.matchConditions?.find((c) => c.matchType === "prefix")?.path;
            const suffix = op.matchConditions?.find((c) => c.matchType === "suffix")?.path;
            const resp = await fetch(`${this.baseUrl}/internal/store/namespaces`, {
                method: "POST",
                headers: { ...this.headers, "Content-Type": "application/json" },
                body: JSON.stringify({
                    prefix: prefix ?? null,
                    suffix: suffix ?? null,
                    max_depth: op.maxDepth ?? null,
                    limit: op.limit,
                    offset: op.offset,
                }),
            });
            if (!resp.ok)
                throw new Error(`list namespaces failed: ${resp.status} ${await resp.text()}`);
            const body = (await resp.json());
            return body ?? [];
        }
        throw new TypeError(`unsupported store op: ${JSON.stringify(op)}`);
    }
}
function itemFromJson(d) {
    return {
        namespace: d.namespace ?? [],
        key: d.key,
        value: d.value ?? {},
        createdAt: parseTs(d.created_at),
        updatedAt: parseTs(d.updated_at),
    };
}
