/**
 * Factory Graph support for the TypeScript runner. TypeScript mirror of
 * the Python runner's factory_graph.py -- see that file's docstring for
 * the full rationale; repeated briefly here:
 *
 * Implements LangGraph's OWN documented factory-graph / `ServerRuntime`
 * convention (LangChain's spec for LangGraph Platform / LangSmith
 * Deployments, docs.langchain.com/langsmith/graph-rebuild) -- any server
 * that markets itself as LangGraph SDK-compatible implements this same
 * spec, which is *why* a `graph.ts` written for one of them
 * (`async function graph(config, runtime) { ... }`) runs here unchanged.
 *
 * Without this, a factory-style export previously crashed on the first
 * dispatched run: a plain callable, not a compiled graph, has no
 * `.stream()`.
 *
 * A graph.ts export is one of:
 *
 * - a compiled graph (or a StateGraph, compiled here) -- shared across
 *   every concurrent run.
 * - a 0-arg callable -- called ONCE at worker startup; the result is
 *   then used like a compiled graph.
 * - a factory accepting `config`, `runtime`, or both (in any parameter
 *   order) -- called freshly PER RUN, for agents that need request-
 *   isolated state (fresh middleware instances per user, avoiding
 *   cross-user state leakage). May be sync, async, or return an object
 *   exposing an async `dispose()` method for teardown (this package's
 *   own convention -- see FactoryGraphBuild's doc comment for why this
 *   isn't Python's `@asynccontextmanager`).
 *
 * Which kind an export is gets decided by INSPECTING its parameter
 * NAMES (matching LangGraph's own "the server inspects your function's
 * parameters" convention) -- an agent author's graph.ts needs zero
 * changes beyond what the LangGraph SDK itself already required.
 *
 * Known limitation, stated plainly: `runtime.user` is populated from
 * RunAssignment.user (see internal/transport.UserContext on the Go
 * side), itself populated from auth.AuthResult -- so it carries
 * whatever the configured auth provider resolved. With no auth provider
 * configured, `runtime.user` is null and `runtime.ensureUser()` throws,
 * matching LangGraph's own documented behavior for an unauthenticated
 * factory call.
 */
import { RunnerUser } from "./runnerUser.js";
const FACTORY_PARAM_NAMES = new Set(["config", "runtime"]);
/**
 * Minimal ServerRuntime stand-in, covering the stable/documented surface
 * (`.user`, `.store`, `.ensureUser()`) LangGraph's own docs describe.
 * Unlike Python (which best-effort constructs a REAL
 * `langgraph_sdk.runtime._ExecutionRuntime` when that package is
 * importable, falling back to a minimal stand-in otherwise), LangGraph.js
 * has no published `ServerRuntime` type to construct at all as of the
 * installed version -- this is always the duck-typed stand-in, not a
 * best-effort/fallback pair. Factory graphs that only touch
 * `runtime.user`/`runtime.store` (the common case, matching
 * examples/factory_agent/graph.py) work the same either way.
 */
export class ServerRuntimeStandIn {
    user;
    store;
    constructor(user, store) {
        this.user = user;
        this.store = store;
    }
    /** Throws if no authenticated user is available -- matches LangGraph's
     * own documented behavior for an unauthenticated factory call, same
     * as Python's `ensure_user()`. */
    ensureUser() {
        if (!this.user) {
            throw new Error("No authenticated user available. Ensure an auth provider is configured on the control plane.");
        }
        return this.user;
    }
}
/** True for a CompiledStateGraph (or anything else already runnable) --
 * duck-typed on `.stream` (the one method this runner actually calls),
 * to avoid a hard coupling on LangGraph.js's internal class hierarchy.
 * A StateGraph BUILDER (not yet compiled) has `.compile` but not
 * `.stream` -- see isStateGraphBuilder below. */
function isAlreadyCompiled(obj) {
    return obj != null && typeof obj.stream === "function";
}
function isStateGraphBuilder(obj) {
    return obj != null && typeof obj.compile === "function" && typeof obj.stream !== "function";
}
/** Finds the first top-level `(...)` group in a function's source and
 * returns its contents, or the bare identifier for a parenthesis-free
 * single-param arrow function (`x => ...`), whose "parameter list" is
 * just that one name. Null for a 0-arg parenthesis-free form (which
 * can't actually occur in valid JS -- `=> ...` alone isn't legal syntax
 * -- kept only so the type signature doesn't lie about always returning
 * a string). */
