"""Self-check for Factory Graph support (factory_graph.py).

Proves the pieces that make a LangGraph factory-style `graph.py` (e.g. a
`graph(config, runtime) -> AsyncContextManager[CompiledStateGraph]`
factory) work on the Runkite runner instead of crashing on the first
dispatched run:

1. classify_graph_export correctly distinguishes static graphs, 0-arg
   callables, and factories -- by signature, matching LangGraph's own
   documented "inspects your function signature" convention.
2. Every documented factory signature shape works: `graph()`,
   `graph(config)`, `graph(runtime)`, `graph(config, runtime)` (any
   order), as a plain function, an async function, or an
   `@contextlib.asynccontextmanager`.
3. A factory gets a genuinely FRESH instance per build -- the entire
   point of this feature (per-user isolation) -- not a cached/shared one.
4. runtime.user is populated from RunAssignment.user's raw dict, with
   identity/display_name/is_authenticated/permissions available as
   attributes AND provider-specific extra fields (email, an internal
   user ID, etc.) available via dict-like access -- matching
   langgraph_sdk.auth.types.BaseUser's protocol shape.
5. checkpointer/store attachment happens on the per-run instance, not
   globally.

Usage:
    python/.venv/bin/python python/tests/test_factory_graph.py
"""

import asyncio
import contextlib
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from typing import Annotated, TypedDict  # noqa: E402

from langgraph.graph import END, START, StateGraph  # noqa: E402
from langgraph.graph.message import add_messages  # noqa: E402
from runkite_runner.factory_graph import (  # noqa: E402
    RunFactoryContext,
    classify_graph_export,
)
from runkite_runner.worker import LangGraphAdapter, build_run_config, execute_run  # noqa: E402


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


class State(TypedDict):
    messages: Annotated[list, add_messages]


def _make_static_graph():
    builder = StateGraph(State)
    builder.add_node("noop", lambda state: {})
    builder.add_edge(START, "noop")
    builder.add_edge("noop", END)
    return builder


class _FakeCheckpointerManager:
    def __init__(self):
        self.attached = []
        self.mode = "fake"

    def attach(self, graph):
        self.attached.append(graph)
        graph.checkpointer = "fake-checkpointer"


class _FakeStore:
    mode = "fake"


async def test_classify_static_graph():
    kind, resolved = await classify_graph_export(_make_static_graph())
    check("StateGraph classified as static", kind == "static")
    check("StateGraph is compiled", hasattr(resolved, "astream"))


async def test_classify_already_compiled():
    compiled = _make_static_graph().compile()
    kind, resolved = await classify_graph_export(compiled)
    check("already-compiled graph classified as static", kind == "static")
    check("already-compiled graph returned as-is", resolved is compiled)


async def test_classify_zero_arg_callable():
    calls = []

    def make_graph():
        calls.append(1)
        return _make_static_graph()

    kind, resolved = await classify_graph_export(make_graph)
    check("0-arg callable classified as static", kind == "static")
    check("0-arg callable was called exactly once", len(calls) == 1)
    check("0-arg callable's result got compiled", hasattr(resolved, "astream"))


async def test_classify_factory_signatures():
    def graph_config_only(config):
        return _make_static_graph().compile()

    def graph_runtime_only(runtime):
        return _make_static_graph().compile()

    def graph_both_orders(runtime, config):
        return _make_static_graph().compile()

    async def graph_async(config, runtime):
        return _make_static_graph().compile()

    for label, func, expected_params in [
        ("config-only", graph_config_only, {"config"}),
        ("runtime-only", graph_runtime_only, {"runtime"}),
        ("reversed order", graph_both_orders, {"config", "runtime"}),
        ("async def", graph_async, {"config", "runtime"}),
    ]:
        kind, resolved = await classify_graph_export(func)
        check(f"factory signature '{label}' classified as factory", kind == "factory")
        check(f"factory signature '{label}' captured expected params", set(resolved.param_names) == expected_params)


async def test_factory_gets_fresh_instance_per_build():
    build_count = 0

    def factory(config, runtime):
        nonlocal build_count
        build_count += 1
        return _make_static_graph().compile()

    kind, factory_graph = await classify_graph_export(factory)
    check("factory classified correctly for isolation test", kind == "factory")

    ctx = RunFactoryContext(run_id="r1", thread_id="t1", user=None)
    cm1 = factory_graph.build({}, ctx, checkpointer_manager=None, store=None)
    async with cm1 as g1:
        pass
    cm2 = factory_graph.build({}, ctx, checkpointer_manager=None, store=None)
    async with cm2 as g2:
        pass

    check("factory called once per build (2 builds -> 2 calls)", build_count == 2)
    check("each build returns a distinct graph instance", g1 is not g2)


