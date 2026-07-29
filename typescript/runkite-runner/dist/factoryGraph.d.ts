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
import type { RunnableGraph } from "./adapter.js";
import type { CheckpointerManager } from "./checkpoint.js";
import type { RunkiteStore } from "./store.js";
/** Everything a factory graph might need about the run it's being built
 * for, independent of any specific ServerRuntime implementation. Direct
 * mirror of Python's RunFactoryContext dataclass. */
export interface RunFactoryContext {
    runId: string;
    threadId: string;
    /** Raw dict from RunAssignment.user -- see runnerUser.ts. */
    user?: Record<string, unknown>;
}
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
export declare class ServerRuntimeStandIn {
    readonly user: RunnerUser | null;
    readonly store: unknown;
    constructor(user: RunnerUser | null, store: unknown);
    /** Throws if no authenticated user is available -- matches LangGraph's
     * own documented behavior for an unauthenticated factory call, same
     * as Python's `ensure_user()`. */
    ensureUser(): RunnerUser;
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
export declare function extractParamNames(fn: (...args: any[]) => unknown): string[];
export type GraphClassification = {
    kind: "static";
    graph: RunnableGraph;
} | {
    kind: "factory";
    factory: FactoryGraph;
};
/** Classifies a graph.ts export as LangGraph's own docs describe:
 * `{kind: "static", graph}` or `{kind: "factory", factory}`. Direct
 * mirror of Python's `classify_graph_export`. */
export declare function classifyGraphExport(exportValue: unknown): Promise<GraphClassification>;
/** A graph.ts export that must be called fresh per run. Wraps the raw
 * callable plus which of config/runtime it declared. Direct mirror of
 * Python's FactoryGraph. */
export declare class FactoryGraph {
    private readonly func;
    readonly paramNames: string[];
    constructor(func: (...args: any[]) => unknown, paramNames: string[]);
    /** Returns a builder for exactly one run -- callers do
     * `const graph = await adapter.buildFactoryGraph(...).open()` then
     * `await build.close()` in a finally block (see executeRun.ts). No
     * `using`/`Symbol.asyncDispose` here: TypeScript's Explicit Resource
     * Management support needs an extra `lib` entry this package doesn't
     * otherwise need, and an explicit open()/close() pair is no harder to
     * get right in one call site (executeRun.ts) than a `using`
     * declaration would be. */
    build(config: Record<string, unknown>, runContext: RunFactoryContext, deps: {
        checkpointerManager: CheckpointerManager | null;
        store: RunkiteStore | null;
    }): FactoryGraphBuild;
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
export declare class FactoryGraphBuild {
    private readonly func;
    private readonly paramNames;
    private readonly config;
    private readonly runContext;
    private readonly checkpointerManager;
    private readonly store;
    private disposable;
    constructor(func: (...args: any[]) => unknown, paramNames: string[], config: Record<string, unknown>, runContext: RunFactoryContext, checkpointerManager: CheckpointerManager | null, store: RunkiteStore | null);
    open(): Promise<RunnableGraph>;
    close(): Promise<void>;
}