function extractParamListSource(fnSource) {
    const trimmed = fnSource.replace(/^async\s+/, "").trimStart();
    const parenIdx = trimmed.indexOf("(");
    const arrowIdx = trimmed.indexOf("=>");
    if (parenIdx === -1 || (arrowIdx !== -1 && arrowIdx < parenIdx)) {
        // No parens before the arrow (or no parens at all) -- must be the
        // parenthesis-free single-identifier arrow form (`x => ...`).
        const singleParam = trimmed.slice(0, arrowIdx === -1 ? trimmed.length : arrowIdx).trim();
        return singleParam.length > 0 && /^[A-Za-z_$]/.test(singleParam) ? singleParam : null;
    }
    // Balanced-paren scan from parenIdx, NOT a naive /\(([^)]*)\)/ regex --
    // a default value containing its own parens (`config = getDefault()`)
    // would otherwise truncate the match at the FIRST `)`, well before the
    // parameter list actually ends.
    let depth = 0;
    for (let i = parenIdx; i < trimmed.length; i++) {
        if (trimmed[i] === "(")
            depth++;
        else if (trimmed[i] === ")") {
            depth--;
            if (depth === 0)
                return trimmed.slice(parenIdx + 1, i);
        }
    }
    return null;
}
/** Splits a parameter list on top-level commas only -- a comma nested
 * inside a default value's own `()`, `[]`, or `{}` (destructuring,
 * object/array literal defaults) doesn't count as a separator. */
function splitTopLevelCommas(s) {
    const parts = [];
    let depth = 0;
    let start = 0;
    for (let i = 0; i < s.length; i++) {
        const c = s[i];
        if (c === "(" || c === "[" || c === "{")
            depth++;
        else if (c === ")" || c === "]" || c === "}")
            depth--;
        else if (c === "," && depth === 0) {
            parts.push(s.slice(start, i));
            start = i + 1;
        }
    }
    parts.push(s.slice(start));
    return parts;
}
/**
 * Extracts parameter NAMES from a function's source text (via
 * `fn.toString()`) -- JavaScript has no runtime reflection for
 * parameter names the way Python's `inspect.signature` does, so this
 * parses the source instead. By the time a graph.ts module is imported
 * (tsx transpiles on the fly), TypeScript type annotations are already
 * stripped, so this only ever sees plain JS parameter lists -- no
 * `: Type` to account for.
 *
 * Handles plain function declarations/expressions, async functions, and
 * both arrow function forms (`(a, b) => ...` and the parenthesis-free
 * single-param `a => ...`). A destructured parameter (`({ x }) =>
 * ...`) or one with a default value contributes no name here -- an
 * intentional simplification, not an oversight: every documented
 * LangGraph SDK factory example names `config`/`runtime` as plain
 * identifiers, never destructured, so this covers the actual
 * convention without a full JS parser.
 */
export function extractParamNames(fn) {
    const paramList = extractParamListSource(fn.toString());
    if (paramList === null)
        return [];
    return splitTopLevelCommas(paramList)
        .map((p) => p.trim())
        .filter((p) => p.length > 0)
        .map((p) => /^[A-Za-z_$][A-Za-z0-9_$]*/.exec(p)?.[0])
        .filter((name) => !!name);
}
/** Unwraps a 0-arg factory's result: a plain graph, a Promise resolving
 * to one, or a StateGraph builder -- same "called once at startup"
 * semantics as Python's `_resolve_once`, so its resources live for the
 * runner's whole lifetime, same as a checkpointer/store attached once.
 * Unlike Python, this does NOT look for an async-context-manager
 * protocol here -- a 0-arg factory's teardown-on-shutdown case isn't
 * exercised by examples/factory_agent's own convention, and adding one
 * only for this narrower path (not the per-run factory path, which DOES
 * support `dispose()` -- see FactoryGraphBuild) isn't worth the surface
 * area until a real use case needs it. */
async function resolveOnce(result) {
    let resolved = result;
    if (resolved != null && typeof resolved.then === "function") {
        resolved = await resolved;
    }
    if (isStateGraphBuilder(resolved)) {
        resolved = resolved.compile();
    }
    return resolved;
}
/** Classifies a graph.ts export as LangGraph's own docs describe:
 * `{kind: "static", graph}` or `{kind: "factory", factory}`. Direct
 * mirror of Python's `classify_graph_export`. */
