"""Factory Graph support for the Python runner.

Implements LangGraph's OWN documented factory-graph / `ServerRuntime`
convention -- this is LangChain's spec for their LangGraph Platform /
LangSmith Deployments product (docs.langchain.com/langsmith/graph-rebuild),
not anything invented by a specific third-party server. Any server that
markets itself as LangGraph SDK-compatible implements this same spec,
which is *why* a `graph.py` written for one of them (e.g.
`async def graph(config, runtime): ...`) runs here unchanged: it's
written against the LangGraph SDK's own public API, and this module
implements that API, not a proprietary one.

Without this, a factory-style export previously crashed on the first
dispatched run: a plain callable, not a compiled graph, has no
`.astream()`.

A graph.py export is one of:

- a compiled graph (or a StateGraph, compiled here) -- the ONLY form
  previously supported. Shared across every concurrent run.
- a 0-arg callable -- called ONCE at worker startup (e.g. for one-time
  MCP adapter setup); the result is then used like a compiled graph.
- a factory accepting `config` (a RunnableConfig dict), `runtime` (a
  ServerRuntime), or both, in any parameter order -- called freshly PER
  RUN, for agents that need request-isolated state (fresh middleware
  instances per user, avoiding cross-user state leakage). May be a plain
  function, an async function, or an `@contextlib.asynccontextmanager`
  -decorated one.

Which kind an export is gets decided by INSPECTING its parameter names
(matching LangGraph's own "the server inspects your function's type
annotations" convention) -- an agent author's graph.py needs zero
changes beyond what the LangGraph SDK itself already required.

Known limitation, stated plainly: `runtime.user` is populated from
RunAssignment.user (see internal/transport.UserContext on the Go side),
itself populated from auth.AuthResult -- so it carries whatever the
configured auth provider resolved (identity/display_name/permissions,
plus any Extra fields a webhook auth sidecar returned). With no auth
provider configured, `runtime.user` is None and `runtime.ensure_user()`
raises, matching LangGraph's own documented behavior for an
unauthenticated factory call.
"""

from __future__ import annotations

import inspect
import logging
from dataclasses import dataclass
from typing import Any

logger = logging.getLogger("runkite.runner")

_FACTORY_PARAM_NAMES = {"config", "runtime"}


@dataclass
class RunFactoryContext:
    """Everything a factory graph might need about the run it's being
    built for, independent of any specific ServerRuntime implementation."""

    run_id: str
    thread_id: str
    user: dict | None = None  # raw dict from RunAssignment.user


def _flatten_user_data(data: dict | None) -> dict:
    """Flatten a nested ``extra`` bag into the top-level user dict.

    The Go control plane now emits a flat wire shape, but older in-flight
    assignments (or hand-built test fixtures) may still nest provider
    fields under ``extra``. Application code written against LangGraph
    Platform's own User model conventions expects
    ``to_dict().get("sso_token")`` at the top level -- nesting would
    silently yield None.
    """
    flat = dict(data or {})
    nested = flat.pop("extra", None)
    if isinstance(nested, dict):
        for k, v in nested.items():
            flat.setdefault(k, v)
    return flat


class RunnerUser:
    """Minimal langgraph_sdk.auth.types.BaseUser-protocol-compatible
    wrapper over the raw user dict from RunAssignment.user. Duck-typed
    rather than inheriting BaseUser -- it's a typing.Protocol, not a base
    class -- this just satisfies its shape (identity/display_name/
    is_authenticated/permissions attributes, plus dict-like / attribute
    access for provider-specific Extra fields, e.g. email, an internal
    user ID). Matches LangGraph Platform's own User model conventions
    (flat to_dict, ``user.email`` via attribute access) so existing
    agent tool code keeps working."""

    def __init__(self, data: dict):
        self._data = _flatten_user_data(data)

    @property
    def identity(self) -> str:
        return self._data.get("identity", "")

    @property
    def display_name(self) -> str:
        return self._data.get("display_name") or self.identity

    @property
    def is_authenticated(self) -> bool:
        return self._data.get("is_authenticated", True)

    @property
    def permissions(self) -> list[str]:
        return self._data.get("permissions") or []

    def __getitem__(self, key: str) -> Any:
        return self._data[key]

    def __contains__(self, key: str) -> bool:
        return key in self._data

    def get(self, key: str, default: Any = None) -> Any:
        return self._data.get(key, default)

    def __getattr__(self, name: str) -> Any:
        # LangGraph Platform's own User model conventions expose extra
        # auth fields as attributes (user.email, user.sso_token). Only
        # reached for names that are not real properties / methods on
        # this class.
        data = object.__getattribute__(self, "_data")
        if name in data:
            return data[name]
        raise AttributeError(f"{type(self).__name__!r} object has no attribute {name!r}")

    def to_dict(self) -> dict:
        """Not part of langgraph_sdk's BaseUser protocol, but a common
        convention (LangGraph Platform's own User model provides it)
        that existing application code may already call directly --
        returning the flattened dict here means such code needs no
        changes to run against this runner."""
        return dict(self._data)

    def __repr__(self) -> str:
        return f"RunnerUser(identity={self.identity!r})"


@dataclass
class _MinimalExecutionRuntime:
    context: Any = None


