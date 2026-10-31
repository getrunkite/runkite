/**
 * Per-run tenant + run-binding for direct-mode store SQL and proxy headers.
 * Mirrors python/runkite_runner/tenant_ctx.py -- see that module's docstring.
 *
 * Also owns the LangGraph checkpointer thread-id encoding (see
 * checkpointThreadId) so PostgresSaver rows do not collide across tenants.
 */
import { AsyncLocalStorage } from "node:async_hooks";
const DEFAULT_TENANT = "default";
const als = new AsyncLocalStorage();
/** Header runners send on /internal/* (matches internal/auth.HeaderTenantID). */
export const HEADER_TENANT_ID = "X-Runkite-Tenant-Id";
export const HEADER_TENANT_ID_B64 = "X-Runkite-Tenant-Id-B64";
export const HEADER_RUN_ID = "X-Runkite-Run-Id";
export const HEADER_RUN_ID_B64 = "X-Runkite-Run-Id-B64";
export const HEADER_GENERATION = "X-Runkite-Generation";
export function currentTenant() {
    return als.getStore()?.tenantId || DEFAULT_TENANT;
}
export function runWithTenant(tenantId, fn) {
    const tid = tenantId || DEFAULT_TENANT;
    const prev = als.getStore();
    return als.run({
        ...prev,
        tenantId: tid,
        runId: prev?.runId ?? "",
        generation: prev?.generation ?? 0,
    }, fn);
}
/** Bind tenant + run id/generation for the duration of fn (proxy headers). */
export function runWithBinding(opts, fn) {
    const tid = opts.tenantId || DEFAULT_TENANT;
    const runId = opts.runId || "";
    return als.run({
        tenantId: tid,
        runId,
        generation: opts.generation ?? 0,
        runnerKind: opts.runnerKind,
        runnerToken: opts.runnerToken,
    }, fn);
}
export function tenantHeaders() {
    const store = als.getStore();
    const headers = tenantIdHeaders(store?.tenantId || DEFAULT_TENANT);
    if (store?.runId) {
        Object.assign(headers, runIdHeaders(store.runId));
        headers[HEADER_GENERATION] = String(store.generation ?? 0);
    }
    return headers;
}
/** HTTP header parsing must not trim or reinterpret an authoritative run ID. */
export function runIdHeaders(runId) {
    return identityHeaders(runId, HEADER_RUN_ID, HEADER_RUN_ID_B64);
}
export function tenantIdHeaders(tenantId) {
    return identityHeaders(tenantId || DEFAULT_TENANT, HEADER_TENANT_ID, HEADER_TENANT_ID_B64);
}
function identityHeaders(value, rawHeader, encodedHeader) {
    for (const character of value) {
        const code = character.codePointAt(0);
        if (code >= 0xd800 && code <= 0xdfff)
            throw new Error("identity contains invalid Unicode");
    }
    if (value === value.trim() && /^[\x20-\x7e]*$/.test(value))
        return { [rawHeader]: value };
    return { [encodedHeader]: Buffer.from(value, "utf8").toString("base64url") };
}
/** Shared kind-scoped authentication for helpers called inside a worker. */
export function runnerHeaders(runnerToken, runnerKind) {
    const binding = als.getStore();
    const token = runnerToken ?? binding?.runnerToken ?? process.env.RUNNER_TOKEN;
    return {
        ...tenantHeaders(),
        ...(token
            ? { "X-Runner-Kind": runnerKind ?? binding?.runnerKind ?? "typescript-langgraphjs", "X-Runner-Token": token }
            : {}),
    };
}
/** LangGraph checkpointer key for this assignment's logical thread.
 * Empty / "default" → bare threadId (existing single-tenant rows stay
 * reachable). Any other tenant → `${tenantId}:${threadId}`. Native execution
 * validates the delimiter domain before use; proxy paths use the bare plane ID. */
export function checkpointThreadId(tenantId, threadId) {
    const tid = tenantId || DEFAULT_TENANT;
    if (tid === DEFAULT_TENANT)
        return threadId;
    return `${tid}:${threadId}`;
}
/** Persistent prefix keys cannot prove legacy ownership for named tenants. */
export function validateNativeCheckpointIdentity(tenantId, threadId, mode) {
    const tenant = tenantId || DEFAULT_TENANT;
    if (mode === "direct-postgres" && tenant !== DEFAULT_TENANT)
        throw new Error("ambiguous native LangGraph checkpoint ownership for named tenants in direct-postgres mode; " +
            "use proxy checkpoint mode after explicitly resolving legacy native history. " +
            "An existing tenant-prefix key cannot prove its original owner; no state is imported automatically");
    if (tenant !== tenant.trim() ||
        /[\x00-\x1f\x7f]/.test(tenant) ||
        (tenant === DEFAULT_TENANT ? threadId.includes(":") : tenant.includes(":")))
        throw new Error("ambiguous native LangGraph checkpoint identity; native savers require delimiter-free tenant IDs " +
            "and default-tenant thread IDs without ':'. Use proxy checkpoint mode after explicitly resolving " +
            "any existing native history; no state is imported automatically");
}
/** Bare threads.thread_id for /internal/checkpoints HTTP paths.
 * Inverse of checkpointThreadId for the active tenant: direct
 * PostgresSaver needs the prefixed configurable.thread_id (no
 * tenant column on LangGraph's tables), but proxy mode stores under the
 * real threads.thread_id PK and run-binding compares against that bare
 * id. Stripping here keeps both modes correct. */
export function storageThreadId(configThreadId) {
    const tenant = currentTenant();
    if (tenant && tenant !== DEFAULT_TENANT) {
        const prefix = `${tenant}:`;
        if (configThreadId.startsWith(prefix)) {
            return configThreadId.slice(prefix.length);
        }
    }
    return configThreadId;
}
