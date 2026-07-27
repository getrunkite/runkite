/**
 * Custom routes, in-runner mode (master plan: "Custom routes"). TypeScript
 * mirror of the Python runner's custom_app.py, adapted to Node's request
 * handler convention instead of ASGI: the exported value must be callable
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
import { type RequestListener, type Server } from "node:http";
export type CustomAppHandler = RequestListener;
/** Loads the "path:exportName" module reference from custom_app.module,
 * same convention as langgraph.json's "graphs" entries. */
export declare function loadRequestHandler(configDir: string, moduleRef: string): Promise<CustomAppHandler>;
/** Serves handler on host:port until stopped. Runs in the SAME process and
 * event loop as the worker's poll loop -- a slow custom-route handler can,
 * in principle, delay the runner's own async work. Use sidecar mode
 * instead if a route needs isolation or independent scaling. */
export declare function serveCustomApp(handler: CustomAppHandler, host: string, port: number): {
    server: Server;
    stop: () => Promise<void>;
};