export async function classifyGraphExport(exportValue) {
    if (isStateGraphBuilder(exportValue)) {
        return { kind: "static", graph: exportValue.compile() };
    }
    if (isAlreadyCompiled(exportValue)) {
        return { kind: "static", graph: exportValue };
    }
    if (typeof exportValue !== "function") {
        throw new TypeError(`graph.ts export must be a StateGraph, a compiled graph, or a callable -- got ${typeof exportValue}`);
    }
    const fn = exportValue;
    const paramNames = extractParamNames(fn);
    const factoryParams = paramNames.filter((p) => FACTORY_PARAM_NAMES.has(p));
    if (factoryParams.length === 0) {
        // 0-arg callable: resolve once now, treat as static thereafter --
        // same lifetime as a graph compiled directly in module scope.
        const graph = await resolveOnce(fn());
        return { kind: "static", graph };
    }
    return { kind: "factory", factory: new FactoryGraph(fn, factoryParams) };
}
/** A graph.ts export that must be called fresh per run. Wraps the raw
 * callable plus which of config/runtime it declared. Direct mirror of
 * Python's FactoryGraph. */
export class FactoryGraph {
    func;
    paramNames;
    constructor(func, paramNames) {
        this.func = func;
        this.paramNames = paramNames;
    }
    /** Returns a builder for exactly one run -- callers do
     * `const graph = await adapter.buildFactoryGraph(...).open()` then
     * `await build.close()` in a finally block (see executeRun.ts). No
     * `using`/`Symbol.asyncDispose` here: TypeScript's Explicit Resource
     * Management support needs an extra `lib` entry this package doesn't
     * otherwise need, and an explicit open()/close() pair is no harder to
     * get right in one call site (executeRun.ts) than a `using`
     * declaration would be. */
    build(config, runContext, deps) {
        return new FactoryGraphBuild(this.func, this.paramNames, config, runContext, deps.checkpointerManager, deps.store);
    }
}
/**
 * Builds (and later tears down) one factory graph instance for exactly
 * one run. TypeScript mirror of Python's `_FactoryGraphBuild`, minus the
 * `async with` sugar Python gets from implementing
 * `__aenter__`/`__aexit__` -- `open()`/`close()` here do the same two
 * jobs explicitly.
 *
 * Teardown convention: if the factory function's OWN returned value
 * (before any StateGraph compile step) exposes an async `dispose()`
 * method, `close()` calls it. This is this package's own convention,
 * not a port of Python's `@contextlib.asynccontextmanager` support --
 * LangGraph.js has no equivalent decorator/protocol to mirror, and
 * inventing one here (e.g. via `Symbol.asyncDispose`) would need a
 * `tsconfig.json` `lib` entry ("ESNext.Disposable") this package
 * otherwise has no reason to add. A factory that needs cleanup (closing
 * an MCP session, releasing a per-run resource) implements `dispose()`;
 * one that doesn't (the common case, matching
 * examples/factory_agent/graph.py) needs no changes.
 */
export class FactoryGraphBuild {
    func;
    paramNames;
    config;
    runContext;
    checkpointerManager;
    store;
    disposable = null;
    constructor(func, paramNames, config, runContext, checkpointerManager, store) {
        this.func = func;
        this.paramNames = paramNames;
        this.config = config;
        this.runContext = runContext;
        this.checkpointerManager = checkpointerManager;
        this.store = store;
    }
    async open() {
        try {
            const kwargs = {};
            if (this.paramNames.includes("config"))
                kwargs.config = this.config;
            if (this.paramNames.includes("runtime")) {
                const user = this.runContext.user ? new RunnerUser(this.runContext.user) : null;
                kwargs.runtime = new ServerRuntimeStandIn(user, this.store);
            }
            // Call with kwargs in the SAME declared parameter order the
            // function actually uses (not always config-then-runtime) -- LangGraph's
            // own convention documents "any parameter order/subset" as
            // supported, and examples/factory_agent/graph.py's own signature
            // (config, runtime) happens to match declaration order already, but
            // this must not assume that.
            const args = this.paramNames.map((name) => kwargs[name]);
            let result = this.func(...args);
            if (result != null && typeof result.then === "function") {
                result = await result;
            }
            if (result != null && typeof result.dispose === "function") {
                this.disposable = result;
            }
            let graph = result;
            if (isStateGraphBuilder(graph)) {
                graph = graph.compile();
            }
            const compiled = graph;
            if (this.checkpointerManager)
                this.checkpointerManager.attach(compiled);
            if (this.store)
                compiled.store = this.store;
            return compiled;
        }
        catch (err) {
            // open() can set disposable then throw on compile/attach -- tear
            // down here so a caller that only closes after a successful open
            // (or whose finally never runs) still doesn't leak. Idempotent
            // close() below means executeRun's finally can call close again
            // harmlessly.
            await this.close();
            throw err;
        }
    }
    async close() {
        if (!this.disposable)
            return;
        const d = this.disposable;
        this.disposable = null;
        await d.dispose();
    }
}