async def test_factory_async_context_manager_variant():
    entered = []
    exited = []

    @contextlib.asynccontextmanager
    async def factory(config, runtime):
        entered.append(1)
        try:
            yield _make_static_graph().compile()
        finally:
            exited.append(1)

    kind, factory_graph = await classify_graph_export(factory)
    ctx = RunFactoryContext(run_id="r1", thread_id="t1", user=None)
    cm = factory_graph.build({}, ctx, checkpointer_manager=None, store=None)
    async with cm as g:
        check("async-context-manager factory yields a usable graph", hasattr(g, "astream"))
        check("context manager entered before yielding control", len(entered) == 1)
        check("context manager not yet exited while still inside the `with`", len(exited) == 0)
    check("context manager exited after the `with` block", len(exited) == 1)


async def test_runtime_user_identity_and_extra_fields():
    seen = {}

    def factory(config, runtime):
        seen["identity"] = runtime.user.identity if runtime.user else None
        seen["display_name"] = runtime.user.display_name if runtime.user else None
        seen["is_authenticated"] = runtime.user.is_authenticated if runtime.user else None
        seen["permissions"] = runtime.user.permissions if runtime.user else None
        seen["email"] = runtime.user["email"] if runtime.user and "email" in runtime.user else None
        return _make_static_graph().compile()

    kind, factory_graph = await classify_graph_export(factory)
    ctx = RunFactoryContext(
        run_id="r1",
        thread_id="t1",
        user={
            "identity": "alice-123",
            "display_name": "Alice Example",
            "is_authenticated": True,
            "permissions": ["read", "write"],
            "email": "alice@example.com",
        },
    )
    cm = factory_graph.build({}, ctx, checkpointer_manager=None, store=None)
    async with cm:
        pass

    check("runtime.user.identity populated", seen["identity"] == "alice-123")
    check("runtime.user.display_name populated", seen["display_name"] == "Alice Example")
    check("runtime.user.is_authenticated populated", seen["is_authenticated"] is True)
    check("runtime.user.permissions populated", seen["permissions"] == ["read", "write"])
    check("runtime.user's dict-like access exposes extra provider fields (email)", seen["email"] == "alice@example.com")


async def test_runtime_user_none_when_no_auth():
    seen = {}

    def factory(runtime):
        seen["user"] = runtime.user
        return _make_static_graph().compile()

    kind, factory_graph = await classify_graph_export(factory)
    ctx = RunFactoryContext(run_id="r1", thread_id="t1", user=None)
    cm = factory_graph.build({}, ctx, checkpointer_manager=None, store=None)
    async with cm:
        pass

    check("runtime.user is None when no auth identity was forwarded", seen["user"] is None)


async def test_checkpointer_and_store_attached_per_instance():
    def factory(config, runtime):
        return _make_static_graph().compile()

    kind, factory_graph = await classify_graph_export(factory)
    ctx = RunFactoryContext(run_id="r1", thread_id="t1", user=None)
    manager = _FakeCheckpointerManager()
    store = _FakeStore()

    cm = factory_graph.build({}, ctx, checkpointer_manager=manager, store=store)
    async with cm as g:
        check("checkpointer attached to the per-run instance", g.checkpointer == "fake-checkpointer")
        check("store attached to the per-run instance", g.store is store)
    check("checkpointer manager saw exactly one attach call", len(manager.attached) == 1)


async def test_runtime_user_to_dict_for_non_baseuser_consumers():
    """Regression test for a real gap found integrating a production
    agent: its tool code calls `user.to_dict().get("sso_token")`
    directly (a convention from LangGraph Platform's own User model),
    not the BaseUser protocol's attribute/dict access. Without to_dict(), this
    silently returns an empty dict and the tool reports "no SSO token"
    even when auth IS configured and the token WAS forwarded."""
    seen = {}

    def factory(runtime):
        seen["as_dict"] = runtime.user.to_dict() if runtime.user else None
        return _make_static_graph().compile()

    kind, factory_graph = await classify_graph_export(factory)
    ctx = RunFactoryContext(
        run_id="r1",
        thread_id="t1",
        user={"identity": "alice-123", "sso_token": "raw-jwt-value"},
    )
    cm = factory_graph.build({}, ctx, checkpointer_manager=None, store=None)
    async with cm:
        pass

    check("to_dict() returns a plain dict", isinstance(seen["as_dict"], dict))
    check("to_dict() includes forwarded raw-token-style fields", seen["as_dict"]["sso_token"] == "raw-jwt-value")


