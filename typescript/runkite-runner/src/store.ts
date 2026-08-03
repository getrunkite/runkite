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
import type {
  GetOperation,
  Item,
  ListNamespacesOperation,
  Operation,
  OperationResults,
  PutOperation,
  SearchItem,
  SearchOperation,
} from "@langchain/langgraph-checkpoint";
import pg from "pg";
import { logger } from "./logger.js";
import { httpDispatcher, type FetchInit } from "./tls.js";

export const NS_DELIM = "\x1f";

/** Exported for direct unit testing of the round-trip/boundary-safety
 * properties -- same \x1F-delimited encoding as internal/state/postgres's
 * Go implementation and the Python runner's store.py, so all three must
 * agree on every edge case (empty namespace, segments containing "/",
 * prefix-vs-sibling boundary matching). */
export function nsToString(ns: readonly string[]): string {
  return NS_DELIM + ns.join(NS_DELIM) + NS_DELIM;
}

export function stringToNs(s: string): string[] {
  const trimmed = s.replace(new RegExp(`^${NS_DELIM}+|${NS_DELIM}+$`, "g"), "");
  return trimmed === "" ? [] : trimmed.split(NS_DELIM);
}

export function nsPrefixPattern(prefix: readonly string[]): string {
  if (prefix.length === 0) return "%";
  return NS_DELIM + prefix.join(NS_DELIM) + NS_DELIM + "%";
}

function parseTs(value: unknown): Date {
  if (value instanceof Date) return value;
  if (typeof value === "string" && value) return new Date(value);
  return new Date();
}

function itemFromRow(row: {
  namespace: string;
  key: string;
  value: unknown;
  created_at: unknown;
  updated_at: unknown;
}): Item {
  return {
    namespace: stringToNs(row.namespace),
    key: row.key,
    value: (typeof row.value === "string" ? JSON.parse(row.value || "{}") : row.value) ?? {},
    createdAt: parseTs(row.created_at),
    updatedAt: parseTs(row.updated_at),
  };
}

function isGetOp(op: Operation): op is GetOperation {
  return "namespace" in op && !("value" in op);
}
function isPutOp(op: Operation): op is PutOperation {
  return "namespace" in op && "value" in op;
}
function isSearchOp(op: Operation): op is SearchOperation {
  return "namespacePrefix" in op;
}
function isListNamespacesOp(op: Operation): op is ListNamespacesOperation {
  return !("namespace" in op) && !("namespacePrefix" in op);
}

export type StoreMode = "direct" | "proxy";

export class RunkiteStore extends BaseStore {
  mode: StoreMode;
  private pool: pg.Pool | null = null;
  private baseUrl: string | null = null;
  private headers: Record<string, string> = {};
  // Constructed once (not per-call): mirrors tls.ts's own doc comment --
  // undefined when RUNKITE_TLS_CA_FILE is unset, meaning "no dispatcher
  // option," which is fetch's own default behavior (plain http:// or
  // https:// with a publicly-trusted cert).
  private readonly dispatcher = httpDispatcher();

  // Direct mode has no per-request tenant identity to work with (it's a
  // raw DB connection, not an authenticated HTTP call) -- see the module
  // docstring's Direct Mode Trust Model note. Must match
  // internal/tenant.DefaultTenant on the Go side exactly.
  private static readonly TENANT_ID = "default";

  constructor(opts: { postgresDsn?: string; httpBaseUrl?: string; runnerToken?: string; poolSize?: number }) {
    super();
    if (!opts.postgresDsn && !opts.httpBaseUrl) {
      throw new Error("RunkiteStore requires postgresDsn or httpBaseUrl");
    }
    this.mode = opts.postgresDsn ? "direct" : "proxy";
    if (opts.postgresDsn) {
      // poolSize mirrors the Python runner's pool_size=concurrency --
      // see checkpoint.ts's CheckpointerManager.start() for the full
      // rationale. Undefined leaves node-postgres's own default (10).
      // See checkpoint.ts for idle/connect timeout rationale.
      this.pool = new pg.Pool({
        connectionString: opts.postgresDsn,
        idleTimeoutMillis: 60_000,
        connectionTimeoutMillis: 10_000,
        ...(opts.poolSize ? { max: opts.poolSize } : {}),
      });
      this.pool.on("error", (err) => {
        // Idle clients can error after server-side close; log and let
        // the pool drop them rather than crashing the process.
        logger.error(`store pool idle client error: ${err.message}`);
      });
    }
    if (opts.httpBaseUrl) {
      this.baseUrl = opts.httpBaseUrl.replace(/\/+$/, "");
    }
    if (opts.runnerToken) {
      this.headers["X-Runner-Kind"] = "typescript-langgraphjs";
      this.headers["X-Runner-Token"] = opts.runnerToken;
    }
  }

