/**
 * Run-bound connector session and MCP proxy helpers. One operation deadline
 * covers session minting, optional renewal, network calls and response bodies.
 */
import { HEADER_GENERATION, HEADER_RUN_ID, HEADER_RUN_ID_B64, runIdHeaders, runnerHeaders } from "./tenantCtx.js";
import { httpDispatcher } from "./tls.js";
export const HEADER_CONNECTOR_SESSION = "X-Runkite-Connector-Session";
export class ConnectorError extends Error {
}
function configurableOf(config) {
    if (!config)
        return {};
    if (config.configurable && typeof config.configurable === "object")
        return config.configurable;
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
    const headers = runnerHeaders();
    delete headers[HEADER_RUN_ID];
    delete headers[HEADER_RUN_ID_B64];
    return {
        "Content-Type": "application/json",
        ...headers,
        ...runIdHeaders(String(runId)),
        [HEADER_GENERATION]: String(cfg.generation ?? 0),
    };
}
function connectorName(name) {
    if (typeof name !== "string" || !name || name === "." || name === "..")
        throw new TypeError("connector name must be a nonempty path segment");
    return encodeURIComponent(name);
}
function baseUrl(controlPlaneUrl) {
    return (controlPlaneUrl || process.env.RUNKITE_HTTP_URL || "http://localhost:2026").replace(/\/+$/, "");
}
async function boundedOperation(timeoutMs, signal, operation) {
    if (!Number.isSafeInteger(timeoutMs) || timeoutMs <= 0 || timeoutMs > 2_147_483_647)
        throw new RangeError("connector timeoutMs must be a positive integer at most 2147483647");
    signal?.throwIfAborted();
    const controller = new AbortController();
    const abort = () => controller.abort(signal?.reason);
    signal?.addEventListener("abort", abort, { once: true });
    const timer = setTimeout(() => controller.abort(new DOMException("Connector operation timed out", "TimeoutError")), timeoutMs);
    let rejectAbort;
    const aborted = new Promise((_, reject) => {
        rejectAbort = () => reject(controller.signal.reason);
        controller.signal.addEventListener("abort", rejectAbort, { once: true });
    });
    try {
        const value = await Promise.race([operation(controller.signal), aborted]);
        controller.signal.throwIfAborted();
        return value;
    }
    finally {
        clearTimeout(timer);
        if (rejectAbort)
            controller.signal.removeEventListener("abort", rejectAbort);
        signal?.removeEventListener("abort", abort);
    }
}
async function session(url, headers, body, signal) {
    signal.throwIfAborted();
    const resp = await fetch(url, {
        method: "POST",
        redirect: "error",
        headers,
        body,
        dispatcher: httpDispatcher(),
        signal,
    });
    if (!resp.ok)
        throw new ConnectorError(`getConnectorSession: HTTP ${resp.status}: ${await resp.text()}`);
    return (await resp.json());
}
/** Mint a pre-authenticated connector session for this run. */
export async function getConnectorSession(config, name, opts = {}) {
    opts.signal?.throwIfAborted();
    const headers = runBoundHeaders(config);
    const url = `${baseUrl(opts.controlPlaneUrl)}/internal/connectors/${connectorName(name)}/session`;
    const body = JSON.stringify(opts.userContext === undefined ? {} : { user_context: opts.userContext });
    return boundedOperation(opts.timeoutMs ?? 30_000, opts.signal, (signal) => session(url, headers, body, signal));
}
/** Proxy one JSON-RPC request. Only an explicit CP session rejection permits renewal. */
export async function proxyConnectorMcp(config, name, request, opts = {}) {
    opts.signal?.throwIfAborted();
    const headers = runBoundHeaders(config);
    const url = `${baseUrl(opts.controlPlaneUrl)}/internal/connectors/${connectorName(name)}`;
    const body = JSON.stringify(request);
    const suppliedToken = opts.sessionToken;
    return boundedOperation(opts.timeoutMs ?? 60_000, opts.signal, async (signal) => {
        const mint = async () => {
            const value = await session(`${url}/session`, headers, "{}", signal);
            if (typeof value.session_token !== "string" || !value.session_token)
                throw new ConnectorError(`proxyConnectorMcp ${name}: session response missing session_token`);
            return value.session_token;
        };
        const once = async (token) => {
            signal.throwIfAborted();
            return fetch(`${url}/mcp`, {
                method: "POST",
                redirect: "error",
                headers: { ...headers, [HEADER_CONNECTOR_SESSION]: token },
                body,
                dispatcher: httpDispatcher(),
                signal,
            });
        };
        let resp = await once(suppliedToken || (await mint()));
        if (resp.status === 401 && suppliedToken === undefined) {
            // An upstream tool can return 401 after doing work. Only the plane's
            // explicit pre-dispatch capability rejection is safe to renew and retry.
            const text = await resp.text();
            let invalidSession = false;
            try {
                const error = JSON.parse(text);
                invalidSession = error?.reason_code === "connector_session_invalid";
            }
            catch {
                /* preserve the original HTTP failure */
            }
            signal.throwIfAborted();
            if (!invalidSession)
                throw new ConnectorError(`proxyConnectorMcp ${name}: HTTP 401: ${text}`);
            resp = await once(await mint());
        }
        if (!resp.ok)
            throw new ConnectorError(`proxyConnectorMcp ${name}: HTTP ${resp.status}: ${await resp.text()}`);
        return (await resp.json());
    });
}
