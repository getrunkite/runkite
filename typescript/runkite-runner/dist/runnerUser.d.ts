/**
 * Minimal langgraph_sdk BaseUser-protocol-compatible wrapper over the raw
 * user dict from RunAssignment.user -- TypeScript mirror of the Python
 * runner's RunnerUser (factory_graph.py). Duck-typed rather than
 * implementing any specific LangGraph.js runtime user interface: this
 * just satisfies the same shape (identity/displayName/isAuthenticated/
 * permissions) plus BOTH of the two access styles application code
 * written against LangGraph Platform's own User model conventions
 * might use for provider-specific Extra fields -- `user.get("email")`
 * (dict-like, always works, TypeScript-idiomatic) AND `user.email`
 * (attribute-style, via the Proxy in the constructor below, matching
 * Python's `__getattr__` for code ported straight from a Python graph).
 *
 * Shared by a2a.ts (forwarded as `on_behalf_of` on a delegated call) and
 * factoryGraph.ts (exposed as `runtime.user`) -- same wrapper, two
 * different consumers, same reason Python's RunnerUser is used by both
 * a2a.py and factory_graph.py.
 */
export declare class RunnerUser {
    private readonly data;
    constructor(data: Record<string, unknown> | null | undefined);
    get identity(): string;
    get displayName(): string;
    get isAuthenticated(): boolean;
    get permissions(): string[];
    /** Dict-like access for provider-specific Extra fields -- matches
     * Python's RunnerUser.get(), same default-value convention. (Also
     * reachable as `user.email`-style attribute access via the
     * constructor's Proxy; this method is the explicit, always-typed
     * way to do the same lookup.) */
    get(key: string, defaultValue?: unknown): unknown;
    has(key: string): boolean;
    /** Not part of any LangGraph.js runtime user interface, but a common
     * convention (LangGraph Platform's own User model provides it, same
     * as Python's RunnerUser.to_dict()) that existing application code or
     * a2a.ts's on_behalf_of forwarding may call directly. */
    toDict(): Record<string, unknown>;
}