  async close(): Promise<void> {
    if (this.pool) await this.pool.end();
  }

  async batch<Op extends Operation[]>(operations: Op): Promise<OperationResults<Op>> {
    const results = await Promise.all(
      operations.map((op) => (this.mode === "direct" ? this.directOne(op) : this.proxyOne(op))),
    );
    return results as OperationResults<Op>;
  }

  // -- direct mode: pg straight to store_items -----------------------------

  private async directOne(op: Operation): Promise<unknown> {
    const pool = this.pool!;
    if (isGetOp(op)) {
      // Hide expired rows the same way Python/Go stores do.
      const res = await pool.query(
        `SELECT namespace, key, value, created_at, updated_at FROM store_items
         WHERE tenant_id = $1 AND namespace = $2 AND key = $3
           AND (expires_at IS NULL OR expires_at > NOW())`,
        [RunkiteStore.TENANT_ID, nsToString(op.namespace), op.key],
      );
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
      } else {
        // LangGraph JS PutOperation typings omit ttl today; accept it when
        // present (parity with Python runner / Go store_items columns).
        const ttlMinutes = (op as PutOperation & { ttl?: number }).ttl;
        const expiresAt =
          ttlMinutes == null ? null : new Date(Date.now() + ttlMinutes * 60_000);
        await pool.query(
          `INSERT INTO store_items (tenant_id, namespace, key, value, created_at, updated_at, ttl_minutes, expires_at)
           VALUES ($1, $2, $3, $4, NOW(), NOW(), $5, $6)
           ON CONFLICT (tenant_id, namespace, key)
           DO UPDATE SET value = EXCLUDED.value, updated_at = NOW(),
             ttl_minutes = EXCLUDED.ttl_minutes, expires_at = EXCLUDED.expires_at`,
          [RunkiteStore.TENANT_ID, ns, op.key, JSON.stringify(op.value), ttlMinutes ?? null, expiresAt],
        );
      }
      return undefined;
    }

    if (isSearchOp(op)) {
      const where = ["tenant_id = $1", "namespace LIKE $2", "(expires_at IS NULL OR expires_at > NOW())"];
      const args: unknown[] = [RunkiteStore.TENANT_ID, nsPrefixPattern(op.namespacePrefix)];
      let argN = 3;
      for (const [k, v] of Object.entries(op.filter ?? {})) {
        where.push(`value->>$${argN} = $${argN + 1}`);
        args.push(k, typeof v === "string" ? v : JSON.stringify(v));
        argN += 2;
      }
      const limit = op.limit ?? 10;
      const query =
        `SELECT namespace, key, value, created_at, updated_at FROM store_items ` +
        `WHERE ${where.join(" AND ")} ORDER BY updated_at DESC LIMIT $${argN} OFFSET $${argN + 1}`;
      args.push(limit, op.offset ?? 0);
      const res = await pool.query(query, args);
      return res.rows.map((r) => itemFromRow(r) as SearchItem);
    }

