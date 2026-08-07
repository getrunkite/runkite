/**
 * Connector session + MCP proxy helpers for agent node code.
 * TypeScript mirror of python/runkite_runner/connectors.py -- see that
 * module's docstring. Session and MCP paths are run-bound; these helpers
 * send X-Runkite-Run-Id + X-Runkite-Generation from configurable
 * (set by buildRunConfig) plus tenantHeaders().
 */
import { HEADER_GENERATION, HEADER_RUN_ID, tenantHeaders } from "./tenantCtx.js";
import { httpDispatcher } from "./tls.js";
export class ConnectorError extends Error {
}
function configurableOf(config) {
    if (!config)
        return {};
    if (config.configurable && typeof config.configurable === "object") {
        return config.configurable;
    }
    return config;
}
function runBoundHeaders(config) {
    const cfg = configurableOf(config);
    const runId = cfg.run_id;
    if (!runId) {
        throw new Error("connector helper: config has no configurable.run_id -- must be called " +
            "with the RunnableConfig a graph node itself received (buildRunConfig " +
            "sets run_id and generation)");
    }
    const headers = {
        "Content-Type": "application/json",
        ...tenantHeaders(),
        [HEADER_RUN_ID]: String(runId),
        [HEADER_GENERATION]: String(cfg.generation ?? 0),
    };
    const runnerToken = process.env.RUNNER_TOKEN;
    if (runnerToken) {
        // Same hardcoded kind as store.ts / a2a.ts (no RUNNER_KIND env).
        headers["X-Runner-Kind"] = "typescript-langgraphjs";
        headers["X-Runner-Token"] = runnerToken;
    }
    return headers;
}
function baseUrl(controlPlaneUrl) {
    return (controlPlaneUrl || process.env.RUNKITE_HTTP_URL || "http://localhost:2026").replace(/\/+$/, "");
}
/** Mint a pre-authenticated connector session for this run. */
export async function getConnectorSession(config, name, opts = {}) {
    const body = {};
    if (opts.userContext !== undefined)
        body.user_context = opts.userContext;
    const init = {
        method: "POST",
        headers: runBoundHeaders(config),
        body: JSON.stringify(body),
        dispatcher: httpDispatcher(),
        signal: opts.timeoutMs ? AbortSignal.timeout(opts.timeoutMs) : undefined,
    };
    const resp = await fetch(`${baseUrl(opts.controlPlaneUrl)}/internal/connectors/${encodeURIComponent(name)}/session`, init);
    if (!resp.ok) {
        throw new ConnectorError(`getConnectorSession ${name}: HTTP ${resp.status}: ${await resp.text()}`);
    }
    return (await resp.json());
}
/** Proxy one JSON-RPC request through the connector MCP gate. */
export async function proxyConnectorMcp(config, name, request, opts = {}) {
    const init = {
        method: "POST",
        headers: runBoundHeaders(config),
        body: JSON.stringify(request),
        dispatcher: httpDispatcher(),
        signal: opts.timeoutMs ? AbortSignal.timeout(opts.timeoutMs) : undefined,
    };
    const resp = await fetch(`${baseUrl(opts.controlPlaneUrl)}/internal/connectors/${encodeURIComponent(name)}/mcp`, init);
    if (!resp.ok) {
        throw new ConnectorError(`proxyConnectorMcp ${name}: HTTP ${resp.status}: ${await resp.text()}`);
    }
    return (await resp.json());
}
