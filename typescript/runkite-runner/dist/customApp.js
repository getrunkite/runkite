/**
 * Custom routes, in-runner mode. TypeScript mirror of the Python runner's
 * custom_app.py, adapted to Node's request handler convention instead of
 * ASGI: the exported value must be callable
 * as `(req, res) => void` -- Node's http.RequestListener shape. This
 * covers plain node:http handlers directly and Express apps directly
 * (an Express `app` IS a valid request listener); Koa needs `app.callback()`
 * exported instead of `app` itself, since Koa's app object isn't callable
 * on its own -- a minor, documented difference from Python's uvicorn (which
 * accepts any ASGI app uniformly).
 *
 * Sidecar mode needs nothing here: it's a separate process the control
 * plane reverse-proxies to directly, configured entirely on the Go side.
 */
import { createServer } from "node:http";
import path from "node:path";
import { pathToFileURL } from "node:url";
import { logger } from "./logger.js";
/** Loads the "path:exportName" module reference from custom_app.module,
 * same convention as langgraph.json's "graphs" entries. */
export async function loadRequestHandler(configDir, moduleRef) {
    const idx = moduleRef.indexOf(":");
    if (idx === -1) {
        throw new Error(`Invalid custom_app.module "${moduleRef}" -- expected "path:exportName"`);
    }
    const filePath = moduleRef.slice(0, idx);
    const exportName = moduleRef.slice(idx + 1);
    const absPath = path.resolve(configDir, filePath);
    const mod = await import(pathToFileURL(absPath).href);
    const handler = mod[exportName];
    if (typeof handler !== "function") {
        throw new Error(`custom_app.module "${moduleRef}" has no callable export "${exportName}"`);
    }
    return handler;
}
/** Serves handler on host:port until stopped. Runs in the SAME process and
 * event loop as the worker's poll loop -- a slow custom-route handler can,
 * in principle, delay the runner's own async work. Use sidecar mode
 * instead if a route needs isolation or independent scaling. */
export function serveCustomApp(handler, host, port) {
    const server = createServer(handler);
    server.listen(port, host, () => {
        logger.info(`Custom app serving on http://${host}:${port}`);
    });
    return {
        server,
        stop: () => new Promise((resolve) => {
            server.close(() => resolve());
        }),
    };
}
