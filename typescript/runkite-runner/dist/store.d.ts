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
import type { Operation, OperationResults } from "@langchain/langgraph-checkpoint";
export declare const NS_DELIM = "\u001F";
/** Exported for direct unit testing of the round-trip/boundary-safety
 * properties -- same \x1F-delimited encoding as internal/state/postgres's
 * Go implementation and the Python runner's store.py, so all three must
 * agree on every edge case (empty namespace, segments containing "/",
 * prefix-vs-sibling boundary matching). */
export declare function nsToString(ns: readonly string[]): string;
export declare function stringToNs(s: string): string[];
export declare function nsPrefixPattern(prefix: readonly string[]): string;
export type StoreMode = "direct" | "proxy";
export declare class RunkiteStore extends BaseStore {
    mode: StoreMode;
    private pool;
    private baseUrl;
    private headers;
    private static readonly TENANT_ID;
    constructor(opts: {
        postgresDsn?: string;
        httpBaseUrl?: string;
        runnerToken?: string;
    });
    close(): Promise<void>;
    batch<Op extends Operation[]>(operations: Op): Promise<OperationResults<Op>>;
    private directOne;
    private proxyOne;
}
