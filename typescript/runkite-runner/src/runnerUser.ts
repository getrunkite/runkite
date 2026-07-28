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

/** Flattens a nested `extra` bag into the top-level user dict -- the Go
 * control plane emits a flat wire shape (see internal/transport.
 * UserContext.MarshalJSON), but a hand-built test fixture or an older
 * in-flight assignment might still nest provider fields under `extra`.
 * Application code expects `toDict().sso_token` at the top level;
 * nesting would silently yield undefined. */
function flattenUserData(data: Record<string, unknown> | null | undefined): Record<string, unknown> {
  const flat: Record<string, unknown> = { ...(data ?? {}) };
  const nested = flat.extra;
  delete flat.extra;
  if (nested && typeof nested === "object") {
    for (const [k, v] of Object.entries(nested as Record<string, unknown>)) {
      if (!(k in flat)) flat[k] = v;
    }
  }
  return flat;
}

export class RunnerUser {
  private readonly data: Record<string, unknown>;

  constructor(data: Record<string, unknown> | null | undefined) {
    this.data = flattenUserData(data);
    // Proxy so `user.email`-style attribute access for provider-
    // specific Extra fields actually works, matching Python's
    // RunnerUser.__getattr__ -- real parity, not just a documented
    // limitation. Real class members (identity, displayName, get,
    // toDict, ...) are checked FIRST via the `prop in target` branch,
    // only falling through to `data` for names that aren't real
    // properties/methods -- same fallback order Python's __getattr__
    // has (only invoked when normal attribute lookup already missed).
    // Proxy forwards getPrototypeOf to the target by default, so
    // `instanceof RunnerUser` still holds on the returned proxy.
    return new Proxy(this, {
      get(target, prop, receiver) {
        if (typeof prop === "symbol" || prop in target) {
          return Reflect.get(target, prop, receiver);
        }
        return target.data[prop];
      },
      has(target, prop) {
        return prop in target || (typeof prop === "string" && prop in target.data);
      },
    });
  }

  get identity(): string {
    return typeof this.data.identity === "string" ? this.data.identity : "";
  }

  get displayName(): string {
    return typeof this.data.display_name === "string" ? this.data.display_name : this.identity;
  }

  get isAuthenticated(): boolean {
    return this.data.is_authenticated !== false;
  }

  get permissions(): string[] {
    return Array.isArray(this.data.permissions) ? (this.data.permissions as string[]) : [];
  }

  /** Dict-like access for provider-specific Extra fields -- matches
   * Python's RunnerUser.get(), same default-value convention. (Also
   * reachable as `user.email`-style attribute access via the
   * constructor's Proxy; this method is the explicit, always-typed
   * way to do the same lookup.) */
  get(key: string, defaultValue: unknown = undefined): unknown {
    return key in this.data ? this.data[key] : defaultValue;
  }

  has(key: string): boolean {
    return key in this.data;
  }

  /** Not part of any LangGraph.js runtime user interface, but a common
   * convention (LangGraph Platform's own User model provides it, same
   * as Python's RunnerUser.to_dict()) that existing application code or
   * a2a.ts's on_behalf_of forwarding may call directly. */
  toDict(): Record<string, unknown> {
    return { ...this.data };
  }
}
