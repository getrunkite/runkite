"""Native saver identities must fail before ambiguous legacy keys are touched."""

import unittest
from types import SimpleNamespace
from typing import TypedDict
from unittest.mock import AsyncMock, MagicMock, patch

from langgraph.checkpoint.memory import MemorySaver
from langgraph.graph import END, START, StateGraph
from runkite_runner.tenant_ctx import checkpoint_thread_id
from runkite_runner.worker import LangGraphAdapter, execute_run


def assignment(tenant, thread):
    return {"run_id": "identity-run", "thread_id": thread, "tenant_id": tenant, "graph_id": "graph", "input": {}}


class CheckpointIdentityTests(unittest.IsolatedAsyncioTestCase):
    async def test_legacy_persistent_owner_is_not_inferred_from_a_valid_new_pair(self):
        class LegacyState(TypedDict):
            owner_only: str

        for legacy_tenant, legacy_thread, new_tenant, new_thread in (
            ("default", "acme:target", "acme", "target"),
            ("acme:west", "target", "acme", "west:target"),
        ):
            with self.subTest(legacy_tenant=legacy_tenant):
                builder = StateGraph(LegacyState)
                builder.add_node("keep", lambda state: state)
                builder.add_edge(START, "keep")
                builder.add_edge("keep", END)
                graph = builder.compile(checkpointer=MemorySaver())
                legacy = {"configurable": {"thread_id": checkpoint_thread_id(legacy_tenant, legacy_thread)}}
                await graph.ainvoke({"owner_only": "legacy owner's synthetic conversation"}, legacy)
                adapter = LangGraphAdapter()
                adapter.graphs["graph"] = graph
                adapter._checkpointer_manager = SimpleNamespace(mode="direct-postgres", recover_if_wedged=AsyncMock())
                events = []
                with patch.object(adapter, "get_graph", wraps=adapter.get_graph) as lookup:
                    status = await execute_run(
                        adapter, assignment(new_tenant, new_thread), AsyncMock(side_effect=events.append)
                    )
                    self.assertEqual(status, "error", f"legacy owner data escaped: {events!r}")
                    lookup.assert_not_called()
                self.assertIn("ownership", events[0]["data"]["message"])
                self.assertEqual(adapter._checkpointer_manager.recover_if_wedged.await_count, 0)

    async def assert_native_refused(self, owner_tenant, owner_thread, caller_tenant, caller_thread):
        builder = StateGraph(dict)
        builder.add_node("keep", lambda state: state)
        builder.add_edge(START, "keep")
        builder.add_edge("keep", END)
        graph = builder.compile(checkpointer=MemorySaver())
        owner = {"configurable": {"thread_id": checkpoint_thread_id(owner_tenant, owner_thread)}}
        caller = {"configurable": {"thread_id": checkpoint_thread_id(caller_tenant, caller_thread)}}
        # These preexisting rows demonstrate why the invocation must refuse the
        # ambiguous identity, without silently renaming or importing history.
        self.assertEqual(owner, caller)
        await graph.ainvoke({"owner_only": "synthetic owner conversation"}, owner)
        for mode in ("direct-postgres", "memory"):
            with self.subTest(mode=mode):
                adapter = LangGraphAdapter()
                adapter.graphs["graph"] = graph
                adapter._checkpointer_manager = SimpleNamespace(mode=mode, recover_if_wedged=AsyncMock())
                events = []
                with (
                    patch.object(graph, "aget_state", new_callable=AsyncMock) as read,
                    patch.object(graph, "astream", new_callable=MagicMock) as stream,
                ):
                    status = await execute_run(
                        adapter, assignment(caller_tenant, caller_thread), AsyncMock(side_effect=events.append)
                    )
                    self.assertEqual(status, "error")
                    read.assert_not_called()
                    stream.assert_not_called()
                self.assertEqual([event["method"] for event in events], ["error"])
                self.assertIn("ambiguous native LangGraph", events[0]["data"]["message"])
        self.assertEqual((await graph.aget_state(owner)).values["owner_only"], "synthetic owner conversation")

    async def test_default_thread_cannot_alias_a_named_tenant(self):
        await self.assert_native_refused("acme", "target", "default", "acme:target")

    async def test_colon_tenant_cannot_alias_a_safe_tenant(self):
        await self.assert_native_refused("acme", "west:target", "acme:west", "target")

    async def test_ambiguous_identity_stops_before_factory_construction(self):
        for tenant, thread in (("default", "cron:daily"), ("acme", "target")):
            with self.subTest(tenant=tenant):
                adapter = LangGraphAdapter()
                adapter._checkpointer_manager = SimpleNamespace(mode="direct-postgres", recover_if_wedged=AsyncMock())
                with (
                    patch.object(adapter, "is_factory", return_value=True) as lookup,
                    patch.object(adapter, "build_factory_graph", new_callable=AsyncMock) as factory,
                ):
                    status = await execute_run(adapter, assignment(tenant, thread), AsyncMock())
                self.assertEqual(status, "error")
                lookup.assert_not_called()
                factory.assert_not_called()
                adapter._checkpointer_manager.recover_if_wedged.assert_not_called()

    async def test_proxy_mode_keeps_ambiguous_native_shape_as_exact_plane_identity(self):
        class Graph:
            seen = None

            async def aget_state(self, config):
                return SimpleNamespace(values={})

            async def astream(self, input, config, stream_mode):
                self.seen = config
                yield "values", {"answer": "proxy path"}

        for tenant, thread in (("default", "cron:daily"), ("acme:west", "target")):
            with self.subTest(tenant=tenant):
                graph = Graph()
                adapter = LangGraphAdapter()
                adapter.graphs["graph"] = graph
                adapter._checkpointer_manager = SimpleNamespace(mode="proxy-http", recover_if_wedged=AsyncMock())
                self.assertEqual(await execute_run(adapter, assignment(tenant, thread), AsyncMock()), "success")
                self.assertEqual(graph.seen["configurable"]["thread_id"], checkpoint_thread_id(tenant, thread))

    async def test_safe_existing_native_history_keeps_the_same_key(self):
        class ExistingState(TypedDict):
            prior: str

        for mode, tenant, thread in (
            ("memory", "acme", "west:target"),
            ("direct-postgres", "default", "existing-target"),
        ):
            with self.subTest(mode=mode):
                builder = StateGraph(ExistingState)
                builder.add_node("keep", lambda state: state)
                builder.add_edge(START, "keep")
                builder.add_edge("keep", END)
                graph = builder.compile(checkpointer=MemorySaver())
                key = checkpoint_thread_id(tenant, thread)
                config = {"configurable": {"thread_id": key}}
                await graph.ainvoke({"prior": "existing native conversation"}, config)
                adapter = LangGraphAdapter()
                adapter.graphs["graph"] = graph
                adapter._checkpointer_manager = SimpleNamespace(mode=mode, recover_if_wedged=AsyncMock())
                with patch.object(graph, "aget_state", wraps=graph.aget_state) as read:
                    self.assertEqual(await execute_run(adapter, assignment(tenant, thread), AsyncMock()), "success")
                    self.assertEqual(read.await_args.args[0]["configurable"]["thread_id"], key)
                self.assertEqual((await graph.aget_state(config)).values["prior"], "existing native conversation")


if __name__ == "__main__":
    unittest.main()