@dataclass
class _MinimalRuntime:
    """Fallback ServerRuntime stand-in, covering the stable/documented
    surface (.user, .store, .execution_runtime, .ensure_user())
    LangGraph's own docs describe. Used only if the real
    langgraph_sdk.ServerRuntime
    can't be constructed directly (its internals changed, or the package
    is missing) -- factory graphs that only touch runtime.user/.store
    (the common case) work identically either way."""

    user: Any
    store: Any
    access_context: str = "threads.create_run"

    @property
    def execution_runtime(self) -> "_MinimalExecutionRuntime":
        return _MinimalExecutionRuntime()

    def ensure_user(self) -> Any:
        if self.user is None:
            raise PermissionError(
                f"No authenticated user available in access_context='{self.access_context}'. "
                "Ensure an auth provider is configured on the control plane."
            )
        return self.user


def _build_server_runtime(run_context: RunFactoryContext, store: Any, graph_config: dict | None):
    """Best-effort construction of a real langgraph_sdk ServerRuntime
    instance, so factory graphs written against the official type (not
    just duck typing) work unchanged. Falls back to _MinimalRuntime if
    langgraph_sdk isn't installed or its internals changed."""
    user = RunnerUser(run_context.user) if run_context.user else None
    context = (graph_config or {}).get("configurable", {})
    try:
        from langgraph_sdk.runtime import _ExecutionRuntime  # type: ignore[attr-defined]

        return _ExecutionRuntime(
            access_context="threads.create_run",
            user=user,
            store=store,
            context=context,
        )
    except Exception:
        logger.debug(
            "langgraph_sdk ServerRuntime unavailable/incompatible, using minimal stand-in",
            exc_info=True,
        )
        return _MinimalRuntime(user=user, store=store)


def _is_already_compiled(obj: Any) -> bool:
    """A CompiledStateGraph (or anything else already runnable) exposes
    .astream/.ainvoke -- duck-typed to avoid a hard coupling on
    LangGraph's internal class hierarchy."""
    return hasattr(obj, "astream") and hasattr(obj, "ainvoke")


async def classify_graph_export(export: Any) -> tuple[str, Any]:
    """Classify a graph.py export as LangGraph's own docs describe:
    ('static', compiled_graph) or ('factory', FactoryGraph)."""
    from langgraph.graph import StateGraph

    if isinstance(export, StateGraph):
        return "static", export.compile()
    if _is_already_compiled(export):
        return "static", export
    if not callable(export):
        raise TypeError(
            f"graph.py export must be a StateGraph, a compiled graph, or a callable -- got {type(export)!r}"
        )

    try:
        sig = inspect.signature(export)
        param_names = tuple(sig.parameters.keys())
    except (TypeError, ValueError):
        param_names = ()

    factory_params = tuple(p for p in param_names if p in _FACTORY_PARAM_NAMES)

    if not factory_params:
        # 0-arg callable: resolve once now, treat as static thereafter --
        # same lifetime as a graph compiled directly in module scope.
        resolved = export()
        graph = await _resolve_once(resolved)
        return "static", graph

    return "factory", FactoryGraph(export, factory_params)


async def _resolve_once(result: Any) -> Any:
    """Unwrap a 0-arg factory's result: a plain graph, a coroutine
    resolving to one, or an async context manager yielding one (entered
    once and never exited -- deliberately: this variant is documented as
    "called once at startup", so its resources live for the runner's
    whole lifetime, same as a checkpointer/store attached once)."""
    if inspect.isawaitable(result):
        result = await result
    if hasattr(result, "__aenter__"):
        result = await result.__aenter__()
    from langgraph.graph import StateGraph

    if isinstance(result, StateGraph):
        result = result.compile()
    return result


class FactoryGraph:
    """A graph.py export that must be called fresh per run. Wraps the
    raw callable plus which of config/runtime it declared."""

    def __init__(self, func, param_names: tuple[str, ...]):
        self._func = func
        self.param_names = param_names

    def build(self, config: dict, run_context: RunFactoryContext, *, checkpointer_manager, store):
        """Returns an async context manager yielding a ready-to-run
        compiled graph (checkpointer/store already attached) scoped to
        exactly one run: `async with factory.build(...) as graph:`."""
        return _FactoryGraphBuild(self._func, self.param_names, config, run_context, checkpointer_manager, store)


class _FactoryGraphBuild:
    def __init__(self, func, param_names, config, run_context, checkpointer_manager, store):
        self._func = func
        self._param_names = param_names
        self._config = config
        self._run_context = run_context
        self._checkpointer_manager = checkpointer_manager
        self._store = store
        self._entered_cm = None  # the factory's own async context manager, if it returned one

    async def __aenter__(self):
        kwargs: dict[str, Any] = {}
        if "config" in self._param_names:
            kwargs["config"] = self._config
        if "runtime" in self._param_names:
            kwargs["runtime"] = _build_server_runtime(self._run_context, self._store, self._config)

        result = self._func(**kwargs)
        if inspect.isawaitable(result):
            result = await result

        if hasattr(result, "__aenter__"):
            self._entered_cm = result
            graph = await result.__aenter__()
        else:
            graph = result

        from langgraph.graph import StateGraph

        if isinstance(graph, StateGraph):
            graph = graph.compile()

        if self._checkpointer_manager is not None:
            self._checkpointer_manager.attach(graph)
        if self._store is not None:
            graph.store = self._store

        return graph

    async def __aexit__(self, exc_type, exc, tb):
        if self._entered_cm is not None:
            return await self._entered_cm.__aexit__(exc_type, exc, tb)
        return False