    if (isListNamespacesOp(op)) {
      const prefix = op.matchConditions?.find((c) => c.matchType === "prefix")?.path as string[] | undefined;
      const where = ["tenant_id = $1"];
      const args: unknown[] = [RunkiteStore.TENANT_ID];
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

  private async proxyOne(op: Operation): Promise<unknown> {
    // /internal/store/* (not the client-facing /store/*): a runner
    // authenticates with its runner token, not a client API key/JWT it
    // may not have. Same handlers on the Go side, different auth
    // boundary -- see internal/auth/auth.go.
    if (isGetOp(op)) {
      const url = `${this.baseUrl}/internal/store/items?namespace=${encodeURIComponent(op.namespace.join(","))}&key=${encodeURIComponent(op.key)}`;
      const opts: FetchInit = { headers: this.headers, dispatcher: this.dispatcher };
      const resp = await fetch(url, opts);
      if (resp.status === 404) return null;
      if (!resp.ok) throw new Error(`GET store item failed: ${resp.status} ${await resp.text()}`);
      return itemFromJson(await resp.json());
    }

    if (isPutOp(op)) {
      if (op.value === null) {
        const opts: FetchInit = {
          method: "DELETE",
          headers: { ...this.headers, "Content-Type": "application/json" },
          body: JSON.stringify({ namespace: op.namespace, key: op.key }),
          dispatcher: this.dispatcher,
        };
        const resp = await fetch(`${this.baseUrl}/internal/store/items`, opts);
        if (!resp.ok) throw new Error(`DELETE store item failed: ${resp.status} ${await resp.text()}`);
      } else {
        // LangGraph JS PutOperation typings omit ttl; forward when present
        // (matches Python proxy + Go StorePutRequest.ttl_minutes).
        const ttlMinutes = (op as PutOperation & { ttl?: number }).ttl;
        const body: Record<string, unknown> = {
          namespace: op.namespace,
          key: op.key,
          value: op.value,
        };
        if (ttlMinutes != null) body.ttl_minutes = ttlMinutes;
        const opts: FetchInit = {
          method: "PUT",
          headers: { ...this.headers, "Content-Type": "application/json" },
          body: JSON.stringify(body),
          dispatcher: this.dispatcher,
        };
        const resp = await fetch(`${this.baseUrl}/internal/store/items`, opts);
        if (!resp.ok) throw new Error(`PUT store item failed: ${resp.status} ${await resp.text()}`);
      }
      return undefined;
    }

    if (isSearchOp(op)) {
      const opts: FetchInit = {
        method: "POST",
        headers: { ...this.headers, "Content-Type": "application/json" },
        body: JSON.stringify({
          namespace_prefix: op.namespacePrefix,
          filter: op.filter ?? {},
          limit: op.limit,
          offset: op.offset,
        }),
        dispatcher: this.dispatcher,
      };
      const resp = await fetch(`${this.baseUrl}/internal/store/items/search`, opts);
      if (!resp.ok) throw new Error(`search store items failed: ${resp.status} ${await resp.text()}`);
      const body = (await resp.json()) as { items?: unknown[] };
      return (body.items ?? []).map((i) => itemFromJson(i) as SearchItem);
    }

    if (isListNamespacesOp(op)) {
      const prefix = op.matchConditions?.find((c) => c.matchType === "prefix")?.path as string[] | undefined;
      const suffix = op.matchConditions?.find((c) => c.matchType === "suffix")?.path as string[] | undefined;
      const opts: FetchInit = {
        method: "POST",
        headers: { ...this.headers, "Content-Type": "application/json" },
        body: JSON.stringify({
          prefix: prefix ?? null,
          suffix: suffix ?? null,
          max_depth: op.maxDepth ?? null,
          limit: op.limit,
          offset: op.offset,
        }),
        dispatcher: this.dispatcher,
      };
      const resp = await fetch(`${this.baseUrl}/internal/store/namespaces`, opts);
      if (!resp.ok) throw new Error(`list namespaces failed: ${resp.status} ${await resp.text()}`);
      const body = (await resp.json()) as string[][] | null;
      return body ?? [];
    }

    throw new TypeError(`unsupported store op: ${JSON.stringify(op)}`);
  }
}

function itemFromJson(d: any): Item {
  return {
    namespace: d.namespace ?? [],
    key: d.key,
    value: d.value ?? {},
    createdAt: parseTs(d.created_at),
    updatedAt: parseTs(d.updated_at),
  };
}
