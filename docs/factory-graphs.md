# Factory Graphs (LangGraph SDK compatibility)

> Deep dive moved from the root README. For a 60-second overview see the [root README](../README.md).

A `graph.py` export can be a per-request **factory** instead of a plain compiled graph -- needed for agents that require request-isolated state (fresh middleware/tool instances per user, avoiding cross-user state leakage). This implements [LangGraph's own documented factory-graph / `ServerRuntime` convention](https://docs.langchain.com/langsmith/graph-rebuild) -- LangChain's spec for their LangGraph Platform product, not anything specific to one third-party server. Any server that's LangGraph SDK-compatible implements the same spec, which is why a `graph.py` written against it runs here unchanged -- it's written against the LangGraph SDK's own public API, and this is that API, not a proprietary one. Which kind an export is gets decided by **inspecting its parameter names**, not a config flag -- zero changes needed beyond what the LangGraph SDK itself already required:

```python
from langgraph_sdk.runtime import ServerRuntime

# Any of these signatures work, in any parameter order:
def graph(): ...                                  # 0-arg: called once at startup
def graph(config: dict): ...                      # per-run, receives RunnableConfig
def graph(runtime: ServerRuntime): ...             # per-run, receives user/store/access_context
async def graph(config: dict, runtime: ServerRuntime): ...   # per-run, both -- may also be
                                                               # an @contextlib.asynccontextmanager
```

A factory (any variant with `config`/`runtime` params) is called **fresh for every run** -- the Python runner builds a new graph instance, attaches that run's checkpointer/store to it specifically, executes it, and tears it down afterward. A 0-arg callable is called once at worker startup and reused like a plain compiled graph from then on (matching LangGraph's own documented behavior for that variant).

`runtime.user` is populated from whichever auth provider authenticated the HTTP request that created the run (see Auth above) -- forwarded from the control plane through `RunAssignment.user`, so a factory can make per-user decisions (which MCP tools to attach, which store namespace to use) without the control plane needing to know anything about LangGraph's `ServerRuntime` type. `identity`/`display_name`/`is_authenticated`/`permissions` are always present; a `webhook` auth provider's response can include arbitrary additional fields (email, an internal user ID, upstream tokens to forward) that stay reachable via dict-like access (`runtime.user["email"]`) -- nothing beyond the fixed identity/permissions/tenant_id set is silently dropped. `runtime.user` is `None` (and `runtime.ensure_user()` raises `PermissionError`) when no auth provider is configured, matching LangGraph's own documented behavior for an unauthenticated factory call.

See `examples/factory_agent/` for a minimal working example, and `python/runkite_runner/factory_graph.py` for the full implementation notes.

### TypeScript SDK

The TypeScript runner implements the same classification and per-run lifecycle -- `graph.ts` exports work identically, just with JavaScript's own syntax for each variant:

```typescript
// Any of these signatures work, in any parameter order:
export function graph() { ... }                                   // 0-arg: called once at startup
export function graph(config: Record<string, unknown>) { ... }    // per-run, receives RunnableConfig
export function graph(runtime: { user, store, ensureUser() }) { ... }  // per-run, receives user/store
export async function graph(config, runtime) { ... }               // per-run, both -- may also
                                                                     // return an object with an
                                                                     // async dispose() method
```

Since JavaScript has no `inspect.signature`-equivalent for parameter names, classification parses the factory function's own source text (`fn.toString()`) instead -- by the time a `graph.ts` module is imported (`tsx` transpiles on the fly), TypeScript's type annotations are already stripped, so this only ever sees plain parameter names. A destructured parameter (`({ config }) => ...`) contributes no detectable name -- every documented LangGraph SDK factory example names `config`/`runtime` as plain identifiers, never destructured, so this covers the real convention without needing a full JS parser.

LangGraph.js has no published `ServerRuntime` type to construct (unlike Python, which best-effort constructs a real `langgraph_sdk._ExecutionRuntime`) -- the TypeScript runner always passes a duck-typed `ServerRuntimeStandIn` covering the same stable surface (`.user`, `.store`, `.ensureUser()`). There's also no `@contextlib.asynccontextmanager` equivalent to mirror for teardown: a factory that returns an object with an async `dispose()` method gets it called after the run finishes (this package's own convention, not a langgraph_sdk one).

See `examples/echo_agent_ts/factoryGraph.ts` for a minimal working example, and `typescript/runkite-runner/src/factoryGraph.ts` for the full implementation notes.