async def test_runtime_user_flattens_nested_extra_for_to_dict():
    """The Go UserContext used to nest AuthResult.Extra under an
    ``extra`` key on the wire. LangGraph Platform's own User.to_dict()
    convention is flat, so ``.get("sso_token")`` must work against the
    nested shape too (defense for legacy queue messages / hand-built
    fixtures)."""
    seen = {}

    def factory(runtime):
        u = runtime.user
        seen["token"] = u.to_dict().get("sso_token") if u else None
        seen["email_attr"] = getattr(u, "email", None) if u else None
        seen["email_get"] = u.get("email") if u else None
        return _make_static_graph().compile()

    kind, factory_graph = await classify_graph_export(factory)
    ctx = RunFactoryContext(
        run_id="r1",
        thread_id="t1",
        user={
            "identity": "alice-123",
            "is_authenticated": True,
            "extra": {
                "sso_token": "nested-jwt",
                "email": "alice@example.com",
            },
        },
    )
    cm = factory_graph.build({}, ctx, checkpointer_manager=None, store=None)
    async with cm:
        pass

    check("to_dict() flattens nested extra.sso_token", seen["token"] == "nested-jwt")
    check("attribute access reaches flattened email", seen["email_attr"] == "alice@example.com")
    check("dict get reaches flattened email", seen["email_get"] == "alice@example.com")


def test_build_run_config_sets_server_info_keys():
    """Regression test for a real bug found integrating a production
    LangGraph agent (deepagents' StoreBackend crashed with
    AttributeError on `get_runtime().server_info.user` before this):
    proves the three configurable keys LangGraph's own
    _build_server_info (langgraph/pregel/main.py) reads are actually
    set, not just documented in a comment."""
    assignment = {
        "run_id": "run-1",
        "thread_id": "thread-1",
        "graph_id": "my_agent",
        "config": {},
    }
    config = build_run_config(assignment)
    configurable = config["configurable"]
    check("thread_id set", configurable["thread_id"] == "thread-1")
    check("run_id set", configurable["run_id"] == "run-1")
    check("assistant_id set to graph_id", configurable["assistant_id"] == "my_agent")
    check("graph_id set", configurable["graph_id"] == "my_agent")
    check("langgraph_auth_user absent when no user forwarded", "langgraph_auth_user" not in configurable)


def test_build_run_config_sets_langgraph_auth_user_when_authenticated():
    """Proves the identity that authenticated the run's HTTP request
    (see internal/transport.UserContext on the Go side) actually reaches
    the exact configurable key LangGraph's _build_server_info reads --
    the one config key that makes runtime.server_info.user non-None for
    node-level code."""
    assignment = {
        "run_id": "run-1",
        "thread_id": "thread-1",
        "graph_id": "my_agent",
        "config": {},
        "user": {"identity": "alice-123", "display_name": "Alice", "email": "alice@example.com"},
    }
    config = build_run_config(assignment)
    auth_user = config["configurable"]["langgraph_auth_user"]
    check("langgraph_auth_user present when authenticated", auth_user is not None)
    check("langgraph_auth_user.identity correct", auth_user.identity == "alice-123")
    check("langgraph_auth_user has BaseUser-protocol dict access too", auth_user["email"] == "alice@example.com")
    check(
        "user_id shortcut set (LangGraph Platform hosting convention)", config["configurable"]["user_id"] == "alice-123"
    )
    check(
        "user_display_name shortcut set (LangGraph Platform hosting convention)",
        config["configurable"]["user_display_name"] == "Alice",
    )


def test_build_run_config_preserves_existing_configurable_keys():
    """Proves this doesn't clobber caller-supplied config (e.g. a
    run created with its own configurable.foo=bar)."""
    assignment = {
        "run_id": "run-1",
        "thread_id": "thread-1",
        "graph_id": "my_agent",
        "config": {"configurable": {"memories_enabled": False}},
    }
    config = build_run_config(assignment)
    check("caller-supplied configurable keys survive", config["configurable"]["memories_enabled"] is False)
    check("server_info keys added alongside", config["configurable"]["graph_id"] == "my_agent")


def test_build_run_config_maps_checkpoint_ref_to_checkpoint_id():
    """Time-travel: non-empty checkpoint_ref must become LangGraph's
    configurable.checkpoint_id so astream resumes from that past
    checkpoint instead of silently using the thread's latest."""
    assignment = {
        "run_id": "run-1",
        "thread_id": "thread-1",
        "graph_id": "my_agent",
        "checkpoint_ref": "  past-cp-42  ",
    }
    config = build_run_config(assignment)
    check("checkpoint_id set from checkpoint_ref", config["configurable"]["checkpoint_id"] == "past-cp-42")

    no_ref = build_run_config({"run_id": "r", "thread_id": "t", "graph_id": "g", "checkpoint_ref": None})
    check("checkpoint_id absent when checkpoint_ref is null", "checkpoint_id" not in no_ref["configurable"])

    blank = build_run_config({"run_id": "r", "thread_id": "t", "graph_id": "g", "checkpoint_ref": "   "})
    check("checkpoint_id absent for whitespace-only ref", "checkpoint_id" not in blank["configurable"])


def test_build_run_config_tenant_scopes_checkpoint_thread_id():
    """Non-default tenants must not share LangGraph checkpoint rows when
    they reuse the same client-chosen thread_id -- configurable.thread_id
    becomes the checkpointer key."""
    bare = build_run_config({"run_id": "r", "thread_id": "t1", "graph_id": "g"})
    check("absent tenant keeps bare thread_id", bare["configurable"]["thread_id"] == "t1")

    default = build_run_config({"run_id": "r", "thread_id": "t1", "graph_id": "g", "tenant_id": "default"})
    check("default tenant keeps bare thread_id", default["configurable"]["thread_id"] == "t1")

    blank = build_run_config({"run_id": "r", "thread_id": "t1", "graph_id": "g", "tenant_id": "  "})
    check("whitespace tenant keeps bare thread_id", blank["configurable"]["thread_id"] == "t1")

    acme = build_run_config({"run_id": "r", "thread_id": "t1", "graph_id": "g", "tenant_id": "acme"})
    check("non-default tenant prefixes thread_id", acme["configurable"]["thread_id"] == "acme:t1")


async def test_lifecycle_running_emitted_before_slow_factory_construction():
    """Regression test for a real bug found live: the 'lifecycle: running'
    event -- the only signal a client gets that its run was accepted
    before the first real token -- previously fired only AFTER the
    factory function resolved. For a factory graph doing real work at
    construction time (LLM client setup, tool binding -- seconds, not
    milliseconds, in a real production agent), a client saw nothing at
    all until that finished. Proves the event now fires first."""
    events: list[str] = []
    factory_call_started = asyncio.Event()

    async def slow_factory(config, runtime):
        events.append("factory_called")
        factory_call_started.set()
        await asyncio.sleep(0.05)  # stands in for real, slow construction work
        return _make_static_graph().compile()

    adapter = LangGraphAdapter()
    kind, factory_graph = await classify_graph_export(slow_factory)
    check("slow_factory classified as factory (test setup sanity check)", kind == "factory")
    adapter.factories["test_agent"] = factory_graph

    async def event_callback(event: dict):
        events.append(event["method"])

    assignment = {
        "run_id": "run-1",
        "thread_id": "thread-1",
        "graph_id": "test_agent",
        "input": {"messages": []},
        "config": {},
        "stream_modes": ["values"],
    }
    status = await execute_run(adapter, assignment, event_callback)

    check("run completed successfully", status == "success")
    check(
        "lifecycle event recorded before the factory was called (not after)",
        events.index("lifecycle") < events.index("factory_called"),
    )


async def main():
    await test_classify_static_graph()
    await test_classify_already_compiled()
    await test_classify_zero_arg_callable()
    await test_classify_factory_signatures()
    await test_factory_gets_fresh_instance_per_build()
    await test_factory_async_context_manager_variant()
    await test_runtime_user_identity_and_extra_fields()
    await test_runtime_user_none_when_no_auth()
    await test_runtime_user_to_dict_for_non_baseuser_consumers()
    await test_runtime_user_flattens_nested_extra_for_to_dict()
    await test_checkpointer_and_store_attached_per_instance()
    test_build_run_config_sets_server_info_keys()
    test_build_run_config_sets_langgraph_auth_user_when_authenticated()
    test_build_run_config_preserves_existing_configurable_keys()
    test_build_run_config_maps_checkpoint_ref_to_checkpoint_id()
    test_build_run_config_tenant_scopes_checkpoint_thread_id()
    await test_lifecycle_running_emitted_before_slow_factory_construction()
    print("\nAll checks passed.")


if __name__ == "__main__":
    asyncio.run(main())
